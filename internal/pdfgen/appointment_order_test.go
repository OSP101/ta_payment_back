package pdfgen

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func sampleOrder() AppointmentOrderData {
	return AppointmentOrderData{
		OrderNo:       "6/2569",
		AcademicYear:  "2568",
		SemesterLabel: "ภาคปลาย",
		OrderDate:     "14 มกราคม 2569",
		EffectiveDate: "24 พฤศจิกายน 2568",
		SignerName:    "รองศาสตราจารย์สิรภัทร เชี่ยวชาญวัฒนา",
		SignerTitle:   "คณบดีวิทยาลัยการคอมพิวเตอร์",
		Levels: []AppointmentLevel{
			{
				Heading: "รายวิชาระดับปริญญาตรี",
				Courses: []AppointmentCourse{
					{
						Code: "SC310003", Name: "Database System and Design", CreditText: "3 (3-0-6)",
						Appointees: []AppointmentAppointee{
							{StudentID: "663380555-8", FirstName: "นายชาคริต", LastName: "อ่วมอ่ำ"},
							{StudentID: "663380160-1", FirstName: "นางสาวธนาภา", LastName: "เจริญสุข"},
						},
					},
				},
			},
			{
				Heading: "รายวิชาระดับบัณฑิตศึกษา",
				Courses: []AppointmentCourse{
					{
						Code: "CP362104", Name: "Advanced Topics", CreditText: "3 (3-0-6)",
						Appointees: []AppointmentAppointee{
							{StudentID: "665380001-2", FirstName: "นางสาวสมหญิง", LastName: "ยิ้มแย้ม"},
						},
					},
				},
			},
		},
	}
}

func TestBuildAppointmentOrderPDF_Smoke(t *testing.T) {
	fonts := filepath.FromSlash("../../assets/fonts")
	if _, err := os.Stat(filepath.Join(fonts, "Sarabun-Regular.ttf")); err != nil {
		t.Skipf("fonts missing; skipping smoke test")
	}
	out, err := BuildAppointmentOrderPDF(AppointmentOrderInput{
		FontDir: fonts,
		Data:    sampleOrder(),
	})
	if err != nil {
		t.Fatalf("BuildAppointmentOrderPDF: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("%PDF-")) {
		t.Errorf("not a PDF (first bytes %q)", out[:8])
	}
	// A single-page A4 with Sarabun subset + table is at least ~5 KB.
	if len(out) < 3_000 {
		t.Errorf("PDF too small: %d bytes", len(out))
	}
}

// The renderer must not crash on an empty appointee roster (caller should
// guard this upstream, but a defensive test guards against a nil-slice panic).
func TestBuildAppointmentOrderPDF_EmptyRoster(t *testing.T) {
	fonts := filepath.FromSlash("../../assets/fonts")
	if _, err := os.Stat(filepath.Join(fonts, "Sarabun-Regular.ttf")); err != nil {
		t.Skipf("fonts missing")
	}
	d := sampleOrder()
	d.Levels = nil
	if _, err := BuildAppointmentOrderPDF(AppointmentOrderInput{FontDir: fonts, Data: d}); err != nil {
		t.Fatalf("empty roster crashed: %v", err)
	}
}

// Font dir missing should return an error, not panic.
func TestBuildAppointmentOrderPDF_MissingFonts(t *testing.T) {
	_, err := BuildAppointmentOrderPDF(AppointmentOrderInput{
		FontDir: "/nonexistent/dir",
		Data:    sampleOrder(),
	})
	if err == nil {
		t.Errorf("expected error when FontDir is missing")
	}
}

// The acting-signer form adds a line to the signature block. The PDF is binary,
// so this cannot read the text back — but a renderer that dropped the line (or
// drew it on top of the one above without advancing y) would produce a file the
// same size as the plain form.
func TestBuildAppointmentOrderPDF_ActingSignerAddsALine(t *testing.T) {
	fonts := filepath.FromSlash("../../assets/fonts")
	if _, err := os.Stat(filepath.Join(fonts, "Sarabun-Regular.ttf")); err != nil {
		t.Skipf("fonts missing")
	}
	plain, err := BuildAppointmentOrderPDF(AppointmentOrderInput{FontDir: fonts, Data: sampleOrder()})
	if err != nil {
		t.Fatal(err)
	}
	d := sampleOrder()
	d.SignerTitle = "รองคณบดีฝ่ายวิชาการ รักษาการแทน"
	d.SignerActingFor = "คณบดีวิทยาลัยการคอมพิวเตอร์"
	acting, err := BuildAppointmentOrderPDF(AppointmentOrderInput{FontDir: fonts, Data: d})
	if err != nil {
		t.Fatal(err)
	}
	if len(acting) <= len(plain) {
		t.Errorf("acting form is %d bytes vs plain %d — the extra authority line "+
			"does not appear to have been drawn", len(acting), len(plain))
	}
}
