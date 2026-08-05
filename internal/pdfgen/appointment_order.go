// appointment_order.go generates the formal TA appointment order (คำสั่ง) as a
// PDF, mirroring the KKU registrar template in
// document_template/คำสั่ง6-2569*.docx: the KKU seal, a centered heading, the
// statutory-authority preamble (มาตรา 40), the dean's signature block, then a
// second page carrying the บัญชีแนบท้าย (attached roster) grouped by TA level
// and course.
//
// Rendered from scratch with gopdf using the bundled Sarabun TTF. The DOCX
// counterpart lives in internal/docxgen.
package pdfgen

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	_ "image/png"
	"strings"

	"github.com/signintech/gopdf"
)

// kkuSeal is the มหาวิทยาลัยขอนแก่น emblem printed at the head of the order.
//
//go:embed kku_seal.png
var kkuSeal []byte

// AppointmentAppointee is one TA on the roster. First and last names print in
// separate columns, as the template does.
type AppointmentAppointee struct {
	StudentID string // "663380555-8"
	FirstName string // "ชาคริต"
	LastName  string // "อ่วมอ่ำ"
}

// AppointmentCourse is one course block within a level.
type AppointmentCourse struct {
	Code       string // "SC310003"
	Name       string // "Database System and Design"
	CreditText string // "3 (3-0-6)"
	Appointees []AppointmentAppointee
}

// AppointmentLevel buckets courses under a study-level heading.
type AppointmentLevel struct {
	Heading string // "รายวิชาระดับปริญญาตรี"
	Courses []AppointmentCourse
}

// AppointmentOrderData is the fully-composed input for one order.
type AppointmentOrderData struct {
	OrderNo       string // "6/2569"
	AcademicYear  string // "2568"
	SemesterLabel string // "ภาคปลาย"
	OrderDate     string // "14 มกราคม 2569"
	EffectiveDate string // "24 พฤศจิกายน 2568"
	SignerName    string // "รองศาสตราจารย์สิรภัทร เชี่ยวชาญวัฒนา"
	// SignerTitle is the position line, already carrying the acting phrase when
	// there is one ("รองคณบดีฝ่ายวิชาการ รักษาการแทน"). Wording is the
	// caller's business; this renderer only places the lines.
	SignerTitle string
	// SignerActingFor is the seat whose authority is being exercised, printed on
	// its own line under SignerTitle. Empty when the signer holds the seat.
	SignerActingFor string
	Levels          []AppointmentLevel
}

// AppointmentOrderInput controls a single render.
type AppointmentOrderInput struct {
	FontDir string
	Data    AppointmentOrderData
}

