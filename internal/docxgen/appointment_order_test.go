package docxgen

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

func sampleData() AppointmentOrderData {
	return AppointmentOrderData{
		OrderNo:       "6/2569",
		AcademicYear:  "2569",
		SemesterLabel: "ภาคปลาย",
		OrderDate:     "24 มกราคม 2569",
		EffectiveDate: "24 มกราคม 2569",
		SignerName:    "รศ.ดร.ทดสอบ ระบบ",
		SignerTitle:   "คณบดี",
		Appointees: []Appointee{
			{FullName: "สมชาย ใจดี", Level: "ปริญญาตรี", Track: "ภาคปกติ", CourseCode: "CP421024", Returning: true},
			{FullName: "สมหญิง ยิ้มแย้ม", Level: "ปริญญาโท", Track: "ภาคพิเศษ", CourseCode: "CP362104", Returning: false},
		},
	}
}

func TestBuildAppointmentOrderDOCX_IsValidZip(t *testing.T) {
	b, err := BuildAppointmentOrderDOCX(sampleData())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(b) < 500 {
		t.Fatalf("bytes too small: %d", len(b))
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	want := map[string]bool{
		"[Content_Types].xml": false,
		"_rels/.rels":         false,
		"word/document.xml":   false,
	}
	for _, f := range zr.File {
		if _, ok := want[f.Name]; ok {
			want[f.Name] = true
		}
	}
	for name, ok := range want {
		if !ok {
			t.Errorf("missing required part: %s", name)
		}
	}
}

func TestDocumentXML_ContainsRosterFields(t *testing.T) {
	d := sampleData()
	b, err := BuildAppointmentOrderDOCX(d)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	var docXML string
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			rc, _ := f.Open()
			body, _ := io.ReadAll(rc)
			rc.Close()
			docXML = string(body)
		}
	}
	// Header fields
	for _, needle := range []string{
		d.OrderNo, d.AcademicYear, d.SemesterLabel, d.OrderDate, d.EffectiveDate, d.SignerName, d.SignerTitle,
	} {
		if !strings.Contains(docXML, needle) {
			t.Errorf("document.xml missing %q", needle)
		}
	}
	// Appointees + status labels
	for _, ap := range d.Appointees {
		if !strings.Contains(docXML, ap.FullName) {
			t.Errorf("missing appointee name %q", ap.FullName)
		}
		if !strings.Contains(docXML, ap.CourseCode) {
			t.Errorf("missing course code %q", ap.CourseCode)
		}
	}
	if !strings.Contains(docXML, "เก่า") {
		t.Errorf("missing 'เก่า' badge (returning TA)")
	}
	if !strings.Contains(docXML, "ใหม่") {
		t.Errorf("missing 'ใหม่' badge (new TA)")
	}
}

func TestDocumentXML_IsWellFormed(t *testing.T) {
	b, err := BuildAppointmentOrderDOCX(sampleData())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".xml") {
			continue
		}
		rc, _ := f.Open()
		body, _ := io.ReadAll(rc)
		rc.Close()
		dec := xml.NewDecoder(bytes.NewReader(body))
		for {
			_, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("%s not well-formed: %v", f.Name, err)
				break
			}
		}
	}
}

// A name that would be XML-hostile (< > & ") must not break document.xml.
func TestDocumentXML_EscapesHostileInput(t *testing.T) {
	d := sampleData()
	d.SignerName = `<Sneaky> "Injection" & Co.`
	d.Appointees[0].FullName = `A & B <XSS> "!"`
	b, err := BuildAppointmentOrderDOCX(d)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".xml") {
			continue
		}
		rc, _ := f.Open()
		body, _ := io.ReadAll(rc)
		rc.Close()
		dec := xml.NewDecoder(bytes.NewReader(body))
		for {
			_, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("%s not well-formed after hostile input: %v", f.Name, err)
			}
		}
	}
}
