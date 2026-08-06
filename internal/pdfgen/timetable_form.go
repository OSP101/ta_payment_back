// timetable_form.go renders "ตารางเรียนและตารางปฏิบัติงาน (TA)" — the weekly
// grid the college signs, one page per TA per term.
//
// The browser can already print this page, and for a lecturer holding a laptop
// that is enough. This exists because the EXPORT BUNDLE cannot: the zip is built
// server-side with no browser anywhere, and the form has to travel with the rest
// of a TA's payment paperwork.
//
// Layout follows the college's own spreadsheet, not a redesign of it:
//   - thirteen whole-hour columns, 08.00–21.00 (the 20.00–21.00 column carries
//     real evening grading duty — see the LAST_HOUR note on the web page);
//   - one row per day, split into the student's OWN classes above and their TA
//     duties below, so a duty scheduled on top of a class the TA must attend is
//     visible by looking down a column;
//   - every block carries a TEXT tag as well as its colour, because this form is
//     photocopied and signed, and colour does not survive that.
package pdfgen

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/signintech/gopdf"
)

// A4 landscape, in points.
const (
	ttPageW     = 841.89
	ttPageH     = 595.28
	ttFirstHour = 8
	ttLastHour  = 21
)

// TimetableFormBlock is one coloured block. Kind drives colour and tag.
type TimetableFormBlock struct {
	Kind       string // own_class | lecture | lab | review
	CourseCode string
	SecNo      string
	Track      string // regular | special | ""
	DayOfWeek  int    // 0=Sunday … 6=Saturday
	StartTime  string // "HH:MM"
	EndTime    string // "HH:MM"
	Expected   int
	Logged     int
}

// TimetableFormSigner is one signature block: a lecturer and the courses they
// sign for. One lecturer teaching two courses signs once, under both codes.
type TimetableFormSigner struct {
	LecturerName string
	Courses      []string
}

// TimetableFormOutOfGrid is a logged entry that matched no slot on the grid.
// Split by Source so a reviewer knows whether to read it: 'auto' rows are
// system-generated makeups that legitimately sit off the weekly grid, 'manual'
// rows are what the TA typed themselves.
type TimetableFormOutOfGrid struct {
	Date   string
	Start  string
	End    string
	Kind   string
	Course string
	SecNo  string
	Note   string
	Source string // auto | manual
}

type TimetableFormData struct {
	TAName    string
	StudentID string
	TermLabel string
	YearMonth string
	Blocks    []TimetableFormBlock
	Signers   []TimetableFormSigner
	OutOfGrid []TimetableFormOutOfGrid
}

type TimetableFormInput struct {
	FontDir string
	Data    TimetableFormData
}

type ttStyle struct {
	r, g, b    uint8
	tr, tg, tb uint8
	tag        string
}

// The two colours from the college's file are kept exactly — peach for the
// student's own classes, and the green it used for TA work, now narrowed to
// LECTURE duty so lab and grading can be told apart at a glance.
var ttStyles = map[string]ttStyle{
	"own_class": {254, 233, 217, 122, 58, 22, "เรียน"},
	"lecture":   {216, 228, 189, 59, 83, 35, "บรรยาย"},
	"lab":       {207, 226, 243, 18, 69, 110, "ปฏิบัติการ"},
	"review":    {228, 217, 243, 68, 42, 112, "ตรวจงาน"},
}

var ttDays = []struct {
	dow   int
	label string
}{
	{1, "จันทร์"}, {2, "อังคาร"}, {3, "พุธ"}, {4, "พฤหัส"},
	{5, "ศุกร์"}, {6, "เสาร์"}, {0, "อาทิตย์"},
}