const (
	orderMarginX = 60.0
	orderTopY    = 50.0
	orderBottomY = pageH - 60.0
)

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
	d := in.Data

	// ---- Order page -------------------------------------------------------
	pdf.AddPage()
	y := orderTopY

	// KKU seal, centered.
	if img, _, err := image.Decode(bytes.NewReader(kkuSeal)); err == nil {
		const sealW = 52.0
		sealH := sealW * float64(img.Bounds().Dy()) / float64(img.Bounds().Dx())
		_ = pdf.ImageFrom(img, (pageW-sealW)/2, y, &gopdf.Rect{W: sealW, H: sealH})
		y += sealH + 8
	}

	// Title + subject: everything at the body size (16pt), bold — the template
	// keeps one size throughout. Subject is broken across three fixed lines.
	setBold(&pdf, 16)
	centerText(&pdf, y, "คำสั่งวิทยาลัยการคอมพิวเตอร์")
	y += 22
	centerText(&pdf, y, "ที่ "+d.OrderNo)
	y += 22
	centerText(&pdf, y, "เรื่อง  แต่งตั้งผู้ช่วยสอนและผู้ช่วยปฏิบัติงาน ระดับปริญญาตรีและระดับบัณฑิตศึกษา")
	y += 20
	centerText(&pdf, y, "ภาคปกติและโครงการพิเศษ สาขาวิชาวิทยาการคอมพิวเตอร์ มหาวิทยาลัยขอนแก่น")
	y += 20
	centerText(&pdf, y, "ประจำ"+d.SemesterLabel+"  ปีการศึกษา "+d.AcademicYear)
	y += 20
	setReg(&pdf, 14)
	centerText(&pdf, y, "-------------------------------------------------")
	y += 24

	// Statutory preamble.
	setReg(&pdf, 16)
	contentW := pageW - 2*orderMarginX
	y = drawWrappedText(&pdf, orderMarginX, y, contentW, 20,
		"          เพื่อให้การดำเนินการจัดการเรียนการสอนตามหลักสูตร สาขาวิทยาการคอมพิวเตอร์ "+
			"สาขาเทคโนโลยีสารสนเทศ สาขาภูมิสารสนเทศศาสตร์ สาขาปัญญาประดิษฐ์ และสาขาความมั่นคงปลอดภัยไซเบอร์ "+
			"ในระดับปริญญาตรีและระดับบัณฑิตศึกษา เป็นไปด้วยความเรียบร้อย มีประสิทธิภาพและบังเกิดผลดี")
	y += 4
	y = drawWrappedText(&pdf, orderMarginX, y, contentW, 20,
		"          อาศัยอำนาจตามความในมาตรา 40 แห่งพระราชบัญญัติมหาวิทยาลัยขอนแก่น พ.ศ. 2558 "+
			"จึงแต่งตั้งผู้ช่วยสอนและผู้ช่วยปฏิบัติงาน ระดับปริญญาตรีและระดับบัณฑิตศึกษา "+
			"ภาคปกติและโครงการพิเศษ สาขาวิชาวิทยาการคอมพิวเตอร์ วิทยาลัยการคอมพิวเตอร์ "+
			"มหาวิทยาลัยขอนแก่น ประจำ"+d.SemesterLabel+" ปีการศึกษา "+d.AcademicYear+
			" ดังบัญชีรายชื่อแนบท้ายคำสั่งนี้")
	y += 20

	// Effective clause, then the order date on the next line at a deeper
	// left indent (the template does not center it). Double spaces around the
	// date components follow the government-letter style.
	textAt(&pdf, orderMarginX, y, "          ทั้งนี้  ตั้งแต่วันที่  "+d.EffectiveDate+"  เป็นต้นไป")
	y += 22
	textAt(&pdf, orderMarginX+60, y, "สั่ง  ณ  วันที่  "+d.OrderDate)
	y += 66

	// Signature block. A deputy signing for the dean gets the two-line acting
	// form required of Thai official documents — own position + the acting
	// phrase, then the seat whose authority is being exercised — because the
	// order is issued under the DEAN's power, not the deputy's.
	setReg(&pdf, 16)
	centerText(&pdf, y, "("+d.SignerName+")")
	y += 22
	centerText(&pdf, y, d.SignerTitle)
	if d.SignerActingFor != "" {
		y += 22
		centerText(&pdf, y, d.SignerActingFor)
	}

	// ---- Attached roster page --------------------------------------------
	pdf.AddPage()
	y = orderTopY
	setBold(&pdf, 16)
	rosterW := pageW - 2*orderMarginX
	y = centerWrapped(&pdf, y, rosterW, 20,
		"รายชื่อผู้ช่วยสอนและผู้ช่วยปฏิบัติงาน ระดับปริญญาตรีและระดับบัณฑิตศึกษา")
	centerText(&pdf, y, "ประจำ"+d.SemesterLabel+"  ปีการศึกษา "+d.AcademicYear)
	y += 20
	y = centerWrapped(&pdf, y, rosterW, 20,
		"(บัญชีแนบท้ายคำสั่งวิทยาลัยการคอมพิวเตอร์ ที่ "+d.OrderNo+"  ลงวันที่  "+d.OrderDate+")")
	// Full-width asterisk rule, then a short centered "*****" (template style).
	// Star count is measured so the rule fills the text width without spilling.
	if starW, err := pdf.MeasureTextWidth("*"); err == nil && starW > 0 {
		centerText(&pdf, y, strings.Repeat("*", int(rosterW/starW)))
	} else {
		centerText(&pdf, y, strings.Repeat("*", 80))
	}
	y += 20
	centerText(&pdf, y, "*****")
	y += 24

	y = drawRoster(&pdf, y, d.Levels)

	var buf bytes.Buffer
	if _, err := pdf.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// drawRoster renders each level → course → appointee block, paginating as it
