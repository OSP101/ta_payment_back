package docxgen

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// partNames lists every entry in the generated package, so a test can assert
// that what the content types declare is exactly what is shipped.
func partNames(t *testing.T, docBytes []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(docBytes), int64(len(docBytes)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		out = append(out, f.Name)
	}
	return out
}

// A .docx is a zip of XML parts; Word repairs (or refuses) a file whose parts
// and relationships do not line up. These tests read the generated package
// rather than trusting that it opened once by hand.

func sampleWorkload() WorkloadDetailData {
	return WorkloadDetailData{
		SemesterLabel: "ภาคปลาย",
		AcademicYear:  "2568",
		CourseCode:    "CP363761",
		CourseName:    "Seminar in Information Technology",
		CreditText:    "1 (1-0-2)",
		LecturerName:  "ผศ.ดร.วรัญญา วรรณศรี",
		TAName:        "นายวรพจน์ สุวรรณภิภพ",
		StudentID:     "627020002-0",
		LevelLabel:    "ระดับบัณฑิตศึกษา",
		Months: []WorkloadMonth{
			{Label: "เดือน พฤศจิกายน 2568", Prep: "4", Review: "2", Total: "6"},
			{Label: "เดือน ธันวาคม 2568", Prep: "20", Review: "10", Total: "30"},
			{Label: "เดือน มกราคม 2569", HelpTeach: "12", Prep: "20", Review: "10", Total: "42"},
		},
		CertifierName:      "(ผศ.ดร.ณกร วัฒนกิจ)",
		CertifierTitle:     "ตำแหน่ง รองคณบดีฝ่ายวิชาการ",
		CertifierActingFor: "รักษาการแทนหัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์",
	}
}

func TestBuildWorkloadDetailDOCX_HasEveryPartItDeclares(t *testing.T) {
	b, err := BuildWorkloadDetailDOCX(sampleWorkload())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"[Content_Types].xml":          false,
		"_rels/.rels":                  false,
		"word/_rels/document.xml.rels": false,
		"word/styles.xml":              false,
		"word/document.xml":            false,
	}
	for _, name := range partNames(t, b) {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected part %q", name)
			continue
		}
		want[name] = true
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing part %q", name)
		}
	}
	// This document has no image. Declaring a png default or an image
	// relationship with no part behind it is what makes Word offer to repair
	// the file on open.
	ct := partText(t, b, "[Content_Types].xml")
	if strings.Contains(ct, "png") {
		t.Error("content types declare a png default, but no image part is written")
	}
	rels := partText(t, b, "word/_rels/document.xml.rels")
	if strings.Contains(rels, "image") {
		t.Error("document rels reference an image relationship with no part behind it")
	}
}

func TestBuildWorkloadDetailDOCX_CarriesTheFormsContent(t *testing.T) {
	b, err := BuildWorkloadDetailDOCX(sampleWorkload())
	if err != nil {
		t.Fatal(err)
	}
	doc := partText(t, b, "word/document.xml")
	for _, want := range []string{
		"แบบแสดงรายละเอียดภาระงานของผู้ช่วยสอน",
		"ประจำภาคปลาย",
		"CP363761",
		"Seminar in Information Technology",
		"1 (1-0-2)",
		"นายวรพจน์ สุวรรณภิภพ",
		"627020002-0",
		"ระดับบัณฑิตศึกษา",
		// The three work lines and their total — the reason the form exists.
		"ช่วยสอน", "เตรียมการสอน", "ตรวจแบบทดสอบ", "รวม",
		"เดือน พฤศจิกายน 2568", "เดือน ธันวาคม 2568", "เดือน มกราคม 2569",
		"อาจารย์ประจำวิชา", "ผู้รับรอง",
		"(ผศ.ดร.วรัญญา วรรณศรี)", "(ผศ.ดร.ณกร วัฒนกิจ)",
		"รักษาการแทนหัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("document.xml is missing %q", want)
		}
	}
	// The month blocks are the form's substance, so they must be a real
	// bordered table — a borderless one would print as loose text.
	if !strings.Contains(doc, "<w:tblBorders>") {
		t.Error("the month table draws no borders; the printed form would have no grid")
	}
}

