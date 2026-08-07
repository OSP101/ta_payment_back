package service

// announce_reach.go is everything an announcement does BEYOND the in-app feed:
// mailing named addresses, and being readable without an account.
//
// The role fanout in announce.go reaches accounts that already exist and hold
// an audience role. That leaves out the people this file serves: a guest
// lecturer with no account, a faculty mailing list, or one specific person the
// officer wants to be sure sees it.

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// truncateRunes cuts s to at most n CHARACTERS, never mid-character.
//
// Go slices strings by byte. Thai is three bytes per character, so the old
// `body[:240]` landed inside a glyph on essentially every real announcement
// and the tail arrived as "\xe0\xb8" — a replacement square in the notification
// and in the email. Confirmed on 06/08/2026 against a sample body: the cut
// produced invalid UTF-8.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	count := 0
	for idx := range s { // ranging a string steps by rune, not byte
		if count == n {
			return strings.TrimSpace(s[:idx]) + "…"
		}
		count++
	}
	return s
}

// loadRecipients reads the ledger for the composer.
func (s *AnnounceService) loadRecipients(ctx context.Context, id uuid.UUID) ([]AnnouncementRecipient, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.email, r.user_id, r.status, r.sent_at, COALESCE(r.error, ''),
		       COALESCE(u.first_name || ' ' || u.last_name, '')
		  FROM announcement_recipients r
		  LEFT JOIN users u ON u.id = r.user_id
		 WHERE r.announcement_id = $1
		 ORDER BY r.created_at, r.email`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AnnouncementRecipient{}
	for rows.Next() {
		var r AnnouncementRecipient
		if err := rows.Scan(&r.Email, &r.UserID, &r.Status, &r.SentAt, &r.Error, &r.Name); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Deliver sends an announcement to everyone on its ledger who has not had it
// yet: the in-app notification and the email, in one pass.
//
// This is the ONLY delivery path. It used to be two — a role fanout and a
// separate loop for extra addresses — which was safe only while the two read
// different sets. Now that targeting materialises one audience, two loops over
// it would mail every person twice.
//
// Safe to call repeatedly. Rows already sent are skipped, so the "ส่งอีกครั้ง"
// button reaches only people added since.
func (s *AnnounceService) Deliver(ctx context.Context, id uuid.UUID) (sent int, err error) {
	var (
		title, body, category string
		publishedAt           *time.Time
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT title, body, category, published_at
		  FROM announcements WHERE id = $1`, id).Scan(
		&title, &body, &category, &publishedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	// Telling people about a draft sends them to a page that is not there.
	if publishedAt == nil || publishedAt.After(time.Now()) {
		return 0, Invalid("ยังไม่ได้เผยแพร่ประกาศนี้ จึงยังส่งไม่ได้")
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id FROM announcement_recipients
		 WHERE announcement_id = $1 AND status = 'pending' AND user_id IS NOT NULL
		 ORDER BY created_at`, id)
	if err != nil {
		return 0, err
	}
	type target struct{ rowID, userID uuid.UUID }
	targets := []target{}
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.rowID, &t.userID); err != nil {
			rows.Close()
			return 0, err
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	subject := announceSubject(category, title)
	// Strip the markup before it goes anywhere that cannot render it: the bell
	// preview and the email would otherwise show raw "**" and ":::center".
	preview := truncateRunes(announceBodyPlain(body), 240)
	link := "/announcements/" + id.String()

	for _, t := range targets {
		// notify.Send writes the in-app row and mails the user; its failures
		// are logged inside rather than returned, because one unreachable
		// mailbox must not stop the rest of the announcement.
		s.notify.Send(ctx, t.userID, subject, preview, link)
		if _, err := s.pool.Exec(ctx, `
			UPDATE announcement_recipients
			   SET status='sent', sent_at=NOW(), error=NULL WHERE id=$1`, t.rowID); err != nil {
			return sent, err
		}
		sent++
	}
	log.Printf("announce.deliver: id=%s sent=%d", id, sent)
	return sent, nil
}

// announceSubject prefixes the title the way the in-app fanout does, so the
// same announcement reads identically in the inbox and in the bell.
func announceSubject(category, title string) string {
	switch category {
	case "urgent":
		return "[ด่วน] " + title
	case "warning":
		return "[แจ้งเตือน] " + title
	case "event":
		return "[กิจกรรม] " + title
	}
	return title
}

// PublicGet returns an announcement to a reader with no account.
//
// Everything that could leak is checked here rather than by the caller: the
// row must be opted into public sharing AND currently live. A draft, an
// expired notice, or any announcement staff did not open is reported as
// missing — the same answer an unknown id gets, so the endpoint cannot be used
// to discover which announcements exist.
func (s *AnnounceService) PublicGet(ctx context.Context, id uuid.UUID) (*Announcement, error) {
	var a Announcement
	err := s.pool.QueryRow(ctx, `
		SELECT id, title, body, category, cover_image_key, published_at, expires_at
		  FROM announcements
		 WHERE id = $1
		   AND is_public
		   AND published_at IS NOT NULL AND published_at <= NOW()
		   AND (expires_at IS NULL OR expires_at > NOW())`, id).Scan(
		&a.ID, &a.Title, &a.Body, &a.Category,
		&a.CoverImageKey, &a.PublishedAt, &a.ExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	a.CoverImageURL = coverURL(a.CoverImageKey)
	a.IsPublic = true
	a.Status = "live"
	// 200 runes: enough for Facebook's description line, short enough that it is
	// never truncated mid-card by the platform instead.
	a.Excerpt = truncateRunes(announceBodyPlain(a.Body), 200)
	if att, err := s.loadAttachments(ctx, id); err == nil {
		a.Attachments = att
	}
	// Audience, pinning, and the recipient ledger are internal bookkeeping;
	// a public reader gets the notice itself and nothing about who else got it.
	return &a, nil
}
