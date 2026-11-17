package service

import (
	"bytes"
	"encoding/json"
	"github.com/google/uuid"
	"strings"
	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/pii"
	"testing"
	"time"
)

func TestMailDeliveryAuditSearchAndQueueCorrelation(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	m := mail.New(config.Config{})
	observeMailDelivery(m, audit.New(f.Pool))
	job := uuid.NewString()
	ctx := mail.WithDeliveryMeta(f.ctx, mail.DeliveryMeta{Source: "outbox", JobID: job, Attempt: 2, Name: "Synthetic Recipient"})
	if err := m.SendMessageContext(ctx, mail.Message{To: "synthetic@example.test", Subject: "Queue notice", Text: "private password"}); err != nil {
		t.Fatal(err)
	}
	svc := auditSvc(f)
	rows, total, err := svc.ListAudit(ctx, AuditQuery{Entity: "mail_delivery", Q: "synthetic@example.test", MailJobID: job})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("want both steps, got %d", total)
	}
	var found bool
	for _, r := range rows {
		if strings.Contains(string(r.After), "private password") {
			t.Fatal("message content leaked into audit")
		}
		if r.Action == "mail.delivery.skipped" {
			found = true
		}
		if r.ActorKind != "system" {
			t.Fatal("mail event not identified as automatic")
		}
		var fields map[string]any
		if err := json.Unmarshal(r.After, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["job_id"] != job {
			t.Fatal("lost queue id")
		}
	}
	if !found {
		t.Fatal("disabled SMTP recorded as successful delivery")
	}
	_, total, err = svc.ListAudit(ctx, AuditQuery{MailJobID: uuid.NewString()})
	if err != nil || total != 0 {
		t.Fatalf("queue filter leaked unrelated events: %d %v", total, err)
	}
}

func TestMailQueueAuditRecordsRetryScheduleWithoutBody(t *testing.T) {
	s, user, ctx := notifyFixture(t)
	cipher, err := pii.New(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s.queueCipher = cipher
	s.mailer = mail.New(config.Config{SMTPHost: "127.0.0.1", SMTPPort: 1})
	observeMailDelivery(s.mailer, audit.New(s.pool))
	if err := s.enqueue(ctx, user, "Synthetic queue notice", "private message body", "/ta", closingInform, MailLayout{}); err != nil {
		t.Fatal(err)
	}
	if err := s.processNotice(ctx); err != nil {
		t.Fatal(err)
	}
	var job string
	var scheduled time.Time
	if err := s.pool.QueryRow(ctx, `SELECT id::text,available_at FROM notification_outbox WHERE user_id=$1`, user).Scan(&job, &scheduled); err != nil {
		t.Fatal(err)
	}
	rows, total, err := (&AuditService{pool: s.pool}).ListAudit(ctx, AuditQuery{MailJobID: job})
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Fatalf("want queued, started, failed, retry; got %d", total)
	}
	var retry bool
	for _, r := range rows {
		if strings.Contains(string(r.After), "private message body") {
			t.Fatal("queue audit leaked body")
		}
		if r.Action != "mail.delivery.retry" {
			continue
		}
		retry = true
		var fields struct {
			RetryAt time.Time `json:"retry_at"`
			Attempt int       `json:"attempt"`
			To      string    `json:"to"`
		}
		if err := json.Unmarshal(r.After, &fields); err != nil {
			t.Fatal(err)
		}
		if fields.Attempt != 1 || fields.To == "" || !fields.RetryAt.Equal(scheduled) {
			t.Fatalf("incorrect retry routing or schedule: %+v, scheduled %v", fields, scheduled)
		}
	}
	if !retry {
		t.Fatal("missing retry event")
	}
}
