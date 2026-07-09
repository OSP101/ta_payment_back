package pdfgen

import (
	"bytes"
	"os"
	"path/filepath"
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
