package service

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/testutil"
)

// Staff edit the contact block under ตั้งค่า > อีเมลแจ้งเตือน; every e-mail
// sent afterwards must carry what they saved.
func TestMailSettings_SavedContactReachesEveryEmail(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	svc := &MailSettingsService{pool: pool, aud: audit.New(pool)}

	got, err := svc.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Heading == "" || !strings.Contains(got.Unit, "วิทยาลัยการคอมพิวเตอร์") {
		t.Fatalf("seeded default = %+v", got.MailContact)
	}

	staff := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active) VALUES ($1,$2,'จนท','ทดสอบ',TRUE)`,
		staff, "mailset-"+staff.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	saved, err := svc.Update(ctx, staff, MailContact{
		Heading: "  ติดต่อสอบถาม  ",
		Unit:    "งานบริการการศึกษา วิทยาลัยการคอมพิวเตอร์",
		Detail:  "อาคาร CP ชั้น 1\r\n\r\nโทร 043-009700 ต่อ 44001 — ในเวลาราชการ\n",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if saved.Heading != "ติดต่อสอบถาม" {
		t.Errorf("heading not trimmed: %q", saved.Heading)
	}
	if saved.Detail != "อาคาร CP ชั้น 1\nโทร 043-009700 ต่อ 44001 ในเวลาราชการ" {
		t.Errorf("detail = %q (blank lines and dashes should go)", saved.Detail)
	}
	if saved.UpdatedBy == nil || !strings.Contains(*saved.UpdatedBy, "จนท") {
		t.Errorf("updated_by = %v", saved.UpdatedBy)
	}

	// A notification rendered now carries the saved block.
	c := loadMailContact(ctx, pool)
	html := renderMailHTML(mailContent{Title: "t", Body: "b", Recipient: "คุณก ข", Closing: closingInform, Contact: c})
	for _, want := range []string{"ติดต่อสอบถาม", "งานบริการการศึกษา", "อาคาร CP ชั้น 1<br>โทร 043-009700"} {
		if !strings.Contains(html, want) {
			t.Errorf("email footer missing %q", want)
		}
	}
	text := renderMailText(mailContent{Title: "t", Body: "b", Recipient: "คุณก ข", Closing: closingInform, Contact: c})
	if !strings.Contains(text, "โทร 043-009700") {
		t.Error("plain-text alternative missing the contact detail")
	}

	var audited int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_logs WHERE action = 'mail_settings.update'`).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if audited != 1 {
		t.Errorf("audit rows = %d, want 1", audited)
	}

	// And NotifyService.Send picks it up without any wiring beyond the pool.
	n := &NotifyService{pool: pool, mailer: mail.New(config.Config{})}
	n.Send(ctx, staff, "หัวข้อ", "เนื้อหา", "")
}

func TestMailSettings_Validation(t *testing.T) {
	cases := []MailContact{
		{Heading: "", Unit: "u"},
		{Heading: "h", Unit: "  "},
		{Heading: "h", Unit: "u", Detail: "1\n2\n3\n4\n5\n6\n7"},
		{Heading: strings.Repeat("ก", 201), Unit: "u"},
	}
	for i, c := range cases {
		if _, err := normalizeMailContact(c); err == nil {
			t.Errorf("case %d accepted: %+v", i, c)
		}
	}
}

func TestMailSettings_PreviewInlinesLogo(t *testing.T) {
	s := &MailSettingsService{}
	html, err := s.Preview(MailContact{Heading: "ติดต่อ", Unit: "หน่วยงาน <ทดสอบ>"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(html, "cid:") || !strings.Contains(html, "data:image/png;base64,") {
		t.Error("preview must inline the logo; a browser cannot load cid:")
	}
	if !strings.Contains(html, "หน่วยงาน &lt;ทดสอบ&gt;") {
		t.Error("contact must be escaped")
	}
}

// Without the row (older database, hand-built services) mail still has a footer.
func TestMailContact_DefaultWhenUnset(t *testing.T) {
	html := renderMailHTML(mailContent{Title: "t", Body: "b", Recipient: "คุณก ข", Closing: closingInform})
	if !strings.Contains(html, defaultMailContact.Heading) {
		t.Error("zero Contact should fall back to the default footer")
	}
}
