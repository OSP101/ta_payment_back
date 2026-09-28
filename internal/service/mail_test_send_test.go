package service

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/testutil"
)

func TestSendTest_ValidatesLimitsAndAudits(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()

	// A port nothing listens on: the send fails fast, which is all this test
	// needs; delivery itself is covered in internal/mail.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	svc := &MailSettingsService{pool: pool, aud: audit.New(pool),
		mailer: mail.New(config.Config{SMTPHost: "127.0.0.1", SMTPPort: port, MailFrom: "no-reply@coco.kku.ac.th"})}
	actor := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active) VALUES ($1,$2,'จนท','ทดสอบ',TRUE)`,
		actor, "mailtest-"+actor.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"", "not-an-address", "a@b.test, c@d.test"} {
		if _, err := svc.SendTest(ctx, actor, bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}

	res, err := svc.SendTest(ctx, actor, "Someone <someone@example.test>")
	if err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	if res.Sent || res.Error == "" || res.To != "someone@example.test" {
		t.Errorf("failed delivery should come back as a result with the reason: %+v", res)
	}

	for i := 0; i < testSendLimit-1; i++ {
		if _, err := svc.SendTest(ctx, actor, "someone@example.test"); err != nil {
			t.Fatalf("send %d within the limit refused: %v", i+2, err)
		}
	}
	_, err = svc.SendTest(ctx, actor, "someone@example.test")
	var ue *UserError
	if !errors.As(err, &ue) || ue.Status != 429 {
		t.Errorf("send past the limit: err=%v, want 429", err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_logs WHERE action = 'mail.test_send' AND actor_id = $1`, actor).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != testSendLimit {
		t.Errorf("audit rows = %d, want %d (one per attempted send)", n, testSendLimit)
	}
}

func TestSendTest_ReportsDisabledMail(t *testing.T) {
	svc := &MailSettingsService{mailer: mail.New(config.Config{})}
	res, err := svc.SendTest(context.Background(), uuid.New(), "a@example.test")
	if err != nil || !res.Disabled || !strings.Contains(res.Error, "SMTP_HOST") {
		t.Errorf("res=%+v err=%v", res, err)
	}
}

func TestAllowTestSend_WindowSlides(t *testing.T) {
	actor := uuid.New()
	now := time.Now()
	for i := 0; i < testSendLimit; i++ {
		if !allowTestSend(actor, now) {
			t.Fatalf("attempt %d refused", i+1)
		}
	}
	if allowTestSend(actor, now) {
		t.Error("attempt past the limit allowed")
	}
	if !allowTestSend(actor, now.Add(testSendWindow+time.Second)) {
		t.Error("limit should reset once the window has passed")
	}
}