// runs out of vertical room. Borderless columns, matching the template:
//
//	1.1  วิชา  SC310003 Database System and Design            3 (3-0-6)
//	     ที่   รหัสประจำตัว     ชื่อ                สกุล
//	     1    663380555-8     นายชาคริต           อ่วมอ่ำ
func drawRoster(pdf *gopdf.GoPdf, y float64, levels []AppointmentLevel) float64 {
	// Column x-positions, scaled from the template's twip grid.
	const (
		xIdx    = orderMarginX + 8   // "1.1" / row number column
		xNo     = orderMarginX + 42  // ที่
		xID     = orderMarginX + 76  // รหัสประจำตัว
		xFirst  = orderMarginX + 192 // ชื่อ
		xLast   = orderMarginX + 340 // สกุล
		rowH    = 22.0
		blockH  = 66.0 // heading + header row + first data row
		rightX  = pageW - orderMarginX
		courseX = orderMarginX + 24
	)

	pageIfNeeded := func(need float64) {
		if y+need > orderBottomY {
			pdf.AddPage()
			y = orderTopY
		}
	}
	rightText := func(yy float64, s string) {
		w, err := pdf.MeasureTextWidth(s)
		if err != nil {
			w = float64(len([]rune(s))) * 7
		}
		textAt(pdf, rightX-w, yy, s)
	}

	for li, lv := range levels {
		pageIfNeeded(blockH)
		setBold(pdf, 16)
		textAt(pdf, orderMarginX, y, fmt.Sprintf("%d %s", li+1, lv.Heading))
		y += rowH

		for ci, c := range lv.Courses {
			pageIfNeeded(blockH)
			// Course line with the credit text at the right margin. The name
			// wraps before it can collide with the credit column.
			setBold(pdf, 16)
			textAt(pdf, xIdx, y, fmt.Sprintf("%d.%d", li+1, ci+1))
			textAt(pdf, xNo, y, "วิชา")
			nameW := rightX - xID - 90 // reserve room for "3 (2-2-5)"
			nameLines := wrapThai(pdf, c.Code+" "+c.Name, nameW)
			if c.CreditText != "" {
				rightText(y, c.CreditText)
			}
			for _, ln := range nameLines {
				textAt(pdf, xID, y, ln)
				y += rowH
			}

			// Column headers (no rules under them, as the template).
			setBold(pdf, 16)
			textAt(pdf, xNo, y, "ที่")
			textAt(pdf, xID, y, "รหัสประจำตัว")
			textAt(pdf, xFirst, y, "ชื่อ-สกุล")
			y += rowH

			setReg(pdf, 16)
			for i, ap := range c.Appointees {
				if y+rowH > orderBottomY {
					pdf.AddPage()
					y = orderTopY
					setBold(pdf, 16)
					textAt(pdf, xNo, y, "ที่")
					textAt(pdf, xID, y, "รหัสประจำตัว")
					textAt(pdf, xFirst, y, "ชื่อ-สกุล")
					y += rowH
					setReg(pdf, 16)
				}
				id := ap.StudentID
				if id == "" {
					id = "-"
				}
				textAt(pdf, xNo, y, fmt.Sprintf("%d", i+1))
				textAt(pdf, xID, y, id)
				textAt(pdf, xFirst, y, ap.FirstName)
				textAt(pdf, xLast, y, ap.LastName)
				y += rowH
			}
			y += 14
		}
	}
	return y
}

