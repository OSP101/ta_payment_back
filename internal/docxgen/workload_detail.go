// workload_detail.go builds "แบบแสดงรายละเอียดภาระงานของผู้ช่วยสอน" — the form
// that backs a GRADUATE TA's hourly claim with a month-by-month breakdown of
// what those hours were spent on.
//
// Modelled on the college's own docs/14.CP363761-บัณฑิต.docx: a short header
// naming the course, the lecturer and the TA, then one bordered block per month
// listing ช่วยสอน / เตรียมการสอน / ตรวจแบบทดสอบ and their รวม, laid out two
// months side by side, and finally two signature columns — the course lecturer
// who witnessed the work, and the certifier who approves the claim.
//
// Built the same way as appointment_order.go, by writing OOXML strings into a
// zip: no template is read at runtime, so the layout is the code's to keep
// correct and can be tested without opening Word. This document carries no
// image, so it ships without the media part and its relationship.
package docxgen

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
)

// WorkloadMonth is one month's block on the form. Hours are pre-formatted
// strings so a zero can print as an empty cell rather than "0" — the college's
// own form leaves a row blank when there was no work of that kind, and a grid
// of zeros reads as a claim for nothing.
type WorkloadMonth struct {
	Label     string // "เดือน พฤศจิกายน 2568"
	HelpTeach string // ช่วยสอน
	Prep      string // เตรียมการสอน
	Review    string // ตรวจแบบทดสอบ
	Total     string // รวม
}

// WorkloadDetailData is one TA's form.
type WorkloadDetailData struct {
	SemesterLabel string // "ภาคปลาย"
	AcademicYear  string // "2568"
	CourseCode    string // "CP363761"
	CourseName    string // "Seminar in Information Technology"
	CreditText    string // "1 (1-0-2)"
	LecturerName  string // "ผศ.ดร.วรัญญา วรรณศรี"
	TAName        string // "นายวรพจน์ สุวรรณภิภพ"
	StudentID     string // "627020002-0"
	LevelLabel    string // "ระดับบัณฑิตศึกษา"
	Months        []WorkloadMonth
	// Certifier lines, already worded by the caller. Empty CertifierName leaves
	// the block as a bare signature line for a wet signature rather than
	// inventing a name — the same rule the claim workbook follows.
	CertifierName      string // "(ผศ.ดร.ณกร วัฒนกิจ)"
	CertifierTitle     string // "ตำแหน่ง รองคณบดีฝ่ายวิชาการ"
	CertifierActingFor string // "รักษาการแทนหัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์"
}

// This document has no image, so its content types and relationships are
// narrower than the appointment order's — declaring a png default and an image
// relationship with no part behind them produces a file Word repairs on open.
const workloadContentTypes = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
  <Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>
</Types>`

const workloadDocumentRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>
</Relationships>`

