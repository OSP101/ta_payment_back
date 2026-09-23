package mail

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"mime"
	"mime/multipart"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	"ta-payment-back/internal/config"
)

// logoPNG is the college symbol in white, for the brand-coloured e-mail
// header (160 px square, shown at 56 for sharp high-DPI rendering). It travels inside the message as an inline part rather than as a
// link to the web server: mail clients block remote images by default, and
// the site may only be reachable on the campus network.
//
//go:embed logo.png
var logoPNG []byte

// LogoCID is what an HTML body puts in <img src="cid:..."> to show the logo.
// The part is attached only when the body actually references it.
const LogoCID = "college-logo@ta-payment"

// LogoPNG returns the embedded logo, for previews that cannot use cid:.
func LogoPNG() []byte { return logoPNG }

type Mailer struct{ cfg config.Config }

func New(cfg config.Config) *Mailer { return &Mailer{cfg: cfg} }

// Message is one e-mail. Text is the plain-text alternative; mail clients that
// do not render HTML show it, and spam filters score HTML-only mail worse.
type Message struct {
	To      string
	Subject string
	HTML    string
	Text    string
}

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

// Send delivers an HTML-only message. Kept for callers that have no text form.
func (m *Mailer) Send(to, subject, html string) error {
	return m.SendMessage(Message{To: to, Subject: subject, HTML: html})
}

func (m *Mailer) SendMessage(msg Message) error {
	if m.cfg.SMTPHost == "" {
		log.Printf("[mail-disabled] to=%s subject=%s", msg.To, msg.Subject)
		return nil
	}
	to := stripCRLF(strings.TrimSpace(msg.To))
	raw, err := buildMessage(m.cfg.MailFrom, msg, time.Now())
	if err != nil {
		return err
	}
	addr := fmt.Sprintf("%s:%d", m.cfg.SMTPHost, m.cfg.SMTPPort)
	var auth smtp.Auth
	if m.cfg.SMTPUser != "" {
		auth = smtp.PlainAuth("", m.cfg.SMTPUser, m.cfg.SMTPPass, m.cfg.SMTPHost)
	}
	return smtp.SendMail(addr, auth, m.cfg.MailFrom, []string{to}, raw)
}

// buildMessage renders the full RFC 5322 message:
//
//	multipart/alternative
//	├── text/plain              (only when Text is set)
//	└── multipart/related
//	    ├── text/html
//	    └── image/png           (the logo, only when the HTML references it)
//
// Date and Message-ID are required or strongly expected by receivers; their
// absence was one of the reasons these mails could land in spam.
func buildMessage(from string, msg Message, now time.Time) ([]byte, error) {
	from = stripCRLF(from)
	to := stripCRLF(strings.TrimSpace(msg.To))
	// Q-encode the subject: it doubles as the CRLF guard (encoded form has no
	// raw newlines) and fixes Thai subjects, which are naked UTF-8 on a header
	// line without it — some receivers render that as mojibake.
	subject := mime.QEncoding.Encode("utf-8", stripCRLF(msg.Subject))

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "From: %s\r\nTo: %s\r\nSubject: %s\r\n", from, to, subject)
	fmt.Fprintf(&buf, "Date: %s\r\nMessage-ID: %s\r\nMIME-Version: 1.0\r\n",
		now.Format(time.RFC1123Z), messageID(from))

	alt := multipart.NewWriter(&buf)
	fmt.Fprintf(&buf, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", alt.Boundary())

	if msg.Text != "" {
		if err := writeBase64Part(alt, textproto.MIMEHeader{
			"Content-Type": {"text/plain; charset=UTF-8"},
		}, []byte(msg.Text)); err != nil {
			return nil, err
		}
	}

	var rel bytes.Buffer
	related := multipart.NewWriter(&rel)
	if err := writeBase64Part(related, textproto.MIMEHeader{
		"Content-Type": {"text/html; charset=UTF-8"},
	}, []byte(msg.HTML)); err != nil {
		return nil, err
	}
	if strings.Contains(msg.HTML, "cid:"+LogoCID) {
		if err := writeBase64Part(related, textproto.MIMEHeader{
			"Content-Type":        {`image/png; name="logo.png"`},
			"Content-ID":          {"<" + LogoCID + ">"},
			"Content-Disposition": {`inline; filename="logo.png"`},
		}, logoPNG); err != nil {
			return nil, err
		}
	}
	if err := related.Close(); err != nil {
		return nil, err
	}
	relPart, err := alt.CreatePart(textproto.MIMEHeader{
		"Content-Type": {fmt.Sprintf("multipart/related; boundary=%q", related.Boundary())},
	})
	if err != nil {
		return nil, err
	}
	if _, err := relPart.Write(rel.Bytes()); err != nil {
		return nil, err
	}
	if err := alt.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeBase64Part adds one base64 part, wrapped at 76 columns as RFC 2045
// requires. Base64 rather than 8bit because Thai text is multi-byte UTF-8 and
// not every relay on the way is 8BITMIME-clean.
func writeBase64Part(w *multipart.Writer, h textproto.MIMEHeader, body []byte) error {
	h.Set("Content-Transfer-Encoding", "base64")
	p, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	enc := base64.StdEncoding.EncodeToString(body)
	for len(enc) > 76 {
		if _, err := p.Write([]byte(enc[:76] + "\r\n")); err != nil {
			return err
		}
		enc = enc[76:]
	}
	_, err = p.Write([]byte(enc + "\r\n"))
	return err
}

// messageID builds <random@domain-of-sender>. The domain half comes from the
// From address so the id matches the sending domain, as receivers expect.
func messageID(from string) string {
	domain := "ta-payment.local"
	if at := strings.LastIndex(from, "@"); at >= 0 {
		d := strings.Trim(from[at+1:], "> ")
		if d != "" {
			domain = d
		}
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "<" + hex.EncodeToString(b) + "@" + domain + ">"
}
