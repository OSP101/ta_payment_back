package alertmail

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"ta-payment-back/internal/mail"
)

type sender struct {
	messages []mail.Message
	err      error
}

func (s *sender) SendMessage(m mail.Message) error { s.messages = append(s.messages, m); return s.err }

func TestAuthenticatedAlertsAndDeliveryRetry(t *testing.T) {
	s := &sender{}
	h := New(s, "private-token", []string{"admin@example.com"})
	body := `{"status":"firing","alerts":[{"labels":{"alertname":"APIDown","secret":"do-not-copy"},"annotations":{"summary":"<script>bad</script>"}}]}`
	for _, test := range []struct {
		token   string
		failure error
		status  int
	}{{"wrong", nil, 401}, {"private-token", nil, 204}, {"private-token", errors.New("SMTP refused"), 502}} {
		s.err = test.failure
		r := httptest.NewRequest("POST", "/alerts", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+test.token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("status %d, expected %d", w.Code, test.status)
		}
	}
	if len(s.messages) != 2 {
		t.Fatal("unauthenticated request sent email")
	}
	if strings.Contains(s.messages[0].HTML, "<script>") || strings.Contains(s.messages[0].Text, "do-not-copy") {
		t.Fatal("unsafe webhook content in email")
	}
}

func TestRejectsMalformedOversizedAndEmptyAlerts(t *testing.T) {
	s := &sender{}
	h := New(s, "token", []string{"admin@example.com"})
	for _, body := range []string{`{}`, `{"status":"firing","alerts":[]}`, strings.Repeat("x", 513<<10)} {
		r := httptest.NewRequest("POST", "/alerts", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 || len(s.messages) != 0 {
			t.Fatal("invalid payload accepted")
		}
	}
}
