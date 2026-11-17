package mail

import (
	"context"
	"encoding/json"
	"strings"
	"ta-payment-back/internal/config"
	"testing"
)

func TestDeliveryObserverDistinguishesDisabledAndFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    config.Config
		status string
	}{
		{"disabled", config.Config{}, "skipped"},
		{"failed", config.Config{SMTPHost: "127.0.0.1", SMTPPort: 1}, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(tc.cfg)
			var events []DeliveryEvent
			m.Observe(func(_ context.Context, e DeliveryEvent) { events = append(events, e) })
			ctx := WithDeliveryMeta(context.Background(), DeliveryMeta{Source: "outbox", JobID: "job-a", Attempt: 2, Name: "Synthetic recipient"})
			err := m.SendMessageContext(ctx, Message{To: "recipient@example.test", Subject: "Synthetic notice", Text: "private credential", HTML: "private body", Attachments: []Attachment{{Name: "private file", Data: []byte("private document")}}})
			if tc.status == "failed" && err == nil {
				t.Fatal("expected SMTP failure")
			}
			if len(events) != 2 || events[0].Status != "started" || events[1].Status != tc.status {
				t.Fatalf("wrong lifecycle: %+v", events)
			}
			start, end := events[0], events[1]
			if start.ID == "" || start.ID != end.ID || end.FinishedAt == nil || end.FinishedAt.Before(start.StartedAt) {
				t.Fatal("missing correlation or timestamps")
			}
			if end.JobID != "job-a" || end.Attempt != 2 || end.To != "recipient@example.test" {
				t.Fatal("lost routing information")
			}
			raw, _ := json.Marshal(events)
			if strings.Contains(string(raw), "private") {
				t.Fatal("observer retained message content")
			}
		})
	}
}

func TestDeliveryObserverRecordsSMTPAcceptance(t *testing.T) {
	tlsConfig, roots := rsaOnlyTLS(t)
	port, got := fakeStartTLS(t, tlsConfig)
	m := New(config.Config{SMTPHost: "127.0.0.1", SMTPPort: port, SMTPEncryption: "tls", SMTPTLSLegacyCiphers: true, MailFrom: "sender@example.test"})
	m.rootCAs = roots
	var last DeliveryEvent
	m.Observe(func(_ context.Context, e DeliveryEvent) { last = e })
	if err := m.SendMessage(Message{To: "recipient@example.test", Subject: "Accepted", Text: "private"}); err != nil {
		t.Fatal(err)
	}
	<-got
	if last.Status != "accepted" || last.FinishedAt == nil || last.From != "sender@example.test" {
		t.Fatalf("wrong acceptance: %+v", last)
	}
}
