// docxgen builds the formal "คำสั่งแต่งตั้งผู้ช่วยสอนและผู้ช่วยปฏิบัติงาน" as a
// DOCX, laid out to match the KKU registrar template in
// document_template/คำสั่ง6-2569*.docx: the KKU seal at the top, TH Sarabun New
// throughout (the Thai government standard font at 16pt), an order page
// (heading → statutory preamble → effective clause → dean signature), then a
// page break to the บัญชีแนบท้าย (attached roster) grouped by TA level and course.
//
// The file is assembled as a minimal-but-valid OOXML ZIP (no unioffice/gooxml):
//   [Content_Types].xml, _rels/.rels,
//   word/document.xml, word/styles.xml,
//   word/_rels/document.xml.rels, word/media/image1.png
// Word / LibreOffice / Pages all accept this shape. Staff open it, tweak in
// Word if needed, and forward to the registrar.
package docxgen

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"encoding/xml"
	"fmt"
	"strings"
)

// kkuSeal is the มหาวิทยาลัยขอนแก่น emblem placed at the head of the order.
// Sourced from the registrar template (word/media/image1.png, 187×331 px).
//
//go:embed kku_seal.png
var kkuSeal []byte

// Seal display size, in EMU (English Metric Units, 914400 per inch). Matches the
// template: ~2.0 cm wide × ~3.6 cm tall, preserving the 187:331 aspect ratio.
const (
	sealCX = 727075
	sealCY = 1290320
)

// Appointee is one TA on the roster. First and last names are separate
// columns in the template's roster.
type Appointee struct {
	StudentID string // "663380555-8"
	FirstName string // "ชาคริต"
	LastName  string // "อ่วมอ่ำ"
}

// CourseGroup is one course block within a level: a heading line plus its TAs.
type CourseGroup struct {
	Code       string // "SC310003"
	Name       string // "Database System and Design"
	CreditText string // "3 (3-0-6)"
	Appointees []Appointee
}

// LevelGroup buckets courses under a study-level heading
// ("รายวิชาระดับปริญญาตรี" / "รายวิชาระดับบัณฑิตศึกษา").
type LevelGroup struct {
	Heading string
	Courses []CourseGroup
}

// AppointmentOrderData is one document's inputs.
type AppointmentOrderData struct {
	OrderNo       string // "6/2569"
	AcademicYear  string // "2568"
	SemesterLabel string // "ภาคปลาย"
	OrderDate     string // "14 มกราคม 2569"
	EffectiveDate string // "24 พฤศจิกายน 2568"
	SignerName    string // "รองศาสตราจารย์สิรภัทร เชี่ยวชาญวัฒนา"
	SignerTitle   string // "คณบดีวิทยาลัยการคอมพิวเตอร์"
	Levels        []LevelGroup
}

const contentTypes = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Default Extension="png" ContentType="image/png"/>
  <Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
  <Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>
</Types>`

const rootRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>
</Relationships>`

const documentRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="media/image1.png"/>
</Relationships>`

// styles.xml pins TH Sarabun New at 16pt (sz/szCs 32) as the document default,
// matching the government-letter standard. Everything inherits unless a run
// overrides the size (e.g. the larger title lines).
const stylesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:docDefaults>
    <w:rPrDefault><w:rPr>
      <w:rFonts w:ascii="TH Sarabun New" w:hAnsi="TH Sarabun New" w:cs="TH Sarabun New" w:eastAsia="TH Sarabun New"/>
      <w:sz w:val="32"/><w:szCs w:val="32"/>
      <w:lang w:val="th-TH" w:eastAsia="th-TH" w:bidi="th-TH"/>
    </w:rPr></w:rPrDefault>
    <w:pPrDefault/>
  </w:docDefaults>
  <w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>
</w:styles>`

