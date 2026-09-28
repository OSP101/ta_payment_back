package mail

import (
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

// session.go opens an SMTP session up to the point of naming the sender:
// connect, greeting, EHLO, TLS (per SMTP_ENCRYPTION), AUTH. Real sends and the
// connection check in settings share it, so the check tests exactly the path
// mail takes.

// Timeouts for one session. smtp.SendMail, used before, has none: a server
// that accepted the connection and then went quiet held the caller forever,
// and callers include request handlers.
const (
	dialTimeout    = 15 * time.Second
	sessionTimeout = 60 * time.Second
)

// traceFn records one step of opening a session. err nil and skip false is a
// pass; detail explains a skip or adds context to a pass.
type traceFn func(key, label string, started time.Time, err error, skip bool, detail string)

func noTrace(string, string, time.Time, error, bool, string) {}

// tlsConfig is the client TLS configuration for this server. legacy adds the
// RSA key-exchange suites Go leaves out by default: smtp.kku.ac.th offers
// nothing else, so without them the handshake fails with handshake_failure.
// The modern suites stay first, so a server that supports them still gets
// forward secrecy.
func (m *Mailer) tlsConfig(legacy bool) *tls.Config {
	// The certificate is always verified: smtp.kku.ac.th presents a valid one
	// (CN=smtp.kku.ac.th), so there is no setting to turn the check off.
	c := &tls.Config{ServerName: m.cfg.SMTPHost, RootCAs: m.rootCAs}
	if legacy {
		for _, s := range tls.CipherSuites() {
			c.CipherSuites = append(c.CipherSuites, s.ID)
		}
		for _, s := range tls.InsecureCipherSuites() {
			c.CipherSuites = append(c.CipherSuites, s.ID)
		}
		// TLS 1.3 ignores CipherSuites; the RSA suites only exist up to 1.2.
		c.MaxVersion = tls.VersionTLS12
	}
	return c
}

// isHandshakeFailure reports the server refusing every cipher we offered,
// the symptom of a relay that needs the legacy suites.
func isHandshakeFailure(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "handshake failure")
}

func (m *Mailer) addr() string {
	return net.JoinHostPort(m.cfg.SMTPHost, strconv.Itoa(m.cfg.SMTPPort))
}

// openSession returns a client ready for MAIL FROM, or the first error.
func (m *Mailer) openSession(legacy bool, trace traceFn) (*smtp.Client, error) {
	if trace == nil {
		trace = noTrace
	}
	mode := m.cfg.SMTPEncryption
	addr := m.addr()
	deadline := time.Now().Add(sessionTimeout)

	t0 := time.Now()
	var conn net.Conn
	var err error
	if mode == "ssl" {
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: dialTimeout}, "tcp", addr, m.tlsConfig(legacy))
		trace("connect", "เชื่อมต่อแบบเข้ารหัสตั้งแต่ต้น (SSL) "+addr, t0, err, false, "")
	} else {
		conn, err = net.DialTimeout("tcp", addr, dialTimeout)
		trace("connect", "เชื่อมต่อ "+addr, t0, err, false, "")
	}
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(deadline)

	t0 = time.Now()
	c, err := smtp.NewClient(conn, m.cfg.SMTPHost)
	trace("greeting", "รับข้อความทักทายจากเซิร์ฟเวอร์", t0, err, false, "")
	if err != nil {
		conn.Close()
		return nil, err
	}

	t0 = time.Now()
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "localhost"
	}
	err = c.Hello(hostname)
	trace("ehlo", "แนะนำตัวกับเซิร์ฟเวอร์ (EHLO)", t0, err, false, "")
	if err != nil {
		c.Close()
		return nil, err
	}

	t0 = time.Now()
	label := "เข้ารหัสการเชื่อมต่อ (STARTTLS)"
	offered, _ := c.Extension("STARTTLS")
	switch {
	case mode == "ssl":
		trace("tls", label, t0, nil, true, "เข้ารหัสตั้งแต่เชื่อมต่อแล้ว")
	case mode == "none":
		trace("tls", label, t0, nil, true, "ตั้งค่าให้ส่งโดยไม่เข้ารหัส (SMTP_ENCRYPTION=none)")
	case offered:
		if legacy {
			label += " แบบรองรับการเข้ารหัสรุ่นเก่า"
		}
		err = c.StartTLS(m.tlsConfig(legacy))
		trace("tls", label, t0, err, false, "")
		if err != nil {
			c.Close()
			return nil, err
		}
	case mode == "starttls":
		err = errors.New("เซิร์ฟเวอร์ไม่รองรับ STARTTLS แต่ตั้งค่า SMTP_ENCRYPTION=tls ไว้")
		trace("tls", label, t0, err, false, "")
		c.Close()
		return nil, err
	default:
		trace("tls", label, t0, nil, true, "เซิร์ฟเวอร์ไม่รองรับ STARTTLS อีเมลจะถูกส่งโดยไม่เข้ารหัส")
	}

	t0 = time.Now()
	user, pass := m.cfg.SMTPUser, m.cfg.SMTPPass
	switch {
	case user == "" && pass == "":
		trace("auth", "เข้าสู่ระบบเมลเซิร์ฟเวอร์", t0, nil, true,
			"ไม่ได้กำหนด SMTP_USER และ SMTP_PASS จึงส่งโดยอาศัยการอนุญาตตาม IP ของเซิร์ฟเวอร์")
	case user == "" || pass == "":
		err = errors.New("ต้องกำหนด SMTP_USER และ SMTP_PASS คู่กัน หรือเว้นว่างทั้งคู่")
		trace("auth", "เข้าสู่ระบบเมลเซิร์ฟเวอร์", t0, err, false, "")
		c.Close()
		return nil, err
	default:
		err = c.Auth(smtp.PlainAuth("", user, pass, m.cfg.SMTPHost))
		trace("auth", "เข้าสู่ระบบด้วยบัญชี "+user, t0, err, false, "")
		if err != nil {
			c.Close()
			return nil, err
		}
	}
	return c, nil
}

// openWithFallback opens a session, retrying once with the legacy cipher
// suites when the server rejects the handshake and they were not already on.
func (m *Mailer) openWithFallback(trace traceFn) (*smtp.Client, bool, error) {
	legacy := m.cfg.SMTPTLSLegacyCiphers
	c, err := m.openSession(legacy, trace)
	if err == nil || legacy || !isHandshakeFailure(err) {
		return c, legacy, err
	}
	log.Printf("mail: TLS handshake refused by %s, retrying with legacy RSA cipher suites "+
		"(set SMTP_TLS_LEGACY_CIPHERS=true to skip the failed first attempt)", m.cfg.SMTPHost)
	c, err = m.openSession(true, trace)
	return c, true, err
}

// deliver sends one prepared message to one recipient.
func (m *Mailer) deliver(to string, raw []byte) error {
	c, _, err := m.openWithFallback(nil)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Mail(m.envelopeFrom()); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