// Formatting the office checked against their own file
// (docs/14.CP363761-บัณฑิต.docx). These are not cosmetic preferences — a form
// that does not look like the one they file gets sent back.
func TestBuildWorkloadDetailDOCX_MatchesTheCollegesTableFormatting(t *testing.T) {
	b, err := BuildWorkloadDetailDOCX(sampleWorkload())
	if err != nil {
		t.Fatal(err)
	}
	doc := partText(t, b, "word/document.xml")

	// Rules are Word's Table Grid weight in the AUTOMATIC colour. A hard-coded
	// black stops following the document colour and reads as a heavier,
	// off-palette line — exactly what was reported.
	if strings.Contains(doc, `w:color="000000"`) {
		t.Error("a table rule is hard-coded to #000000; the college's file uses the automatic colour")
	}
	if !strings.Contains(doc, `<w:top w:val="single" w:sz="4" w:space="0" w:color="auto"/>`) {
		t.Error("table rules are not the college's sz=4 automatic-colour Table Grid weight")
	}
	// The month caption and งาน rows sit on the light grey band.
	if n := strings.Count(doc, `w:fill="F2F2F2"`); n == 0 {
		t.Error("the month caption / งาน rows have no F2F2F2 shading")
	}
	// รวม closes with its own rule.
	if !strings.Contains(doc, "<w:tcBorders>") {
		t.Error("the รวม row carries no closing bottom rule")
	}
	// The signature block is explicitly borderless: it inherits Table Grid in
	// Word, so omitting borders is not the same as switching them off.
	if !strings.Contains(doc, `<w:top w:val="none" w:sz="0" w:space="0" w:color="auto"/>`) {
		t.Error("the signature table does not explicitly clear its borders and would print boxed")
	}
	// Column widths are the college's own.
	for _, w := range []string{`w:w="4253"`, `w:w="992"`, `w:w="4111"`} {
		if !strings.Contains(doc, w) {
			t.Errorf("month grid is missing the college's column width %s", w)
		}
	}
}

// An odd number of months still prints two-up, with the right-hand half blank —
// the college's own file does exactly this. The failure it guards against is a
// stray "จำนวนชั่วโมง" heading over an empty column.
func TestBuildWorkloadDetailDOCX_OddMonthCountLeavesTheRightHalfBlank(t *testing.T) {
	d := sampleWorkload()
	d.Months = d.Months[:1]
	b, err := BuildWorkloadDetailDOCX(d)
	if err != nil {
		t.Fatal(err)
	}
	doc := partText(t, b, "word/document.xml")
	if n := strings.Count(doc, "จำนวนชั่วโมง"); n != 1 {
		t.Errorf("จำนวนชั่วโมง appears %d times for a single month, want 1 — "+
			"the empty half must not carry a column heading", n)
	}
	// เตรียมการสอน rather than ช่วยสอน as the probe: the latter is a substring
	// of ผู้ช่วยสอน in the title and the identification line, so counting it
	// measures the heading, not the table.
	if n := strings.Count(doc, "เตรียมการสอน"); n != 1 {
		t.Errorf("เตรียมการสอน appears %d times for a single month, want 1", n)
	}

	// ...and two months really do print two blocks, so the check above is
	// measuring the month count rather than a layout that only ever emits one.
	two := sampleWorkload()
	two.Months = two.Months[:2]
	b2, err := BuildWorkloadDetailDOCX(two)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(partText(t, b2, "word/document.xml"), "เตรียมการสอน"); n != 2 {
		t.Errorf("เตรียมการสอน appears %d times for two months, want 2", n)
	}
}

// A term with no certifier on file leaves a bare signature line rather than
// inventing a name — the same rule the claim workbook follows.
func TestBuildWorkloadDetailDOCX_NoCertifierLeavesTheLineBlank(t *testing.T) {
	d := sampleWorkload()
	d.CertifierName, d.CertifierTitle, d.CertifierActingFor = "", "", ""
	b, err := BuildWorkloadDetailDOCX(d)
	if err != nil {
		t.Fatal(err)
	}
	doc := partText(t, b, "word/document.xml")
	if strings.Contains(doc, "ณกร") {
		t.Error("a name leaked into the certifier block when none was resolved")
	}
	if !strings.Contains(doc, "ผู้รับรอง") {
		t.Error("the certifier column heading must remain for a wet signature")
	}
}