// BuildAppointmentOrderDOCX returns a ready-to-write .docx file.
func BuildAppointmentOrderDOCX(d AppointmentOrderData) ([]byte, error) {
	docXML := buildDocumentXML(d)
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)

	// Text parts.
	textParts := []struct{ name, body string }{
		{"[Content_Types].xml", contentTypes},
		{"_rels/.rels", rootRels},
		{"word/_rels/document.xml.rels", documentRels},
		{"word/styles.xml", stylesXML},
		{"word/document.xml", docXML},
	}
	for _, f := range textParts {
		w, err := zw.Create(f.name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(f.body)); err != nil {
			return nil, err
		}
	}
	// Binary part: the seal image.
	iw, err := zw.Create("word/media/image1.png")
	if err != nil {
		return nil, err
	}
	if _, err := iw.Write(kkuSeal); err != nil {
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildDocumentXML(d AppointmentOrderData) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<w:document ` +
		`xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" ` +
		`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" ` +
		`xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" ` +
		`xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" ` +
		`xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture">`)
	b.WriteString(`<w:body>`)

	// ---- Order page -------------------------------------------------------
	writeSeal(&b)
	// Title lines: bold at the body size (the template keeps everything 16pt).
	writePara(&b, "center", true, 0, "คำสั่งวิทยาลัยการคอมพิวเตอร์")
	writePara(&b, "center", true, 0, "ที่ "+d.OrderNo)
	// Subject broken across three centered lines exactly like the template —
	// one long paragraph would let Word wrap mid-word (e.g. "ปีการ/ศึกษา").
	writePara(&b, "center", true, 0,
		"เรื่อง  แต่งตั้งผู้ช่วยสอนและผู้ช่วยปฏิบัติงาน ระดับปริญญาตรีและระดับบัณฑิตศึกษา")
	writePara(&b, "center", true, 0,
		"ภาคปกติและโครงการพิเศษ สาขาวิชาวิทยาการคอมพิวเตอร์ มหาวิทยาลัยขอนแก่น")
	writePara(&b, "center", true, 0, "ประจำ"+d.SemesterLabel+"  ปีการศึกษา "+d.AcademicYear)
	writePara(&b, "center", false, 0, strings.Repeat("-", 61))

	// Statutory preamble (Thai-distributed justification, as the template).
	writePara(&b, "thaiDistribute", false, 0,
		"          เพื่อให้การดำเนินการจัดการเรียนการสอนตามหลักสูตร สาขาวิทยาการคอมพิวเตอร์ "+
			"สาขาเทคโนโลยีสารสนเทศ สาขาภูมิสารสนเทศศาสตร์ สาขาปัญญาประดิษฐ์ และสาขาความมั่นคงปลอดภัยไซเบอร์ "+
			"ในระดับปริญญาตรีและระดับบัณฑิตศึกษา เป็นไปด้วยความเรียบร้อย มีประสิทธิภาพและบังเกิดผลดี")
	writePara(&b, "thaiDistribute", false, 0,
		"          อาศัยอำนาจตามความในมาตรา 40 แห่งพระราชบัญญัติมหาวิทยาลัยขอนแก่น พ.ศ. 2558 "+
			"จึงแต่งตั้งผู้ช่วยสอนและผู้ช่วยปฏิบัติงาน ระดับปริญญาตรีและระดับบัณฑิตศึกษา "+
			"ภาคปกติและโครงการพิเศษ สาขาวิชาวิทยาการคอมพิวเตอร์ วิทยาลัยการคอมพิวเตอร์ "+
			"มหาวิทยาลัยขอนแก่น ประจำ"+d.SemesterLabel+" ปีการศึกษา "+d.AcademicYear+
			" ดังบัญชีรายชื่อแนบท้ายคำสั่งนี้")
	writePara(&b, "left", false, 0, "")

	// Effective clause, then the order date on the next line at a deeper
	// indent — both left-aligned, back to back. The template double-spaces
	// around each date component (Thai government letter style).
	writePara(&b, "left", false, 0, "          ทั้งนี้  ตั้งแต่วันที่  "+d.EffectiveDate+"  เป็นต้นไป")
	writePara(&b, "left", false, 0, "                    สั่ง  ณ  วันที่  "+d.OrderDate)

	// Signature block (blank lines leave room for a wet signature).
	for i := 0; i < 3; i++ {
		writePara(&b, "center", false, 0, "")
	}
	writePara(&b, "center", false, 0, "("+d.SignerName+")")
	writePara(&b, "center", false, 0, d.SignerTitle)

	// ---- Page break to the attached roster --------------------------------
	writePageBreak(&b)

	// Roster header: three centered bold lines, a full-width asterisk rule,
	// then a short centered "*****" — exactly as the template.
	writePara(&b, "center", true, 0,
		"รายชื่อผู้ช่วยสอนและผู้ช่วยปฏิบัติงาน ระดับปริญญาตรีและระดับบัณฑิตศึกษา")
	writePara(&b, "center", true, 0, "ประจำ"+d.SemesterLabel+"  ปีการศึกษา "+d.AcademicYear)
	writePara(&b, "center", true, 0,
		"(บัญชีแนบท้ายคำสั่งวิทยาลัยการคอมพิวเตอร์ ที่ "+d.OrderNo+"  ลงวันที่  "+d.OrderDate+")")
	writePara(&b, "center", true, 0, strings.Repeat("*", 84))
	writePara(&b, "center", true, 0, "*****")
	writePara(&b, "left", false, 0, "")

	// Level → course → borderless roster block.
	for li, lv := range d.Levels {
		writePara(&b, "left", true, 0, fmt.Sprintf("%d %s", li+1, lv.Heading))
		for ci, c := range lv.Courses {
			writeCourseBlock(&b, fmt.Sprintf("%d.%d", li+1, ci+1), c)
			writePara(&b, "left", false, 0, "")
		}
	}

	// Section properties: A4, government-letter margins (from the template).
	b.WriteString(`<w:sectPr>` +
		`<w:pgSz w:w="11907" w:h="16840"/>` +
		`<w:pgMar w:top="851" w:right="1134" w:bottom="851" w:left="1134" w:header="720" w:footer="720" w:gutter="0"/>` +
		`</w:sectPr>`)

	b.WriteString(`</w:body></w:document>`)
	return b.String()
}

