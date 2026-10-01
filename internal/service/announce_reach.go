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
	"ta-payment-back/internal/audit"
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
		SELECT r.email, r.user_id, r.status, r.sent_at, r.read_at, COALESCE(r.error, ''),
		       COALESCE(COALESCE(NULLIF(u.title,''), '') || u.first_name || ' ' || u.last_name, '')
		  FROM announcement_recipients r
		  LEFT JOIN users u ON u.id = r.user_id
		 WHERE r.announcement_id = $1
		 ORDER BY u.first_name, u.last_name, r.email`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AnnouncementRecipient{}
	for rows.Next() {
		var r AnnouncementRecipient
		if err := rows.Scan(&r.Email, &r.UserID, &r.Status, &r.SentAt, &r.ReadAt, &r.Error, &r.Name); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeliveryResult is what one delivery pass did.
type DeliveryResult struct {
	Sent   int `json:"sent"`
	Failed int `json:"failed"`
}

// Deliver sends an announcement to everyone on its ledger who has not had it
// yet: the in-app notification and the email, in one pass.
//
// This is the ONLY delivery path. It used to be two — a role fanout and a
// separate loop for extra addresses — which was safe only while the two read
// different sets. Now that targeting materialises one audience, two loops over
// it would mail every person twice.
//
// Safe to call repeatedly. Rows already sent are skipped.
func (s *AnnounceService) Deliver(ctx context.Context, id uuid.UUID) (sent int, err error) {
	res, err := s.deliver(ctx, id, false)
	return res.Sent, err
}

// Redeliver is Deliver plus a second try for the people whose email failed.
// This is the "ส่งซ้ำ" button; the automatic paths never retry on their own, so
// a dead mailbox is not hammered on every save.
func (s *AnnounceService) Redeliver(ctx context.Context, id uuid.UUID) (DeliveryResult, error) {
	return s.deliver(ctx, id, true)
}

func (s *AnnounceService) deliver(ctx context.Context, id uuid.UUID, retryFailed bool) (res DeliveryResult, err error) {
	var (
		title, body, category string
		publishedAt           *time.Time
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT title, body, category, published_at
		  FROM announcements WHERE id = $1`, id).Scan(
		&title, &body, &category, &publishedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return res, ErrNotFound
		}
		return res, err
	}
	// Telling people about a draft sends them to a page that is not there.
	if publishedAt == nil || publishedAt.After(time.Now()) {
		return res, Invalid("ยังไม่ได้เผยแพร่ประกาศนี้ จึงยังส่งไม่ได้")
	}

	statuses := []string{"pending"}
	if retryFailed {
		statuses = append(statuses, "failed")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, status FROM announcement_recipients
		 WHERE announcement_id = $1 AND status = ANY($2) AND user_id IS NOT NULL
		 ORDER BY created_at`, id, statuses)
	if err != nil {
		return res, err
	}
	type target struct {
		rowID, userID uuid.UUID
		status        string
	}
	targets := []target{}
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.rowID, &t.userID, &t.status); err != nil {
			rows.Close()
			return res, err
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	subject := announceSubject(category, title)
	// Strip the markup before it goes anywhere that cannot render it: the bell
	// preview and the email would otherwise show raw "**" and ":::center".
	preview := truncateRunes(announceBodyPlain(body), 240)
	link := "/announcements/" + id.String()

	for _, t := range targets {
		// One unreachable mailbox must not stop the rest of the announcement,
		// so a failure is written on that person's row and the loop goes on.
		// A row that already failed has its bell line; only the email is retried.
		var sendErr error
		if t.status == "failed" {
			sendErr = s.notify.SendEmailOnly(ctx, t.userID, subject, preview, link)
		} else {
			sendErr = s.notify.SendChecked(ctx, t.userID, subject, preview, link)
		}
		if sendErr != nil {
			if _, err := s.pool.Exec(ctx, `
				UPDATE announcement_recipients
				   SET status='failed', error=$2 WHERE id=$1`,
				t.rowID, truncateRunes(sendErr.Error(), 300)); err != nil {
				return res, err
			}
			res.Failed++
			continue
		}
		if _, err := s.pool.Exec(ctx, `
			UPDATE announcement_recipients
			   SET status='sent', sent_at=NOW(), error=NULL WHERE id=$1`, t.rowID); err != nil {
			return res, err
		}
		res.Sent++
	}
	log.Printf("announce.deliver: id=%s sent=%d failed=%d", id, res.Sent, res.Failed)
	return res, nil
}

// MarkRead records that a recipient opened the announcement. First open only.
func (s *AnnounceService) MarkRead(ctx context.Context, id, userID uuid.UUID) {
	if _, err := s.pool.Exec(ctx, `
		UPDATE announcement_recipients r SET read_at = NOW()
		 WHERE r.announcement_id = $1 AND r.user_id = $2 AND r.read_at IS NULL
		   AND EXISTS (SELECT 1 FROM announcements a
		                WHERE a.id = r.announcement_id
		                  AND a.published_at IS NOT NULL AND a.published_at <= NOW())`, id, userID); err != nil {
		log.Printf("announce.markread %s: %v", id, err)
	}
}

// remindEvery is the shortest gap between two reminders of one announcement.
const remindEvery = 24 * time.Hour

// RemindUnread nudges the people who were sent the announcement and have not
// opened it. Everyone else is left alone — that is the point of tracking reads.
func (s *AnnounceService) RemindUnread(ctx context.Context, actor, id uuid.UUID) (int, error) {
	var (
		title, body, category  string
		publishedAt, expiresAt *time.Time
		remindedAt             *time.Time
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT title, body, category, published_at, expires_at, reminded_at
		  FROM announcements WHERE id = $1`, id).Scan(
		&title, &body, &category, &publishedAt, &expiresAt, &remindedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	if deriveStatus(publishedAt, expiresAt) != "live" {
		return 0, Invalid("เตือนซ้ำได้เฉพาะประกาศที่กำลังเผยแพร่อยู่")
	}
	if remindedAt != nil && time.Since(*remindedAt) < remindEvery {
		return 0, Invalid("ประกาศนี้เพิ่งเตือนซ้ำไปแล้ว เตือนได้อีกครั้งหลังผ่านไป 24 ชั่วโมง")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT user_id FROM announcement_recipients
		 WHERE announcement_id = $1 AND status = 'sent' AND read_at IS NULL AND user_id IS NOT NULL`, id)
	if err != nil {
		return 0, err
	}
	ids := []uuid.UUID{}
	for rows.Next() {
		var u uuid.UUID
		if err := rows.Scan(&u); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, Invalid("ทุกคนเปิดอ่านประกาศนี้แล้ว ไม่มีใครต้องเตือน")
	}
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "announce.remind", Entity: "announcement", EntityID: id.String(),
			After: map[string]any{"reminded": len(ids)}},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE announcements SET reminded_at = NOW() WHERE id = $1`, id)
			return err
		}); err != nil {
		return 0, err
	}
	// The title differs from the first notice on purpose: an unread bell line
	// with the same title would be folded into the old one and not move.
	subject := "เตือนอีกครั้ง: " + announceSubject(category, title)
	preview := truncateRunes(announceBodyPlain(body), 240)
	link := "/announcements/" + id.String()
	for _, u := range ids {
		s.notify.Send(ctx, u, subject, preview, link)
	}
	return len(ids), nil
}

// SendTest mails the officer a copy of what recipients would get, without
// saving or publishing anything.
func (s *AnnounceService) SendTest(ctx context.Context, actor uuid.UUID, title, body, category string) error {
	title = strings.TrimSpace(title)
	if title == "" || strings.TrimSpace(body) == "" {
		return Invalid("กรุณากรอกหัวข้อและเนื้อหาก่อนส่งทดสอบ")
	}
	subject := "[ทดสอบ] " + announceSubject(category, title)
	if err := s.notify.SendEmailOnly(ctx, actor, subject, truncateRunes(announceBodyPlain(body), 240), ""); err != nil {
		return Invalid("ส่งอีเมลทดสอบไม่สำเร็จ: " + truncateRunes(err.Error(), 200))
	}
	return nil
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
	a.Excerpt = announceExcerpt(a.Body, 200)
	if att, err := s.loadAttachments(ctx, id); err == nil {
		a.Attachments = att
	}
	// Audience, pinning, and the recipient ledger are internal bookkeeping;
	// a public reader gets the notice itself and nothing about who else got it.
	return &a, nil
}
