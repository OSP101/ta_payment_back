// export_combined_book.go builds the ONE workbook a course's whole payout is
// printed from.
//
// The export used to ship a folder per TA, each holding one claim workbook per
// month plus a PDF coversheet. Printing a course therefore meant opening five
// folders and twenty files, and the finance office reassembled them by hand into
// the order the paperwork is actually filed in. The college's own file
// (docs/15.CP362104.xlsx) shows what they end up with: every TA stacked down a
// single sheet, each with ONE claim block covering the entire term, and a
// หลักฐาน sheet whose rows link back to those blocks.
//
// So that is what is generated now:
//
//	บันทึกเวลา (ปกติ)   one claim block per TA with regular-track hours
//	บันทึกเวลา (พิเศษ)  the same for the special programme
//	หลักฐาน-ปกติ        the payment-evidence table, one row per TA
//	หลักฐาน-พิเศษ
//
// A track with nobody in it produces no sheets at all rather than a blank one to
// leaf past.
//
// Written cell by cell rather than stamped from the template: the template holds
// exactly one block, and duplicating it would have to carry merges, array
// formulas and per-row styles across a variable number of copies. Every value
// here is either literal or a formula whose row is computed, so the layout is
// the code's to keep correct — and testable without opening Excel.
package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"
)

// Sheet names. The timesheet ones carry the month range in the college's file
// ("พ.ย.-ธ.ค. (ปกติ)"); a fixed name is used instead because the range is
// already printed inside every block, and a sheet name that moves with the term
// breaks the หลักฐาน sheet's cross-references on every edit.
const (
	sheetClaimRegular    = "บันทึกเวลา (ปกติ)"
	sheetClaimSpecial    = "บันทึกเวลา (พิเศษ)"
	sheetEvidenceRegular = "หลักฐาน-ปกติ"
	sheetEvidenceSpecial = "หลักฐาน-พิเศษ"
)

// claimant is one TA's whole-term claim on one track.
type claimant struct {
	TAID    uuid.UUID
	Name    string
	LevelTH string // "ป.ตรี" / "ป.โท/เอก"
	Rows    []claimSheetRow
	Rate    float64
	// PaidBaht is what the payout actually funds for this claimant on this
	// track. The office's instruction (ส.ค. 2569) is that the sheet lists
	// EVERY hour taught and totals it in รวมเป็นเงินทั้งสิ้น; the funded
	// figure prints separately, in ขอเบิกจ่ายเพียง and the evidence sheet's
	// รับจริง column. Priced by the same source the settlement runs on, so
	// this figure and the actual transfer cannot disagree.
	PaidBaht float64
	// LumpSum marks the grad-special claimant, whose pay is the flat term
	// lump the staff fill in by hand — no hourly funded figure exists, so
	// their money cells stay blank exactly as before.
	LumpSum bool
	// nameRow / hoursRow are filled while writing so the evidence sheet can
	// reference the block instead of repeating its numbers — the college's file
	// does the same, and a linked total cannot drift from the block above it.
	nameRow  int
	hoursRow int
}

// combinedBookData is everything the workbook prints.
type combinedBookData struct {
	CourseCode   string
	AcademicYear int
	Semester     int
	MonthRange   string // "มิถุนายน 2569 - ตุลาคม 2569"
	LecturerName string
	Certifier    CertifierChoice
	// Rates for the three hourly lines the money block prints. Every line shows
	// its rate the way the college's file does, even the ones this claimant does
	// not bill against — the form is read as a rate card as well as a claim.
	RateUGRegular   float64
	RateUGSpecial   float64
	RateGradRegular float64
	Regular         []claimant
	Special         []claimant
}

// BuildCombinedClaimWorkbook renders one course's entire payout as a single
// printable workbook.
func (s *ExportService) BuildCombinedClaimWorkbook(ctx context.Context, courseID uuid.UUID) ([]byte, error) {
	d, err := s.collectCombinedBook(ctx, courseID)
	if err != nil {
		return nil, err
	}
	if len(d.Regular) == 0 && len(d.Special) == 0 {
		return nil, Invalid("ไม่มีบันทึกเวลาที่อนุมัติแล้วในวิชานี้ — ยังสร้างเอกสารเบิกจ่ายไม่ได้")
	}

	f := excelize.NewFile()
	defer f.Close()
	// excelize seeds a "Sheet1"; every sheet below is created explicitly, so it
	// is removed at the end once at least one real sheet exists.
	st, err := newClaimStyles(f)
	if err != nil {
		return nil, err
	}

	for _, side := range []struct {
		claim, evidence string
		people          []claimant
		trackTH         string
	}{
		{sheetClaimRegular, sheetEvidenceRegular, d.Regular, "ภาคปกติ"},
		{sheetClaimSpecial, sheetEvidenceSpecial, d.Special, "โครงการพิเศษ"},
	} {
		if len(side.people) == 0 {
			continue
		}
		if err := writeClaimSheet(f, st, side.claim, side.trackTH, d, side.people); err != nil {
			return nil, err
		}
		if err := writeEvidenceSheet(f, st, side.evidence, side.claim, side.trackTH, d, side.people); err != nil {
			return nil, err
		}
	}

	f.DeleteSheet("Sheet1")
	if idx, err := f.GetSheetIndex(sheetClaimRegular); err == nil && idx >= 0 {
		f.SetActiveSheet(idx)
	}

	buf, err := f.WriteToBuffer()
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// The formats below are copied from the college's own file
// (docs/15.CP362104.xlsx): the claim sheet is TH Sarabun New 13, the evidence
// sheet 16; hours and money print in the comma (accounting) format; the data
// grid rules hair lines between rows inside a thin outer frame; and the
// รวมเป็นเงินทั้งสิ้น band is filled grey.
const (
	claimFontSize    = 13
	evidenceFontSize = 16
	fmtComma00       = `_-* #,##0.00_-;\-* #,##0.00_-;_-* "-"??_-;_-@_-`
	fmtComma0        = `_-* #,##0_-;\-* #,##0_-;_-* "-"??_-;_-@_-`
	fmtHours1        = `#,##0.0_ ;\-#,##0.0\ `
	fillGrey         = "D8D8D8"
	// Builtin short-date format, what the college's file uses on the ว/ด/ป
	// column: it follows the reader's locale, so Thai Excel shows 28/11/2568-
	// style dates without the sheet hardcoding a pattern.
	dateFmtID = 14
)

// cellSpec is one cell format, described the way the college's file styles it.
// Styles are built on demand and cached: the sheets use a few dozen
// combinations of the same handful of ingredients, and enumerating them as
// struct fields proved harder to keep faithful than naming them at the cell.
type cellSpec struct {
	size           float64
	bold           bool
	color          string
	h, v           string
	wrap           bool
	numFmt         string // custom number-format string ("" = General)
	dateID         int    // builtin number-format id; 0 = none
	fill           string
	bl, br, bt, bb string // border style per side: "", "thin", "hair", "medium"
}

type claimStyles struct {
	f     *excelize.File
	cache map[cellSpec]int
}

func newClaimStyles(f *excelize.File) (*claimStyles, error) {
	return &claimStyles{f: f, cache: map[cellSpec]int{}}, nil
}

var claimBorderID = map[string]int{"thin": 1, "medium": 2, "hair": 7}

func (st *claimStyles) id(c cellSpec) (int, error) {
	if id, ok := st.cache[c]; ok {
		return id, nil
	}
	s := &excelize.Style{Font: &excelize.Font{
		Family: "TH Sarabun New", Size: c.size, Bold: c.bold, Color: c.color,
	}}
	if c.h != "" || c.v != "" || c.wrap {
		s.Alignment = &excelize.Alignment{Horizontal: c.h, Vertical: c.v, WrapText: c.wrap}
	}
	if c.numFmt != "" {
		nf := c.numFmt
		s.CustomNumFmt = &nf
	}
	if c.dateID != 0 {
		s.NumFmt = c.dateID
	}
	if c.fill != "" {
		s.Fill = excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{c.fill}}
	}
	for _, side := range []struct{ name, style string }{
		{"left", c.bl}, {"right", c.br}, {"top", c.bt}, {"bottom", c.bb},
	} {
		if side.style != "" {
			s.Border = append(s.Border, excelize.Border{
				Type: side.name, Style: claimBorderID[side.style], Color: "000000",
			})
		}
	}
	id, err := st.f.NewStyle(s)
	if err != nil {
		return 0, err
	}
	st.cache[c] = id
	return id, nil
}