// writeSeal emits a centered paragraph containing the inline KKU emblem.
func writeSeal(b *strings.Builder) {
	b.WriteString(`<w:p><w:pPr><w:jc w:val="center"/></w:pPr><w:r><w:drawing>`)
	fmt.Fprintf(b, `<wp:inline distT="0" distB="0" distL="0" distR="0">`+
		`<wp:extent cx="%d" cy="%d"/>`+
		`<wp:effectExtent l="0" t="0" r="0" b="0"/>`+
		`<wp:docPr id="1" name="ตรามหาวิทยาลัยขอนแก่น"/>`+
		`<wp:cNvGraphicFramePr><a:graphicFrameLocks noChangeAspect="1"/></wp:cNvGraphicFramePr>`+
		`<a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/picture">`+
		`<pic:pic><pic:nvPicPr><pic:cNvPr id="1" name="image1.png"/><pic:cNvPicPr/></pic:nvPicPr>`+
		`<pic:blipFill><a:blip r:embed="rId2"/><a:stretch><a:fillRect/></a:stretch></pic:blipFill>`+
		`<pic:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="%d" cy="%d"/></a:xfrm>`+
		`<a:prstGeom prst="rect"><a:avLst/></a:prstGeom></pic:spPr>`+
		`</pic:pic></a:graphicData></a:graphic></wp:inline>`,
		sealCX, sealCY, sealCX, sealCY)
	b.WriteString(`</w:drawing></w:r></w:p>`)
}

func writePageBreak(b *strings.Builder) {
	b.WriteString(`<w:p><w:r><w:br w:type="page"/></w:r></w:p>`)
}

// writePara appends one paragraph. align: left/center/right/both. szHalf is the
// font size in half-points (0 = inherit the 16pt default). Text is XML-escaped.
func writePara(b *strings.Builder, align string, bold bool, szHalf int, text string) {
	b.WriteString(`<w:p><w:pPr><w:jc w:val="`)
	b.WriteString(align)
	b.WriteString(`"/></w:pPr><w:r>`)
	writeRunPr(b, bold, szHalf)
	b.WriteString(`<w:t xml:space="preserve">`)
	escapeXML(b, text)
	b.WriteString(`</w:t></w:r></w:p>`)
}

// writeRunPr writes a <w:rPr> when any property is set. Thai text needs the
// complex-script twins: w:bCs alongside w:b, and w:szCs alongside w:sz.
func writeRunPr(b *strings.Builder, bold bool, szHalf int) {
	if !bold && szHalf == 0 {
		return
	}
	b.WriteString(`<w:rPr>`)
	if bold {
		b.WriteString(`<w:b/><w:bCs/>`)
	}
	if szHalf > 0 {
		fmt.Fprintf(b, `<w:sz w:val="%d"/><w:szCs w:val="%d"/>`, szHalf, szHalf)
	}
	b.WriteString(`</w:rPr>`)
}

