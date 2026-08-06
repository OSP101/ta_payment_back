package service

import (
	"strings"
	"testing"
)

// The plain form is what lands in the bell and in an email client. Whatever it
// does, it must not show the reader a markup character.
func TestAnnounceBodyPlain(t *testing.T) {
	cases := []struct{ in, want string }{
		{"**ด่วน** กรุณาส่งเอกสาร", "ด่วน กรุณาส่งเอกสาร"},
		{"*เน้น* ข้อความ", "เน้น ข้อความ"},
		{"ดูที่ [เว็บไซต์คณะ](https://cp.kku.ac.th) ครับ", "ดูที่ เว็บไซต์คณะ (https://cp.kku.ac.th) ครับ"},
		{":::center\nหัวข้อกลางหน้า", "หัวข้อกลางหน้า"},
		{"- ข้อหนึ่ง\n- ข้อสอง", "- ข้อหนึ่ง\n- ข้อสอง"},
		{"1. ขั้นแรก\n2. ขั้นสอง", "1. ขั้นแรก\n2. ขั้นสอง"},
		// A bare URL is already readable; leave it alone.
		{"เปิดที่ https://cp.kku.ac.th", "เปิดที่ https://cp.kku.ac.th"},
		// Multiplication and footnote stars must not be eaten as emphasis.
		{"ค่าตอบแทน 40 บาท", "ค่าตอบแทน 40 บาท"},
		// Both markers at once. Strip the shorter one first and the outer
		// stars are left stranded in the mail.
		{"***เน้นมาก***", "เน้นมาก"},
		// An alignment line sitting between paragraphs is the ordinary shape:
		// removing it must not leave a hole where it used to be.
		{"ย่อหน้าหนึ่ง\n\n:::center\n\nย่อหน้าสอง", "ย่อหน้าหนึ่ง\n\nย่อหน้าสอง"},
	}
	for _, c := range cases {
		if got := announceBodyPlain(c.in); got != c.want {
			t.Errorf("announceBodyPlain(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

// Nothing that reaches a reader may still carry markup.
func TestAnnounceBodyPlain_LeavesNoMarkers(t *testing.T) {
	body := "**หัวข้อ**\n:::center\n*เน้น* และ [ลิงก์](https://x.th)\n- รายการ"
	got := announceBodyPlain(body)
	for _, marker := range []string{"**", ":::", "](", "["} {
		if strings.Contains(got, marker) {
			t.Errorf("plain text still contains %q: %q", marker, got)
		}
	}
}
