package mail

import (
	"fmt"
	"log"
	"mime"
	"net/smtp"
	"strings"

	"ta-payment-back/internal/config"
)

type Mailer struct{ cfg config.Config }

func New(cfg config.Config) *Mailer { return &Mailer{cfg: cfg} }

// stripCRLF removes header-breaking characters from a value that is about to
// be placed on a header line. SMTP headers are delimited by CRLF, so a value
// carrying its own CRLF would END the header and start writing new ones —
// classic header injection, e.g. a Subject that smuggles in a Bcc. Values
// here come from announcement titles and typed addresses, which are
// staff-authored today, but "trusted today" is not a property of the wire
// format.
func stripCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	return strings.ReplaceAll(s, "\n", " ")
}

func (m *Mailer) Send(to, subject, body string) error {
	if m.cfg.SMTPHost == "" {
		log.Printf("[mail-disabled] to=%s subject=%s", to, subject)
		return nil
	}
	to = stripCRLF(strings.TrimSpace(to))
	// Q-encode the subject: it doubles as the CRLF guard (encoded form has no
	// raw newlines) and fixes Thai subjects, which are naked UTF-8 on a header
	// line without it — some receivers render that as mojibake.
	subject = mime.QEncoding.Encode("utf-8", stripCRLF(subject))
	addr := fmt.Sprintf("%s:%d", m.cfg.SMTPHost, m.cfg.SMTPPort)
	msg := []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n%s",
		stripCRLF(m.cfg.MailFrom), to, subject, body))
	var auth smtp.Auth
	if m.cfg.SMTPUser != "" {
		auth = smtp.PlainAuth("", m.cfg.SMTPUser, m.cfg.SMTPPass, m.cfg.SMTPHost)
	}
	return smtp.SendMail(addr, auth, m.cfg.MailFrom, []string{to}, msg)
}