// claimStr is a local pointer helper; the package already has a strPtr in a
// test file, and this file must not depend on test code.
func claimStr(s string) *string { return &s }

// claimBlockMinRows keeps every block the same height when a TA has only a
// handful of sittings, so the printed pages line up.
const claimBlockMinRows = 16

// writeClaimSheet stacks one block per person down a single sheet.
func writeClaimSheet(f *excelize.File, st *claimStyles, sheet, trackTH string,
	d *combinedBookData, people []claimant) error {
	if _, err := f.NewSheet(sheet); err != nil {
		return err
	}
	// Widths as the college's file sets them; F is left at the default width
	// there, so it is left alone here too.
	for _, c := range []struct {
		col   string
		width float64
	}{
		{"A", 4.6}, {"B", 20.8}, {"C", 10.2}, {"D", 3.2}, {"E", 9.8},
		{"G", 10.2}, {"H", 6.8}, {"I", 8.8}, {"J", 24.6},
	} {
		if err := f.SetColWidth(sheet, c.col, c.col, c.width); err != nil {
			return err
		}
	}
	// One block per printed page: fit the width, and break between people so a
	// claim never straddles two sheets of paper. A4, with the narrow margins
	// the college's file prints with.
	fitWidth, fitHeight := 1, 0
	paperA4 := 9
	if err := f.SetPageLayout(sheet, &excelize.PageLayoutOptions{
		Orientation: claimStr("portrait"),
		Size:        &paperA4,
		FitToWidth:  &fitWidth,
		FitToHeight: &fitHeight,
	}); err != nil {
		return err
	}
	left, right, top, bottom := 0.31496, 0.11811, 0.39370, 0.39370
	if err := f.SetPageMargins(sheet, &excelize.PageLayoutMarginsOptions{
		Left: &left, Right: &right, Top: &top, Bottom: &bottom,
	}); err != nil {
		return err
	}

	row := 1
	for i := range people {
		next, err := writeClaimBlock(f, st, sheet, trackTH, d, &people[i], i+1, row)
		if err != nil {
			return err
		}
		if i < len(people)-1 {
			if err := f.InsertPageBreak(sheet, fmt.Sprintf("A%d", next)); err != nil {
				return err
			}
		}
		row = next
	}
	return nil
}