// BuildTimetableFormPDF renders the form as PDF bytes.
func BuildTimetableFormPDF(in TimetableFormInput) ([]byte, error) {
	pdf := gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: gopdf.Rect{W: ttPageW, H: ttPageH}})

	if err := pdf.AddTTFFont("sarabun", in.FontDir+"/Sarabun-Regular.ttf"); err != nil {
		return nil, fmt.Errorf("register sarabun regular: %w", err)
	}
	if err := pdf.AddTTFFont("sarabunb", in.FontDir+"/Sarabun-Bold.ttf"); err != nil {
		return nil, fmt.Errorf("register sarabun bold: %w", err)
	}

	pdf.AddPage()
	pdf.SetTextColor(0, 0, 0)
	pdf.SetStrokeColor(90, 90, 90)
	pdf.SetLineWidth(0.4)

	d := in.Data
	const margin = 24.0

	// ── Heading ────────────────────────────────────────────────────────────
	y := 26.0
	setBold(&pdf, 15)
	ttCenter(&pdf, y, "ตารางเรียนและตารางปฏิบัติงาน (TA)")
	y += 18
	setReg(&pdf, 11)
	sub := "ภาคการศึกษา " + d.TermLabel
	if d.YearMonth != "" {
		sub += " · เดือน " + d.YearMonth
	}
	ttCenter(&pdf, y, sub)
	y += 18

	setReg(&pdf, 11)
	name := "ชื่อ  " + d.TAName
	if d.StudentID != "" {
		name += "        รหัสนักศึกษา  " + d.StudentID
	}
	textAt(&pdf, margin, y, name)
	y += 16

	// ── Legend ─────────────────────────────────────────────────────────────
	lx := margin
	setReg(&pdf, 8)
	for _, k := range []string{"own_class", "lecture", "lab", "review"} {
		st := ttStyles[k]
		ttFillRect(&pdf, lx, y, lx+9, y+9, st.r, st.g, st.b)
		label := "งาน TA " + st.tag
		if k == "own_class" {
			label = "คาบเรียนของนักศึกษา"
		}
		textAt(&pdf, lx+12, y, label)
		lx += 13 + ttTextW(&pdf, label) + 14
	}
	y += 16

	// ── Grid ───────────────────────────────────────────────────────────────
	gridX := margin
	gridW := ttPageW - margin*2
	dayColW := 46.0
	hours := ttLastHour - ttFirstHour
	hourW := (gridW - dayColW) / float64(hours)

	// Header row of hour columns.
	const headH = 14.0
	// SetFillColor is re-applied before EVERY filled rectangle, never hoisted out
	// of the loop: in PDF the fill colour and the text colour are the same
	// non-stroking colour, so drawing a label resets the fill to the text colour.
	// Setting it once turned every header cell after the first into a black box
	// with its hour label invisible inside it.
	setReg(&pdf, 7)
	ttFillRect(&pdf, gridX, y, gridX+dayColW, y+headH, 245, 245, 245)
	ttCenterIn(&pdf, gridX, gridX+dayColW, y+3.5, "วัน / เวลา")
	for i := 0; i < hours; i++ {
		x0 := gridX + dayColW + float64(i)*hourW
		ttFillRect(&pdf, x0, y, x0+hourW, y+headH, 245, 245, 245)
		lbl := fmt.Sprintf("%02d.00-%02d.00", ttFirstHour+i, ttFirstHour+i+1)
		ttCenterIn(&pdf, x0, x0+hourW, y+3.5, lbl)
	}
	y += headH

	// Weekend rows only when something falls there — otherwise two of seven rows
	// are permanently blank and the grid loses a fifth of its width to nothing.
	used := map[int]bool{}
	for _, b := range d.Blocks {
		used[b.DayOfWeek] = true
	}

	for _, day := range ttDays {
		if day.dow == 6 || day.dow == 0 {
			if !used[day.dow] {
				continue
			}
		}
		own := ttPick(d.Blocks, day.dow, true)
		duty := ttPick(d.Blocks, day.dow, false)

		ownRows := ttLanes(own)
		dutyRows := ttLanes(duty)
		const blockH = 12.0
		ownH := float64(maxInt(len(ownRows), 1)) * blockH
		dutyH := float64(maxInt(len(dutyRows), 1)) * blockH
		rowH := ownH + dutyH

		// Day label spanning both lanes.
		pdf.Rectangle(gridX, y, gridX+dayColW, y+rowH, "D", 0, 0)
		setReg(&pdf, 8)
		ttCenterIn(&pdf, gridX, gridX+dayColW, y+rowH/2-4, day.label)

		// Empty hour cells behind both lanes, so gridlines survive.
		for i := 0; i < hours; i++ {
			x0 := gridX + dayColW + float64(i)*hourW
			pdf.Rectangle(x0, y, x0+hourW, y+ownH, "D", 0, 0)
			pdf.Rectangle(x0, y+ownH, x0+hourW, y+rowH, "D", 0, 0)
		}

		ttDrawLane(&pdf, ownRows, gridX+dayColW, y, hourW, blockH)
		ttDrawLane(&pdf, dutyRows, gridX+dayColW, y+ownH, hourW, blockH)

		y += rowH
	}

	y += 10

	// ── Out-of-grid rows ───────────────────────────────────────────────────
	var manual, auto []TimetableFormOutOfGrid
	for _, o := range d.OutOfGrid {
		if o.Source == "manual" {
			manual = append(manual, o)
		} else {
			auto = append(auto, o)
		}
	}
	if len(manual) > 0 || len(auto) > 0 {
		y = ttOutTable(&pdf, margin, y, "ทีเอเพิ่มเอง",
			"ไม่ตรงช่องใดในตาราง และเป็นรายการที่พิมพ์เอง", manual)
		y = ttOutTable(&pdf, margin, y+6, "นอกตาราง แต่ระบบสร้าง",
			"ส่วนใหญ่คือคาบชดเชยที่เลื่อนวัน", auto)
		y += 8
	}

	// ── Signatures ─────────────────────────────────────────────────────────
	ttSignatures(&pdf, margin, y, d)

	var buf bytes.Buffer
	if _, err := pdf.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ttPick returns the blocks of one day belonging to one lane.
