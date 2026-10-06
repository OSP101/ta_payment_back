package pdfgen

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFillCreditor_Smoke exercises the whole overlay pipeline against the
// shipped template + fonts. It's not a golden-file test — layout iteration is
// expected — so it only asserts the output is a valid PDF and non-trivial.
// The test skips when the assets aren't present (e.g. running from a source
// checkout without go generate).
func TestFillCreditor_Smoke(t *testing.T) {
	tpl := filepath.FromSlash("../../assets/creditor_form_template.pdf")
	fonts := filepath.FromSlash("../../assets/fonts")
	if _, err := os.Stat(tpl); err != nil {
		t.Skipf("template missing (%v); skipping smoke test", err)
	}
	if _, err := os.Stat(filepath.Join(fonts, "Sarabun-Regular.ttf")); err != nil {
		t.Skipf("fonts missing; skipping smoke test")
	}

	out, err := FillCreditor(CreditorInput{
		TemplatePath: tpl,
		FontDir:      fonts,
		Data: CreditorData{
			Prefix:      "นาย",
			FullName:    "สมชาย ใจดี",
			NationalID:  "1234567890123",
			Phone:       "0812345678",
			Email:       "sc@example.com",
			AccountName: "สมชาย ใจดี",
			BankName:    "ธนาคารไทยพาณิชย์",
			BranchCode:  "0555",
			Branch:      "ขอนแก่น",
			AccountNo:   "1234567890",
		},
	})
	if err != nil {
		t.Fatalf("FillCreditor: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("%PDF-")) {
		t.Errorf("output is not a PDF (first bytes: %q)", out[:min(8, len(out))])
	}
	// A real overlay is at least ~10 KB because of the imported template.
	if len(out) < 10_000 {
		t.Errorf("output smaller than expected: %d bytes", len(out))
	}
}

// TestFillCreditor_Overlong feeds every free-text field a value far longer
// than its printed run, and every digit grid more digits than it has boxes.
// Real profiles do hit the short runs — the "(………)" under the signature only
// fits ~25 Thai characters at full size — and the shrink-to-fit loop in
// setTextInField and the clamp in fillGrid are the only things standing
// between that and a form with text sprawling across neighbouring cells.
func TestFillCreditor_Overlong(t *testing.T) {
	tpl := filepath.FromSlash("../../assets/creditor_form_template.pdf")
	fonts := filepath.FromSlash("../../assets/fonts")
	if _, err := os.Stat(tpl); err != nil {
		t.Skipf("template missing (%v); skipping", err)
	}
	if _, err := os.Stat(filepath.Join(fonts, "Sarabun-Regular.ttf")); err != nil {
		t.Skipf("fonts missing; skipping")
	}

	long := strings.Repeat("ประภัสสรากานต์ศรีวิโรจนานนท์", 4)
	out, err := FillCreditor(CreditorInput{
		TemplatePath: tpl,
		FontDir:      fonts,
		Data: CreditorData{
			Prefix:      "นางสาว",
			FullName:    long,
			NationalID:  "12345678901234567890",
			Phone:       "081-234-5678 ต่อ 12345",
			Email:       strings.Repeat("a", 80) + "@kkumail.com",
			AccountName: long,
			BankName:    "ธนาคาร" + long,
			BranchCode:  "05550555055505550555",
			Branch:      long,
			AccountNo:   "12345678901234567890",
		},
	})
	if err != nil {
		t.Fatalf("FillCreditor: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("%PDF-")) {
		t.Errorf("output is not a PDF (first bytes: %q)", out[:min(8, len(out))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestPrintedNameSpacesLatinNames(t *testing.T) {
	if got := printedName("นาย", "สมชาย ใจดี"); got != "นายสมชาย ใจดี" {
		t.Errorf("Thai name = %q", got)
	}
	if got := printedName("นาย", "John Smith"); got != "นาย John Smith" {
		t.Errorf("Latin name = %q", got)
	}
}

func TestIDCharsKeepsPassportLetters(t *testing.T) {
	if got := idChars("1-1018-00123-45-6"); got != "1101800123456" {
		t.Errorf("citizen ID = %q", got)
	}
	if got := idChars("ab 123-4567"); got != "AB1234567" {
		t.Errorf("passport = %q", got)
	}
}