// writeClaimBlock renders one person's claim and returns the first row after it.
//
// Fonts, weights, rules and fills all mirror docs/15.CP362104.xlsx: everything
// is TH Sarabun New 13; headings, the table header, the totals cluster and the
// รวมเป็นเงินทั้งสิ้น band print bold; the data grid draws thin verticals with
// hair lines between rows and a thin line closing the table; the section below
// the table keeps the thin frame down its sides; and the grand-total band is
// filled grey.
func writeClaimBlock(f *excelize.File, st *claimStyles, sheet, trackTH string,
	d *combinedBookData, p *claimant, ordinal, top int) (int, error) {
	// excelize stores a string beginning with "=" as literal TEXT; only
	// SetCellFormula makes it calculate. Routed here once so no call site can
	// forget and ship a claim whose totals never add up.
	set := func(cell string, v any) error {
		if str, ok := v.(string); ok && strings.HasPrefix(str, "=") {
			return f.SetCellFormula(sheet, cell, strings.TrimPrefix(str, "="))
		}
		return f.SetCellValue(sheet, cell, v)
	}
	at := func(col string, r int) string { return fmt.Sprintf("%s%d", col, r) }
	sty := func(from, to string, c cellSpec) error {
		c.size = claimFontSize
		id, err := st.id(c)
		if err != nil {
			return err
		}
		return f.SetCellStyle(sheet, from, to, id)
	}
	styAt := func(col string, r int, c cellSpec) error { return sty(at(col, r), at(col, r), c) }

	semMark := func(n int) string {
		if d.Semester == n {
			return "( / )"
		}
		return "(   )"
	}
	ugMark, gradMark := "( / )", "(    )"
	if p.LevelTH != "ป.ตรี" {
		ugMark, gradMark = "(    )", "( / )"
	}
	regMark, spMark := "( / )", "(    )"
	if trackTH != "ภาคปกติ" {
		regMark, spMark = "(    )", "( / )"
	}

	// ── heading: four bold centred lines ─────────────────────────────────
	for i, line := range []string{
		"แบบใบเบิกค่าตอบแทนผู้ช่วยสอนและผู้ช่วยปฏิบัติงาน",
		"วิทยาลัยการคอมพิวเตอร์  มหาวิทยาลัยขอนแก่น ",
		fmt.Sprintf("ภาคการศึกษา  %s  ต้น     %s  ปลาย     %s  ฤดูร้อน     ปีการศึกษา  %d",
			semMark(1), semMark(2), semMark(3), d.AcademicYear),
		"ประจำเดือน  " + d.MonthRange + "  ",
	} {
		r := top + i
		if err := set(at("A", r), line); err != nil {
			return 0, err
		}
		if err := f.MergeCell(sheet, at("A", r), at("J", r)); err != nil {
			return 0, err
		}
		if err := sty(at("A", r), at("J", r), cellSpec{bold: true, h: "center"}); err != nil {
			return 0, err
		}
	}

	// ── course / level / track lines, all bold ───────────────────────────
	r := top + 4
	for _, c := range []struct {
		col  string
		row  int
		text string
		spec cellSpec
	}{
		{"B", r, "รายวิชาระดับ", cellSpec{bold: true, h: "center"}},
		{"C", r, ugMark + " ปริญญาตรี", cellSpec{bold: true}},
		{"G", r, gradMark + " บัณฑิตศึกษา", cellSpec{bold: true}},
		{"I", r, "รหัสวิชา ", cellSpec{bold: true, h: "right"}},
		{"J", r, d.CourseCode, cellSpec{bold: true, h: "left"}},
		{"C", r + 1, regMark + "  ภาคปกติ  ", cellSpec{bold: true}},
		{"G", r + 1, spMark + " โครงการพิเศษ  ", cellSpec{bold: true}},
	} {
		if err := set(at(c.col, c.row), c.text); err != nil {
			return 0, err
		}
		if err := styAt(c.col, c.row, c.spec); err != nil {
			return 0, err
		}
	}

	// ── table header ─────────────────────────────────────────────────────
	h1, h2 := top+6, top+7
	for _, hc := range []struct{ cell, text string }{
		{at("A", h1), "ลำดับ"}, {at("B", h1), "ชื่อ-สกุล"}, {at("C", h1), "ระดับ"},
		{at("D", h1), "ระยะเวลาที่สอน"}, {at("H", h1), "จำนวนชั่วโมงที่สอน"}, {at("J", h1), "หมายเหตุ"},
		{at("A", h2), "ที่"}, {at("D", h2), "วัน"}, {at("E", h2), "ว/ด/ป"},
		{at("F", h2), "กลุ่มเรียน"}, {at("G", h2), "เวลาสอน"},
		{at("H", h2), "บรรยาย"}, {at("I", h2), "ปฏิบัติการ"},
	} {
		if err := set(hc.cell, hc.text); err != nil {
			return 0, err
		}
	}
	for _, m := range [][2]string{
		{at("B", h1), at("B", h2)}, {at("C", h1), at("C", h2)}, {at("J", h1), at("J", h2)},
		{at("D", h1), at("G", h1)}, {at("H", h1), at("I", h1)},
	} {
		if err := f.MergeCell(sheet, m[0], m[1]); err != nil {
			return 0, err
		}
	}
	head := cellSpec{bold: true, h: "center", v: "center", wrap: true,
		bl: "thin", br: "thin", bt: "thin", bb: "thin"}
	if err := sty(at("A", h1), at("J", h2), head); err != nil {
		return 0, err
	}
	// ลำดับ/ที่ read as ONE box in the college's file: no rule between them.
	headA1, headA2 := head, head
	headA1.bb, headA2.bt = "", ""
	if err := styAt("A", h1, headA1); err != nil {
		return 0, err
	}
	if err := styAt("A", h2, headA2); err != nil {
		return 0, err
	}

	// ── data rows ────────────────────────────────────────────────────────
	// The grid rules the college's file draws: thin verticals, a hair line
	// between rows, and a thin line closing the last row. Nothing is merged
	// vertically — the ordinal, name and level sit in the first row and the
	// hair rules run unbroken beneath them, exactly as their file has it.
	first := top + 8
	slots := len(p.Rows)
	if slots < claimBlockMinRows {
		slots = claimBlockMinRows
	}
	last := first + slots - 1
	p.nameRow = first

	if err := set(at("A", first), ordinal); err != nil {
		return 0, err
	}
	if err := set(at("B", first), p.Name); err != nil {
		return 0, err
	}
	if err := set(at("C", first), p.LevelTH); err != nil {
		return 0, err
	}

	var prev time.Time
	for i, row := range p.Rows {
		rr := first + i
		// The day abbreviation prints only on the first row of each date, the
		// way the college's forms do it.
		if !row.Date.Equal(prev) {
			if err := set(at("D", rr), thaiDayAbbrev[int(row.Date.Weekday())]); err != nil {
				return 0, err
			}
			if err := set(at("E", rr), row.Date); err != nil {
				return 0, err
			}
			prev = row.Date
		}
		if err := set(at("F", rr), row.Group); err != nil {
			return 0, err
		}
		if err := set(at("G", rr), row.Range); err != nil {
			return 0, err
		}
		// บรรยาย vs ปฏิบัติการ is decided by the activity, exactly as the
		// template's own formulas decide it from the หมายเหตุ text.
		col := "H"
		if isLabNote(row.Note) {
			col = "I"
		}
		if err := set(at(col, rr), claimHours(row.Range)); err != nil {
			return 0, err
		}
		if err := set(at("J", rr), row.Note); err != nil {
			return 0, err
		}
	}
	dataCols := map[string]cellSpec{
		"A": {h: "center"},
		"B": {h: "left", v: "center"},
		"C": {h: "center", v: "center"},
		"D": {h: "center", v: "center"},
		"E": {h: "center", v: "center", dateID: dateFmtID},
		"F": {h: "center", v: "center"},
		"G": {h: "center", v: "center"},
		"H": {v: "center", numFmt: fmtComma00},
		"I": {h: "right", v: "center", numFmt: fmtComma00},
		"J": {h: "center", wrap: true},
	}
	for rr := first; rr <= last; rr++ {
		for col, spec := range dataCols {
			spec.bl, spec.br, spec.bb = "thin", "thin", "hair"
			if rr == last {
				spec.bb = "thin"
			}
			if err := styAt(col, rr, spec); err != nil {
				return 0, err
			}
		}
	}

	// ── totals ───────────────────────────────────────────────────────────
	tot := last + 1
	p.hoursRow = tot + 3 // the "ชั่วโมง" cell the evidence sheet links to
	ugRow, gradRow := tot, tot+1
	for _, c := range []struct {
		col  string
		row  int
		text string
	}{
		{"C", tot, "รวมเวลา"}, {"C", tot + 1, "ที่สอน"},
		{"D", tot, "ปริญญาตรี"}, {"D", tot + 1, "ปริญญาโท/เอก"},
	} {
		if err := set(at(c.col, c.row), c.text); err != nil {
			return 0, err
		}
	}
	// รวมเวลา/ที่สอน stack as one bold box; the levels and the sums sit in
	// their own thin boxes beside it.
	if err := styAt("C", tot, cellSpec{bold: true, h: "center", bl: "thin", br: "thin", bt: "thin"}); err != nil {
		return 0, err
	}
	if err := styAt("C", tot+1, cellSpec{bold: true, h: "center", bl: "thin", br: "thin", bb: "thin"}); err != nil {
		return 0, err
	}
	box := cellSpec{bold: true, h: "center", bl: "thin", br: "thin", bt: "thin", bb: "thin"}
	for _, row := range []int{tot, tot + 1} {
		if err := f.MergeCell(sheet, at("D", row), at("G", row)); err != nil {
			return 0, err
		}
		if err := sty(at("D", row), at("G", row), box); err != nil {
			return 0, err
		}
		sums := box
		sums.numFmt = fmtComma00
		sums.h = ""
		if err := sty(at("H", row), at("I", row), sums); err != nil {
			return 0, err
		}
		if err := styAt("J", row, box); err != nil {
			return 0, err
		}
	}
	// Only the row matching this person's level carries the sums; the other
	// stays empty, as the college's file leaves it.
	sumRow := ugRow
	if p.LevelTH != "ป.ตรี" {
		sumRow = gradRow
	}
	if err := set(at("H", sumRow), fmt.Sprintf("=SUM(H%d:H%d)", first, last)); err != nil {
		return 0, err
	}
	if err := set(at("I", sumRow), fmt.Sprintf("=SUM(I%d:I%d)", first, last)); err != nil {
		return 0, err
	}

	// ── money ────────────────────────────────────────────────────────────
	// The section below the table keeps the table's thin frame running down
	// its sides: column A carries the left rule, column J the right one.
	m := tot + 2
	if err := set(at("A", m), "จำนวนเงินที่ขอเบิก "); err != nil {
		return 0, err
	}
	if err := styAt("A", m, cellSpec{bold: true, h: "left", bl: "thin"}); err != nil {
		return 0, err
	}
	if err := styAt("J", m, cellSpec{br: "thin"}); err != nil {
		return 0, err
	}
	lines := []struct {
		label string
		level string
		track string
		rate  float64
	}{
		{"- ปริญญาตรี  (ภาคปกติ)", "ป.ตรี", "ภาคปกติ", d.RateUGRegular},
		{"- ปริญญาตรี  (โครงการพิเศษ)", "ป.ตรี", "โครงการพิเศษ", d.RateUGSpecial},
		{"- ปริญญาโท/เอก  (ภาคปกติ)", "ป.โท/เอก", "ภาคปกติ", d.RateGradRegular},
	}
	moneyRow := 0
	for i, ln := range lines {
		rr := m + 1 + i
		if err := set(at("B", rr), ln.label); err != nil {
			return 0, err
		}
		if err := set(at("G", rr), ln.rate); err != nil {
			return 0, err
		}
		mine := ln.level == p.LevelTH && ln.track == trackTH
		if mine {
			if err := set(at("C", rr), fmt.Sprintf("=H%d+I%d", sumRow, sumRow)); err != nil {
				return 0, err
			}
			if err := set(at("J", rr), fmt.Sprintf("=C%d*G%d", rr, rr)); err != nil {
				return 0, err
			}
			moneyRow = rr
			p.hoursRow = rr
		} else if err := set(at("C", rr), 0); err != nil {
			return 0, err
		}
		for _, c := range []struct {
			col  string
			text string
		}{
			{"E", "ชั่วโมง"}, {"F", "อัตราชั่วโมงละ"}, {"H", "บาท"}, {"I", "เป็นเงิน"},
		} {
			if err := set(at(c.col, rr), c.text); err != nil {
				return 0, err
			}
		}
		for col, spec := range map[string]cellSpec{
			"A": {bl: "thin"},
			"B": {h: "left"},
			"C": {numFmt: fmtHours1},
			"E": {h: "center"},
			"F": {h: "center"},
			"G": {numFmt: fmtComma00},
			"H": {},
			"I": {h: "right"},
			"J": {h: "center", numFmt: fmtComma00, br: "thin"},
		} {
			if err := styAt(col, rr, spec); err != nil {
				return 0, err
			}
		}
	}
	lump := m + 4
	for _, c := range []struct {
		col  string
		text string
	}{
		{"B", "- ปริญญาโท/เอก  (ภาคพิเศษ)"}, {"C", "เหมาจ่าย"}, {"I", "เป็นเงิน"},
	} {
		if err := set(at(c.col, lump), c.text); err != nil {
			return 0, err
		}
	}
	for col, spec := range map[string]cellSpec{
		"A": {bl: "thin"}, "B": {h: "left"}, "C": {h: "right"},
		"I": {h: "right"}, "J": {br: "thin"},
	} {
		if err := styAt(col, lump, spec); err != nil {
			return 0, err
		}
	}

	// ── grand total: the grey band ───────────────────────────────────────
	grand := m + 5
	if err := set(at("B", grand), "รวมเป็นเงินทั้งสิ้น"); err != nil {
		return 0, err
	}
	if moneyRow > 0 {
		if err := set(at("C", grand), fmt.Sprintf("=J%d", moneyRow)); err != nil {
			return 0, err
		}
	}
	if err := set(at("E", grand), "บาท"); err != nil {
		return 0, err
	}
	// BAHTTEXT is an Excel-only function and the college's own file uses it;
	// LibreOffice shows #NAME? for it, which is why nothing here depends on its
	// value.
	if err := set(at("G", grand), fmt.Sprintf(`=" = "&BAHTTEXT(C%d)&" = "`, grand)); err != nil {
		return 0, err
	}
	if err := set(at("B", grand+1), "ขอเบิกจ่ายเพียง"); err != nil {
		return 0, err
	}
	// ขอเบิกจ่ายเพียง carries what the payout actually funds. The totals above
	// cover every hour taught, so when the budget stops short the two figures
	// differ and finance reads the claimable amount here. A literal, not a
	// formula: the cutoff is the settlement's decision and cannot be derived
	// from the sheet. Grad-special lumps stay blank for staff, as before.
	if moneyRow > 0 && !p.LumpSum {
		if err := set(at("C", grand+1), p.PaidBaht); err != nil {
			return 0, err
		}
	}
	for _, row := range []int{grand, grand + 1} {
		if err := f.MergeCell(sheet, at("G", row), at("J", row)); err != nil {
			return 0, err
		}
		// A thin line above the band, one between its two rows, one below —
		// with the grey fill stopping at column G as the college's file has it
		// (the merged G:J cell carries the fill across the rest of the row).
		spec := cellSpec{bold: true, fill: fillGrey, bt: "thin"}
		if row == grand+1 {
			spec.bb = "thin"
		}
		grey := func(col string, s cellSpec) error {
			s.bold, s.fill, s.bt, s.bb = spec.bold, spec.fill, spec.bt, spec.bb
			return styAt(col, row, s)
		}
		for _, col := range []string{"B", "D", "F"} {
			if err := grey(col, cellSpec{}); err != nil {
				return 0, err
			}
		}
		if err := grey("A", cellSpec{bl: "thin"}); err != nil {
			return 0, err
		}
		if err := grey("C", cellSpec{h: "center", numFmt: fmtComma00}); err != nil {
			return 0, err
		}
		if err := grey("E", cellSpec{}); err != nil {
			return 0, err
		}
		if err := grey("G", cellSpec{h: "center"}); err != nil {
			return 0, err
		}
		for _, col := range []string{"H", "I"} {
			if err := styAt(col, row, cellSpec{bt: spec.bt, bb: spec.bb}); err != nil {
				return 0, err
			}
		}
		if err := styAt("J", row, cellSpec{bt: spec.bt, bb: spec.bb, br: "thin"}); err != nil {
			return 0, err
		}
	}

	// ── signatures: three thin-framed boxes ──────────────────────────────
	sig := grand + 2
	for _, c := range []struct{ col, text string }{
		{"A", "ผู้ปฏิบัติงาน"}, {"E", "อาจารย์ผู้สอน"}, {"H", "ผู้รับรอง"},
	} {
		if err := set(at(c.col, sig), c.text); err != nil {
			return 0, err
		}
	}

	rule := sig + 2
	for _, c := range []struct{ col, text string }{
		{"A", "ลงชื่อ…………....…...……………….....…"},
		{"E", "ลงชื่อ…………..............……………………"},
		{"H", "ลงชื่อ…………....………….........…………"},
	} {
		if err := set(at(c.col, rule), c.text); err != nil {
			return 0, err
		}
	}
	// The performer's name is LINKED to the block's own name cell, as the
	// college's file does (=B9): the signature line cannot drift from the
	// claim it signs.
	if err := set(at("A", rule+1), fmt.Sprintf("=B%d", first)); err != nil {
		return 0, err
	}
	if d.LecturerName != "" {
		if err := set(at("E", rule+1), "("+d.LecturerName+")"); err != nil {
			return 0, err
		}
	}
	if err := set(at("A", rule+2), "วันที่….เดือน…………..…พ.ศ…..……"); err != nil {
		return 0, err
	}
	if err := set(at("E", rule+2), "วันที่….เดือน…………..…พ.ศ…..……"); err != nil {
		return 0, err
	}
	bottom := rule + 2
	if certName, _, ok := d.Certifier.ClaimCells(); ok {
		if err := set(at("H", rule+1), certName); err != nil {
			return 0, err
		}
		// Own position on one line, the seat being exercised on the next —
		// the same two-line acting form the appointment order prints.
		if err := set(at("H", rule+2), "ตำแหน่ง "+d.Certifier.TitleLine); err != nil {
			return 0, err
		}
		if d.Certifier.ActingFor != "" {
			if err := set(at("H", rule+3), d.Certifier.ActingFor); err != nil {
				return 0, err
			}
			bottom = rule + 3
		}
	}
	for rr := sig; rr <= bottom; rr++ {
		for _, m3 := range [][2]string{{"A", "C"}, {"E", "G"}, {"H", "J"}} {
			if err := f.MergeCell(sheet, at(m3[0], rr), at(m3[1], rr)); err != nil {
				return 0, err
			}
		}
		// Header row: bold labels boxed top and bottom. Below: the verticals
		// of the three boxes run to the block's last row, which closes them.
		for col, spec := range map[string]cellSpec{
			"A": {bl: "thin"}, "C": {br: "thin"}, "G": {br: "thin"},
			"H": {bl: "thin"}, "J": {br: "thin"},
			"B": {}, "D": {}, "E": {}, "F": {}, "I": {},
		} {
			spec.h = "center"
			if rr == sig {
				spec.bold, spec.bt, spec.bb = true, "thin", "thin"
			}
			if rr == bottom {
				spec.bb = "thin"
			}
			if err := styAt(col, rr, spec); err != nil {
				return 0, err
			}
		}
	}

	// Row heights as the college's file sets them: 23.25 throughout, 22.5 for
	// the data grid.
	for rr := top; rr <= bottom; rr++ {
		h := 23.25
		if rr >= first && rr <= last {
			h = 22.5
		}
		if err := f.SetRowHeight(sheet, rr, h); err != nil {
			return 0, err
		}
	}

	// One blank row so the next block does not butt against this one when the
	// sheet is read on screen rather than printed.
	return bottom + 2, nil
}

