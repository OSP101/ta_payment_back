package service

import (
	"context"
	"log"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/mail"
)

type NotifyService struct {
	pool   *pgxpool.Pool
	mailer *mail.Mailer
	// baseURL is config.AppBaseURL. Links are stored as in-app paths
	// ("/lecturer"), which the bell resolves against the current origin; a mail
	// client has no origin, so the e-mail copy needs the absolute URL.
	baseURL string
}

type Notification struct {
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Link      *string   `json:"link,omitempty"`
	ReadAt    *string   `json:"read_at,omitempty"`
	CreatedAt string    `json:"created_at"`
	Channel   string    `json:"channel"`
}

// Closing lines of the e-mail, in the form of a Thai official letter. The
// in-app copy carries neither the salutation nor the closing: the bell is a
// list of short notices, not a letter.
const (
	closingInform = "จึงเรียนมาเพื่อโปรดทราบ"
	closingAction = "จึงเรียนมาเพื่อโปรดดำเนินการ"
)

// Send emits both in-app and email notification, for news the recipient only
// needs to know about. Email delivery is best-effort: if the SMTP call fails,
// the in-app row still stands so the recipient sees the update on next bell
// open.
func (s *NotifyService) Send(ctx context.Context, userID uuid.UUID, title, body, link string) {
	s.deliver(ctx, userID, title, body, link, closingInform, MailLayout{})
}

// SendAction is Send for a notice that asks the recipient to do something
// (fix a document, approve hours, sign). Only the e-mail's closing differs.
func (s *NotifyService) SendAction(ctx context.Context, userID uuid.UUID, title, body, link string) {
	s.deliver(ctx, userID, title, body, link, closingAction, MailLayout{})
}

// SendLaidOut is Send with a structured e-mail: the in-app row still carries
// the plain body, while the e-mail shows layout's info box, highlight and
// table in place of that body. action picks the closing, as SendAction does.
func (s *NotifyService) SendLaidOut(ctx context.Context, userID uuid.UUID, title, body, link string, action bool, layout MailLayout) {
	closing := closingInform
	if action {
		closing = closingAction
	}
	s.deliver(ctx, userID, title, body, link, closing, layout)
}