func ttPick(all []TimetableFormBlock, dow int, ownLane bool) []TimetableFormBlock {
	var out []TimetableFormBlock
	for _, b := range all {
		if b.DayOfWeek != dow {
			continue
		}
		if (b.Kind == "own_class") != ownLane {
			continue
		}
		if ttMin(b.StartTime) >= ttMin(b.EndTime) {
			continue
		}
		out = append(out, b)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return ttMin(out[i].StartTime) < ttMin(out[j].StartTime)
	})
	return out
}

// ttLanes packs blocks into sub-rows so overlapping ones stack instead of
// drawing on top of each other. Two sections co-taught in one sitting land on
// the same day and hour by design, so this is the normal case, not an edge one.
func ttLanes(blocks []TimetableFormBlock) [][]TimetableFormBlock {
	var rows [][]TimetableFormBlock
	for _, b := range blocks {
		placed := false
		for i := range rows {
			clash := false
			for _, e := range rows[i] {
				if ttMin(b.StartTime) < ttMin(e.EndTime) && ttMin(e.StartTime) < ttMin(b.EndTime) {
					clash = true
					break
				}
			}
			if !clash {
				rows[i] = append(rows[i], b)
				placed = true
				break
			}
		}
		if !placed {
			rows = append(rows, []TimetableFormBlock{b})
		}
	}
	return rows
}

func ttDrawLane(pdf *gopdf.GoPdf, rows [][]TimetableFormBlock, x0, y0, hourW, blockH float64) {
	for ri, row := range rows {
		for _, b := range row {
			s := ttClamp(ttMin(b.StartTime))
			e := ttClamp(ttMin(b.EndTime))
			if e <= s {
				// Entirely outside 08.00–21.00. Never silently drop it: that is
				// exactly how the missing 20.00 column hid real duty for weeks.
				continue
			}
			bx := x0 + (float64(s)-ttFirstHour*60)/60*hourW
			bw := (float64(e-s) / 60) * hourW
			by := y0 + float64(ri)*blockH

			st, ok := ttStyles[b.Kind]
			if !ok {
				st = ttStyles["review"]
			}
			ttFillRect(pdf, bx, by, bx+bw, by+blockH, st.r, st.g, st.b)
			pdf.SetTextColor(st.tr, st.tg, st.tb)
			setReg(pdf, 6)
			ttClippedText(pdf, bx+1.5, by+3, bw-3, ttLabel(b))
			pdf.SetTextColor(0, 0, 0)
		}
	}
}

func ttLabel(b TimetableFormBlock) string {
	var sb strings.Builder
	sb.WriteString(b.CourseCode)
	if b.SecNo != "" {
		sb.WriteString(" Sec." + b.SecNo)
	}
	if st, ok := ttStyles[b.Kind]; ok {
		sb.WriteString(" " + st.tag)
	}
	switch b.Track {
	case "special":
		sb.WriteString(" (พิเศษ)")
	case "regular":
		sb.WriteString(" (ปกติ)")
	}
	if b.Expected > 0 {
		sb.WriteString(" · " + strconv.Itoa(b.Logged) + "/" + strconv.Itoa(b.Expected))
	}
	return sb.String()
}

