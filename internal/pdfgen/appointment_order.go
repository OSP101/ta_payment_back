// appointment_order.go generates the formal TA appointment order (คำสั่ง)
// as a PDF. The layout mirrors the KKU government-letter style seen in
// document_template/คำสั่ง6-2569*.docx: header, subject line, statutory
// authority body, appointee table grouped by branch/track/level, effective
// date, and a signature block for the dean.
//
// Generated from scratch with gopdf (Sarabun TTF). The DOCX counterpart lives
// in internal/docxgen — this file is PDF only.
package pdfgen

import (
	"bytes"
	"fmt"

	"github.com/signintech/gopdf"
)

// AppointmentAppointee is one row on the appointee roster.
type AppointmentAppointee struct {
	FullName    string
	NationalID  string
	Level       string // "ปริญญาตรี" / "ปริญญาโท" / "ปริญญาเอก"
	Track       string // "ภาคปกติ" / "ภาคพิเศษ"
	CourseCode  string
	CourseName  string
	IsReturning bool
}

// AppointmentOrderData is the fully-composed input for one order.
type AppointmentOrderData struct {
	OrderNo       string // "6/2569"
	AcademicYear  string // "2569"
	SemesterLabel string // "ภาคปลาย"
	OrderDate     string // "24 มกราคม 2569"
	EffectiveDate string // "24 มกราคม 2569"
	SignerName    string // "รศ.ดร.ก. ข."
	SignerTitle   string // "คณบดี"
	Appointees    []AppointmentAppointee
}

// AppointmentOrderInput controls a single render.
type AppointmentOrderInput struct {
	FontDir string
	Data    AppointmentOrderData
}

// BuildAppointmentOrderPDF renders the appointment order as bytes.
func BuildAppointmentOrderPDF(in AppointmentOrderInput) ([]byte, error) {
	pdf := gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4})
	if err := pdf.AddTTFFont("sarabun", in.FontDir+"/Sarabun-Regular.ttf"); err != nil {
		return nil, fmt.Errorf("register sarabun regular: %w", err)
	}
	if err := pdf.AddTTFFont("sarabunb", in.FontDir+"/Sarabun-Bold.ttf"); err != nil {
		return nil, fmt.Errorf("register sarabun bold: %w", err)
	}
	pdf.AddPage()
	d := in.Data

	// Header
	y := 40.0
	setBold(&pdf, 16)
	textAt(&pdf, pageW/2-90, y, "คำสั่งวิทยาลัยการคอมพิวเตอร์")
	y += 18
	textAt(&pdf, pageW/2-100, y, "มหาวิทยาลัยขอนแก่น")
	y += 18
	textAt(&pdf, pageW/2-80, y, "ที่ "+d.OrderNo)
	y += 22
	setBold(&pdf, 12)
	textAt(&pdf, pageW/2-180, y, "เรื่อง แต่งตั้งนักศึกษาผู้ช่วยสอน / ผู้ช่วยปฏิบัติงาน")
	y += 16
	setReg(&pdf, 11)
	textAt(&pdf, pageW/2-140, y, "ประจำ"+d.SemesterLabel+" ปีการศึกษา "+d.AcademicYear)
	y += 24

	// Body
	setReg(&pdf, 12)
	body := "     เพื่อให้การเรียนการสอนของวิทยาลัยการคอมพิวเตอร์ มหาวิทยาลัยขอนแก่น เป็นไปด้วยความเรียบร้อยและมีประสิทธิภาพ อาศัยอำนาจตามความในมาตรา 37 แห่งพระราชบัญญัติมหาวิทยาลัยขอนแก่น พ.ศ. 2558 จึงแต่งตั้งบุคคลตามรายชื่อดังต่อไปนี้เป็นนักศึกษาผู้ช่วยสอนและผู้ช่วยปฏิบัติงาน"
	y = drawWrappedText(&pdf, 50, y, pageW-100, 14, body)
	y += 10

	// Appointee table
	y = drawAppointeeTable(&pdf, y, d.Appointees)
	y += 12

	// Effective clause
	setReg(&pdf, 12)
	textAt(&pdf, 50, y, "     ทั้งนี้ ตั้งแต่วันที่ "+d.EffectiveDate+" เป็นต้นไป")
	y += 22
	textAt(&pdf, pageW/2, y, "สั่ง ณ วันที่ "+d.OrderDate)
	y += 60

	// Signature block
	setBold(&pdf, 12)
	textAt(&pdf, pageW/2, y, "("+d.SignerName+")")
	y += 16
	setReg(&pdf, 11)
	textAt(&pdf, pageW/2, y, d.SignerTitle)

	var buf bytes.Buffer
	if _, err := pdf.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func drawAppointeeTable(pdf *gopdf.GoPdf, y float64, rows []AppointmentAppointee) float64 {
	widths := []float64{28, 150, 90, 60, 90, 40}
	headers := []string{"ลำดับ", "ชื่อ-นามสกุล", "รหัสวิชา", "ระดับ", "ภาค", "สถานะ"}
	setBold(pdf, 10)
	y = drawTableRow(pdf, 50, y, headers, widths, true)
	setReg(pdf, 10)
	for i, r := range rows {
		if y > pageH-120 {
			pdf.AddPage()
			y = 40
			setBold(pdf, 10)
			y = drawTableRow(pdf, 50, y, headers, widths, true)
			setReg(pdf, 10)
		}
		status := "ใหม่"
		if r.IsReturning {
			status = "เก่า"
		}
		y = drawTableRow(pdf, 50, y, []string{
			fmt.Sprintf("%d", i+1),
			r.FullName,
			r.CourseCode,
			r.Level,
			r.Track,
			status,
		}, widths, false)
	}
	return y
}

// drawWrappedText breaks a paragraph into lines that fit width w at the given
// line height. Returns the y-coordinate below the last line. Simple char-based
// wrap — sufficient for Thai text since Sarabun ligatures render cleanly.
func drawWrappedText(pdf *gopdf.GoPdf, x, y, w, lineH float64, s string) float64 {
	// Simple word-wrap using rune-count budget; treats Thai as continuous runes.
	// Words split on space.
	remaining := []rune(s)
	// Rough character budget: 12pt Sarabun ~5.4pt per rune → width/5.4
	perLine := int(w / 6.0)
	if perLine < 20 {
		perLine = 20
	}
	for len(remaining) > 0 {
		end := perLine
		if end > len(remaining) {
			end = len(remaining)
		}
		// Try to break on nearest space before end
		if end < len(remaining) {
			for k := end; k > perLine/2; k-- {
				if remaining[k] == ' ' {
					end = k
					break
				}
			}
		}
		line := string(remaining[:end])
		textAt(pdf, x, y, line)
		y += lineH
		remaining = remaining[end:]
		for len(remaining) > 0 && remaining[0] == ' ' {
			remaining = remaining[1:]
		}
	}
	return y
}
