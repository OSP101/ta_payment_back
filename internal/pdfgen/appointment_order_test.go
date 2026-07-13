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
		AcademicYear:  "2569",
		SemesterLabel: "ภาคปลาย",
		OrderDate:     "24 มกราคม 2569",
		EffectiveDate: "24 มกราคม 2569",
		SignerName:    "รศ.ดร.ทดสอบ ระบบ",
		SignerTitle:   "คณบดี",
		Appointees: []AppointmentAppointee{
			{FullName: "สมชาย ใจดี", Level: "ปริญญาตรี", Track: "ภาคปกติ", CourseCode: "CP421024", IsReturning: true},
			{FullName: "สมหญิง ยิ้มแย้ม", Level: "ปริญญาโท", Track: "ภาคพิเศษ", CourseCode: "CP362104", IsReturning: false},
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
	d.Appointees = nil
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
