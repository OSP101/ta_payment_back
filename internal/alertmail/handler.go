// Package alertmail delivers Alertmanager notifications through the same SMTP
// implementation as the app, while running independently of the API/database.
package alertmail

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"

	"ta-payment-back/internal/mail"
)

type Sender interface{ SendMessage(mail.Message) error }
type Handler struct {
	sender     Sender
	token      string
	recipients []string
	busy       chan struct{}
}

func New(sender Sender, token string, recipients []string) *Handler {
	return &Handler{sender, token, recipients, make(chan struct{}, 1)}
}

type Payload struct {
	Status string `json:"status"`
	Alerts []struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		StartsAt    string            `json:"startsAt"`
	} `json:"alerts"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if h.token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.token)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	select {
	case h.busy <- struct{}{}:
		defer func() { <-h.busy }()
	default:
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	var p Payload
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512<<10))
	if err := decoder.Decode(&p); err != nil || len(p.Alerts) == 0 || len(p.Alerts) > 50 || p.Status != "firing" && p.Status != "resolved" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var body strings.Builder
	fmt.Fprintf(&body, "TA Payment monitoring: %s\n\n", p.Status)
	for _, a := range p.Alerts {
		// Select fields; do not dump arbitrary labels, URLs or webhook bodies.
		for _, line := range []string{a.Labels["alertname"], a.Labels["severity"], a.Annotations["summary"], a.Annotations["description"], a.StartsAt} {
			if len(line) > 2000 {
				line = line[:2000]
			}
			body.WriteString(line + "\n")
		}
		body.WriteString("\n")
	}
	for _, recipient := range h.recipients {
		if err := h.sender.SendMessage(mail.Message{To: recipient, Subject: "Monitoring " + p.Status, Text: body.String(), HTML: "<pre>" + html.EscapeString(body.String()) + "</pre>"}); err != nil {
			// Returning a failure makes Alertmanager retain/retry delivery.
			http.Error(w, "notification delivery failed", http.StatusBadGateway)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