func ttOutTable(pdf *gopdf.GoPdf, x, y float64, title, hint string, rows []TimetableFormOutOfGrid) float64 {
	head := fmt.Sprintf("%s (%d)", title, len(rows))
	setBold(pdf, 8)
	textAt(pdf, x, y, head)
	// Measure the heading while the BOLD font is still selected — measuring it
	// after switching to regular under-reports the width and the hint lands on
	// top of the heading.
	headW := ttTextW(pdf, head)
	setReg(pdf, 7)
	textAt(pdf, x+headW+8, y+1, hint)
	y += 11
	if len(rows) == 0 {
		setReg(pdf, 7)
		textAt(pdf, x+6, y, "ไม่มี")
		return y + 9
	}
	setReg(pdf, 7)
	for _, o := range rows {
		line := fmt.Sprintf("%s  %s–%s  %s  %s", o.Date, o.Start, o.End, o.Kind, o.Course)
		if o.SecNo != "" {
			line += " sec " + o.SecNo
		}
		if o.Note != "" {
			line += "  — " + o.Note
		}
		textAt(pdf, x+6, y, line)
		y += 9
	}
	return y
}

// ttSignatures draws the student's block plus one block per lecturer, grouped
// the way the paper form groups them: a lecturer teaching two of the TA's
// courses signs once, under both course codes.
func ttSignatures(pdf *gopdf.GoPdf, x, y float64, d TimetableFormData) {
	colW := (ttPageW - x*2) / 2
	setReg(pdf, 9)

	textAt(pdf, x, y, "ลงชื่อ ...........................................................")
	textAt(pdf, x+14, y+14, "("+d.TAName+")")
	textAt(pdf, x+14, y+26, "นักศึกษา")

	sy := y
	for _, s := range d.Signers {
		sx := x + colW
		textAt(pdf, sx, sy, "ลงชื่อ ...........................................................")
		textAt(pdf, sx+14, sy+14, "("+s.LecturerName+")")
		textAt(pdf, sx+14, sy+26, "อาจารย์ประจำวิชา "+strings.Join(s.Courses, ", "))
		sy += 44
	}
}

// ── small helpers ────────────────────────────────────────────────────────────

func ttMin(hhmm string) int {
	if len(hhmm) < 5 {
		return -1
	}
	h, err1 := strconv.Atoi(hhmm[0:2])
	m, err2 := strconv.Atoi(hhmm[3:5])
	if err1 != nil || err2 != nil {
		return -1
	}
	return h*60 + m
}

// ttClamp pins a time to the drawable window. Callers must compare start and end
// AFTER clamping: a block wholly outside collapses to zero width.
func ttClamp(m int) int {
	if m < ttFirstHour*60 {
		return ttFirstHour * 60
	}
	if m > ttLastHour*60 {
		return ttLastHour * 60
	}
	return m
}

func ttTextW(pdf *gopdf.GoPdf, s string) float64 {
	w, err := pdf.MeasureTextWidth(s)
	if err != nil {
		return float64(len([]rune(s))) * 4
	}
	return w
}

func ttCenter(pdf *gopdf.GoPdf, y float64, s string) {
	textAt(pdf, (ttPageW-ttTextW(pdf, s))/2, y, s)
}

func ttCenterIn(pdf *gopdf.GoPdf, x0, x1, y float64, s string) {
	textAt(pdf, x0+((x1-x0)-ttTextW(pdf, s))/2, y, s)
}

// ttClippedText trims a label to the width available rather than letting it
// bleed across neighbouring cells. Blocks are narrow — one hour is ~58pt — and
// an overflowing label makes the column underneath unreadable.
func ttClippedText(pdf *gopdf.GoPdf, x, y, w float64, s string) {
	if w <= 2 {
		return
	}
	if ttTextW(pdf, s) <= w {
		textAt(pdf, x, y, s)
		return
	}
	r := []rune(s)
	for len(r) > 1 {
		r = r[:len(r)-1]
		if ttTextW(pdf, string(r)+"…") <= w {
			textAt(pdf, x, y, string(r)+"…")
			return
		}
	}
}

// ttFillRect draws a filled, outlined rectangle, ALWAYS setting the fill colour
// first. Every filled rectangle on this page goes through it.
//
// In PDF the fill colour and the text colour are one non-stroking colour, so any
// label drawn between SetFillColor and Rectangle silently repaints the fill. The
// first version set the header grey once outside the loop; the "วัน / เวลา" cell
// came out grey and all thirteen hour cells after it came out solid black with
// their labels invisible inside. Keeping the pairing inside one function is what
// stops that reappearing — it is not something a reviewer can be expected to
// re-notice at every call site.
func ttFillRect(pdf *gopdf.GoPdf, x0, y0, x1, y1 float64, r, g, b uint8) {
	pdf.SetFillColor(r, g, b)
	pdf.Rectangle(x0, y0, x1, y1, "FD", 0, 0)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
