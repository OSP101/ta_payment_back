package mail

import (
	"strings"
	"testing"
)

// A Subject (or To) carrying its own CRLF would terminate the header line and
// start writing new headers — the classic smuggled-Bcc. Nothing that goes on a
// header line may keep a newline.
func TestStripCRLF(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ปกติ", "ปกติ"},
		{"หัวข้อ\r\nBcc: attacker@evil.th", "หัวข้อ Bcc: attacker@evil.th"},
		{"a\rb\nc", "ab c"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripCRLF(c.in); got != c.want {
			t.Errorf("stripCRLF(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.ContainsAny(stripCRLF(c.in), "\r\n") {
			t.Errorf("stripCRLF(%q) still contains a newline", c.in)
		}
	}
}
