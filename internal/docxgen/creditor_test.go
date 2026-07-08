package docxgen

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFill_TextSubstitutions(t *testing.T) {
	tpl := findTemplate(t)
	out, err := Fill(tpl, Data{
		FullName:    "สมชาย ใจดี",
		NationalID:  "1234567890123",
		Phone:       "081-234-5678",
		Email:       "test@kkumail.com",
		AccountName: "นายสมชาย ใจดี",
		BankName:    "ไทยพาณิชย์",
		BranchCode:  "0555",
		Branch:      "ขอนแก่น",
		AccountNo:   "555-1-23456-7",
	})
	if err != nil {
		t.Fatalf("Fill: %v", err)
	}
	// Verify it's a valid zip
	zr, err := zip.NewReader(bytes.NewReader(out), int64(len(out)))
	if err != nil {
		t.Fatalf("output not a zip: %v", err)
	}
	// Read document.xml and check substitutions landed
	var doc string
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			rc, _ := f.Open()
			b, _ := readAll(rc)
			rc.Close()
			doc = string(b)
			break
		}
	}
	if doc == "" {
		t.Fatal("no document.xml in output")
	}
	// Substitutions should be present:
	must := []string{"สมชาย ใจดี", "1 2 3 4 5 6 7 8 9 0 1 2 3", "081-234-5678", "test@kkumail.com", "ไทยพาณิชย์", "0555", "ขอนแก่น", "555-1-23456-7"}
	for _, s := range must {
		if !strings.Contains(doc, s) {
			t.Errorf("missing substituted value: %q", s)
		}
	}
	// Original placeholder should be gone
	if strings.Contains(doc, "(นาย/นาง/นางสาว) …………………………………………………………………………..…………") {
		t.Error("name placeholder not replaced")
	}
}

func TestFill_WithSignature(t *testing.T) {
	tpl := findTemplate(t)
	// 1×1 transparent PNG
	png := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0, 0, 0, 13, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1F, 0x15, 0xC4, 0x89,
		0, 0, 0, 10, 'I', 'D', 'A', 'T', 0x78, 0x9C, 0x62, 0x00, 0x00, 0x00, 0x02, 0x00, 0x01, 0xE5, 0x27, 0xDE, 0xFC,
		0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xAE, 0x42, 0x60, 0x82}
	out, err := Fill(tpl, Data{FullName: "ทดสอบ", NationalID: "1111111111111", SignaturePNG: png})
	if err != nil {
		t.Fatalf("Fill: %v", err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(out), int64(len(out)))
	haveSig := false
	haveRel := false
	for _, f := range zr.File {
		if f.Name == "word/media/signature.png" {
			haveSig = true
		}
		if f.Name == "word/_rels/document.xml.rels" {
			rc, _ := f.Open()
			b, _ := readAll(rc)
			rc.Close()
			if strings.Contains(string(b), "media/signature.png") {
				haveRel = true
			}
		}
	}
	if !haveSig {
		t.Error("signature.png missing from output zip")
	}
	if !haveRel {
		t.Error("relationship for signature missing")
	}
}

func findTemplate(t *testing.T) string {
	// Walk up from cwd looking for assets/creditor_form_template.docx
	dir, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		p := filepath.Join(dir, "assets", "creditor_form_template.docx")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("template not found; run tests from repo root")
	return ""
}

func readAll(r interface{ Read(p []byte) (int, error) }) ([]byte, error) {
	var buf bytes.Buffer
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf.Bytes(), nil
			}
			return buf.Bytes(), err
		}
	}
}
