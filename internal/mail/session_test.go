package mail

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"ta-payment-back/internal/config"
)

// rsaOnlyTLS is a server TLS config shaped like smtp.kku.ac.th: TLS 1.2 and
// only RSA key-exchange suites, which Go clients do not offer by default.
func rsaOnlyTLS(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12, MaxVersion: tls.VersionTLS12,
		// The one suite the university's relay offers.
		CipherSuites: []uint16{tls.TLS_RSA_WITH_AES_256_GCM_SHA384},
	}, pool
}

// fakeStartTLS serves STARTTLS with the given TLS config on every accepted
// connection, then accepts MAIL/RCPT/DATA. It returns the port and a channel
// of each delivered message's MAIL FROM line and data.
func fakeStartTLS(t *testing.T, srvTLS *tls.Config) (int, <-chan [2]string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan [2]string, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveStartTLS(conn, srvTLS, got)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, got
}

func serveStartTLS(conn net.Conn, srvTLS *tls.Config, got chan<- [2]string) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := func(s string) { conn.Write([]byte(s + "\r\n")) }
	w("220 fake ESMTP")
	var from string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		up := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(up, "EHLO"):
			if _, isTLS := conn.(*tls.Conn); isTLS {
				w("250 fake")
			} else {
				w("250-fake")
				w("250 STARTTLS")
			}
		case up == "STARTTLS":
			w("220 go ahead")
			tc := tls.Server(conn, srvTLS)
			if err := tc.Handshake(); err != nil {
				return
			}
			conn, r = tc, bufio.NewReader(tc)
			w = func(s string) { conn.Write([]byte(s + "\r\n")) }
		case strings.HasPrefix(up, "MAIL FROM"):
			from = cmd
			w("250 ok")
		case strings.HasPrefix(up, "RCPT TO"), strings.HasPrefix(up, "RSET"):
			w("250 ok")
		case up == "DATA":
			w("354 go ahead")
			var body strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				body.WriteString(l)
			}
			got <- [2]string{from, body.String()}
			w("250 queued")
		case up == "QUIT":
			w("221 bye")
			return
		default:
			w("502 no")
		}
	}
}

