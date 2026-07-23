// worklog_report.go builds the monthly TA worklog PDF (Phase 2 of the payment
// automation plan). Unlike the creditor form (an overlay onto a fixed KKU
// template), this PDF is generated from scratch — its layout follows the
// example in document_template/ but the coordinates are all owned by us.
//
// One PDF per (TA, teaching_course, year_month) with three logical blocks:
//   Block A  หน้าปะ (coversheet) — TA identity, course, level/track, bank,
//            hours total, baht total, signature areas for TA + lecturer.
//   Block B  ตารางปฏิบัติงาน — condensed weekly schedule for the section.
//   Block C  หลักฐานการปฏิบัติงาน — approved work_logs for the month, split
//            into a ภาคปกติ / ภาคพิเศษ column (Q&A rule 5 quota is invisible
//            here — the payment engine already applied it in export.go).
package pdfgen

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/signintech/gopdf"
)

// WorklogPDFData is the payload for a single (TA × course × month) render.
type WorklogPDFData struct {
	// Header
	CourseCode      string
	CourseName      string
	YearMonthLabel  string // "มิถุนายน 2569"
	// TA identity
	FullName    string
	NationalID  string
	Email       string
	BankAcct    string
	Level       string // "ป.ตรี" / "บัณฑิต"
	Track       string // "ภาคปกติ" / "ภาคพิเศษ" / "รวม"
	// Totals
	HoursTotal float64
	PayBaht    float64
	// Weekly schedule rows (Block B)
	Schedule []ScheduleRow
	// Approved work_log rows (Block C)
	Entries []WorklogRow
	// Signature blocks — either raw PNG bytes (auto-placed) or blank if unsigned.
	TASignaturePNG      []byte
	LecturerSignaturePNG []byte
	LecturerName        string
}

type ScheduleRow struct {
	DayTH     string // "จันทร์" ..
	StartTime string
	EndTime   string
	Kind      string // "บรรยาย" / "ปฏิบัติการ" / "ตรวจงาน"
	Room      string
}

type WorklogRow struct {
	Date     string // "2026-06-05"
	Start    string
	End      string
	Hours    float64
	Activity string // display label
	Room     string
	Note     string
}

// WorklogPDFInput controls a single render.
type WorklogPDFInput struct {
	FontDir string
	Data    WorklogPDFData
}

