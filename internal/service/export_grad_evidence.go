// export_grad_evidence.go builds หลักฐานการจ่ายเงินอื่น ๆ for GRADUATE TAs.
//
// The college files a different form for บัณฑิตศึกษา than for ป.ตรี, and the
// difference is not cosmetic — it is a column layout the undergrad form does
// not have. Compare their own two files:
//
//	docs/15.CP362104.xlsx        ป.ตรี   — one total column: จำนวนชั่วโมง → อัตรา → เงิน
//	docs/14. CP363761-บัณฑิต.xls บัณฑิต — one column PER MONTH, then the total
//
// export_combined_book.go already renders the undergrad shape (writeEvidenceSheet)
// and is left alone: it is what ป.ตรี courses have always produced, and every
// course in the college still files it. This file adds the graduate form as its
// own workbook alongside it, so a mixed course gets both and neither has to
// compromise.
//
// The two graduate sheets are different documents, not two copies of one:
//
//	หลักฐาน - ปกติ   grad-regular, paid by the hour — month columns hold HOURS,
//	                 and the money is รวมชั่วโมง × อัตราต่อชั่วโมง.
//	หลักฐาน-พิเศษ    grad-special, เหมาจ่าย — month columns hold BAHT directly.
//	                 There is no hour or rate column at all, because there are no
//	                 hours: these TAs stopped logging work_logs entirely (2026
//	                 meeting) and the system splits the flat term lump across the
//	                 months by the regular track's real teaching schedule.
package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"
)

// Sheet names are the college's own, spacing included — their graduate file
// writes "หลักฐาน - ปกติ" with spaces and "หลักฐาน-พิเศษ" without. Reproduced
// rather than normalised: staff recognise these tabs, and the undergrad book's
// sheets (sheetEvidenceRegular/Special) live in a different workbook so there
// is no collision to resolve.
const (
	sheetGradEvidenceRegular = "หลักฐาน - ปกติ"
	sheetGradEvidenceSpecial = "หลักฐาน-พิเศษ"
	// fillSilver is the band behind the (ตัวอักษร) amount-in-words line —
	// palette index 22 in the college's own .xls, i.e. plain silver. The claim
	// sheet's own fillGrey (D8D8D8) is a different, lighter band used for a
	// different row, so the two are not interchangeable.
	fillSilver = "C0C0C0"
)

// gradEvidencePerson is one graduate TA's row on one of the two sheets.
type gradEvidencePerson struct {
	Name    string
	LevelTH string // "ป.โท" / "ป.เอก"
	// ByMonth is keyed by Gregorian "YYYY-MM". On the ปกติ sheet it holds HOURS,
	// on the พิเศษ sheet BAHT — the same field because the sheets are otherwise
	// the same table, and keeping them apart bought nothing but two writers.
	ByMonth map[string]float64
}

// total sums the months the sheet actually prints. Months outside the printed
// set are deliberately not counted: the export can be scoped to a fiscal slice
// (งบแผ่นดิน closes 30 กันยายน mid-term), and a total that quietly included
// October would not match the columns above it.
func (p gradEvidencePerson) total(months []string) float64 {
	var t float64
	for _, m := range months {
		t += p.ByMonth[m]
	}
	return t
}

// gradEvidenceData is everything the graduate workbook prints.
type gradEvidenceData struct {
	CourseCode   string
	AcademicYear int
	Semester     int
	Certifier    CertifierChoice
	// Months are Gregorian "YYYY-MM", ascending — one column each.
	Months          []string
	RateGradRegular float64
	Regular         []gradEvidencePerson
	Special         []gradEvidencePerson
}