// Roster grid, in twips, lifted from the registrar template: indent | ที่ |
// รหัสประจำตัว | ชื่อ | สกุล | หน่วยกิต. The table draws NO borders — the template
// uses an invisible table purely for column alignment.
var rosterGrid = [6]int{578, 662, 2307, 2939, 1880, 1515}

// tblCell is one cell spec: text, alignment, gridSpan (0/1 = one column).
type tblCell struct {
	text  string
	align string
	span  int
}

// writeCourseBlock renders one course as a borderless table:
//
//	1.1 | วิชา | CODE Course Name            | 3 (3-0-6)
//	    | ที่  | รหัสประจำตัว | ชื่อ-สกุล    |
//	    | 1   | 663380555-8 | ชาคริต | อ่วมอ่ำ |
func writeCourseBlock(b *strings.Builder, idx string, c CourseGroup) {
	b.WriteString(`<w:tbl><w:tblPr><w:tblW w:w="9881" w:type="dxa"/><w:tblLayout w:type="fixed"/></w:tblPr>`)
	b.WriteString(`<w:tblGrid>`)
	for _, w := range rosterGrid {
		fmt.Fprintf(b, `<w:gridCol w:w="%d"/>`, w)
	}
	b.WriteString(`</w:tblGrid>`)

	// Course line: 1.1 | วิชา | code + name (spans ชื่อ+สกุล+รหัส) | credit.
	writeRosterRow(b, true, []tblCell{
		{idx, "right", 1},
		{"วิชา", "center", 1},
		{c.Code + " " + c.Name, "left", 3},
		{c.CreditText, "left", 1},
	})
	// Header line: ที่ | รหัสประจำตัว | ชื่อ-สกุล (spans ชื่อ+สกุล).
	writeRosterRow(b, true, []tblCell{
		{"", "left", 1},
		{"ที่", "center", 1},
		{"รหัสประจำตัว", "left", 1},
		{"ชื่อ-สกุล", "left", 2},
		{"", "left", 1},
	})
	for i, r := range c.Appointees {
		id := r.StudentID
		if strings.TrimSpace(id) == "" {
			id = "-"
		}
		writeRosterRow(b, false, []tblCell{
			{"", "left", 1},
			{fmt.Sprintf("%d", i+1), "center", 1},
			{id, "left", 1},
			{r.FirstName, "left", 1},
			{r.LastName, "left", 1},
			{"", "left", 1},
		})
	}
	b.WriteString(`</w:tbl>`)
}

// writeRosterRow emits one borderless row. Cell paragraphs carry a little
// before/after spacing so rows breathe like the template's.
func writeRosterRow(b *strings.Builder, bold bool, cells []tblCell) {
	b.WriteString(`<w:tr>`)
	col := 0
	for _, c := range cells {
		span := c.span
		if span < 1 {
			span = 1
		}
		width := 0
		for i := 0; i < span && col+i < len(rosterGrid); i++ {
			width += rosterGrid[col+i]
		}
		col += span
		b.WriteString(`<w:tc><w:tcPr>`)
		fmt.Fprintf(b, `<w:tcW w:w="%d" w:type="dxa"/>`, width)
		if span > 1 {
			fmt.Fprintf(b, `<w:gridSpan w:val="%d"/>`, span)
		}
		b.WriteString(`<w:vAlign w:val="center"/></w:tcPr>`)
		b.WriteString(`<w:p><w:pPr><w:spacing w:before="40" w:after="40"/><w:jc w:val="`)
		b.WriteString(c.align)
		b.WriteString(`"/></w:pPr><w:r>`)
		writeRunPr(b, bold, 0)
		b.WriteString(`<w:t xml:space="preserve">`)
		escapeXML(b, c.text)
		b.WriteString(`</w:t></w:r></w:p></w:tc>`)
	}
	b.WriteString(`</w:tr>`)
}

func escapeXML(b *strings.Builder, s string) {
	_ = xml.EscapeText(b, []byte(s))
}
