package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Only best-effort notices use this queue. Checked delivery / credential mail
// remain synchronous so their callers never mistake "queued" for "delivered".
var errNoticeTooLarge = errors.New("notification exceeds queue limit")

type queuedNotice struct {
	Title string
	Body string
	Link string
	Closing string
	Layout MailLayout
}

func (s *NotifyService) enqueue(ctx context.Context, user uuid.UUID, title, body, link, closing string, layout MailLayout) error {
	title, body = plainPunct(title), plainPunct(body)
	id := uuid.New()
	raw, err := json.Marshal(queuedNotice{title, body, link, closing, layout})
	if err != nil { return err }
	if len(raw) > 2*1024*1024 { return errNoticeTooLarge }
	payload, err := s.queueCipher.Seal([]byte("notification-outbox:"+id.String()), raw)
	if err != nil { return err }
	tx, err := s.pool.Begin(ctx)
	if err != nil { return err }
	defer tx.Rollback(ctx)
	// Match user deletion's row lock; never enqueue after the deletion commits.
	var active bool
	if err := tx.QueryRow(ctx, `SELECT deleted_at IS NULL AND is_active FROM users WHERE id=$1 FOR UPDATE`, user).Scan(&active); err != nil { return err }
	if !active { return fmt.Errorf("notification recipient unavailable") }
	if err := writeInAppWith(ctx, tx, user, title, body, nilStr(&link)); err != nil { return err }
	if _, err := tx.Exec(ctx, `INSERT INTO notification_outbox(id,user_id,payload_enc) VALUES($1,$2,$3)`, id, user, payload); err != nil { return err }
	return tx.Commit(ctx)
}

// RunOutbox limits mail to one in-flight informational notice per process.
// SKIP LOCKED leases support multiple API replicas without concurrent claims.
// Delivery is at-least-once: an SMTP acceptance followed by a process failure
// can cause a duplicate notice. This worker NEVER approves or sends payments.
func (s *NotifyService) RunOutbox(ctx context.Context) {
	if s.queueCipher == nil { return }
	ticks := 0
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done(): return
		case <-ticker.C:
			ticks++
			if ticks%3600 == 0 {
				gcCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_, _ = s.pool.Exec(gcCtx, `WITH expired AS (SELECT id FROM notification_outbox WHERE state IN ('sent','failed') AND finished_at<now()-interval '30 days' LIMIT 500) DELETE FROM notification_outbox q USING expired e WHERE q.id=e.id`)
				cancel()
			}
			if err := s.processNotice(ctx); err != nil && ctx.Err() == nil { slog.Error("notification worker failed") }
		}
	}
}

func (s *NotifyService) processNotice(ctx context.Context) error {
	var id, user uuid.UUID
	var payload []byte
	var attempts int
	lease := uuid.New()
	err := s.pool.QueryRow(ctx, `WITH due AS (
	    SELECT id FROM notification_outbox
	    WHERE (state='pending' AND available_at<=now())
	       OR (state='processing' AND lease_until<now())
	    ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED
	) UPDATE notification_outbox q SET state='processing', lease_until=now()+interval '5 minutes',
	    lease_token=$1, attempts=LEAST(attempts+1,5)
	  FROM due WHERE q.id=due.id
	  RETURNING q.id,q.user_id,q.payload_enc,q.attempts`, lease).Scan(&id, &user, &payload, &attempts)
	if errors.Is(err, pgx.ErrNoRows) { return nil }
	if err != nil { return err }
	raw, deliveryErr := s.queueCipher.Open([]byte("notification-outbox:"+id.String()), payload)
	var n queuedNotice
	if deliveryErr == nil { deliveryErr = json.Unmarshal(raw, &n) }
	if deliveryErr == nil { deliveryErr = s.sendEmail(ctx, user, n.Title, n.Body, n.Link, nilStr(&n.Link), n.Closing, n.Layout) }
	// Finalize even during shutdown, otherwise a delivered notice is retried.
	finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state := "sent"
	if deliveryErr != nil {
		state = "pending"
		if attempts >= 5 { state = "failed" }
		slog.Warn("notification delivery retry", "job_id", id, "attempt", attempts)
	}
	_, err = s.pool.Exec(finishCtx, `UPDATE notification_outbox
	    SET state=$3, lease_token=NULL, lease_until=NULL,
	        available_at=now()+($4::int*interval '1 minute'),
	        payload_enc=CASE WHEN $3 IN ('sent','failed') THEN NULL ELSE payload_enc END,
	        finished_at=CASE WHEN $3 IN ('sent','failed') THEN now() ELSE NULL END
	    WHERE id=$1 AND lease_token=$2`, id, lease, state, attempts*attempts)
	return err
}
