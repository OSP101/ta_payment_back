package handler

import (
	"mime"
	"strings"
	"testing"
)

// The bug these guard against was silent: every header looked fine in the Go
// source and arrived mangled at the browser, because the failure happens in the
// ISO-8859-1 header encoding rather than in any code path a Go test would
// normally exercise. So these tests decode the header the way a client does.

// The real case: a Thai filename must come back byte-identical after a standards
// -compliant client parses the header.
func TestContentDisposition_ThaiFilenameSurvivesRoundTrip(t *testing.T) {
	const want = "123456789-0_จุฑามาศ_ชะรานันท์.pdf"

	_, params, err := mime.ParseMediaType(contentDisposition("attachment", want))
	if err != nil {
		t.Fatalf("header does not parse: %v", err)
	}
	if got := params["filename"]; got != want {
		t.Errorf("decoded filename = %q, want %q", got, want)
	}
}

// Both parameters have to be present. Dropping the ASCII one breaks old
// clients; dropping filename* brings the original bug straight back.
func TestContentDisposition_CarriesBothParameters(t *testing.T) {
	h := contentDisposition("attachment", "633020334-8_สุพพิธาน_ภักสวัสดิ์.pdf")

	if !strings.Contains(h, `filename="633020334-8_.pdf"`) {
		t.Errorf("missing or wrong ASCII fallback in %q", h)
	}
	if !strings.Contains(h, "filename*=UTF-8''") {
		t.Errorf("missing RFC 5987 parameter in %q", h)
	}
	// filename must precede filename* so a parser that takes the first match
	// still lands on something usable.
	if strings.Index(h, `filename="`) > strings.Index(h, "filename*=") {
		t.Errorf("filename* comes before filename in %q", h)
	}
}

// No byte outside printable ASCII may reach the wire — that is the whole
// failure mode. A control character in a header is also how header injection
// starts.
func TestContentDisposition_EmitsOnlyPrintableASCII(t *testing.T) {
	h := contentDisposition("inline", "รายงาน\r\nX-Injected: yes\t\"quoted\";x=1.pdf")
	for i := 0; i < len(h); i++ {
		if h[i] < 0x20 || h[i] > 0x7E {
			t.Fatalf("byte %d = %#x is not printable ASCII: %q", i, h[i], h)
		}
	}
	if strings.Contains(h, "X-Injected: yes") {
		t.Fatalf("injected header text survived unescaped: %q", h)
	}
}

// A CRLF-bearing filename must not smuggle a second header, and the name must
// still decode back to exactly what was uploaded.
func TestContentDisposition_NeutralisesHeaderInjection(t *testing.T) {
	const hostile = "a\r\nX-Evil: 1.pdf"

	h := contentDisposition("attachment", hostile)
	if strings.Contains(h, "\r") || strings.Contains(h, "\n") {
		t.Fatalf("header still contains a line break: %q", h)
	}
	_, params, err := mime.ParseMediaType(h)
	if err != nil {
		t.Fatalf("header does not parse: %v", err)
	}
	if got := params["filename"]; got != hostile {
		t.Errorf("decoded filename = %q, want the original %q", got, hostile)
	}
}

// Semicolons and commas terminate a header parameter. url.PathEscape leaves
// them alone, which is precisely why rfc5987Escape does not use it.
func TestRFC5987Escape_EncodesParameterDelimiters(t *testing.T) {
	for _, c := range []string{";", ",", "=", "&", `"`, " "} {
		if got := rfc5987Escape(c); !strings.HasPrefix(got, "%") {
			t.Errorf("rfc5987Escape(%q) = %q, want percent-encoded", c, got)
		}
	}
}

func TestASCIIFilenameFallback(t *testing.T) {
	cases := []struct{ in, want string }{
		// One underscore per run, not one per byte: Thai is 3 bytes a character.
		{"123456789-0_จุฑามาศ_ชะรานันท์.pdf", "123456789-0_.pdf"},
		{"already-ascii.pdf", "already-ascii.pdf"},
		// An all-Thai name reduces to a bare extension, which is hidden on Unix
		// and rejected by some download handlers — it needs a real stem.
		{"เอกสาร.pdf", "download.pdf"},
		{"ไทยล้วน", "download"},
	}
	for _, c := range cases {
		if got := asciiFilenameFallback(c.in); got != c.want {
			t.Errorf("asciiFilenameFallback(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
