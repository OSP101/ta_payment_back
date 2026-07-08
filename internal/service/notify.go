package service

import (
	"context"
	"log"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/mail"
)

type NotifyService struct {
	pool   *pgxpool.Pool
	mailer *mail.Mailer
}

type Notification struct {
	ID      uuid.UUID `json:"id"`
	Title   string    `json:"title"`
	Body    string    `json:"body"`
	Link    *string   `json:"link,omitempty"`
	ReadAt  *string   `json:"read_at,omitempty"`
	Channel string    `json:"channel"`
}

// Send emits both in-app and email notification.
func (s *NotifyService) Send(ctx context.Context, userID uuid.UUID, title, body, link string) {
	// in-app
	_, err := s.pool.Exec(ctx,
		`INSERT INTO notifications (id, user_id, channel, title, body, link)
		 VALUES (gen_random_uuid(), $1, 'in_app', $2, $3, $4)`,
		userID, title, body, nilStr(&link))
	if err != nil {
		log.Printf("notify in_app: %v", err)
	}
	// email
	var email string
	if err := s.pool.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, userID).Scan(&email); err == nil {
		if err := s.mailer.Send(email, title, body); err == nil {
			_, _ = s.pool.Exec(ctx,
				`INSERT INTO notifications (id, user_id, channel, title, body, link, sent_at)
				 VALUES (gen_random_uuid(), $1, 'email', $2, $3, $4, NOW())`,
				userID, title, body, nilStr(&link))
		}
	}
}

func (s *NotifyService) List(ctx context.Context, userID uuid.UUID, limit int) ([]Notification, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, title, body, link, TO_CHAR(read_at, 'YYYY-MM-DD"T"HH24:MI:SSOF'), channel::text
		FROM notifications WHERE user_id=$1 AND channel='in_app' ORDER BY created_at DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notification
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.Title, &n.Body, &n.Link, &n.ReadAt, &n.Channel); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func (s *NotifyService) MarkRead(ctx context.Context, userID, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE notifications SET read_at=NOW() WHERE id=$1 AND user_id=$2 AND read_at IS NULL`,
		id, userID)
	return err
}
