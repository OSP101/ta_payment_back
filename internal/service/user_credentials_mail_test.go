package service

import (
	"strings"
	"testing"

	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
)

func (f *fixture) usersWithMail() *UserService {
	u := f.users()
	u.notify = &NotifyService{pool: f.Pool, mailer: mail.New(config.Config{})}
	return u
}

func TestSendCredentials_MailsOnlyTheUnusedTempPassword(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	id := f.insertUserWithTempPassword("Tmp9pwAbcdEf")
	svc := f.usersWithMail()

	// Anything but the account's own temp password is refused, so the
	// endpoint cannot be used to mail arbitrary text.
	err := svc.SendCredentials(f.ctx, f.StaffID, id, "something else")
	if ue, ok := err.(*UserError); !ok || ue.Status != 409 {
		t.Fatalf("wrong password: want 409, got %v", err)
	}

	if err := svc.SendCredentials(f.ctx, f.StaffID, id, "Tmp9pwAbcdEf"); err != nil {
		t.Fatalf("send: %v", err)
	}
	// The stored copy of the e-mail must never carry the password.
	var body string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT body FROM notifications WHERE user_id=$1 AND channel='email'`, id).Scan(&body); err != nil {
		t.Fatalf("email row: %v", err)
	}
	if strings.Contains(body, "Tmp9pwAbcdEf") {
		t.Fatalf("password leaked into notifications.body: %q", body)
	}
	var inApp int
	_ = f.Pool.QueryRow(f.ctx,
		`SELECT count(*) FROM notifications WHERE user_id=$1 AND channel='in_app'`, id).Scan(&inApp)
	if inApp != 0 {
		t.Fatalf("sign-in letter should not reach the bell, got %d rows", inApp)
	}

	// Once the person has set their own password the temp one is dead.
	f.exec(`UPDATE users SET must_change_password=FALSE WHERE id=$1`, id)
	err = svc.SendCredentials(f.ctx, f.StaffID, id, "Tmp9pwAbcdEf")
	if ue, ok := err.(*UserError); !ok || ue.Status != 409 {
		t.Fatalf("after change: want 409, got %v", err)
	}
}

func TestCredentialsMailContent_PasswordOnlyInLayout(t *testing.T) {
	_, body, layout := credentialsMailContent("ta@kkumail.com", "Secret23xyZ", true)
	if strings.Contains(body, "Secret23xyZ") {
		t.Fatal("body must not carry the password")
	}
	if layout.Highlight == nil || layout.Highlight.Value != "Secret23xyZ" {
		t.Fatal("password should be the highlight")
	}
	m := mailContent{Title: "t", Body: body, Layout: layout}
	if !strings.Contains(renderMailHTML(m), "Secret23xyZ") {
		t.Fatal("rendered mail should show the password")
	}
}
