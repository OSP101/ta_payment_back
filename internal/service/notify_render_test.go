package service

import (
	"strings"
	"testing"
	"time"

	"ta-payment-back/internal/mail"
)

// The e-mail is a Thai official letter: the reader is addressed by the title
// the system holds for them, or "คุณ" when there is none.
func TestRecipientName(t *testing.T) {
	cases := []struct{ prefix, first, last, want string }{
		{"ผศ. ดร.", "สมชาย", "ใจดี", "ผศ. ดร.สมชาย ใจดี"},
		{"นางสาว", "สมหญิง", "รักเรียน", "นางสาวสมหญิง รักเรียน"},
		{"", "สมชาย", "ใจดี", "คุณสมชาย ใจดี"},
		{"  ", "สมชาย", "ใจดี", "คุณสมชาย ใจดี"},
		{"นาย", "", "", "ผู้ใช้งานระบบ"},
	}
	for _, c := range cases {
		if got := recipientName(c.prefix, c.first, c.last); got != c.want {
			t.Errorf("recipientName(%q,%q,%q) = %q, want %q", c.prefix, c.first, c.last, got, c.want)
		}
	}
}

// Marks that read as machine-written never reach a notice, even when they
// arrive inside a dynamic value such as a typed reason.
func TestPlainPunct(t *testing.T) {
	in := "เหตุผล — เอกสารไม่ชัด · กรุณาส่งใหม่ • ภายใน 10:00–12:00 → “ด่วน”…"
	got := plainPunct(in)
	for _, bad := range []string{"—", "–", "·", "•", "→", "“", "”", "…", "  "} {
		if strings.Contains(got, bad) {
			t.Errorf("plainPunct left %q in %q", bad, got)
		}
	}
	if !strings.Contains(got, "10:00-12:00") {
		t.Errorf("en dash between times should become a hyphen: %q", got)
	}
}

func TestRenderMailHTML_Letter(t *testing.T) {
	html := renderMailHTML(mailContent{
		Title: "หัวเรื่อง <ทดสอบ>", Body: "ย่อหน้าแรก\nบรรทัดสอง\n\nย่อหน้าสอง",
		Link: "https://tas.example/lecturer", Recipient: "ผศ. ดร.สมชาย ใจดี", Closing: closingAction,
	})
	for _, want := range []string{
		"cid:" + mail.LogoCID, // college logo, inline
		mailBrand,             // site brand colour
		"เรียน ผศ. ดร.สมชาย ใจดี",             // salutation with title
		"หัวเรื่อง &lt;ทดสอบ&gt;",             // escaped
		"ย่อหน้าแรก บรรทัดสอง",                // wrapped lines join
		"จึงเรียนมาเพื่อโปรดดำเนินการ",        // closing
		`href="https://tas.example/lecturer"`, // button
		mailButtonLabel,
		"วิทยาลัยการคอมพิวเตอร์ มหาวิทยาลัยขอนแก่น",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("html missing %q", want)
		}
	}
	if strings.Contains(html, "คณะวิทยาการคอมพิวเตอร์") {
		t.Error("the sender is วิทยาลัยการคอมพิวเตอร์, not คณะ")
	}

	noLink := renderMailHTML(mailContent{Title: "t", Body: "b", Recipient: "คุณก ข", Closing: closingInform})
	if strings.Contains(noLink, mailButtonLabel) {
		t.Error("a notice without a link must not show the button")
	}
}

// The structured blocks render in order, escaped, with the custom button.
func TestRenderMailHTML_Layout(t *testing.T) {
	html := renderMailHTML(mailContent{
		Title: "t", Body: "ใช้ไม่ได้เพราะมี Intro", Link: "https://tas.example/x",
		Recipient: "คุณก ข", Closing: closingInform,
		Layout: MailLayout{
			Intro:     "บทนำ",
			Facts:     []MailFact{{"ภาคการศึกษา", "ภาคการศึกษาที่ 2"}},
			Highlight: &MailFact{"กำหนดยื่นคำขอ", "20 พฤศจิกายน 2569"},
			Table:     &MailTable{Title: "รายวิชา", Head: []string{"รหัส", "ชื่อ"}, Rows: [][]string{{"CP1", "วิชา <A>"}}},
			After:     "ข้อความท้าย", ButtonLabel: "ยื่นคำขอผู้ช่วยสอน",
		},
	})
	order := []string{"บทนำ", "ภาคการศึกษา :", "กำหนดยื่นคำขอ", "รายวิชา", "วิชา &lt;A&gt;", "ข้อความท้าย", closingInform, "ยื่นคำขอผู้ช่วยสอน"}
	at := 0
	for _, want := range order {
		i := strings.Index(html[at:], want)
		if i < 0 {
			t.Fatalf("%q missing or out of order", want)
		}
		at += i
	}
	if strings.Contains(html, "ใช้ไม่ได้เพราะมี Intro") {
		t.Error("Intro must replace the body in the e-mail")
	}
}

// The copyright line follows the site footer's wording, with the Thai year.
func TestMailCopyright(t *testing.T) {
	defer func(f func() time.Time) { mailNow = f }(mailNow)
	mailNow = func() time.Time { return time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC) }
	m := mailContent{Title: "t", Body: "b", Recipient: "คุณก ข", Closing: closingInform}
	th := "© 2569 วิทยาลัยการคอมพิวเตอร์ มหาวิทยาลัยขอนแก่น สงวนลิขสิทธิ์"
	en := "© 2026 College of Computing, Khon Kaen University. All rights reserved."
	for name, out := range map[string]string{"html": renderMailHTML(m), "text": renderMailText(m)} {
		if !strings.Contains(out, th) || !strings.Contains(out, en) {
			t.Errorf("%s missing the copyright lines", name)
		}
	}
}

func TestThaiDateHelpers(t *testing.T) {
	if got := thaiLongDateISO("2026-09-23"); got != "23 กันยายน 2569" {
		t.Errorf("thaiLongDateISO = %q", got)
	}
	if got := thaiTimeRange("09:00:00", "12:30"); got != "เวลา 09.00 ถึง 12.30 น." {
		t.Errorf("thaiTimeRange = %q", got)
	}
	for in, want := range map[float64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567"} {
		if got := thaiBaht(in); got != want {
			t.Errorf("thaiBaht(%v) = %q, want %q", in, got, want)
		}
	}
}