// isLabNote decides which hours column a printed row bills into. Kept next to
// claimNote, whose vocabulary it reads.
func isLabNote(note string) bool {
	return note == "สอนปฏิบัติ" || note == "สอนปฏิบัติ (ชดเชย)" || note == "เตรียมเอกสารปฏิบัติ"
}

// claimHours turns "13.00 - 17.00" back into 4. The template computes this with
// a formula over the printed text; doing it here keeps the number readable to
// anything that opens the file, including LibreOffice, which cannot evaluate the
// TEXTBEFORE/TEXTAFTER the original used.
func claimHours(rng string) float64 {
	var sh, sm, eh, em int
	if _, err := fmt.Sscanf(rng, "%d.%d - %d.%d", &sh, &sm, &eh, &em); err != nil {
		return 0
	}
	return float64((eh*60+em)-(sh*60+sm)) / 60
}

// writeEvidenceSheet renders หลักฐานการจ่ายเงินอื่น ๆ — one row per claimant,
// linked to their block on the claim sheet.
//
// Everything here is TH Sarabun New 16 as in the college's file: the heading
// and checkbox lines bold, the table header REGULAR weight (their file's one
// departure from "headers are bold"), amounts in the no-decimal comma format,
// and the รวมเบิก total boxed with medium side rules.
func writeEvidenceSheet(f *excelize.File, st *claimStyles, sheet, claimSheet, trackTH string,
	d *combinedBookData, people []claimant) error {
	if _, err := f.NewSheet(sheet); err != nil {
		return err
	}
	for _, c := range []struct {
		col   string
		width float64
	}{
		{"A", 6.8}, {"B", 29.4}, {"C", 12.8}, {"D", 11.2}, {"E", 11.6},
		{"F", 14.2}, {"G", 14.8}, {"H", 12.4}, {"I", 20.2}, {"J", 15.8},
	} {
		if err := f.SetColWidth(sheet, c.col, c.col, c.width); err != nil {
			return err
		}
	}
	fitWidth, fitHeight := 1, 0
	paperA4 := 9
	if err := f.SetPageLayout(sheet, &excelize.PageLayoutOptions{
		Orientation: claimStr("landscape"),
		Size:        &paperA4,
		FitToWidth:  &fitWidth,
		FitToHeight: &fitHeight,
	}); err != nil {
		return err
	}
	mLeft, mRight, mTop, mBottom := 0.51181, 0.39370, 0.39370, 0.39370
	if err := f.SetPageMargins(sheet, &excelize.PageLayoutMarginsOptions{
		Left: &mLeft, Right: &mRight, Top: &mTop, Bottom: &mBottom,
	}); err != nil {
		return err
	}

	set := func(cell string, v any) error {
		if str, ok := v.(string); ok && strings.HasPrefix(str, "=") {
			return f.SetCellFormula(sheet, cell, strings.TrimPrefix(str, "="))
		}
		return f.SetCellValue(sheet, cell, v)
	}
	at := func(col string, r int) string { return fmt.Sprintf("%s%d", col, r) }
	sty := func(from, to string, c cellSpec) error {
		c.size = evidenceFontSize
		id, err := st.id(c)
		if err != nil {
			return err
		}
		return f.SetCellStyle(sheet, from, to, id)
	}
	styAt := func(col string, r int, c cellSpec) error { return sty(at(col, r), at(col, r), c) }

	semTH := map[int]string{1: "ภาคต้น", 2: "ภาคปลาย", 3: "ภาคฤดูร้อน"}[d.Semester]
	for i, line := range []string{
		"หลักฐานการจ่ายเงินอื่น ๆ",
		"เบิกตามฎีกาที่...................................... วันที่..................... เดือน ...................................... พ.ศ. .......................",
		"ข้าพเจ้าผู้มีรายนามข้างท้ายนี้ ได้รับเงินจากส่วนราชการ   วิทยาลัยการคอมพิวเตอร์   มหาวิทยาลัยขอนแก่น   เป็นค่าตอบแทนผู้ช่วยสอนและผู้ช่วยปฏิบัติงาน",
		fmt.Sprintf("สาขาวิชาวิทยาการคอมพิวเตอร์  ประจำ%s ปีการศึกษา %d", semTH, d.AcademicYear),
		"ตามหนังสืออนุมัติที่ อว 660301.26.6.2/ ง ...........  ลงวันที่    เดือน           พ.ศ. ....  ได้เป็นการถูกต้องแล้วจึงลงลายมือชื่อไว้เป็นสำคัญ  ",
	} {
		r := i + 1
		if err := set(at("A", r), line); err != nil {
			return err
		}
		if err := f.MergeCell(sheet, at("A", r), at("J", r)); err != nil {
			return err
		}
		if err := sty(at("A", r), at("J", r), cellSpec{bold: true, h: "center"}); err != nil {
			return err
		}
	}

	ugMark, gradMark := "(  / )", "(    )"
	if len(people) > 0 && people[0].LevelTH != "ป.ตรี" {
		ugMark, gradMark = "(    )", "(  / )"
	}
	regMark, spMark := "(  / )", "(    )"
	if trackTH != "ภาคปกติ" {
		regMark, spMark = "(    )", "(  / )"
	}
	// The course-code cell and the checkbox lines, merged as the college's
	// file merges them.
	for _, c := range []struct {
		cell, to, text string
		spec           cellSpec
	}{
		{"B6", "", "รหัสวิชา " + d.CourseCode, cellSpec{bold: true, h: "center"}},
		{"C6", "D6", "รายวิชาระดับ", cellSpec{bold: true, h: "center"}},
		{"E6", "G6", ugMark + " ปริญญาตรี", cellSpec{bold: true, h: "left"}},
		{"H6", "J6", gradMark + " บัณฑิตศึกษา", cellSpec{bold: true, h: "left"}},
		{"E7", "G7", regMark + " ภาคปกติ", cellSpec{bold: true, h: "left"}},
		{"H7", "J7", spMark + " โครงการพิเศษ", cellSpec{bold: true, h: "left"}},
	} {
		if err := set(c.cell, c.text); err != nil {
			return err
		}
		to := c.to
		if to == "" {
			to = c.cell
		}
		if c.to != "" {
			if err := f.MergeCell(sheet, c.cell, to); err != nil {
				return err
			}
		}
		if err := sty(c.cell, to, c.spec); err != nil {
			return err
		}
	}

	for _, hc := range []struct{ cell, text string }{
		{"A8", "ลำดับ"}, {"B8", "ชื่อผู้สอน"}, {"C8", "ระดับ"}, {"D8", "จำนวน"},
		{"E8", "อัตรา"}, {"F8", "จำนวนเงิน"}, {"G8", "รับจริง"}, {"H8", "วัน เดือน ปี"},
		{"I8", "ลายมือชื่อผู้รับเงิน"}, {"J8", "หมายเหตุ"},
		{"A9", "ที่"}, {"C9", "ตรี/โท/เอก"}, {"D9", "ชั่วโมง"}, {"E9", "ต่อหน่วย"}, {"H9", "ที่รับเงิน"},
	} {
		if err := set(hc.cell, hc.text); err != nil {
			return err
		}
	}
	// Each header column reads as ONE tall box: thin sides and top/bottom, no
	// rule between the two header rows — and, unlike the claim sheet, the
	// header is NOT bold, faithfully to the college's file.
	if err := sty("A8", "J8", cellSpec{h: "center", bl: "thin", br: "thin", bt: "thin"}); err != nil {
		return err
	}
	if err := sty("A9", "J9", cellSpec{h: "center", bl: "thin", br: "thin", bb: "thin"}); err != nil {
		return err
	}

	// Names and hours are LINKED, not copied: the block above is the source of
	// truth, and a figure typed twice is a figure that will eventually disagree.
	quoted := "'" + claimSheet + "'"
	for i, p := range people {
		r := 10 + i
		if err := set(at("A", r), i+1); err != nil {
			return err
		}
		if err := set(at("B", r), fmt.Sprintf("=%s!B%d", quoted, p.nameRow)); err != nil {
			return err
		}
		if err := set(at("C", r), p.LevelTH); err != nil {
			return err
		}
		if err := set(at("D", r), fmt.Sprintf("=%s!C%d", quoted, p.hoursRow)); err != nil {
			return err
		}
		if err := set(at("E", r), p.Rate); err != nil {
			return err
		}
		if err := set(at("F", r), fmt.Sprintf("=D%d*E%d", r, r)); err != nil {
			return err
		}
		// รับจริง is the funded amount — a literal, because the budget cutoff
		// is not derivable in-sheet. จำนวนเงิน (D×E) stays the full figure, so
		// the two columns diverge exactly when the budget stopped short. The
		// grad-special lump keeps the =F link; staff fill that case by hand.
		if p.LumpSum {
			if err := set(at("G", r), fmt.Sprintf("=F%d", r)); err != nil {
				return err
			}
		} else if err := set(at("G", r), p.PaidBaht); err != nil {
			return err
		}
		if err := set(at("J", r), d.CourseCode); err != nil {
			return err
		}
	}
	// Data grid + one spare row closing the table: thin verticals, hair rules
	// between rows, thin rule closing the bottom — the college's file keeps an
	// empty ruled row between the last name and the total.
	lastRow := 9 + len(people)
	spare := lastRow + 1
	dataCols := map[string]cellSpec{
		"A": {h: "center"},
		"B": {},
		"C": {h: "center"},
		"D": {h: "center", numFmt: fmtComma0},
		"E": {h: "center", numFmt: fmtComma0},
		"F": {numFmt: fmtComma0},
		"G": {h: "center", numFmt: fmtComma0},
		"H": {h: "center"},
		"I": {h: "center"},
		"J": {h: "center"},
	}
	for r := 10; r <= spare; r++ {
		for col, spec := range dataCols {
			spec.bl, spec.br, spec.bb = "thin", "thin", "hair"
			if r == spare {
				spec.bb = "thin"
			}
			if err := styAt(col, r, spec); err != nil {
				return err
			}
		}
	}

	sum := lastRow + 2
	if err := set(at("B", sum), "รวมเบิกเป็นเงินทั้งสิ้น"); err != nil {
		return err
	}
	if err := styAt("B", sum, cellSpec{bold: true, h: "left"}); err != nil {
		return err
	}
	if err := set(at("G", sum), fmt.Sprintf("=SUM(G10:G%d)", lastRow)); err != nil {
		return err
	}
	// The total is set off with MEDIUM rules down its sides in their file.
	if err := styAt("G", sum, cellSpec{bold: true, numFmt: fmtComma00,
		bl: "medium", br: "medium", bt: "thin", bb: "thin"}); err != nil {
		return err
	}
	if err := set(at("B", sum+1), "(ตัวอักษร)"); err != nil {
		return err
	}
	if err := styAt("B", sum+1, cellSpec{bold: true, h: "right"}); err != nil {
		return err
	}
	if err := set(at("C", sum+1), fmt.Sprintf(`="("&BAHTTEXT(G%d)&")"`, sum)); err != nil {
		return err
	}
	if err := f.MergeCell(sheet, at("C", sum+1), at("H", sum+1)); err != nil {
		return err
	}
	if err := sty(at("C", sum+1), at("H", sum+1), cellSpec{bold: true, h: "center"}); err != nil {
		return err
	}

	sig := sum + 4
	if err := set(at("B", sig), "ลงชื่อ………………………………………………….ผู้จ่ายเงิน"); err != nil {
		return err
	}
	if err := styAt("B", sig, cellSpec{}); err != nil {
		return err
	}
	certLines := []string{"ลงชื่อ ….........................................................."}
	if certName, _, ok := d.Certifier.ClaimCells(); ok {
		certLines = append(certLines, certName, "ตำแหน่ง "+d.Certifier.TitleLine)
		if d.Certifier.ActingFor != "" {
			certLines = append(certLines, d.Certifier.ActingFor)
		}
	}
	for i, line := range certLines {
		r := sig + i
		if err := set(at("G", r), line); err != nil {
			return err
		}
		if err := f.MergeCell(sheet, at("G", r), at("I", r)); err != nil {
			return err
		}
		if err := sty(at("G", r), at("I", r), cellSpec{}); err != nil {
			return err
		}
	}
	return nil
}

