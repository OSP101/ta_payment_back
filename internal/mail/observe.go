package mail

import (
	"context"
	"time"
)

// DeliveryMeta carries routing information only, never message bodies or secrets.
type DeliveryMeta struct {
	UserID  string `json:"recipient_id,omitempty"`
	Name    string `json:"recipient_name,omitempty"`
	Source  string `json:"source"`
	JobID   string `json:"job_id,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
}
type deliveryKey struct{}

func WithDeliveryMeta(ctx context.Context, meta DeliveryMeta) context.Context {
	return context.WithValue(ctx, deliveryKey{}, meta)
}
func DeliveryMetaFrom(ctx context.Context) DeliveryMeta {
	meta, _ := ctx.Value(deliveryKey{}).(DeliveryMeta)
	if meta.Source == "" {
		meta.Source = "direct"
	}
	return meta
}

type DeliveryEvent struct {
	DeliveryMeta
	ID         string     `json:"delivery_id"`
	To         string     `json:"to"`
	Subject    string     `json:"subject"`
	From       string     `json:"from"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Millis     int64      `json:"duration_ms"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
}

// Observe installs a synchronous observer at startup, before serving requests.
// It sees metadata only; HTML, text and attachments never enter the audit trail.
func (m *Mailer) Observe(fn func(context.Context, DeliveryEvent)) { m.observe = fn }
