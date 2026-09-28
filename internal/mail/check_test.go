package mail

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"ta-payment-back/internal/config"
)

// fakeSMTP answers just enough of SMTP for Check: greeting, EHLO (no
// STARTTLS, no AUTH), MAIL FROM with the given reply, RSET, QUIT.
func fakeSMTP(t *testing.T, mailFromReply string) int {
	port, _ := fakeSMTPWithData(t, mailFromReply)
	return port
}

// externalRcptReply is what the fake answers for an @kkumail.com recipient.
var externalRcptReply = "250 ok"

// fakeSMTPWithData also accepts RCPT and DATA and hands back what was sent.
func fakeSMTPWithData(t *testing.T, mailFromReply string) (int, <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		w := func(s string) { conn.Write([]byte(s + "\r\n")) }
		w("220 fake ESMTP")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"):
				w("250-fake")
				w("250 8BITMIME")
			case strings.HasPrefix(cmd, "MAIL FROM"):
				w(mailFromReply)
			case strings.HasPrefix(cmd, "RCPT TO") && strings.Contains(cmd, "KKUMAIL"):
				w(externalRcptReply)
			case strings.HasPrefix(cmd, "RCPT TO"):
				w("250 ok")
			case cmd == "DATA":
				w("354 go ahead")
				var body strings.Builder
				for {
					l, err := r.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
					body.WriteString(l)
				}
				got <- body.String()
				w("250 queued")
			case strings.HasPrefix(cmd, "RSET"):
				w("250 ok")
			case strings.HasPrefix(cmd, "QUIT"):
				w("221 bye")
				return
			default:
				w("502 no")
			}
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, got
}

// A real delivery goes through the timed session end to end.
func TestSendMessage_DeliversThroughTimedSession(t *testing.T) {
	port, got := fakeSMTPWithData(t, "250 ok")
	m := New(config.Config{SMTPHost: "127.0.0.1", SMTPPort: port, MailFrom: "no-reply@coco.kku.ac.th"})
	if err := m.SendMessage(Message{To: "a@example.test", Subject: "ทดสอบ", HTML: "<p>x</p>", Text: "x"}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	select {
	case body := <-got:
		if !strings.Contains(body, "Subject: =?utf-8?q?") || !strings.Contains(body, "multipart/alternative") {
			t.Errorf("unexpected message:\n%s", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server never received DATA")
	}
}

func TestCheck_WalksTheHandshake(t *testing.T) {
	port := fakeSMTP(t, "250 ok")
	m := New(config.Config{SMTPHost: "127.0.0.1", SMTPPort: port, MailFrom: "no-reply@coco.kku.ac.th"})
	res := m.Check()
	if !res.OK {
		t.Fatalf("check failed: %+v", res.Steps)
	}
	var keys []string
	for _, s := range res.Steps {
		if s.Key != "links" && s.Key != "from_domain" { // settings checks, tested separately
			keys = append(keys, s.Key)
		}
	}
	if got := strings.Join(keys, ","); got != "connect,greeting,ehlo,tls,auth,from,rcpt_internal,rcpt_external" {
		t.Errorf("steps = %s", got)
	}
	// No STARTTLS and no user: both reported as skipped, not failed.
	for _, s := range res.Steps {
		if (s.Key == "tls" || s.Key == "auth") && !s.Skip {
			t.Errorf("%s should be skipped: %+v", s.Key, s)
		}
	}
}

func TestCheck_ReportsWhereItFails(t *testing.T) {
	port := fakeSMTP(t, "553 sender not allowed")
	res := New(config.Config{SMTPHost: "127.0.0.1", SMTPPort: port, MailFrom: "x@example.test"}).Check()
	last := res.Steps[len(res.Steps)-1]
	if res.OK || last.Key != "from" || !strings.Contains(last.Detail, "MAIL_FROM") {
		t.Errorf("want a failed from step with a MAIL_FROM hint, got %+v", last)
	}

	// Nothing listening: the connect step fails and the check stops there.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	res = New(config.Config{SMTPHost: "127.0.0.1", SMTPPort: closed}).Check()
	if res.OK || len(res.Steps) != 1 || res.Steps[0].Key != "connect" {
		t.Errorf("closed port: %+v", res.Steps)
	}

	// No host at all: one explanatory step, not a network error.
	res = New(config.Config{}).Check()
	if res.OK || res.Steps[0].Key != "config" {
		t.Errorf("unset host: %+v", res.Steps)
	}
}

// The relay smtp.kku.ac.th gave from an untrusted machine: sender fine,
// @kku.ac.th fine, @kkumail.com refused. The check must fail on that step
// and say what to ask the university for.
func TestCheck_RelayRefusesOutsideDomain(t *testing.T) {
	externalRcptReply = `550 "#5.1.0 Address rejected."`
	defer func() { externalRcptReply = "250 ok" }()
	port := fakeSMTP(t, "250 ok")
	res := New(config.Config{SMTPHost: "127.0.0.1", SMTPPort: port, MailFrom: "no-reply-coco-tas@kku.ac.th"}).Check()
	if res.OK {
		t.Fatal("check passed although outside recipients are refused")
	}
	byKey := map[string]CheckStep{}
	for _, st := range res.Steps {
		byKey[st.Key] = st
	}
	if !byKey["rcpt_internal"].OK {
		t.Errorf("internal recipient should pass: %+v", byKey["rcpt_internal"])
	}
	ext := byKey["rcpt_external"]
	if ext.OK || !strings.Contains(ext.Detail, "relay") || !strings.Contains(ext.Detail, "5.1.0") {
		t.Errorf("external step = %+v", ext)
	}
}
