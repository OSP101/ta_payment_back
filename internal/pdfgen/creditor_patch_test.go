package pdfgen

import (
	"os"
	"testing"
)

// A corrected form keeps everything the TA signed (bank, signature, date) and
// carries the new identity fields; it must still be one valid A4 page.
func TestPatchCreditorProducesReadableForm(t *testing.T) {
	tpl, fonts := "../../assets/creditor_form_template.pdf", "../../assets/fonts"
	orig, err := FillCreditor(CreditorInput{TemplatePath: tpl, FontDir: fonts, Data: CreditorData{
		Prefix: "นาย", FullName: "สมชาย ใจดี", NationalID: "1100700000001", Phone: "0812345678",
		Email: "a@kkumail.com", AccountName: "สมชาย ใจดี", BankName: "ธนาคารกรุงไทย", AccountNo: "1234567890",
	}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := PatchCreditor(CreditorPatchInput{SourcePDF: orig, TemplatePath: tpl, FontDir: fonts,
		Prefix: "นางสาว", FullName: "สมหญิง ใจดี", NationalID: "3101234567891"})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if len(out) < 1000 || string(out[:5]) != "%PDF-" {
		t.Fatalf("not a PDF (%d bytes)", len(out))
	}
	if dir := os.Getenv("CREDITOR_PATCH_OUT"); dir != "" {
		_ = os.WriteFile(dir+"/orig.pdf", orig, 0o600)
		_ = os.WriteFile(dir+"/patched.pdf", out, 0o600)
	}
}

// A stored file that is not this system's one-page A4 form is refused, not
// "patched" into a blank page with a few fields on it.
func TestPatchCreditorRefusesForeignPDF(t *testing.T) {
	placeholder := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 200]>>endobj\ntrailer<</Root 1 0 R>>\n%%EOF\n")
	_, err := PatchCreditor(CreditorPatchInput{SourcePDF: placeholder, TemplatePath: "../../assets/creditor_form_template.pdf",
		FontDir: "../../assets/fonts", Prefix: "นาย", FullName: "ก ข", NationalID: "1100700000001"})
	if err == nil {
		t.Fatal("a non-A4 placeholder must be refused")
	}
}