// collectCombinedBook gathers every claimant on a course across the whole term.
func (s *ExportService) collectCombinedBook(ctx context.Context, courseID uuid.UUID) (*combinedBookData, error) {
	d := &combinedBookData{}
	var termID uuid.UUID
	if err := s.pool.QueryRow(ctx, `
		SELECT tc.code, tc.term_id, t.academic_year, t.semester
		FROM teaching_courses tc JOIN academic_terms t ON t.id = tc.term_id
		WHERE tc.id = $1`, courseID).Scan(&d.CourseCode, &termID, &d.AcademicYear, &d.Semester); err != nil {
		return nil, err
	}
	// The same settlement the payout figures come from — the funded figure the
	// document prints in ขอเบิกจ่ายเพียง must be the one the money follows.
	settlement, err := s.SettleCourse(ctx, courseID)
	if err != nil {
		return nil, err
	}
	certifier, err := s.ResolveCertifier(ctx, termID)
	if err != nil {
		return nil, err
	}
	d.Certifier = certifier

	// The lecturer signs with their academic title — the college's file prints
	// "ผศ.ดร.วรัญญา วรรณศรี", not the bare name.
	_ = s.pool.QueryRow(ctx, `
		SELECT COALESCE(NULLIF(u.title,''),'')||COALESCE(u.first_name,'')||' '||COALESCE(u.last_name,'')
		FROM teaching_lecturers tl JOIN users u ON u.id = tl.lecturer_id
		WHERE tl.teaching_course_id = $1
		ORDER BY tl.is_primary DESC LIMIT 1`, courseID).Scan(&d.LecturerName)

	var pr PayRate
	_ = s.pool.QueryRow(ctx, `
		SELECT undergrad_regular, undergrad_special, graduate_regular_hourly,
		       ug_special_monthly_cap
		FROM pay_rates ORDER BY effective_from DESC LIMIT 1`).Scan(
		&pr.UndergradRegular, &pr.UndergradSpecial, &pr.GraduateRegularHourly,
		// The cap has to come along: this PayRate also prices the คาบ that
		// decide what the budget reached, and a zero cap there would pick a
		// different cutoff than the payout used.
		&pr.UGSpecialMonthlyCap)
	d.RateUGRegular = pr.UndergradRegular
	d.RateUGSpecial = pr.UndergradSpecial
	d.RateGradRegular = pr.GraduateRegularHourly

	// Funded baht per (TA, track), priced by the SAME source the settlement
	// settled on and split at its cutoff. The sheet prints every hour taught
	// (the office's instruction), so the budget no longer filters the rows —
	// it only decides the figure ขอเบิกจ่ายเพียง carries.
	costs, err := s.claimCostByTASlot(ctx, courseID, pr, mergedSittingsCTE)
	if err != nil {
		return nil, err
	}
	funded := map[taTrackKey]float64{}
	for _, c := range costs {
		t := settlement.Regular
		if c.Track == "special" {
			t = settlement.Special
		}
		if !t.unpaidFrom(c.Date, c.StartTime) {
			funded[taTrackKey{c.TA, c.Track}] += c.Baht
		}
	}

	// Every TA on the course, with the level their assignment was made at.
	// Names carry the คำนำหน้า (นาย/นาง/นางสาว) the TA declared on their
	// profile, falling back to users.title — the college's forms print
	// "นายสรวิศ ไผ่พันธ์", never the bare name, and the same composition is
	// used by the appointment order.
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT a.ta_id,
		       COALESCE(NULLIF(tp.prefix,''), NULLIF(u.title,''), '')||
		       COALESCE(u.first_name,'')||' '||COALESCE(u.last_name,''),
		       a.level::text
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		JOIN users u ON u.id = a.ta_id
		LEFT JOIN ta_profiles tp ON tp.user_id = u.id
		WHERE sec.teaching_course_id = $1 AND a.state <> 'dropped'
		ORDER BY 2`, courseID)
	if err != nil {
		return nil, err
	}
	type person struct {
		id    uuid.UUID
		name  string
		level string
	}
	var people []person
	for rows.Next() {
		var p person
		if err := rows.Scan(&p.id, &p.name, &p.level); err != nil {
			rows.Close()
			return nil, err
		}
		people = append(people, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var earliest, latest time.Time
	for _, p := range people {
		logs, err := s.claimLogsAllMonths(ctx, p.id, courseID)
		if err != nil {
			return nil, err
		}
		if p.level == "undergrad" {
			// กติกา B2: clock time logged on BOTH tracks at once is claimed
			// once, on the regular sheet. Cut it out of the special rows here
			// so the special sheet's own SUM × rate arrives at the same baht
			// the system pays — otherwise the printed claim over-bills.
			logs = clipSpecialOverlap(logs)
		}
		// No budget clipping here: the office reads the sheet as the record of
		// what was TAUGHT, in full. What the budget could not fund shows up
		// only as the gap between รวมเป็นเงินทั้งสิ้น and ขอเบิกจ่ายเพียง.
		for _, l := range logs {
			if earliest.IsZero() || l.Date.Before(earliest) {
				earliest = l.Date
			}
			if l.Date.After(latest) {
				latest = l.Date
			}
		}
		levelTH := "ป.ตรี"
		if p.level != "undergrad" {
			levelTH = "ป.โท/เอก"
		}
		for _, side := range []struct {
			track, word string
			dst         *[]claimant
			rate        float64
		}{
			{"regular", "ปกติ", &d.Regular, rateFor(pr, p.level, "regular")},
			{"special", "พิเศษ", &d.Special, rateFor(pr, p.level, "special")},
		} {
			var mine []claimLogRow
			for _, l := range logs {
				if l.Track == side.track {
					mine = append(mine, l)
				}
			}
			if len(mine) == 0 {
				continue
			}
			*side.dst = append(*side.dst, claimant{
				TAID: p.id, Name: p.name, LevelTH: levelTH,
				Rows: buildClaimSheetRows(mine, side.word), Rate: side.rate,
				PaidBaht: round2(funded[taTrackKey{p.id, side.track}]),
				LumpSum:  p.level != "undergrad" && side.track == "special",
			})
		}
	}
	sortClaimants(d.Regular)
	sortClaimants(d.Special)

	if !earliest.IsZero() {
		d.MonthRange = thaiMonthRange(earliest, latest)
	}
	return d, nil
}

func sortClaimants(cs []claimant) {
	sort.Slice(cs, func(i, j int) bool { return cs[i].Name < cs[j].Name })
}

// thaiMonthRange renders "มิถุนายน 2569 - ตุลาคม 2569", collapsing to a single
// month when the work does not span one.
func thaiMonthRange(from, to time.Time) string {
	a := fmt.Sprintf("%s %d", thaiMonthNames[int(from.Month())], from.Year()+543)
	b := fmt.Sprintf("%s %d", thaiMonthNames[int(to.Month())], to.Year()+543)
	if a == b {
		return a
	}
	return a + " - " + b
}

func rateFor(pr PayRate, level, track string) float64 {
	if level == "undergrad" {
		if track == "special" {
			return pr.UndergradSpecial
		}
		return pr.UndergradRegular
	}
	return pr.GraduateRegularHourly
}

// claimLogsAllMonths is claimLogs without the month filter: one block now covers
// the whole term, which is the change this file exists for.
func (s *ExportService) claimLogsAllMonths(ctx context.Context, taID, courseID uuid.UUID) ([]claimLogRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sec.sec_no, sec.track::text, wl.work_date,
		       EXTRACT(HOUR FROM wl.start_time)*60 + EXTRACT(MINUTE FROM wl.start_time),
		       EXTRACT(HOUR FROM wl.end_time)*60 + EXTRACT(MINUTE FROM wl.end_time),
		       wl.activity, COALESCE(wl.note,'') LIKE '%ชดเชย%'
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		JOIN sections sec ON sec.id = a.section_id
		WHERE a.ta_id = $1 AND sec.teaching_course_id = $2
		  AND wl.status = 'approved'
		ORDER BY wl.work_date, wl.start_time`, taID, courseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []claimLogRow
	for rows.Next() {
		var r claimLogRow
		var sm, em float64
		if err := rows.Scan(&r.SecNo, &r.Track, &r.Date, &sm, &em, &r.Activity, &r.Makeup); err != nil {
			return nil, err
		}
		r.StartMin, r.EndMin = int(sm), int(em)
		out = append(out, r)
	}
	return out, rows.Err()
}

