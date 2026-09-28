package mail

import (
	"errors"
	"net"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"ta-payment-back/internal/config"
)

// check.go backs the "ทดสอบอีเมล" tab in settings: it opens the same session
// real sends use (session.go: connect, greeting, EHLO, TLS, AUTH), asks for
// MAIL FROM, and reports each step, so staff can see WHERE a delivery problem
// is instead of only "ส่งไม่สำเร็จ". Nothing is sent: the session ends with
// RSET and QUIT before any recipient is named.

// Info is the mail configuration as it is safe to show: never the password.
type Info struct {
	Enabled       bool   `json:"enabled"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Encryption    string `json:"encryption"`
	From          string `json:"from"`
	FromHeader    string `json:"from_header"`
	AuthUser      string `json:"auth_user"`
	HasPass       bool   `json:"has_password"`
	LegacyCiphers bool   `json:"legacy_ciphers"`
}

func (m *Mailer) Info() Info {
	enc := m.cfg.SMTPEncryption
	if enc == "" {
		enc = "auto"
	}
	return Info{
		Enabled: m.cfg.SMTPHost != "", Host: m.cfg.SMTPHost, Port: m.cfg.SMTPPort, Encryption: enc,
		From: m.envelopeFrom(), FromHeader: m.fromHeader(),
		AuthUser: m.cfg.SMTPUser, HasPass: m.cfg.SMTPPass != "",
		LegacyCiphers: m.cfg.SMTPTLSLegacyCiphers,
	}
}

// CheckStep is one step of the connection check.
type CheckStep struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	OK    bool   `json:"ok"`
	Skip  bool   `json:"skipped,omitempty"`
	// Warn marks a problem that does not stop mail from being sent (e.g.
	// links in it that will not open); it does not fail the check.
	Warn   bool   `json:"warning,omitempty"`
	Detail string `json:"detail,omitempty"`
	Millis int64  `json:"ms"`
}

// CheckResult is the whole check; OK only when the final attempt got through.
type CheckResult struct {
	OK     bool        `json:"ok"`
	Steps  []CheckStep `json:"steps"`
	Advice string      `json:"advice,omitempty"`
}

// Check opens a session exactly as a real send does, then asks whether the
// sender address is accepted, and stops before naming any recipient. When the
// first TLS handshake is refused it shows the automatic retry with the
// legacy cipher suites as its own steps, as sending would.
func (m *Mailer) Check() CheckResult {
	res := CheckResult{}
	if m.cfg.SMTPHost == "" {
		res.Steps = append(res.Steps, CheckStep{Key: "config", Label: "ตั้งค่าเมลเซิร์ฟเวอร์",
			Detail: "ยังไม่ได้กำหนด SMTP_HOST ระบบจึงไม่ส่งอีเมลใด ๆ (บันทึกไว้ใน log แทน)"})
		return res
	}
	configOK := m.configSteps(&res)
	attempt := 0
	trace := func(key, label string, started time.Time, err error, skip bool, detail string) {
		if attempt > 0 {
			key = "retry_" + key
		}
		if err != nil {
			detail = Explain(err)
		}
		res.Steps = append(res.Steps, CheckStep{Key: key, Label: label, OK: err == nil && !skip,
			Skip: skip, Detail: detail, Millis: since(started)})
	}

	legacy := m.cfg.SMTPTLSLegacyCiphers
	c, err := m.openSession(legacy, trace)
	if err != nil && !legacy && isHandshakeFailure(err) {
		attempt++
		res.Steps = append(res.Steps, CheckStep{Key: "retry", Label: "ลองใหม่ด้วยการเข้ารหัสรุ่นเก่า", Skip: true,
			Detail: "เซิร์ฟเวอร์ปฏิเสธการเข้ารหัสแบบปัจจุบัน ระบบจะลองใหม่แบบนี้เองทุกครั้งที่ส่ง"})
		c, err = m.openSession(true, trace)
		if err == nil {
			res.Advice = "เซิร์ฟเวอร์นี้รองรับเฉพาะการเข้ารหัสรุ่นเก่า ตั้งค่า SMTP_TLS_LEGACY_CIPHERS=true เพื่อให้ส่งได้เร็วขึ้นโดยไม่ต้องลองสองครั้ง"
		}
	}
	if err != nil {
		return res
	}
	defer c.Close()

	t0 := time.Now()
	from := m.envelopeFrom()
	err = c.Mail(from)
	trace("from", "ผู้ส่ง "+from+" ได้รับอนุญาต", t0, err, false, "")
	if err != nil {
		_ = c.Quit()
		return res
	}

	// Recipients. An IP-trusting relay accepts the sender from anywhere and
	// only refuses at RCPT, and it treats its own domain differently from
	// the rest: from a machine it does not trust, smtp.kku.ac.th took
	// @kku.ac.th but answered 550 5.1.0 for every @kkumail.com and outside
	// address (probed 28/09/2026). TAs use @kkumail.com, so both are checked.
	// RSET follows each RCPT and DATA is never sent, so nothing is delivered.
	res.OK = true
	for _, p := range recipientProbes {
		t0 = time.Now()
		err = c.Rcpt(p.addr)
		detail := ""
		if err != nil {
			detail = p.hint + " (" + err.Error() + ")"
			res.OK = false
		}
		res.Steps = append(res.Steps, CheckStep{Key: prefixed(attempt, p.key), Label: p.label,
			OK: err == nil, Detail: detail, Millis: since(t0)})
		_ = c.Reset()
		if c.Mail(from) != nil {
			break
		}
	}
	_ = c.Reset()
	_ = c.Quit()
	res.OK = res.OK && configOK
	return res
}

// configSteps checks the settings themselves before connecting: the two
// mistakes the university's own notes warn about. Reports whether nothing
// that blocks delivery was found.
func (m *Mailer) configSteps(res *CheckResult) bool {
	ok := true
	from := m.envelopeFrom()
	if strings.HasSuffix(strings.ToLower(m.cfg.SMTPHost), "kku.ac.th") {
		at := strings.LastIndex(from, "@")
		good := at >= 0 && strings.EqualFold(from[at+1:], "kku.ac.th")
		step := CheckStep{Key: "from_domain", Label: "ที่อยู่ผู้ส่ง " + from + " อยู่ในโดเมน @kku.ac.th", OK: good}
		if !good {
			step.Detail = "relay ของมหาวิทยาลัยรับเฉพาะผู้ส่ง @kku.ac.th แก้ MAILER_FROM เป็นที่อยู่ @kku.ac.th"
			ok = false
		}
		res.Steps = append(res.Steps, step)
	}
	base := config.PrimaryBaseURL(m.cfg.AppBaseURL)
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		h := strings.ToLower(u.Hostname())
		if h == "localhost" || h == "127.0.0.1" || h == "::1" || strings.HasSuffix(h, ".localhost") {
			res.Steps = append(res.Steps, CheckStep{Key: "links", Label: "ลิงก์ในอีเมลเปิดได้จากภายนอก", Warn: true,
				Detail: "ลิงก์ในอีเมลใช้ " + base + " (ค่าแรกของ APP_BASE_URL) ผู้รับจะกดลิงก์และปุ่มในอีเมลไม่ได้ ให้ตั้งเป็นโดเมนจริงของระบบ"})
		} else {
			res.Steps = append(res.Steps, CheckStep{Key: "links", Label: "ลิงก์ในอีเมลชี้ไปที่ " + u.Scheme + "://" + u.Host, OK: true})
		}
	}
	return ok
}

// recipientProbes are addresses offered at RCPT and withdrawn with RSET.
var recipientProbes = []struct{ key, label, addr, hint string }{
	{"rcpt_internal", "รับผู้รับภายในมหาวิทยาลัย (@kku.ac.th)", "postmaster@kku.ac.th",
		"เซิร์ฟเวอร์ไม่รับผู้รับ @kku.ac.th ตรวจสอบการตั้งค่ากับสำนักเทคโนโลยีดิจิทัล"},
	{"rcpt_external", "ส่งต่อไปยังผู้รับนอกโดเมน kku.ac.th (เช่น @kkumail.com)", "postmaster@kkumail.com",
		"เซิร์ฟเวอร์ไม่ยอมส่งต่อออกนอกโดเมน kku.ac.th จากเครื่องนี้ ผู้ช่วยสอนที่ใช้ @kkumail.com จะไม่ได้รับอีเมล " +
			"กรุณาขอให้สำนักเทคโนโลยีดิจิทัลอนุญาต IP ของเซิร์ฟเวอร์ระบบให้ส่งต่ออีเมล (relay) หรือขอบัญชี SMTP_USER/SMTP_PASS"},
}

func prefixed(attempt int, key string) string {
	if attempt > 0 {
		return "retry_" + key
	}
	return key
}

func since(t time.Time) int64 { return time.Since(t).Milliseconds() }

// Explain turns a network or SMTP error into a short Thai hint plus the raw
// message, which is what whoever runs the mail server will ask for.
func Explain(err error) string {
	if err == nil {
		return ""
	}
	var ne net.Error
	var dnsErr *net.DNSError
	var opErr *net.OpError
	switch {
	case errors.As(err, &dnsErr):
		return "ไม่พบชื่อโฮสต์นี้ (DNS) ตรวจสอบ SMTP_HOST: " + err.Error()
	case errors.As(err, &ne) && ne.Timeout():
		return "หมดเวลารอ เซิร์ฟเวอร์ไม่ตอบ อาจถูกไฟร์วอลล์ปิดกั้นหรือต้องอยู่ในเครือข่ายมหาวิทยาลัย: " + err.Error()
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return "เชื่อมต่อไม่ได้ ตรวจสอบโฮสต์ พอร์ต และเครือข่าย: " + err.Error()
	}
	msg := strings.ToLower(err.Error())
	switch {
	case isHandshakeFailure(err):
		return "เซิร์ฟเวอร์ปฏิเสธการเข้ารหัส อาจต้องเปิด SMTP_TLS_LEGACY_CIPHERS=true หรือพอร์ตกับโหมดไม่ตรงกัน (465 ใช้ ssl, 587 ใช้ tls): " + err.Error()
	case strings.Contains(msg, "certificate"):
		return "ตรวจสอบใบรับรองของเซิร์ฟเวอร์ไม่ผ่าน ตรวจสอบว่า SMTP_HOST ตรงกับชื่อในใบรับรอง (smtp.kku.ac.th) และเวลาของเครื่องเซิร์ฟเวอร์ถูกต้อง: " + err.Error()
	case strings.Contains(msg, "first record does not look like a tls handshake"):
		return "พอร์ตนี้ไม่ได้เข้ารหัสตั้งแต่ต้น ให้ใช้ SMTP_ENCRYPTION=tls กับพอร์ต 587: " + err.Error()
	}
	var te *textproto.Error
	if errors.As(err, &te) {
		switch {
		case te.Code == 535:
			return "ชื่อผู้ใช้หรือรหัสผ่านไม่ถูกต้อง ตรวจสอบ SMTP_USER และ SMTP_PASS: " + err.Error()
		case te.Code == 530:
			return "เซิร์ฟเวอร์ต้องการให้เข้าสู่ระบบก่อนส่ง กำหนด SMTP_USER และ SMTP_PASS: " + err.Error()
		case te.Code >= 550 && te.Code <= 553:
			return "เซิร์ฟเวอร์ไม่อนุญาตให้ส่งในนามที่อยู่นี้ ตรวจสอบ MAIL_FROM: " + err.Error()
		}
	}
	return err.Error()
}