// smtp.kku.ac.th accepts only RSA key-exchange ciphers. With the university's
// settings as sent (no legacy flag), a send must still get through by
// retrying with the legacy suites, and carry the display name in From while
// the envelope keeps the bare address.
func TestDeliver_KKUStyleRelayNeedsLegacyCiphers(t *testing.T) {
	srvTLS, roots := rsaOnlyTLS(t)
	port, got := fakeStartTLS(t, srvTLS)
	m := New(config.Config{
		SMTPHost: "127.0.0.1", SMTPPort: port, SMTPEncryption: "starttls",
		MailFrom: "no-reply-coco-tas@kku.ac.th", MailFromName: "COCO TAS",
		AppBaseURL: "https://tas.coco.kku.ac.th",
	})
	// Verification stays on: the fake's certificate is trusted, not ignored.
	m.rootCAs = roots
	if err := m.SendMessage(Message{To: "a@example.test", Subject: "ทดสอบ", HTML: "<p>x</p>", Text: "x"}); err != nil {
		t.Fatalf("send through an RSA-only relay: %v", err)
	}
	select {
	case d := <-got:
		if !strings.Contains(d[0], "<no-reply-coco-tas@kku.ac.th>") || strings.Contains(d[0], "COCO") {
			t.Errorf("envelope MAIL FROM = %q, want the bare address", d[0])
		}
		if !strings.Contains(d[1], "From: \"COCO TAS\" <no-reply-coco-tas@kku.ac.th>") {
			t.Errorf("From header missing the display name:\n%s", d[1][:200])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("relay never received the message")
	}

	// The check shows the failed first handshake, the retry, and the advice.
	res := m.Check()
	if !res.OK || res.Advice == "" || !strings.Contains(res.Advice, "SMTP_TLS_LEGACY_CIPHERS") {
		t.Errorf("check = %+v", res)
	}
	var sawFail, sawRetryTLS bool
	for _, s := range res.Steps {
		if s.Key == "tls" && !s.OK && !s.Skip {
			sawFail = true
		}
		if s.Key == "retry_tls" && s.OK {
			sawRetryTLS = true
		}
	}
	if !sawFail || !sawRetryTLS {
		t.Errorf("steps should show the refused handshake and the successful retry: %+v", res.Steps)
	}

	// With the flag set, the first attempt already works: no retry, no advice.
	m.cfg.SMTPTLSLegacyCiphers = true
	res = m.Check()
	if !res.OK || res.Advice != "" {
		t.Errorf("legacy flag set: %+v", res)
	}
}

func TestFromHeader(t *testing.T) {
	cases := []struct{ from, name, header, envelope string }{
		{"no-reply-coco-tas@kku.ac.th", "COCO TAS", `"COCO TAS" <no-reply-coco-tas@kku.ac.th>`, "no-reply-coco-tas@kku.ac.th"},
		{"no-reply@kku.ac.th", "", `"COCO TAS" <no-reply@kku.ac.th>`, "no-reply@kku.ac.th"},
		{"ชื่อระบบ <noreply@kku.ac.th>", "", "=?utf-8?", "noreply@kku.ac.th"},
		{"ชื่อระบบ <noreply@kku.ac.th>", "COCO TAS", `"COCO TAS" <noreply@kku.ac.th>`, "noreply@kku.ac.th"},
		{"Old Name <no-reply@kku.ac.th>", "", `"Old Name" <no-reply@kku.ac.th>`, "no-reply@kku.ac.th"},
		{"no-reply@kku.ac.th", "ระบบเบิกจ่าย", "=?utf-8?", "no-reply@kku.ac.th"},
	}
	for _, c := range cases {
		m := New(config.Config{MailFrom: c.from, MailFromName: c.name})
		if h := m.fromHeader(); !strings.HasPrefix(h, c.header) {
			t.Errorf("fromHeader(%q,%q) = %q, want prefix %q", c.from, c.name, h, c.header)
		}
		if e := m.envelopeFrom(); e != c.envelope {
			t.Errorf("envelopeFrom(%q) = %q", c.from, e)
		}
	}
}

// Half-configured credentials fail clearly instead of attempting AUTH.
func TestOpenSession_HalfCredentials(t *testing.T) {
	port := fakeSMTP(t, "250 ok")
	res := New(config.Config{SMTPHost: "127.0.0.1", SMTPPort: port, SMTPUser: "only-user", MailFrom: "a@kku.ac.th"}).Check()
	last := res.Steps[len(res.Steps)-1]
	if res.OK || last.Key != "auth" || !strings.Contains(last.Detail, "คู่กัน") {
		t.Errorf("half credentials: %+v", res.Steps)
	}
}

// Certificate checking cannot be switched off: an untrusted certificate fails
// the handshake even though the ciphers match.
func TestDeliver_VerifiesCertificate(t *testing.T) {
	srvTLS, _ := rsaOnlyTLS(t)
	port, _ := fakeStartTLS(t, srvTLS)
	m := New(config.Config{SMTPHost: "127.0.0.1", SMTPPort: port, SMTPEncryption: "starttls",
		SMTPTLSLegacyCiphers: true, MailFrom: "no-reply-coco-tas@kku.ac.th"})
	err := m.SendMessage(Message{To: "a@example.test", Subject: "x", HTML: "x"})
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("self-signed relay accepted: err=%v", err)
	}
	if !strings.Contains(Explain(err), "ใบรับรอง") {
		t.Errorf("hint = %q", Explain(err))
	}
}

func TestCheck_ConfigWarnings(t *testing.T) {
	port := fakeSMTP(t, "250 ok")
	m := New(config.Config{SMTPHost: "127.0.0.1", SMTPPort: port, MailFrom: "a@kku.ac.th",
		AppBaseURL: "http://localhost:3000"})
	res := m.Check()
	var links CheckStep
	for _, s := range res.Steps {
		if s.Key == "links" {
			links = s
		}
	}
	if !links.Warn || !strings.Contains(links.Detail, "APP_BASE_URL") {
		t.Errorf("localhost links not flagged: %+v", links)
	}
	if !res.OK {
		t.Errorf("a links warning must not fail the check: %+v", res.Steps)
	}

	// A comma-separated APP_BASE_URL (dev: localhost plus LAN IP) is judged by
	// its first address, the one e-mail links actually use.
	m = New(config.Config{SMTPHost: "127.0.0.1", SMTPPort: fakeSMTP(t, "250 ok"), MailFrom: "a@kku.ac.th",
		AppBaseURL: "http://localhost:3000,http://10.199.10.10:3000"})
	res = m.Check()
	flagged := false
	for _, s := range res.Steps {
		if s.Key == "links" && s.Warn && strings.Contains(s.Detail, "http://localhost:3000 ") {
			flagged = true
		}
	}
	if !flagged {
		t.Errorf("comma-separated localhost base not flagged: %+v", res.Steps)
	}

	// Against the university relay, a sender outside @kku.ac.th fails before
	// anything is attempted on the wire.
	m = New(config.Config{SMTPHost: "smtp.kku.ac.th", SMTPPort: 1, MailFrom: "noreply@gmail.com",
		AppBaseURL: "https://tas.coco.kku.ac.th"})
	res = m.Check()
	if res.OK || res.Steps[0].Key != "from_domain" || res.Steps[0].OK {
		t.Errorf("from domain: %+v", res.Steps)
	}
}
