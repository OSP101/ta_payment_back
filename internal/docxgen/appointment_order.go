// docxgen builds minimal-but-valid DOCX files without a heavy dependency
// (no unioffice / gooxml). The file is a ZIP of three parts:
//   [Content_Types].xml, _rels/.rels, word/document.xml
// Word / LibreOffice / Pages all accept this shape. Formatting is intentionally
// plain — the goal is a machine-produced DOCX that staff can open, tweak in
// Word, and forward to registrar. Not a pixel-perfect template match.
package docxgen

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
)

// Appointee mirrors pdfgen.AppointmentAppointee but kept local so the two
// generators don't couple (docx does not need PDF-specific fields).
type Appointee struct {
	FullName   string
	Level      string
	Track      string
	CourseCode string
	Returning  bool
}

// AppointmentOrderData is one document's inputs.
type AppointmentOrderData struct {
	OrderNo       string
	AcademicYear  string
	SemesterLabel string
	OrderDate     string
	EffectiveDate string
	SignerName    string
	SignerTitle   string
	Appointees    []Appointee
}

const contentTypes = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
</Types>`

const rootRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>
</Relationships>`

// BuildAppointmentOrderDOCX returns a ready-to-write .docx file.
func BuildAppointmentOrderDOCX(d AppointmentOrderData) ([]byte, error) {
	docXML, err := buildDocumentXML(d)
	if err != nil {
		return nil, err
	}
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	files := []struct {
		name, body string
	}{
		{"[Content_Types].xml", contentTypes},
		{"_rels/.rels", rootRels},
		{"word/document.xml", docXML},
	}
	for _, f := range files {
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

func buildDocumentXML(d AppointmentOrderData) (string, error) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)

	// Header block — centered, bold
	writePara(&b, "center", true, "คำสั่งวิทยาลัยการคอมพิวเตอร์")
	writePara(&b, "center", true, "มหาวิทยาลัยขอนแก่น")
	writePara(&b, "center", true, "ที่ "+d.OrderNo)
	writePara(&b, "center", false, "")
	writePara(&b, "left", true, "เรื่อง แต่งตั้งนักศึกษาผู้ช่วยสอน / ผู้ช่วยปฏิบัติงาน ประจำ"+d.SemesterLabel+" ปีการศึกษา "+d.AcademicYear)
	writePara(&b, "left", false, "")

	// Body preamble
	writePara(&b, "thaiDistribute", false,
		"     เพื่อให้การเรียนการสอนของวิทยาลัยการคอมพิวเตอร์ มหาวิทยาลัยขอนแก่น เป็นไปด้วยความเรียบร้อยและมีประสิทธิภาพ อาศัยอำนาจตามความในมาตรา 37 แห่งพระราชบัญญัติมหาวิทยาลัยขอนแก่น พ.ศ. 2558 จึงแต่งตั้งบุคคลตามรายชื่อดังต่อไปนี้เป็นนักศึกษาผู้ช่วยสอนและผู้ช่วยปฏิบัติงาน")
	writePara(&b, "left", false, "")

	// Table
	writeTable(&b, d.Appointees)
	writePara(&b, "left", false, "")

	// Effective + date
	writePara(&b, "left", false, "     ทั้งนี้ ตั้งแต่วันที่ "+d.EffectiveDate+" เป็นต้นไป")
	writePara(&b, "right", false, "สั่ง ณ วันที่ "+d.OrderDate)
	writePara(&b, "center", false, "")
	writePara(&b, "center", false, "")
	writePara(&b, "center", false, "")
	writePara(&b, "center", true, "("+d.SignerName+")")
	writePara(&b, "center", false, d.SignerTitle)

	b.WriteString(`</w:body></w:document>`)
	return b.String(), nil
}

// writePara appends one paragraph with alignment (left/center/right) and
// optional bold. Text is XML-escaped.
func writePara(b *strings.Builder, align string, bold bool, text string) {
	b.WriteString(`<w:p><w:pPr><w:jc w:val="`)
	b.WriteString(align)
	b.WriteString(`"/></w:pPr>`)
	b.WriteString(`<w:r>`)
	if bold {
		b.WriteString(`<w:rPr><w:b/></w:rPr>`)
	}
	b.WriteString(`<w:t xml:space="preserve">`)
	escapeXML(b, text)
	b.WriteString(`</w:t></w:r></w:p>`)
}

func writeTable(b *strings.Builder, rows []Appointee) {
	b.WriteString(`<w:tbl><w:tblPr><w:tblW w:w="0" w:type="auto"/><w:tblBorders>`)
	for _, edge := range []string{"top", "left", "bottom", "right", "insideH", "insideV"} {
		fmt.Fprintf(b, `<w:%s w:val="single" w:sz="4" w:space="0" w:color="000000"/>`, edge)
	}
	b.WriteString(`</w:tblBorders></w:tblPr>`)
	headers := []string{"ลำดับ", "ชื่อ-นามสกุล", "รหัสวิชา", "ระดับ", "ภาค", "สถานะ"}
	writeTableRow(b, headers, true)
	for i, r := range rows {
		status := "ใหม่"
		if r.Returning {
			status = "เก่า"
		}
		writeTableRow(b, []string{
			fmt.Sprintf("%d", i+1),
			r.FullName,
			r.CourseCode,
			r.Level,
			r.Track,
			status,
		}, false)
	}
	b.WriteString(`</w:tbl>`)
}

func writeTableRow(b *strings.Builder, cells []string, header bool) {
	b.WriteString(`<w:tr>`)
	for _, c := range cells {
		b.WriteString(`<w:tc><w:tcPr><w:tcW w:w="0" w:type="auto"/></w:tcPr>`)
		b.WriteString(`<w:p><w:r>`)
		if header {
			b.WriteString(`<w:rPr><w:b/></w:rPr>`)
		}
		b.WriteString(`<w:t xml:space="preserve">`)
		escapeXML(b, c)
		b.WriteString(`</w:t></w:r></w:p></w:tc>`)
	}
	b.WriteString(`</w:tr>`)
}

func escapeXML(b *strings.Builder, s string) {
	_ = xml.EscapeText(b, []byte(s))
}