// BuildGradEvidenceWorkbook renders the graduate หลักฐานการจ่ายเงิน for one
// course, or (nil, nil) when the course has no graduate TAs at all.
//
// A nil return is not an error: most courses are undergrad-only, and the ZIP
// builder simply has nothing to add for them. Returning an error there would
// make every ordinary export log a failure it should ignore.
func (s *ExportService) BuildGradEvidenceWorkbook(ctx context.Context, courseID uuid.UUID, months []string) ([]byte, error) {
	d, err := s.collectGradEvidence(ctx, courseID, months)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, nil
	}

	f := excelize.NewFile()
	defer f.Close()
	st, err := newClaimStyles(f)
	if err != nil {
		return nil, err
	}

	if len(d.Regular) > 0 {
		if err := writeGradEvidenceSheet(f, st, sheetGradEvidenceRegular, d, d.Regular, false); err != nil {
			return nil, err
		}
	}
	if len(d.Special) > 0 {
		if err := writeGradEvidenceSheet(f, st, sheetGradEvidenceSpecial, d, d.Special, true); err != nil {
			return nil, err
		}
	}

	f.DeleteSheet("Sheet1")
	if idx, err := f.GetSheetIndex(sheetGradEvidenceRegular); err == nil && idx >= 0 {
		f.SetActiveSheet(idx)
	}
	buf, err := f.WriteToBuffer()
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// gradEvidenceCols names every column of one sheet by its letter, computed from
// the month count. The layout is fixed on both sides of a variable-width month
// block, so the letters cannot be constants.
type gradEvidenceCols struct {
	Seq, Name, Code, Level string
	Months                 []string // one per printed month
	// Regular sheet only.
	TotalHours, Rate, Amount string
	// Both sheets.
	Received string
	// Trailing columns differ per sheet: the regular form ends in two signature
	// columns, the special form in date / signature / หมายเหตุ.
	Tail []string
	Last string
}

func gradEvidenceLayout(nMonths int, lump bool) gradEvidenceCols {
	col := func(i int) string {
		name, _ := excelize.ColumnNumberToName(i)
		return name
	}
	c := gradEvidenceCols{Seq: col(1), Name: col(2), Code: col(3), Level: col(4)}
	next := 5
	for i := 0; i < nMonths; i++ {
		c.Months = append(c.Months, col(next))
		next++
	}
	if !lump {
		c.TotalHours, c.Rate, c.Amount = col(next), col(next+1), col(next+2)
		next += 3
	}
	c.Received = col(next)
	next++
	tail := 2 // ลายมือชื่อผู้รับเงิน + ผู้รับรอง
	if lump {
		tail = 3 // วัน เดือน ปี + ลายมือชื่อ + หมายเหตุ
	}
	for i := 0; i < tail; i++ {
		c.Tail = append(c.Tail, col(next))
		next++
	}
	c.Last = col(next - 1)
	return c
}

