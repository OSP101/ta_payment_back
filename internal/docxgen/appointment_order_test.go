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
		AcademicYear:  "2568",
		SemesterLabel: "ภาคปลาย",
		OrderDate:     "14 มกราคม 2569",
		EffectiveDate: "24 พฤศจิกายน 2568",
		SignerName:    "รองศาสตราจารย์สิรภัทร เชี่ยวชาญวัฒนา",
		SignerTitle:   "คณบดีวิทยาลัยการคอมพิวเตอร์",
		Levels: []LevelGroup{
			{
				Heading: "รายวิชาระดับปริญญาตรี",
				Courses: []CourseGroup{
					{
						Code: "SC310003", Name: "Database System and Design", CreditText: "3 (3-0-6)",
						Appointees: []Appointee{
							{StudentID: "663380555-8", FirstName: "นายชาคริต", LastName: "อ่วมอ่ำ"},
							{StudentID: "663380160-1", FirstName: "นางสาวธนาภา", LastName: "เจริญสุข"},
						},
					},
				},
			},
			{
				Heading: "รายวิชาระดับบัณฑิตศึกษา",
				Courses: []CourseGroup{
					{
						Code: "CP362104", Name: "Advanced Topics", CreditText: "3 (3-0-6)",
						Appointees: []Appointee{
							{StudentID: "665380001-2", FirstName: "นางสาวสมหญิง", LastName: "ยิ้มแย้ม"},
						},
					},
				},
			},
		},
	}
}

func docBytesFor(t *testing.T, d AppointmentOrderData) []byte {
	t.Helper()
	b, err := BuildAppointmentOrderDOCX(d)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return b
}

func TestBuildAppointmentOrderDOCX_IsValidZip(t *testing.T) {
	b := docBytesFor(t, sampleData())
	if len(b) < 500 {
		t.Fatalf("bytes too small: %d", len(b))
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	want := map[string]bool{
		"[Content_Types].xml":          false,
		"_rels/.rels":                  false,
		"word/document.xml":            false,
		"word/styles.xml":              false,
		"word/_rels/document.xml.rels": false,
		"word/media/image1.png":        false,
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

func TestStyles_PinThaiFont(t *testing.T) {
	b := docBytesFor(t, sampleData())
	styles := partText(t, b, "word/styles.xml")
	if !strings.Contains(styles, "TH Sarabun New") {
		t.Errorf("styles.xml does not pin TH Sarabun New")
	}
	if !strings.Contains(styles, `w:szCs w:val="32"`) {
		t.Errorf("styles.xml missing default 16pt (szCs 32)")
	}
}

func TestSeal_IsEmbeddedAndReferenced(t *testing.T) {
	b := docBytesFor(t, sampleData())
	// The PNG part must be present and non-trivial.
	zr, _ := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	var pngLen int
	for _, f := range zr.File {
		if f.Name == "word/media/image1.png" {
			rc, _ := f.Open()
			body, _ := io.ReadAll(rc)
			rc.Close()
			pngLen = len(body)
		}
	}
	if pngLen < 1000 {
		t.Errorf("seal image too small or missing: %d bytes", pngLen)
	}
	doc := partText(t, b, "word/document.xml")
	if !strings.Contains(doc, `r:embed="rId2"`) {
		t.Errorf("document.xml does not reference the seal image (rId2)")
	}
	rels := partText(t, b, "word/_rels/document.xml.rels")
	if !strings.Contains(rels, "media/image1.png") {
		t.Errorf("relationships do not target the seal image")
	}
}

func TestDocumentXML_ContainsRosterFields(t *testing.T) {
	d := sampleData()
	doc := partText(t, docBytesFor(t, d), "word/document.xml")

	for _, needle := range []string{
		d.OrderNo, d.AcademicYear, d.SemesterLabel, d.OrderDate, d.EffectiveDate,
		d.SignerName, d.SignerTitle,
		"มาตรา 40", "บัญชีรายชื่อแนบท้าย", "รหัสประจำตัว",
	} {
		if !strings.Contains(doc, needle) {
			t.Errorf("document.xml missing %q", needle)
		}
	}
	for _, lv := range d.Levels {
		if !strings.Contains(doc, lv.Heading) {
			t.Errorf("missing level heading %q", lv.Heading)
		}
		for _, c := range lv.Courses {
			if !strings.Contains(doc, c.Code) {
				t.Errorf("missing course code %q", c.Code)
			}
			for _, ap := range c.Appointees {
				if !strings.Contains(doc, ap.FirstName) || !strings.Contains(doc, ap.LastName) {
					t.Errorf("missing appointee name %q %q", ap.FirstName, ap.LastName)
				}
				if !strings.Contains(doc, ap.StudentID) {
					t.Errorf("missing student id %q", ap.StudentID)
				}
			}
		}
	}
}

func TestAllXMLPartsWellFormed(t *testing.T) {
	b := docBytesFor(t, sampleData())
	assertXMLWellFormed(t, b)
}

// A name that would be XML-hostile (< > & ") must not break any XML part.
func TestDocumentXML_EscapesHostileInput(t *testing.T) {
	d := sampleData()
	d.SignerName = `<Sneaky> "Injection" & Co.`
	d.Levels[0].Courses[0].Appointees[0].FirstName = `A & B <XSS> "!"`
	b := docBytesFor(t, d)
	assertXMLWellFormed(t, b)
}

// --- helpers ---------------------------------------------------------------

func partText(t *testing.T, docBytes []byte, name string) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(docBytes), int64(len(docBytes)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	for _, f := range zr.File {
		if f.Name == name {
			rc, _ := f.Open()
			body, _ := io.ReadAll(rc)
			rc.Close()
			return string(body)
		}
	}
	t.Fatalf("part not found: %s", name)
	return ""
}

func assertXMLWellFormed(t *testing.T, docBytes []byte) {
	t.Helper()
	zr, _ := zip.NewReader(bytes.NewReader(docBytes), int64(len(docBytes)))
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".xml") && !strings.HasSuffix(f.Name, ".rels") {
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

// A deputy signing for the dean gets a third signature line naming the seat
// whose authority the order is issued under. Without it the document reads as
// though a รองคณบดี issued an order only the dean may issue.
func TestDocumentXML_ActingSignerNamesTheSeat(t *testing.T) {
	d := sampleData()
	d.SignerName = "ผู้ช่วยศาสตราจารย์ ดร.ณกร วัฒนกิจ"
	d.SignerTitle = "รองคณบดีฝ่ายวิชาการ รักษาการแทน"
	d.SignerActingFor = "คณบดีวิทยาลัยการคอมพิวเตอร์"

	doc := partText(t, docBytesFor(t, d), "word/document.xml")
	for _, needle := range []string{d.SignerName, d.SignerTitle, d.SignerActingFor} {
		if !strings.Contains(doc, needle) {
			t.Errorf("document.xml missing signature line %q", needle)
		}
	}
	// Order matters: own position and acting phrase first, then the seat.
	if strings.Index(doc, d.SignerTitle) > strings.Index(doc, d.SignerActingFor) {
		t.Error("the acting-for seat must come AFTER the signer's own position line")
	}
}

// The dean signing their own order gets no extra line — an empty ActingFor must
// not leave a blank paragraph under the title.
func TestDocumentXML_DeanSignerHasNoActingLine(t *testing.T) {
	d := sampleData() // SignerActingFor left empty
	doc := partText(t, docBytesFor(t, d), "word/document.xml")
	if strings.Contains(doc, "รักษาการแทน") {
		t.Error("a dean signing their own order must not carry an acting phrase")
	}
}