// BuildWorklogPDF renders the monthly worklog report as bytes.
func BuildWorklogPDF(in WorklogPDFInput) ([]byte, error) {
	pdf := gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4})

	if err := pdf.AddTTFFont("sarabun", in.FontDir+"/Sarabun-Regular.ttf"); err != nil {
		return nil, fmt.Errorf("register sarabun regular: %w", err)
	}
	if err := pdf.AddTTFFont("sarabunb", in.FontDir+"/Sarabun-Bold.ttf"); err != nil {
		return nil, fmt.Errorf("register sarabun bold: %w", err)
	}

	pdf.AddPage()
	pdf.SetTextColor(0, 0, 0)
	pdf.SetStrokeColor(0, 0, 0)
	pdf.SetLineWidth(0.6)

	d := in.Data

	// ── Block A: Coversheet ────────────────────────────────────────────────
	y := 40.0
	setBold(&pdf, 16)
	textAt(&pdf, pageW/2-140, y, "ใบปะหน้าเบิกจ่ายค่าตอบแทนผู้ช่วยสอน")
	y += 20
	setReg(&pdf, 12)
	textAt(&pdf, pageW/2-70, y, "วิทยาลัยการคอมพิวเตอร์ มหาวิทยาลัยขอนแก่น")
	y += 26

	// Two-column info block
	col1 := 50.0
	col2 := 90.0
	rowH := 18.0
	setBold(&pdf, 11)
	textAt(&pdf, col1, y, "รหัสวิชา:")
	setReg(&pdf, 11)
	textAt(&pdf, col2, y, d.CourseCode+"  "+d.CourseName)
	y += rowH
	setBold(&pdf, 11)
	textAt(&pdf, col1, y, "เดือน:")
	setReg(&pdf, 11)
	textAt(&pdf, col2, y, d.YearMonthLabel)
	y += rowH
	setBold(&pdf, 11)
	textAt(&pdf, col1, y, "ชื่อ-นามสกุล:")
	setReg(&pdf, 11)
	textAt(&pdf, col2, y, d.FullName)
	y += rowH
	setBold(&pdf, 11)
	textAt(&pdf, col1, y, "เลขบัตร ปชช.:")
	setReg(&pdf, 11)
	textAt(&pdf, col2, y, d.NationalID)
	y += rowH
	setBold(&pdf, 11)
	textAt(&pdf, col1, y, "อีเมล:")
	setReg(&pdf, 11)
	textAt(&pdf, col2, y, d.Email)
	y += rowH
	setBold(&pdf, 11)
	textAt(&pdf, col1, y, "ธนาคาร/บัญชี:")
	setReg(&pdf, 11)
	textAt(&pdf, col2, y, d.BankAcct)
	y += rowH
	setBold(&pdf, 11)
	textAt(&pdf, col1, y, "ระดับ/ภาค:")
	setReg(&pdf, 11)
	textAt(&pdf, col2, y, d.Level+" · "+d.Track)
	y += rowH + 4

	// Totals — highlighted row
	pdf.SetFillColor(240, 245, 255)
	pdf.Rectangle(45, y-3, pageW-45, y+18, "F", 0, 0)
	pdf.SetFillColor(0, 0, 0)
	setBold(&pdf, 12)
	textAt(&pdf, col1, y, "รวมชั่วโมง (อนุมัติ):")
	textAt(&pdf, 260, y, fmt.Sprintf("%.1f ชม.", d.HoursTotal))
	textAt(&pdf, 340, y, "รวมค่าตอบแทน:")
	textAt(&pdf, 460, y, fmt.Sprintf("%s บาท", formatBaht(d.PayBaht)))
	y += 30

	// ── Block B: Schedule ─────────────────────────────────────────────────
	setBold(&pdf, 12)
	textAt(&pdf, col1, y, "ตารางปฏิบัติงานประจำสัปดาห์")
	y += 16
	y = drawScheduleTable(&pdf, y, d.Schedule)
	y += 12

	// ── Block C: Evidence ─────────────────────────────────────────────────
	setBold(&pdf, 12)
	textAt(&pdf, col1, y, "หลักฐานการปฏิบัติงาน (รายการที่อนุมัติแล้ว)")
	y += 16
	y = drawWorklogTable(&pdf, y, d.Entries)
	y += 10

	// ── Signature blocks ──────────────────────────────────────────────────
	if y > pageH-140 {
		pdf.AddPage()
		y = 40
	}
	drawSignatureRow(&pdf, y, d)

	var buf bytes.Buffer
	if _, err := pdf.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// drawScheduleTable renders Block B — weekly schedule. Returns the y coordinate
// below the table.
func drawScheduleTable(pdf *gopdf.GoPdf, y float64, rows []ScheduleRow) float64 {
	headers := []string{"วัน", "เริ่ม", "สิ้นสุด", "ประเภท", "ห้อง"}
	widths := []float64{80, 60, 60, 130, 200}
	y = drawTableRow(pdf, 45, y, headers, widths, true)
	if len(rows) == 0 {
		y = drawTableRow(pdf, 45, y, []string{"— ไม่มีข้อมูลตาราง —", "", "", "", ""}, widths, false)
		return y
	}
	for _, r := range rows {
		y = drawTableRow(pdf, 45, y, []string{r.DayTH, r.StartTime, r.EndTime, r.Kind, r.Room}, widths, false)
		if y > pageH-100 {
			pdf.AddPage()
			y = 40
			y = drawTableRow(pdf, 45, y, headers, widths, true)
		}
	}
	return y
}

// drawWorklogTable renders Block C — approved worklog entries for the month.
func drawWorklogTable(pdf *gopdf.GoPdf, y float64, rows []WorklogRow) float64 {
	headers := []string{"วันที่", "เริ่ม", "สิ้นสุด", "ชม.", "กิจกรรม", "ห้อง", "หมายเหตุ"}
	widths := []float64{75, 45, 45, 40, 100, 60, 135}
	y = drawTableRow(pdf, 45, y, headers, widths, true)
	if len(rows) == 0 {
		y = drawTableRow(pdf, 45, y,
			[]string{"— ไม่มีรายการที่อนุมัติในเดือนนี้ —", "", "", "", "", "", ""}, widths, false)
		return y
	}
	for _, r := range rows {
		row := []string{r.Date, r.Start, r.End, fmt.Sprintf("%.1f", r.Hours), r.Activity, r.Room, r.Note}
		y = drawTableRow(pdf, 45, y, row, widths, false)
		if y > pageH-100 {
			pdf.AddPage()
			y = 40
			y = drawTableRow(pdf, 45, y, headers, widths, true)
		}
	}
	return y
}