// writeGradEvidenceSheet renders one of the two graduate evidence sheets.
//
// lump switches between the two forms: false is the hourly ปกติ sheet (month
// columns are hours, and the money is a formula), true is the เหมาจ่าย พิเศษ
// sheet (month columns are already baht, and there is no rate to show).
func writeGradEvidenceSheet(f *excelize.File, st *claimStyles, sheet string,
	d *gradEvidenceData, people []gradEvidencePerson, lump bool) error {
	if _, err := f.NewSheet(sheet); err != nil {
		return err
	}
	c := gradEvidenceLayout(len(d.Months), lump)

	widths := map[string]float64{c.Seq: 6.8, c.Name: 29.4, c.Code: 13.6, c.Level: 11.4}
	for _, m := range c.Months {
		widths[m] = 12.6
	}
	if !lump {
		widths[c.TotalHours], widths[c.Rate], widths[c.Amount] = 11.2, 11.6, 14.2
	}
	widths[c.Received] = 14.8
	for i, t := range c.Tail {
		widths[t] = 18.4
		if lump && i == 0 {
			widths[t] = 12.4 // วัน เดือน ปี ที่รับเงิน
		}
	}
	for col, w := range widths {
		if err := f.SetColWidth(sheet, col, col, w); err != nil {
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
	sty := func(from, to string, spec cellSpec) error {
		spec.size = evidenceFontSize
		id, err := st.id(spec)
		if err != nil {
			return err
		}
		return f.SetCellStyle(sheet, from, to, id)
	}
	styAt := func(col string, r int, spec cellSpec) error { return sty(at(col, r), at(col, r), spec) }

	// Rows 1-5: the preamble, merged across the whole table. Same five lines the
	// undergrad form carries — this is one government form with two layouts, not
	// two forms.
	semTH := map[int]string{1: "ภาคต้น", 2: "ภาคปลาย", 3: "ภาคฤดูร้อน"}[d.Semester]
	for i, line := range []string{
		"หลักฐานการจ่ายเงินอื่น ๆ",
		"เบิกตามฎีกาที่...................................... วันที่..................... เดือน ...................................... พ.ศ. .......................",
		"ข้าพเจ้าผู้มีรายนามข้างท้ายนี้ ได้รับเงินจากส่วนราชการ   วิทยาลัยการคอมพิวเตอร์  มหาวิทยาลัยขอนแก่น   เป็นค่าตอบแทนผู้ช่วยสอนและผู้ช่วยปฏิบัติงาน",
		fmt.Sprintf("สาขาวิชาวิทยาการคอมพิวเตอร์  ประจำ%s  ปีการศึกษา %d", semTH, d.AcademicYear),
		"ตามหนังสืออนุมัติที่ อว 660301.26.6.1/ ง         ลงวันที่       เดือน               พ.ศ. ....  ได้เป็นการถูกต้องแล้วจึงลงลายมือชื่อไว้เป็นสำคัญ",
	} {
		r := i + 1
		if err := set(at(c.Seq, r), line); err != nil {
			return err
		}
		if err := f.MergeCell(sheet, at(c.Seq, r), at(c.Last, r)); err != nil {
			return err
		}
		if err := sty(at(c.Seq, r), at(c.Last, r), cellSpec{bold: true, h: "center"}); err != nil {
			return err
		}
	}

	// Rows 6-7: the level and track ticks. This workbook exists only for
	// graduate TAs, so บัณฑิตศึกษา is always the ticked level — the ป.ตรี box is
	// printed unticked because the form is filed with both boxes visible.
	trackReg, trackSp := "(  / )", "(    )"
	if lump {
		trackReg, trackSp = "(    )", "(  / )"
	}
	half := len(c.Months)/2 + 4
	if half < 5 {
		half = 5
	}
	midCol, _ := excelize.ColumnNumberToName(half)
	rightCol, _ := excelize.ColumnNumberToName(half + 1)
	for _, box := range []struct {
		cell, to, text string
		spec           cellSpec
	}{
		{at(c.Name, 6), "", "รหัสวิชา " + d.CourseCode, cellSpec{bold: true, h: "center"}},
		{at(c.Code, 6), at(c.Level, 6), "รายวิชาระดับ", cellSpec{bold: true, h: "center"}},
		{at(c.Months[0], 6), at(midCol, 6), "(    ) ปริญญาตรี", cellSpec{bold: true, h: "left"}},
		{at(rightCol, 6), at(c.Last, 6), "(  / ) บัณฑิตศึกษา", cellSpec{bold: true, h: "left"}},
		{at(c.Months[0], 7), at(midCol, 7), trackReg + " ภาคปกติ", cellSpec{bold: true, h: "left"}},
		{at(rightCol, 7), at(c.Last, 7), trackSp + " โครงการพิเศษ", cellSpec{bold: true, h: "left"}},
	} {
		if err := set(box.cell, box.text); err != nil {
			return err
		}
		to := box.to
		if to == "" {
			to = box.cell
		}
		if box.to != "" && box.to != box.cell {
			if err := f.MergeCell(sheet, box.cell, box.to); err != nil {
				return err
			}
		}
		if err := sty(box.cell, to, box.spec); err != nil {
			return err
		}
	}

	// Header rows 8 + 9. The month block is ONE merged caption on row 8 over the
	// individual month labels on row 9 — the shape that makes the per-month
	// layout readable, and the reason this form could not reuse the undergrad
	// writer.
	monthCaption := "จำนวนชั่วโมงการปฏิบัติงาน (ต่อเดือน)"
	if lump {
		monthCaption = "อัตราเบิกจ่าย (แบบเหมาจ่าย)"
	}
	// The college's two sheets label the level column differently —
	// "ผู้ช่วยสอนระดับ" on ปกติ, plain "ระดับ" on พิเศษ. Reproduced rather than
	// unified: staff match these forms against the ones they already file.
	levelHeading := "ผู้ช่วยสอนระดับ"
	if lump {
		levelHeading = "ระดับ"
	}
	top := []struct{ col, text string }{
		{c.Seq, "ลำดับ"}, {c.Name, "ชื่อผู้สอน"}, {c.Code, "รหัสวิชา"},
		{c.Level, levelHeading},
	}
	bottom := []struct{ col, text string }{
		{c.Seq, "ที่"}, {c.Level, "ตรี/โท/เอก"},
	}
	if lump {
		top = append(top,
			struct{ col, text string }{c.Received, "รับจริง"},
			struct{ col, text string }{c.Tail[0], "วัน เดือน ปี"},
			struct{ col, text string }{c.Tail[1], "ลายมือชื่อผู้รับเงิน"},
			struct{ col, text string }{c.Tail[2], "หมายเหตุ"})
		bottom = append(bottom, struct{ col, text string }{c.Tail[0], "ที่รับเงิน"})
	} else {
		top = append(top,
			struct{ col, text string }{c.TotalHours, "รวมจำนวน"},
			struct{ col, text string }{c.Rate, "อัตราตอบแทน"},
			struct{ col, text string }{c.Amount, "จำนวนเงิน"},
			struct{ col, text string }{c.Received, "รับจริง"},
			struct{ col, text string }{c.Tail[0], "ลายมือชื่อผู้รับเงิน/"},
			struct{ col, text string }{c.Tail[1], "ลายมือชื่อผู้รับรอง"})
		bottom = append(bottom,
			struct{ col, text string }{c.TotalHours, "ชั่วโมง"},
			struct{ col, text string }{c.Rate, "(ชม.) ละ"},
			struct{ col, text string }{c.Tail[0], "ผู้ปฏิบัติงาน"},
			struct{ col, text string }{c.Tail[1], "การทำงาน"})
	}
	for _, h := range top {
		if err := set(at(h.col, 8), h.text); err != nil {
			return err
		}
	}
	for _, h := range bottom {
		if err := set(at(h.col, 9), h.text); err != nil {
			return err
		}
	}
	if err := set(at(c.Months[0], 8), monthCaption); err != nil {
		return err
	}
	if len(c.Months) > 1 {
		if err := f.MergeCell(sheet, at(c.Months[0], 8), at(c.Months[len(c.Months)-1], 8)); err != nil {
			return err
		}
	}
	for i, ym := range d.Months {
		if err := set(at(c.Months[i], 9), thaiMonthLabels([]string{ym})[0]); err != nil {
			return err
		}
	}
	// The two header rows read as one tall box, and — faithfully to the
	// college's file — are NOT bold.
	if err := sty(at(c.Seq, 8), at(c.Last, 8), cellSpec{h: "center", wrap: true, bl: "thin", br: "thin", bt: "thin"}); err != nil {
		return err
	}
	if err := sty(at(c.Seq, 9), at(c.Last, 9), cellSpec{h: "center", wrap: true, bl: "thin", br: "thin", bb: "thin"}); err != nil {
		return err
	}

	// Data rows.
	for i, p := range people {
		r := 10 + i
		if err := set(at(c.Seq, r), i+1); err != nil {
			return err
		}
		if err := set(at(c.Name, r), p.Name); err != nil {
			return err
		}
		if err := set(at(c.Code, r), d.CourseCode); err != nil {
			return err
		}
		if err := set(at(c.Level, r), p.LevelTH); err != nil {
			return err
		}
		for j, ym := range d.Months {
			v := p.ByMonth[ym]
			if v == 0 {
				continue // a month with no work is left blank, not printed as 0
			}
			if err := set(at(c.Months[j], r), round2(v)); err != nil {
				return err
			}
		}
		first, last := c.Months[0], c.Months[len(c.Months)-1]
		if lump {
			// เหมาจ่าย: the months ARE the money, so รับจริง is their sum.
			if err := set(at(c.Received, r), fmt.Sprintf("=SUM(%s%d:%s%d)", first, r, last, r)); err != nil {
				return err
			}
			continue
		}
		if err := set(at(c.TotalHours, r), fmt.Sprintf("=SUM(%s%d:%s%d)", first, r, last, r)); err != nil {
			return err
		}
		if err := set(at(c.Rate, r), d.RateGradRegular); err != nil {
			return err
		}
		if err := set(at(c.Amount, r), fmt.Sprintf("=%s%d*%s%d", c.TotalHours, r, c.Rate, r)); err != nil {
			return err
		}
		if err := set(at(c.Received, r), fmt.Sprintf("=%s%d", c.Amount, r)); err != nil {
			return err
		}
	}

	// The grid, plus one spare ruled row closing the table the way the college's
	// file leaves one between the last name and the total.
	lastRow := 9 + len(people)
	spare := lastRow + 1
	specs := map[string]cellSpec{
		c.Seq:      {h: "center"},
		c.Name:     {},
		c.Code:     {h: "center"},
		c.Level:    {h: "center"},
		c.Received: {h: "center", numFmt: fmtComma0},
	}
	for _, m := range c.Months {
		if lump {
			specs[m] = cellSpec{h: "center", numFmt: fmtComma0}
		} else {
			specs[m] = cellSpec{h: "center", numFmt: fmtHours1}
		}
	}
	if !lump {
		specs[c.TotalHours] = cellSpec{h: "center", numFmt: fmtHours1}
		specs[c.Rate] = cellSpec{h: "center", numFmt: fmtComma0}
		specs[c.Amount] = cellSpec{numFmt: fmtComma0}
	}
	for _, t := range c.Tail {
		specs[t] = cellSpec{h: "center"}
	}
	for r := 10; r <= spare; r++ {
		for col, spec := range specs {
			spec.bl, spec.br, spec.bb = "thin", "thin", "hair"
			if r == spare {
				spec.bb = "thin"
			}
			if err := styAt(col, r, spec); err != nil {
				return err
			}
		}
	}

	// รวมเบิกเป็นเงินทั้งสิ้น, under the รับจริง column on both sheets.
	//
	// Both bands are boxed in THIN rules — the graduate form does not use the
	// medium side rules the undergrad one does (compare their two files) — and
	// the ตัวอักษร band carries a silver C0C0C0 fill across its whole width.
	// That fill is the one the office spotted missing.
	sum := lastRow + 2
	if err := set(at(c.Name, sum), "รวมเบิกเป็นเงินทั้งสิ้น"); err != nil {
		return err
	}
	if err := styAt(c.Name, sum, cellSpec{bold: true, h: "left",
		bl: "thin", br: "thin", bt: "thin", bb: "thin"}); err != nil {
		return err
	}
	if err := set(at(c.Received, sum), fmt.Sprintf("=SUM(%s10:%s%d)", c.Received, c.Received, lastRow)); err != nil {
		return err
	}
	if err := styAt(c.Received, sum, cellSpec{bold: true, numFmt: fmtComma00,
		bl: "thin", br: "thin", bt: "thin", bb: "thin"}); err != nil {
		return err
	}
	if err := set(at(c.Name, sum+1), "(ตัวอักษร)"); err != nil {
		return err
	}
	if err := styAt(c.Name, sum+1, cellSpec{bold: true, h: "right", bt: "thin", bb: "thin"}); err != nil {
		return err
	}
	// Excel's own BAHTTEXT, as the undergrad sheet does: the workbook is opened
	// and edited by hand after export, and a Go-rendered string would not follow
	// the total if a figure is corrected.
	//
	// The band runs from the ระดับ column to the last one on the sheet, so the
	// fill reaches the right edge rather than stopping under รับจริง.
	if err := set(at(c.Level, sum+1), fmt.Sprintf(`="("&BAHTTEXT(%s%d)&")"`, c.Received, sum)); err != nil {
		return err
	}
	if err := f.MergeCell(sheet, at(c.Level, sum+1), at(c.Last, sum+1)); err != nil {
		return err
	}
	if err := sty(at(c.Level, sum+1), at(c.Last, sum+1), cellSpec{bold: true, h: "center",
		fill: fillSilver, bt: "thin", bb: "thin"}); err != nil {
		return err
	}

	sig := sum + 4
	if err := set(at(c.Name, sig), "ลงชื่อ………………………………………………….ผู้จ่ายเงิน"); err != nil {
		return err
	}
	if err := styAt(c.Name, sig, cellSpec{}); err != nil {
		return err
	}
	certLines := []string{"ลงชื่อ ….........................................................."}
	if certName, _, ok := d.Certifier.ClaimCells(); ok {
		certLines = append(certLines, certName, "ตำแหน่ง "+d.Certifier.TitleLine)
		if d.Certifier.ActingFor != "" {
			certLines = append(certLines, d.Certifier.ActingFor)
		}
	}
	sigFrom := c.Received
	for i, line := range certLines {
		r := sig + i
		if err := set(at(sigFrom, r), line); err != nil {
			return err
		}
		if err := f.MergeCell(sheet, at(sigFrom, r), at(c.Last, r)); err != nil {
			return err
		}
		if err := sty(at(sigFrom, r), at(c.Last, r), cellSpec{}); err != nil {
			return err
		}
	}
	return nil
}

// collectGradEvidence gathers both sheets' data, or (nil, nil) when the course
// has no graduate TA on either track.
func (s *ExportService) collectGradEvidence(ctx context.Context, courseID uuid.UUID, months []string) (*gradEvidenceData, error) {
	d := &gradEvidenceData{}
	var termID uuid.UUID
	if err := s.pool.QueryRow(ctx, `
		SELECT tc.code, tc.term_id, t.academic_year, t.semester
		FROM teaching_courses tc JOIN academic_terms t ON t.id = tc.term_id
		WHERE tc.id = $1`, courseID).Scan(&d.CourseCode, &termID, &d.AcademicYear, &d.Semester); err != nil {
		return nil, err
	}
	certifier, err := s.ResolveCertifier(ctx, termID)
	if err != nil {
		return nil, err
	}
	d.Certifier = certifier

	var pr PayRate
	if err := s.pool.QueryRow(ctx, `
		SELECT graduate_regular_hourly, graduate_special_lumpsum, grad_special_term_cap
		FROM pay_rates ORDER BY effective_from DESC LIMIT 1`).Scan(
		&pr.GraduateRegularHourly, &pr.GraduateSpecialLumpsum, &pr.GradSpecialTermCap); err != nil {
		return nil, err
	}
	d.RateGradRegular = pr.GraduateRegularHourly

	// Every graduate TA on the course, named the way the college's forms name
	// them (คำนำหน้า from the profile, falling back to users.title).
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT a.ta_id,
		       COALESCE(NULLIF(tp.prefix,''), NULLIF(u.title,''), '')||
		       COALESCE(u.first_name,'')||' '||COALESCE(u.last_name,''),
		       a.level::text
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		JOIN sections sec  ON sec.id = a.section_id
		JOIN users u       ON u.id = a.ta_id
		LEFT JOIN ta_profiles tp ON tp.user_id = u.id
		WHERE sec.teaching_course_id = $1 AND a.state <> 'dropped'
		  AND a.level::text IN ('master','phd')
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
	if len(people) == 0 {
		return nil, nil
	}

	// The grad-special lump and its per-month split. Both sheets need the month
	// list, and grad-special contributes months no work_log could — these TAs
	// log nothing, so their columns come from the teaching schedule alone.
	gradSpecial := map[uuid.UUID]bool{}
	ids, err := s.gradSpecialTAIDs(ctx, courseID)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		gradSpecial[id] = true
	}
	gradLump := pr.GraduateSpecialLumpsum
	if pr.GradSpecialTermCap > 0 && gradLump > pr.GradSpecialTermCap {
		gradLump = pr.GradSpecialTermCap
	}
	weights, err := gradSpecialMonthShares(ctx, s.pool, courseID)
	if err != nil {
		return nil, err
	}

	// termMonths is the WHOLE term, independent of what is being exported now.
	// The เหมาจ่าย lump is a per-course-per-TERM figure, so it can only be
	// divided against the whole term — see gradSpecialSlice below.
	termMonths := []string{}
	all, err := s.CourseTermMonths(ctx, courseID)
	if err != nil {
		return nil, err
	}
	for _, m := range all {
		termMonths = append(termMonths, m.YearMonth)
	}
	// A term whose submission_periods have not been created yet has no months to
	// enumerate, and grad-special contributes none of its own — those TAs log
	// nothing. Falling back to the course's own calendar keeps the เหมาจ่าย
	// sheet from disappearing entirely in that state, which would drop a real
	// payment off the paperwork rather than merely mislabelling it.
	if len(termMonths) == 0 {
		cal, err := courseCalendarMonths(ctx, s.pool, courseID)
		if err != nil {
			return nil, err
		}
		termMonths = cal
	}
	// A month the schedule weights know about but the period list does not still
	// carries lump, so it has to be part of the divisor.
	inTerm := map[string]bool{}
	for _, m := range termMonths {
		inTerm[m] = true
	}
	for ym := range weights {
		if !inTerm[ym] {
			inTerm[ym] = true
			termMonths = append(termMonths, ym)
		}
	}
	sort.Strings(termMonths)

	// The month COLUMNS. A pinned selection is honoured exactly — widening it
	// would bill a fiscal year the caller deliberately excluded.
	scoped := append([]string(nil), months...)
	if len(scoped) == 0 {
		scoped = append([]string(nil), termMonths...)
	}
	seen := map[string]bool{}
	for _, m := range scoped {
		seen[m] = true
	}
	// addMonth widens the column set for real data that falls outside it — only
	// possible when the caller pinned nothing and the term has no
	// submission_periods to enumerate. Dropping such a month would print a total
	// that its own columns cannot account for.
	addMonth := func(ym string) {
		if len(months) > 0 || seen[ym] {
			return
		}
		seen[ym] = true
		scoped = append(scoped, ym)
	}

	// Pass 1: the hours, and the final month set.
	//
	// A TA belongs to a TRACK PER ASSIGNMENT, not per person. One graduate TA
	// can assist the regular sections AND the special-programme section of the
	// same course, which is exactly why this workbook has two sheets — so the
	// two must be computed independently. Treating "is grad-special" as a
	// property of the person dropped such a TA's entire regular-track claim:
	// on the live CP423434 their 108 approved hours vanished from the document
	// while only the เหมาจ่าย row printed.
	//
	// Hours come from the SAME priced คาบ the payout settles on
	// (claimCostByTASlot over merged sittings), never from raw work_log
	// durations. Summing the raw rows double-counts a TA who assists two
	// CO-TAUGHT sections: sec 1 ภาคปกติ and sec 2 โครงการพิเศษ meet in one room
	// at one hour, the generator writes the sitting against both, and the payout
	// settles it once. On the live CP423434 that gap was 196 hours on the
	// document against 108 the system will actually pay — a claim form billing
	// ฿4,400 more than the transfer behind it.
	isGrad := map[uuid.UUID]bool{}
	for _, p := range people {
		isGrad[p.id] = true
	}
	costs, err := s.claimCostByTASlot(ctx, courseID, pr, mergedSittingsCTE)
	if err != nil {
		return nil, err
	}
	hoursByTA := map[uuid.UUID]map[string]float64{}
	for _, c := range costs {
		// Special-track time is เหมาจ่าย and priced at 0 here; only the
		// regular track is billed by the hour.
		if c.Track != "regular" || !isGrad[c.TA] {
			continue
		}
		if len(months) > 0 && !seen[c.YearMonth] {
			continue // a fiscal slice the caller deliberately excluded
		}
		if pr.GraduateRegularHourly <= 0 {
			continue
		}
		// baht = hours × graduate_regular_hourly for this branch of the pricing
		// query, so the division recovers the settled hours exactly.
		if hoursByTA[c.TA] == nil {
			hoursByTA[c.TA] = map[string]float64{}
		}
		hoursByTA[c.TA][c.YearMonth] += c.Baht / pr.GraduateRegularHourly
		addMonth(c.YearMonth)
	}
	for ym := range weights {
		if len(months) > 0 && !seen[ym] {
			continue
		}
		addMonth(ym)
	}

	// Pass 2: the rows, now that `scoped` is final — the lump is split across
	// the printed columns, so it cannot be computed before they are all known.
	for _, p := range people {
		levelTH := "ป.โท"
		if p.level == "phd" {
			levelTH = "ป.เอก"
		}
		if byMonth := hoursByTA[p.id]; len(byMonth) > 0 {
			d.Regular = append(d.Regular, gradEvidencePerson{
				Name: p.name, LevelTH: levelTH, ByMonth: byMonth,
			})
		}
		if gradSpecial[p.id] {
			// Divided over the WHOLE term, then only the printed months show.
			// The sheet's own SUM therefore falls short of the full lump on a
			// partial export — which is the point: the missing months are
			// claimed on the other fiscal year's document.
			d.Special = append(d.Special, gradEvidencePerson{
				Name: p.name, LevelTH: levelTH,
				ByMonth: distributeLump(gradLump, termMonths, weights),
			})
		}
	}
	if len(d.Regular) == 0 && len(d.Special) == 0 {
		return nil, nil
	}

	d.Months = scoped
	sort.Strings(d.Months)
	if len(d.Months) == 0 {
		return nil, nil
	}
	return d, nil
}

// distributeLump splits a flat term lump across months, weighted by the
// regular track's real teaching schedule when there is one and evenly when
// there is not (gradSpecialMonthShares returns nil weights for a course whose
// schedule has not been filled in).
//
// months MUST be the WHOLE TERM, never the slice being exported. เหมาจ่าย is
// 4,000 per course per TERM, and ภาคต้น (มิ.ย.–ต.ค.) straddles the 30 กันยายน
// budget boundary, so it is claimed on two documents. Dividing by the selected
// months instead would put the entire 4,000 on the มิ.ย.–ก.ย. document and
// another 4,000 on ตุลาคม's — 8,000 against a 4,000 cap, on two forms that
// each look correct in isolation. Callers print only the months they are
// claiming, so the partial document simply totals less than the lump.
//
// The last month absorbs the rounding remainder so the columns add back up to
// the lump EXACTLY. Rounding each month independently is off by a satang or
// two — 1000/3 prints as 333.33 three times, totalling 999.99 — and this
// document is reconciled against the actual transfer, where a figure that is
// one satang short is a figure somebody has to chase.
func distributeLump(total float64, months []string, weights map[string]float64) map[string]float64 {
	out := map[string]float64{}
	if len(months) == 0 || total == 0 {
		return out
	}
	// Only months this export covers can carry weight; a weight outside the
	// selection would silently shrink the total.
	var covered []string
	var weightSum float64
	for _, ym := range months {
		if w, ok := weights[ym]; ok && w > 0 {
			covered = append(covered, ym)
			weightSum += w
		}
	}
	if len(covered) == 0 || weightSum <= 0 {
		covered = months
		weightSum = 0 // fall through to the even split below
	}

	var running float64
	for i, ym := range covered {
		var amount float64
		if i == len(covered)-1 {
			amount = round2(total - running) // the remainder, so the sum is exact
		} else if weightSum > 0 {
			amount = round2(total * weights[ym] / weightSum)
		} else {
			amount = round2(total / float64(len(covered)))
		}
		out[ym] = amount
		running += amount
	}
	return out
}

// courseCalendarMonths lists every "YYYY-MM" the course spans, from its own
// dates or the term's when the course leaves them blank — the same range
// gradSpecialMonthShares walks, so the two agree on what "this term" means.
func courseCalendarMonths(ctx context.Context, pool *pgxpool.Pool, courseID uuid.UUID) ([]string, error) {
	var start, end time.Time
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(tc.starts_on, at.starts_on),
		       COALESCE(tc.ends_on,   at.ends_on)
		FROM teaching_courses tc
		JOIN academic_terms at ON at.id = tc.term_id
		WHERE tc.id = $1`, courseID).Scan(&start, &end); err != nil {
		return nil, err
	}
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return nil, nil
	}
	var out []string
	for m := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC); !m.After(end); m = m.AddDate(0, 1, 0) {
		out = append(out, m.Format("2006-01"))
	}
	return out, nil
}