// BuildTimetableWorkbook renders one TA's ตารางเรียนและตารางปฏิบัติงาน as its
// own Excel file.
//
// The weekly grid used to travel as the ตารางสอน sheet inside every monthly
// claim workbook, and again as a PDF. Now that the claims are one combined book
// for the whole course, the grid cannot ride along: it is per PERSON and spans
// every course they assist, so it would be wrong on a course-scoped sheet and
// duplicated five times over. It ships as one file per TA instead.
func (s *ExportService) BuildTimetableWorkbook(ctx context.Context, taID, termID uuid.UUID) ([]byte, error) {
	f, err := excelize.OpenFile(s.claimTemplatePath())
	if err != nil {
		return nil, fmt.Errorf("open claim template: %w", err)
	}
	defer f.Close()

	// With the คำนำหน้า: the college's form prints "นายชนาธิป สีลาพล" at I3.
	var fullName, studentID string
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(NULLIF(tp.prefix,''), NULLIF(u.title,''), '')||
		       COALESCE(u.first_name,'')||' '||COALESCE(u.last_name,''),
		       COALESCE(u.student_id,'')
		FROM users u LEFT JOIN ta_profiles tp ON tp.user_id = u.id
		WHERE u.id=$1`, taID).Scan(&fullName, &studentID); err != nil {
		return nil, err
	}
	var semester, acadYear int
	if err := s.pool.QueryRow(ctx,
		`SELECT semester, academic_year FROM academic_terms WHERE id=$1`,
		termID).Scan(&semester, &acadYear); err != nil {
		return nil, err
	}

	const tt = "ตารางสอน"
	semTH := map[int]string{1: "ภาคต้น", 2: "ภาคปลาย", 3: "ฤดูร้อน"}[semester]
	f.SetCellValue(tt, "B1", fmt.Sprintf("ตารางเรียนและตารางปฏิบัติงาน (TA)  %s  ปีการศึกษา %d", semTH, acadYear))
	f.SetCellValue(tt, "I3", fullName)
	f.SetCellValue(tt, "O3", studentID)
	var returning bool
	_ = s.pool.QueryRow(ctx, `SELECT EXISTS (
	    SELECT 1 FROM ta_request_assignments a
	    JOIN ta_requests r ON r.id=a.request_id AND r.status='approved'
	    JOIN teaching_courses tc ON tc.id=r.teaching_course_id
	    WHERE a.ta_id=$1 AND tc.term_id <> $2 AND a.state <> 'dropped')`,
		taID, termID).Scan(&returning)
	if returning {
		f.SetCellValue(tt, "AA1", "TA เดิม")
	}
	if err := s.fillTimetableGrid(ctx, f, taID, termID); err != nil {
		return nil, err
	}
	if err := s.fillClaimSignatures(ctx, f, taID, termID, fullName); err != nil {
		return nil, err
	}

	// The claim sheets belong to the combined book now; leaving them here would
	// ship a second, course-blind copy of the same document.
	for _, sheet := range []string{"ภาคปกติ", "โครงการพิเศษ"} {
		if err := f.DeleteSheet(sheet); err != nil {
			return nil, err
		}
	}
	if idx, err := f.GetSheetIndex(tt); err == nil && idx >= 0 {
		f.SetActiveSheet(idx)
	}

	buf, err := f.WriteToBuffer()
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// clipSpecialOverlap removes from every special-track row the minutes the same
// TA also logged on a regular-track section that day (B2 — those hours are
// billed once, at the regular rate, on the regular sheet). A fully co-taught
// row disappears; a partially co-taught one keeps only its uncovered minutes.
// Regular rows pass through untouched.
func clipSpecialOverlap(rows []claimLogRow) []claimLogRow {
	regular := map[string][][2]int{}
	for _, r := range rows {
		if r.Track == "regular" {
			d := r.Date.Format("2006-01-02")
			regular[d] = append(regular[d], [2]int{r.StartMin, r.EndMin})
		}
	}
	out := make([]claimLogRow, 0, len(rows))
	for _, r := range rows {
		if r.Track != "special" {
			out = append(out, r)
			continue
		}
		for _, seg := range subtractIntervals([2]int{r.StartMin, r.EndMin}, regular[r.Date.Format("2006-01-02")]) {
			nr := r
			nr.StartMin, nr.EndMin = seg[0], seg[1]
			out = append(out, nr)
		}
	}
	return out
}

// subtractIntervals returns the parts of iv not covered by any cut, in order.
func subtractIntervals(iv [2]int, cuts [][2]int) [][2]int {
	segs := [][2]int{iv}
	for _, c := range cuts {
		next := segs[:0:0]
		for _, s := range segs {
			if c[1] <= s[0] || c[0] >= s[1] { // no overlap with this cut
				next = append(next, s)
				continue
			}
			if s[0] < c[0] {
				next = append(next, [2]int{s[0], c[0]})
			}
			if c[1] < s[1] {
				next = append(next, [2]int{c[1], s[1]})
			}
		}
		segs = next
	}
	return segs
}

