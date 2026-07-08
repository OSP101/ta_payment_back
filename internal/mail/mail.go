package mail

import (
	"fmt"
	"log"
	"net/smtp"

	"ta-payment-back/internal/config"
)

type Mailer struct{ cfg config.Config }

func New(cfg config.Config) *Mailer { return &Mailer{cfg: cfg} }

func (m *Mailer) Send(to, subject, body string) error {
	if m.cfg.SMTPHost == "" {
		log.Printf("[mail-disabled] to=%s subject=%s", to, subject)
		return nil
	}
	addr := fmt.Sprintf("%s:%d", m.cfg.SMTPHost, m.cfg.SMTPPort)
	msg := []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n%s",
		m.cfg.MailFrom, to, subject, body))
	var auth smtp.Auth
	if m.cfg.SMTPUser != "" {
		auth = smtp.PlainAuth("", m.cfg.SMTPUser, m.cfg.SMTPPass, m.cfg.SMTPHost)
	}
	return smtp.SendMail(addr, auth, m.cfg.MailFrom, []string{to}, msg)
}