// BuildWorkloadDetailDOCX returns a ready-to-write .docx file.
func BuildWorkloadDetailDOCX(d WorkloadDetailData) ([]byte, error) {
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	for _, f := range []struct{ name, body string }{
		{"[Content_Types].xml", workloadContentTypes},
		{"_rels/.rels", rootRels},
		{"word/_rels/document.xml.rels", workloadDocumentRels},
		{"word/styles.xml", stylesXML},
		{"word/document.xml", buildWorkloadDocumentXML(d)},
	} {
		w, err := zw.Create(f.name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(f.body)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildWorkloadDocumentXML(d WorkloadDetailData) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<w:document ` +
		`xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" ` +
		`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">`)
	b.WriteString(`<w:body>`)

	writePara(&b, "center", true, 0, "แบบแสดงรายละเอียดภาระงานของผู้ช่วยสอน")
	writePara(&b, "center", true, 0,
		"สาขาวิชาวิทยาการคอมพิวเตอร์ วิทยาลัยการคอมพิวเตอร์ มหาวิทยาลัยขอนแก่น")
	writePara(&b, "center", true, 0,
		"ประจำ"+d.SemesterLabel+"  ปีการศึกษา "+d.AcademicYear)
	writePara(&b, "left", false, 0, "")

	// The three identification lines. Tabs rather than a table: the college's
	// file writes them as plain tabbed paragraphs, and a table here would box
	// text that is meant to read as a sentence.
	writeTabbedPara(&b, "รหัส-ชื่อวิชา", strings.TrimSpace(
		d.CourseCode+"\t"+d.CourseName+"\t"+d.CreditText))
	writeTabbedPara(&b, "อาจารย์ประจำวิชา", d.LecturerName)
	ta := d.TAName
	if d.StudentID != "" {
		ta += "\t   รหัสประจำตัว  " + d.StudentID
	}
	if d.LevelLabel != "" {
		ta += "\t" + d.LevelLabel
	}
	writeTabbedPara(&b, "ผู้ช่วยสอน", ta)
	writePara(&b, "left", false, 0, "")

	// Month blocks, two side by side — the shape of the college's form, and the
	// reason a term of five months still prints on one page.
	for i := 0; i < len(d.Months); i += 2 {
		right := WorkloadMonth{}
		if i+1 < len(d.Months) {
			right = d.Months[i+1]
		}
		writeWorkloadMonthPair(&b, d.Months[i], right)
		writePara(&b, "left", false, 0, "")
	}

	writePara(&b, "left", false, 0, "")
	writeWorkloadSignatures(&b, d)

	b.WriteString(`<w:sectPr>` +
		`<w:pgSz w:w="11907" w:h="16840"/>` +
		`<w:pgMar w:top="851" w:right="1134" w:bottom="851" w:left="1134" w:header="720" w:footer="720" w:gutter="0"/>` +
		`</w:sectPr>`)
	b.WriteString(`</w:body></w:document>`)
	return b.String()
}

// writeTabbedPara writes "<label>\t<value>" with the label bold, so the three
// header lines line up the way the college's form does.
func writeTabbedPara(b *strings.Builder, label, value string) {
	b.WriteString(`<w:p><w:pPr><w:tabs>` +
		`<w:tab w:val="left" w:pos="2160"/>` +
		`<w:tab w:val="left" w:pos="4320"/>` +
		`<w:tab w:val="left" w:pos="7200"/>` +
		`</w:tabs><w:jc w:val="left"/></w:pPr>`)
	b.WriteString(`<w:r>`)
	writeRunPr(b, true, 0)
	b.WriteString(`<w:t xml:space="preserve">`)
	escapeXML(b, label)
	b.WriteString(`</w:t></w:r>`)
	// Each \t becomes a real tab run: Word ignores a literal tab character
	// inside <w:t> and needs the <w:tab/> element instead, so a value split on
	// tabs would otherwise print as one run-on line.
	for _, part := range strings.Split(value, "\t") {
		b.WriteString(`<w:r><w:tab/>`)
		if part != "" {
			b.WriteString(`<w:t xml:space="preserve">`)
			escapeXML(b, part)
			b.WriteString(`</w:t>`)
		}
		b.WriteString(`</w:r>`)
	}
	b.WriteString(`</w:p>`)
}

// workloadGrid is the four-column month grid in twips: งาน | ชั่วโมง | งาน |
// ชั่วโมง — the college's own widths from docs/14.CP363761-บัณฑิต.docx.
var workloadGrid = [4]int{4253, 992, 4111, 992}

// Formatting lifted from that file rather than invented:
//
//   - borders are Word's built-in Table Grid — sz 4 (½pt) in the AUTOMATIC
//     colour, not a hard-coded black. Hard-coding #000000 is visible: it stops
//     following the document/theme colour, which is what the office noticed.
//   - the month caption row and the งาน row sit on a light grey F2F2F2 fill.
//   - the รวม row closes with its own bottom rule.
const (
	workloadBorderSz    = "4"
	workloadBorderColor = "auto"
	workloadHeadFill    = "F2F2F2"
	// captionRowHeight is the taller first row of each block (twips).
	captionRowHeight = 733
)

func workloadBorder(edge string) string {
	return fmt.Sprintf(`<w:%s w:val="single" w:sz="%s" w:space="0" w:color="%s"/>`,
		edge, workloadBorderSz, workloadBorderColor)
}

// writeWorkloadMonthPair renders two months side by side as one bordered table.
// An empty right-hand month prints its half blank, exactly as the college's own
// form does when a term has an odd number of months.
func writeWorkloadMonthPair(b *strings.Builder, left, right WorkloadMonth) {
	b.WriteString(`<w:tbl><w:tblPr><w:tblW w:w="0" w:type="auto"/>` +
		`<w:tblInd w:w="250" w:type="dxa"/>`)
	b.WriteString(`<w:tblBorders>`)
	for _, e := range []string{"top", "left", "bottom", "right", "insideH", "insideV"} {
		b.WriteString(workloadBorder(e))
	}
	b.WriteString(`</w:tblBorders></w:tblPr>`)
	b.WriteString(`<w:tblGrid>`)
	for _, w := range workloadGrid {
		fmt.Fprintf(b, `<w:gridCol w:w="%d"/>`, w)
	}
	b.WriteString(`</w:tblGrid>`)

	hasRight := right.Label != ""
	hours := func(m WorkloadMonth, pick func(WorkloadMonth) string) string {
		if m.Label == "" {
			return ""
		}
		return pick(m)
	}
	// A work row: the label reads left, the figure centred — the label column is
	// a list of duties, not a heading.
	workRow := func(label string, pick func(WorkloadMonth) string) {
		rightLabel := label
		if !hasRight {
			rightLabel = ""
		}
		writeWorkloadRow(b, workloadRow{cells: []workloadCell{
			{text: label},
			{text: hours(left, pick), align: "center"},
			{text: rightLabel},
			{text: hours(right, pick), align: "center"},
		}})
	}

	rightCaption, rightHoursCaption := "", ""
	if hasRight {
		rightCaption, rightHoursCaption = right.Label, "จำนวนชั่วโมง"
	}
	// Month caption, then the งาน / ชั่วโมง headings — both on the grey band.
	writeWorkloadRow(b, workloadRow{height: captionRowHeight, bold: true, fill: workloadHeadFill,
		cells: []workloadCell{
			{text: left.Label, align: "center"}, {text: "จำนวนชั่วโมง", align: "center"},
			{text: rightCaption, align: "center"}, {text: rightHoursCaption, align: "center"},
		}})
	rightWork := ""
	if hasRight {
		rightWork = "งาน"
	}
	writeWorkloadRow(b, workloadRow{bold: true, fill: workloadHeadFill,
		cells: []workloadCell{
			{text: "งาน", align: "center"}, {},
			{text: rightWork, align: "center"}, {},
		}})

	workRow("ช่วยสอน", func(m WorkloadMonth) string { return m.HelpTeach })
	workRow("เตรียมการสอน", func(m WorkloadMonth) string { return m.Prep })
	workRow("ตรวจแบบทดสอบ", func(m WorkloadMonth) string { return m.Review })
	// The college's form leaves a blank row above รวม; it is what makes the
	// total read as a total rather than a fourth kind of work.
	writeWorkloadRow(b, workloadRow{cells: make([]workloadCell, 4)})

	rightTotal := ""
	if hasRight {
		rightTotal = "รวม"
	}
	writeWorkloadRow(b, workloadRow{bold: true, bottomRule: true, cells: []workloadCell{
		{text: "รวม", align: "center"},
		{text: hours(left, func(m WorkloadMonth) string { return m.Total }), align: "center"},
		{text: rightTotal, align: "center"},
		{text: hours(right, func(m WorkloadMonth) string { return m.Total }), align: "center"},
	}})

	b.WriteString(`</w:tbl>`)
}

// workloadCell is one cell; an empty align means Word's default (left).
type workloadCell struct{ text, align string }

// workloadRow carries the per-row formatting the college's form actually uses.
type workloadRow struct {
	cells      []workloadCell
	bold       bool
	fill       string // cell shading, "" for none
	height     int    // twips, 0 = natural
	bottomRule bool   // an explicit closing rule under this row
}

func writeWorkloadRow(b *strings.Builder, r workloadRow) {
	b.WriteString(`<w:tr>`)
	if r.height > 0 {
		fmt.Fprintf(b, `<w:trPr><w:trHeight w:val="%d"/></w:trPr>`, r.height)
	}
	for i, c := range r.cells {
		width := 0
		if i < len(workloadGrid) {
			width = workloadGrid[i]
		}
		b.WriteString(`<w:tc><w:tcPr>`)
		fmt.Fprintf(b, `<w:tcW w:w="%d" w:type="dxa"/>`, width)
		if r.bottomRule {
			b.WriteString(`<w:tcBorders>` + workloadBorder("bottom") + `</w:tcBorders>`)
		}
		if r.fill != "" {
			fmt.Fprintf(b, `<w:shd w:val="clear" w:color="auto" w:fill="%s"/>`, r.fill)
		}
		b.WriteString(`<w:vAlign w:val="center"/></w:tcPr>`)
		// contextualSpacing, as the college's file uses, rather than explicit
		// before/after — the rows sit tight against the rules that way.
		b.WriteString(`<w:p><w:pPr><w:contextualSpacing/>`)
		if c.align != "" {
			fmt.Fprintf(b, `<w:jc w:val="%s"/>`, c.align)
		}
		b.WriteString(`</w:pPr><w:r>`)
		writeRunPr(b, r.bold, 0)
		b.WriteString(`<w:t xml:space="preserve">`)
		escapeXML(b, c.text)
		b.WriteString(`</w:t></w:r></w:p></w:tc>`)
	}
	b.WriteString(`</w:tr>`)
}

// signatureGrid splits the page in two for the closing block.
var signatureGrid = [2]int{4940, 4941}

// writeWorkloadSignatures renders the closing block: the course lecturer on the
// left (they witnessed the work), the certifier on the right (they approve the
// claim).
//
// Explicitly borderless, every edge set to none rather than simply omitted:
// the college's file does the same, because this block inherits Table Grid in
// Word and would otherwise print a box around the signatures.
func writeWorkloadSignatures(b *strings.Builder, d WorkloadDetailData) {
	b.WriteString(`<w:tbl><w:tblPr><w:tblW w:w="0" w:type="auto"/>` +
		`<w:tblInd w:w="250" w:type="dxa"/><w:tblBorders>`)
	for _, e := range []string{"top", "left", "bottom", "right", "insideH", "insideV"} {
		fmt.Fprintf(b, `<w:%s w:val="none" w:sz="0" w:space="0" w:color="auto"/>`, e)
	}
	b.WriteString(`</w:tblBorders></w:tblPr>`)
	b.WriteString(`<w:tblGrid>`)
	for _, w := range signatureGrid {
		fmt.Fprintf(b, `<w:gridCol w:w="%d"/>`, w)
	}
	b.WriteString(`</w:tblGrid>`)

	row := func(bold bool, left, right string) {
		b.WriteString(`<w:tr>`)
		for i, text := range []string{left, right} {
			b.WriteString(`<w:tc><w:tcPr>`)
			fmt.Fprintf(b, `<w:tcW w:w="%d" w:type="dxa"/>`, signatureGrid[i])
			b.WriteString(`</w:tcPr>`)
			b.WriteString(`<w:p><w:pPr><w:contextualSpacing/><w:jc w:val="center"/></w:pPr><w:r>`)
			writeRunPr(b, bold, 0)
			b.WriteString(`<w:t xml:space="preserve">`)
			escapeXML(b, text)
			b.WriteString(`</w:t></w:r></w:p></w:tc>`)
		}
		b.WriteString(`</w:tr>`)
	}

	row(true, "อาจารย์ประจำวิชา", "ผู้รับรอง")
	// Blank rows leave room for a wet signature.
	row(false, "", "")
	row(false, "", "")
	row(false, "ลงชื่อ…………......……………………", "ลงชื่อ…………....………........…………")
	lecturer := d.LecturerName
	if lecturer != "" && !strings.HasPrefix(lecturer, "(") {
		lecturer = "(" + lecturer + ")"
	}
	row(false, lecturer, d.CertifierName)
	// Position and acting-for on ONE line, as the college's file writes them —
	// two rows would push the block onto a second page on a five-month term.
	if position := strings.TrimSpace(d.CertifierTitle + " " + d.CertifierActingFor); position != "" {
		row(false, "", position)
	}
	b.WriteString(`</w:tbl>`)
}