func (s *NotifyService) deliver(ctx context.Context, userID uuid.UUID, title, body, link, closing string, layout MailLayout) {
	title, body = plainPunct(title), plainPunct(body)
	linkArg := nilStr(&link)

	// in-app row — the source of truth for the bell/inbox.
	//
	// An UNREAD notice with the same title and link is REFRESHED rather than
	// repeated. Rejections arrive one per assignment, so a TA holding two
	// sections of a course got the same sentence twice, and a lecturer who
	// bounced the batch again got it twice more — four identical lines about one
	// thing. Collapsing on (user, title, link) folds them into the one line the
	// TA actually reads, carrying the newest reason and moving back to the top.
	//
	// Only while unread: once they have seen it, a later notice is news again.
	tag, err := s.pool.Exec(ctx, `
		UPDATE notifications
		SET body = $3, link = $4, created_at = NOW()
		WHERE user_id = $1 AND channel = 'in_app' AND read_at IS NULL
		  AND title = $2 AND link IS NOT DISTINCT FROM $4`,
		userID, title, body, linkArg)
	if err != nil {
		log.Printf("notify in_app coalesce: %v", err)
		return
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO notifications (id, user_id, channel, title, body, link)
			 VALUES (gen_random_uuid(), $1, 'in_app', $2, $3, $4)`,
			userID, title, body, linkArg); err != nil {
			log.Printf("notify in_app: %v", err)
			return
		}
	}

	// email — informational only, so keep failures out of the caller's path.
	var email, prefix, first, last string
	if err := s.pool.QueryRow(ctx, `
		SELECT u.email,
		       COALESCE(NULLIF(tp.prefix, ''), NULLIF(u.title, ''), ''),
		       COALESCE(u.first_name, ''), COALESCE(u.last_name, '')
		  FROM users u
		  LEFT JOIN ta_profiles tp ON tp.user_id = u.id
		 WHERE u.id = $1`, userID).Scan(&email, &prefix, &first, &last); err != nil {
		return
	}
	m := mailContent{
		Title: title, Body: body, Link: absoluteLink(s.baseURL, link),
		Recipient: recipientName(prefix, first, last), Closing: closing,
		Layout: layout, Contact: loadMailContact(ctx, s.pool),
	}
	if err := s.mailer.SendMessage(mail.Message{
		To: email, Subject: title, HTML: renderMailHTML(m), Text: renderMailText(m),
	}); err != nil {
		log.Printf("notify email: %v", err)
		return
	}
	_, _ = s.pool.Exec(ctx,
		`INSERT INTO notifications (id, user_id, channel, title, body, link, sent_at)
		 VALUES (gen_random_uuid(), $1, 'email', $2, $3, $4, NOW())`,
		userID, title, body, linkArg)
}

// List returns the user's in-app notifications, newest first. When
// `unreadOnly` is true, only unread items are returned — used by the bell
// and the "unread" filter tab on the notifications page.
func (s *NotifyService) List(ctx context.Context, userID uuid.UUID, limit int, unreadOnly bool) ([]Notification, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	q := strings.Builder{}
	q.WriteString(`
		SELECT id, title, body, link,
		       TO_CHAR(read_at, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'),
		       TO_CHAR(created_at, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'),
		       channel::text
		  FROM notifications
		 WHERE user_id = $1 AND channel = 'in_app'`)
	if unreadOnly {
		q.WriteString(` AND read_at IS NULL`)
	}
	q.WriteString(` ORDER BY created_at DESC LIMIT $2`)

	rows, err := s.pool.Query(ctx, q.String(), userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Notification, 0, 16)
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.Title, &n.Body, &n.Link, &n.ReadAt, &n.CreatedAt, &n.Channel); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// UnreadCount powers the little red badge on the bell. Cheap enough to call
// on every page load — hits the index on (user_id, created_at DESC).
func (s *NotifyService) UnreadCount(ctx context.Context, userID uuid.UUID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM notifications
		  WHERE user_id = $1 AND channel = 'in_app' AND read_at IS NULL`,
		userID).Scan(&n)
	return n, err
}

func (s *NotifyService) MarkRead(ctx context.Context, userID, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE notifications SET read_at = NOW()
		  WHERE id = $1 AND user_id = $2 AND read_at IS NULL`,
		id, userID)
	return err
}

// MarkAllRead is a single UPDATE on the (user_id, created_at) index — safe
// even for users with thousands of stale notifications.
func (s *NotifyService) MarkAllRead(ctx context.Context, userID uuid.UUID) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE notifications SET read_at = NOW()
		  WHERE user_id = $1 AND channel = 'in_app' AND read_at IS NULL`,
		userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// absoluteLink turns an in-app path into a URL a mail client can open. Anything
// that is not a root-relative path (already absolute, or empty) is left alone,
// as is every link when no base URL is configured.
func absoluteLink(baseURL, link string) string {
	if baseURL == "" || !strings.HasPrefix(link, "/") || strings.HasPrefix(link, "//") {
		return link
	}
	return strings.TrimRight(baseURL, "/") + link
}

// SendMailTo delivers a message to a bare email address: no user row, no in-app
// notification, no delivery record here.
//
// The announcement recipient list is the only caller — its addresses may belong
// to nobody in the system (a guest lecturer, a faculty mailing list), so
// Send()'s user-id-first shape does not fit. Routing it through NotifyService
// anyway keeps the mailer owned in one place instead of handing a second
// service its own copy.
func (s *NotifyService) SendMailTo(to, subject, html string) error {
	return s.mailer.Send(to, subject, html)
}
