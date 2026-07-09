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

// Send emits both in-app and email notification. Email delivery is
// best-effort: if the SMTP call fails, the in-app row still stands so the
// recipient sees the update on next bell open.
func (s *NotifyService) Send(ctx context.Context, userID uuid.UUID, title, body, link string) {
	linkArg := nilStr(&link)

	// in-app row — the source of truth for the bell/inbox.
	_, err := s.pool.Exec(ctx,
		`INSERT INTO notifications (id, user_id, channel, title, body, link)
		 VALUES (gen_random_uuid(), $1, 'in_app', $2, $3, $4)`,
		userID, title, body, linkArg)
	if err != nil {
		log.Printf("notify in_app: %v", err)
		return
	}

	// email — informational only, so keep failures out of the caller's path.
	var email string
	if err := s.pool.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, userID).Scan(&email); err != nil {
		return
	}
	// Wrap plain body in a minimal HTML template so mail clients render nicely.
	html := renderMailHTML(title, body, link)
	if err := s.mailer.Send(email, title, html); err != nil {
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
		       TO_CHAR(read_at, 'YYYY-MM-DD"T"HH24:MI:SSOF'),
		       TO_CHAR(created_at, 'YYYY-MM-DD"T"HH24:MI:SSOF'),
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

// renderMailHTML wraps a notification body in a minimal responsive HTML
// shell. Text is HTML-escaped inline (no external template) so a title with
// "<" or "&" can't accidentally inject markup.
func renderMailHTML(title, body, link string) string {
	esc := func(s string) string {
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;")
		return r.Replace(s)
	}
	linkHTML := ""
	if link != "" {
		linkHTML = `<p style="margin:16px 0 0"><a href="` + esc(link) + `" style="color:#0f766e;text-decoration:underline">เปิดดูรายละเอียด</a></p>`
	}
	return `<!doctype html><html><body style="font-family:-apple-system,Segoe UI,Roboto,sans-serif;background:#f8fafc;padding:24px;color:#0f172a">
<table role="presentation" style="max-width:560px;margin:auto;background:#fff;border-radius:12px;border:1px solid #e2e8f0;padding:24px">
<tr><td>
  <h2 style="margin:0 0 12px;font-size:18px">` + esc(title) + `</h2>
  <div style="white-space:pre-wrap;font-size:14px;line-height:1.6">` + esc(body) + `</div>
  ` + linkHTML + `
  <p style="margin:24px 0 0;color:#64748b;font-size:12px">อีเมลนี้ส่งจากระบบ TA Payment คณะวิทยาการคอมพิวเตอร์ ม.ขอนแก่น (COCO KKU)</p>
</td></tr></table>
</body></html>`
}
