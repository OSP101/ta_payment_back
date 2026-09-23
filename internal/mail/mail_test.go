package mail

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	netmail "net/mail"
	"strings"
	"testing"
	"time"
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

// The built message must parse as the structure receivers expect: a text
// alternative, then HTML with the logo as an inline related part — and the
// logo only when the HTML actually points at it.
func TestBuildMessage_Structure(t *testing.T) {
	raw, err := buildMessage("TA Payment <no-reply@coco.kku.ac.th>", Message{
		To: "a@example.test", Subject: "แจ้งผลการพิจารณา",
		HTML: `<img src="cid:` + LogoCID + `"><p>สวัสดี</p>`, Text: "สวัสดี",
	}, time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	m, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("not a parseable message: %v", err)
	}
	if m.Header.Get("Date") == "" || !strings.HasSuffix(m.Header.Get("Message-ID"), "@coco.kku.ac.th>") {
		t.Errorf("Date=%q Message-ID=%q", m.Header.Get("Date"), m.Header.Get("Message-ID"))
	}
	dec := new(mime.WordDecoder)
	if s, _ := dec.DecodeHeader(m.Header.Get("Subject")); s != "แจ้งผลการพิจารณา" {
		t.Errorf("subject decodes to %q", s)
	}
	types := partTypes(t, m.Header.Get("Content-Type"), m.Body)
	want := []string{"text/plain", "text/html", "image/png"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Errorf("parts = %v, want %v", types, want)
	}

	raw, _ = buildMessage("no-reply@coco.kku.ac.th", Message{To: "a@example.test", Subject: "x", HTML: "<p>no logo</p>"}, time.Now())
	m, _ = netmail.ReadMessage(bytes.NewReader(raw))
	if got := partTypes(t, m.Header.Get("Content-Type"), m.Body); strings.Join(got, ",") != "text/html" {
		t.Errorf("logo attached although unreferenced: %v", got)
	}
}

// partTypes flattens the leaf media types of a multipart body, depth first.
func partTypes(t *testing.T, contentType string, body io.Reader) []string {
	t.Helper()
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("content type %q: %v", contentType, err)
	}
	if !strings.HasPrefix(mt, "multipart/") {
		return []string{mt}
	}
	var out []string
	r := multipart.NewReader(body, params["boundary"])
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("next part: %v", err)
		}
		out = append(out, partTypes(t, p.Header.Get("Content-Type"), p)...)
	}
}