// drawTableRow renders one horizontal row of a table starting at (x, y). It
// draws cell borders and text, then returns the next row's y.
func drawTableRow(pdf *gopdf.GoPdf, x, y float64, cells []string, widths []float64, header bool) float64 {
	rowH := 18.0
	var fillR, fillG, fillB uint8 = 255, 255, 255
	if header {
		setBold(pdf, 10)
		fillR, fillG, fillB = 230, 235, 245
	} else {
		setReg(pdf, 10)
	}
	// gopdf shares one non-stroking color between shape fill and text fill, so
	// re-assert the background before every rectangle and black before every
	// text draw — otherwise SetTextColor bleeds into the next cell's fill (and
	// vice-versa), producing invisible text or black-filled cells.
	cx := x
	for i, w := range widths {
		pdf.SetFillColor(fillR, fillG, fillB)
		pdf.Rectangle(cx, y, cx+w, y+rowH, "FD", 0, 0)
		if i < len(cells) {
			pdf.SetTextColor(0, 0, 0)
			textAt(pdf, cx+3, y+3, cells[i])
		}
		cx += w
	}
	pdf.SetFillColor(0, 0, 0)
	pdf.SetTextColor(0, 0, 0)
	return y + rowH
}

// drawSignatureRow renders TA and lecturer signature blocks side-by-side.
func drawSignatureRow(pdf *gopdf.GoPdf, y float64, d WorklogPDFData) {
	// TA signature
	taX := 60.0
	lectX := pageW/2 + 30
	sigW := 180.0
	sigH := 40.0

	setReg(pdf, 11)
	textAt(pdf, taX, y, "ลงชื่อผู้ช่วยสอน")
	if len(d.TASignaturePNG) > 0 {
		_ = placePNG(pdf, d.TASignaturePNG, taX+30, y+20, sigW-30, sigH)
	}
	pdf.Line(taX, y+sigH+22, taX+sigW, y+sigH+22)
	textAt(pdf, taX+10, y+sigH+24, "( "+d.FullName+" )")
	textAt(pdf, taX+10, y+sigH+40, "วันที่ "+thaiToday())

	textAt(pdf, lectX, y, "ลงชื่ออาจารย์ประจำวิชา")
	if len(d.LecturerSignaturePNG) > 0 {
		_ = placePNG(pdf, d.LecturerSignaturePNG, lectX+30, y+20, sigW-30, sigH)
	}
	pdf.Line(lectX, y+sigH+22, lectX+sigW, y+sigH+22)
	name := d.LecturerName
	if name == "" {
		name = ".........................................."
	}
	textAt(pdf, lectX+10, y+sigH+24, "( "+name+" )")
	textAt(pdf, lectX+10, y+sigH+40, "วันที่ ...........................")
}

func setBold(pdf *gopdf.GoPdf, size float64) {
	_ = pdf.SetFont("sarabunb", "", size)
}
func setReg(pdf *gopdf.GoPdf, size float64) {
	_ = pdf.SetFont("sarabun", "", size)
}
func textAt(pdf *gopdf.GoPdf, x, y float64, s string) {
	if s == "" {
		return
	}
	pdf.SetXY(x, y)
	_ = pdf.Cell(nil, s)
}

func formatBaht(v float64) string {
	// Insert thousands separators.
	whole := int64(v)
	frac := int64((v - float64(whole)) * 100)
	s := fmt.Sprintf("%d", whole)
	if len(s) > 3 {
		var b strings.Builder
		for i, r := range s {
			if i > 0 && (len(s)-i)%3 == 0 {
				b.WriteRune(',')
			}
			b.WriteRune(r)
		}
		s = b.String()
	}
	if frac > 0 {
		return fmt.Sprintf("%s.%02d", s, frac)
	}
	return s
}

func thaiToday() string {
	now := time.Now()
	return fmt.Sprintf("%d %s พ.ศ. %d", now.Day(), thaiMonth(int(now.Month())), now.Year()+543)
}
