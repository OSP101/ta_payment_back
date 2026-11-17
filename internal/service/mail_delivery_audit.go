package service

import (
	"context"
	"log"
	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/mail"
	"time"
)

func observeMailDelivery(m *mail.Mailer, aud *audit.Auditor) {
	if m == nil || aud == nil {
		return
	}
	m.Observe(func(ctx context.Context, event mail.DeliveryEvent) {
		recordMailEvent(ctx, aud, "mail.delivery."+event.Status, event.ID, event,
			event.To+" | "+event.Name+" | "+event.Subject+" | "+event.JobID)
	})
}

func recordMailEvent(ctx context.Context, aud *audit.Auditor, action, id string, data any, note string) {
	// Preserve request correlation even when SMTP outlives a cancelled request.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := aud.Log(ctx, audit.Entry{Action: action, Entity: "mail_delivery", EntityID: id, After: data, Note: note}); err != nil {
		log.Printf("mail delivery audit: recording %s failed", action)
	}
}
