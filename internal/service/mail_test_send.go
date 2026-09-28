package service

import (
	"context"
	netmail "net/mail"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/timeutil"
)

// mail_test_send.go backs the "ทดสอบเมลเซิร์ฟเวอร์" panel: show the mail
// configuration (never the password), check the connection step by step, and
// send one real test e-mail in the production template.

// ServerInfo is the mail configuration as it is safe to show staff.
func (s *MailSettingsService) ServerInfo() mail.Info {
	if s.mailer == nil {
		return mail.Info{}
	}
	return s.mailer.Info()
}

// CheckServer runs the handshake without sending anything, and records that it
// was run: an admin probing the mail server is worth a line in the trail.
func (s *MailSettingsService) CheckServer(ctx context.Context, actor uuid.UUID) mail.CheckResult {
	if s.mailer == nil {
		return mail.CheckResult{}
	}
	res := s.mailer.Check()
	_ = s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "mail.check", Entity: "mail_settings", EntityID: "smtp",
		After: map[string]any{"ok": res.OK, "steps": len(res.Steps)}})
	return res
}

// Test sends are limited per person: the button sends real mail to any
// address typed, so without a limit it is a spam tool.
const (
	testSendLimit  = 5
	testSendWindow = 10 * time.Minute
)

var (
	testSendMu  sync.Mutex
	testSendLog = map[uuid.UUID][]time.Time{}
)

// allowTestSend records one attempt and reports whether it fits the limit.
func allowTestSend(actor uuid.UUID, now time.Time) bool {
	testSendMu.Lock()
	defer testSendMu.Unlock()
	kept := testSendLog[actor][:0]
	for _, t := range testSendLog[actor] {
		if now.Sub(t) < testSendWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= testSendLimit {
		testSendLog[actor] = kept
		return false
	}
	testSendLog[actor] = append(kept, now)
	return true
}

// TestSendResult is what the panel shows after a test send.
type TestSendResult struct {
	To       string `json:"to"`
	Sent     bool   `json:"sent"`
	Disabled bool   `json:"disabled,omitempty"`
	Error    string `json:"error,omitempty"`
	Millis   int64  `json:"ms"`
}

// SendTest mails one sample notification, in the real template and with the
// saved contact block, to a single address. A delivery failure is reported in
// the result rather than as an error, so the panel can show the server's
// answer next to the connection check.
func (s *MailSettingsService) SendTest(ctx context.Context, actor uuid.UUID, to string) (*TestSendResult, error) {
	addr, err := netmail.ParseAddress(strings.TrimSpace(to))
	if err != nil || strings.ContainsAny(addr.Address, "\r\n,;") {
		return nil, Invalid("รูปแบบอีเมลผู้รับไม่ถูกต้อง")
	}
	if s.mailer == nil || !s.mailer.Info().Enabled {
		return &TestSendResult{To: addr.Address, Disabled: true,
			Error: "ยังไม่ได้กำหนด SMTP_HOST ระบบจึงไม่ส่งอีเมล"}, nil
	}
	if !allowTestSend(actor, time.Now()) {
		return nil, &UserError{Status: 429, Msg: "ส่งอีเมลทดสอบได้ไม่เกิน 5 ฉบับใน 10 นาที กรุณารอสักครู่แล้วลองใหม่"}
	}

	sender := personName(ctx, s.pool, actor)
	if sender == "" {
		sender = "เจ้าหน้าที่"
	}
	m := mailContent{
		Title: "ทดสอบการส่งอีเมลจากระบบ COCO TAS",
		Body: "อีเมลฉบับนี้เป็นการทดสอบการส่งอีเมลจากระบบเบิกจ่ายค่าตอบแทนผู้ช่วยสอน " +
			"ส่งโดย " + sender + " เมื่อ" + thaiLongDateTime(time.Now()) +
			" หากท่านได้รับอีเมลฉบับนี้ แสดงว่าระบบสามารถส่งอีเมลถึงท่านได้ตามปกติ ไม่ต้องดำเนินการใด ๆ",
		Recipient: "ผู้รับอีเมลทดสอบ",
		Closing:   closingInform,
		Contact:   loadMailContact(ctx, s.pool),
	}
	t0 := time.Now()
	err = s.mailer.SendMessage(mail.Message{
		To: addr.Address, Subject: m.Title, HTML: renderMailHTML(m), Text: renderMailText(m),
	})
	res := &TestSendResult{To: addr.Address, Sent: err == nil, Millis: time.Since(t0).Milliseconds()}
	if err != nil {
		res.Error = mail.Explain(err)
	}
	_ = s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "mail.test_send", Entity: "mail_settings", EntityID: "smtp",
		After: map[string]any{"to": addr.Address, "sent": res.Sent, "error": res.Error}})
	return res, nil
}

// thaiLongDateTime is "28 กันยายน 2569 เวลา 14.05 น." in Bangkok time.
func thaiLongDateTime(t time.Time) string {
	d := t.In(timeutil.Bangkok)
	return thaiLongDateISO(d.Format("2006-01-02")) + " เวลา " + d.Format("15.04") + " น."
}