// centerText draws s horizontally centered on the page at baseline y using the
// currently-set font. Falls back to a rough center if measurement fails.
func centerText(pdf *gopdf.GoPdf, y float64, s string) {
	w, err := pdf.MeasureTextWidth(s)
	if err != nil {
		w = float64(len([]rune(s))) * 6
	}
	textAt(pdf, (pageW-w)/2, y, s)
}

// centerWrapped wraps s to width w and draws each line horizontally centered.
// Returns the y below the last line.
func centerWrapped(pdf *gopdf.GoPdf, y, w, lineH float64, s string) float64 {
	for _, line := range wrapThai(pdf, s, w) {
		centerText(pdf, y, line)
		y += lineH
	}
	return y
}

// drawWrappedText breaks a paragraph into lines that fit width w at the given
// line height. Returns the y-coordinate below the last line.
func drawWrappedText(pdf *gopdf.GoPdf, x, y, w, lineH float64, s string) float64 {
	for _, line := range wrapThai(pdf, s, w) {
		textAt(pdf, x, y, line)
		y += lineH
	}
	return y
}

// wrapThai fills lines to width w using character-level breaking, the way Word
// wraps Thai (long Thai runs have no spaces, so space-only wrapping leaves
// lines two-thirds empty). A break is allowed between any two runes except
// where Thai orthography forbids it; a break at a space consumes the space.
func wrapThai(pdf *gopdf.GoPdf, s string, w float64) []string {
	runes := []rune(s)
	var lines []string
	start := 0
	for start < len(runes) {
		end := start
		lastSpace := -1 // last space break position > start
		for end < len(runes) {
			// Candidate cut after this rune plus any following runes that a
			// break rule glues to it (combining marks, leading vowels, …).
			next := end + 1
			for next < len(runes) && !breakAllowed(runes[next-1], runes[next]) {
				next++
			}
			tw, err := pdf.MeasureTextWidth(string(runes[start:next]))
			if err == nil && tw > w && end > start {
				break
			}
			end = next
			if end < len(runes) && runes[end] == ' ' {
				lastSpace = end
			}
		}
		// Prefer a space break when it still fills ≥60% of the line — Thai
		// words stay whole at the cost of a slightly ragged right edge.
		// Otherwise fall back to the character-level break (end is an allowed
		// break position by construction).
		cut := end
		if end < len(runes) && lastSpace > start {
			if sw, err := pdf.MeasureTextWidth(string(runes[start:lastSpace])); err == nil && sw >= 0.6*w {
				cut = lastSpace
			}
		}
		lines = append(lines, strings.TrimRight(string(runes[start:cut]), " "))
		start = cut
		for start < len(runes) && runes[start] == ' ' {
			start++
		}
	}
	return lines
}

// breakAllowed reports whether a line break may occur between runes a and b.
func breakAllowed(a, b rune) bool {
	// Never break before combining marks / trailing vowels / repeaters.
	if isThaiTrailing(b) {
		return false
	}
	// Never break after leading vowels เ แ โ ใ ไ — they belong to the next
	// syllable — or after an opening bracket/quote.
	if (a >= 0x0E40 && a <= 0x0E44) || strings.ContainsRune("([{“‘", a) {
		return false
	}
	// Never break before closing punctuation.
	if strings.ContainsRune(")]}”’.,:;!?", b) {
		return false
	}
	return true
}

// isThaiTrailing reports runes that must stick to the preceding character:
// above/below vowels, tone marks, sara a/am, maiyamok, paiyannoi.
func isThaiTrailing(r rune) bool {
	switch {
	case r == 0x0E30, r == 0x0E31, r == 0x0E33: // ะ ั ำ
		return true
	case r >= 0x0E34 && r <= 0x0E3A: // ิ ี ึ ื ุ ู ฺ
		return true
	case r >= 0x0E45 && r <= 0x0E4E: // ๅ ๆ ็ ่ ้ ๊ ๋ ์ ํ ๎
		return true
	}
	return false
}
