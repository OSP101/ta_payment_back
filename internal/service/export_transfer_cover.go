// export_transfer_cover.go builds "แจ้งโอนจ่ายตรงเข้าบัญชีบุคลากร" (ปะหน้าจ่ายตรง)
// — the transfer-cover sheet staff currently assemble by hand once every
// course in a term has been sent to finance. One workbook, one sheet per
// (curriculum × track) that has anyone to pay
// (docs/ปะหน้าจ่ายตรง-CY.xls is the college's own example this was built
// against — see docs/PLAN-เอกสารสรุปงบและปะหน้าจ่ายตรง.md).
//
// Unlike ใบ A (an estimate, never blocked), this document reports what will
// actually be transferred, so it is gated on every course in the term having
// reached finance_sent (see TermExportBlockers) and every row's money comes
// from the SAME คาบ-cutoff settlement the printed claim already used — not
// the raw uncapped hourly total.
//
// Grouped by PERSON, not by course: one row per (TA × track), course codes
// joined ", ", money summed across every course that track's work touched —
// so a dual-registrar-code class contributes to the same row twice, additively,
// with no separate merging step needed (unlike ใบ A, which prints one block
// per class and must not double the student counts).
package service

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"

	"ta-payment-back/internal/audit"
)

/* -------------------------------------------------------------------------- */
/* Data gathering                                                             */
/* -------------------------------------------------------------------------- */

// transferCoverRow is one printed line: one TA on one (curriculum × track)
// sheet. PromptPay is deliberately excluded from JSON — see snapshot() below
// and internal/pii's own rule that decrypted PII is never written to storage,
// including this document's own reprint ledger.
type transferCoverRow struct {
	TAID      uuid.UUID `json:"ta_id"`
	Name      string    `json:"name"`
	Courses   string    `json:"courses"`
	Baht      float64   `json:"baht"`
	PromptPay string    `json:"-"`
	Seniority string    `json:"seniority"` // "ใหม่" | "เก่า"
}

type transferCoverSheet struct {
	CurriculumCode string `json:"curriculum_code"`
	// CurriculumLabel is the printed sheet identity (cur.SheetName, e.g.
	// "ITII" for the code="IT" programme after its rename) — distinct from
	// SheetName below, which additionally carries the track suffix and names
	// the actual Excel tab.
	CurriculumLabel string             `json:"curriculum_label"`
	CurriculumFull  string             `json:"curriculum_full"`
	CurriculumLevel string             `json:"curriculum_level"`
	SheetName       string             `json:"sheet_name"`
	Track           string             `json:"track"` // "regular" | "special"
	TrackTH         string             `json:"track_th"`
	Rows            []transferCoverRow `json:"rows"`
	TotalBaht       float64            `json:"total_baht"`
}

// transferCoverPrintCurricula resolves, for every course this term, which
// curriculum sheet its TAs' money should be attributed to — the course
// actually taught, never the TA's own curriculum, so a TA teaching across
// curricula appears on more than one sheet. Reuses the exact same course list
// and course_groups resolution ใบ A reads, so the two documents can never
// attribute the same course to different curricula.
func (s *ExportService) transferCoverPrintCurricula(ctx context.Context, termID uuid.UUID) (map[uuid.UUID]string, []string, error) {
	courses, err := s.courseSummaryCourses(ctx, termID)
	if err != nil {
		return nil, nil, err
	}
	groups, err := s.teaching.ListConfirmedCourseGroups(ctx, termID)
	if err != nil {
		return nil, nil, err
	}
	out := map[uuid.UUID]string{}
	var warnings []string
	for _, c := range courses {
		cur := printCurriculumCode(c.Curriculum, c.Level)
		if grp, ok := groups[c.ID]; ok && grp.CurriculumCode != "" {
			cur = grp.CurriculumCode
		}
		if cur == "" {
			warnings = append(warnings, fmt.Sprintf("%s: ยังไม่ทราบหลักสูตร ไม่ได้ลงชีตใด", c.Code))
			continue
		}
		out[c.ID] = cur
	}
	return out, warnings, nil
}

// taNamesByCourse maps every TA with a live approved assignment on courseID to
// their display name — the same roster claimCostByTASlot's rows draw from,
// gathered separately because taSlotCost carries only the id.
func (s *ExportService) taNamesByCourse(ctx context.Context, courseID uuid.UUID) (map[uuid.UUID]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT u.id, u.first_name || ' ' || u.last_name
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		JOIN users u       ON u.id = a.ta_id
		JOIN sections sec  ON sec.id = a.section_id
		WHERE sec.teaching_course_id = $1 AND a.state <> 'dropped'`, courseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// gradSpecialTAIDs returns every graduate TA on courseID's special-track
// section — the holders of the flat term lump (claimCostByTASlot prices
// grad-special hours at 0; the lump is added separately here, exactly as
// settle() commits it off the top of the special pool rather than cutting it
// by คาบ). Eligibility is just an approved assignment: grad-special TAs don't
// log work_logs at all any more (2026 meeting — the system computes their pay
// automatically from the regular track's class schedule instead), so there is
// nothing left to gate on there.
func (s *ExportService) gradSpecialTAIDs(ctx context.Context, courseID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT a.ta_id
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		JOIN sections sec  ON sec.id = a.section_id AND sec.track = 'special'
		JOIN users u       ON u.id = a.ta_id
		WHERE sec.teaching_course_id = $1
		  AND u.study_level::text IN ('master','phd')`, courseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

type transferCoverAcc struct {
	name    string
	baht    float64
	courses map[string]bool
}

type transferCoverKey struct {
	curriculum, track string
	ta                uuid.UUID
}

// buildTransferCoverSheets computes every row's money from the SAME คาบ
// cutoff SettleCourse already applied — never the raw uncapped total — so
// this document can never show more than what ใบ A's own "เบิกจ่ายจริง"
// column reports for the same course. PromptPay is left blank here; see
// fillPromptPay, called separately so a reprint can redo ONLY that step
// (and re-audit it) without recomputing money from scratch.
// months (Gregorian "YYYY-MM") restricts the document to one slice of the term
// — the fiscal-year split of 10/08/2026. It filters the ALREADY-SETTLED คาบ
// rather than re-settling: the budget stays one pool per course for the whole
// term, cut chronologically exactly as before, and a slice is only a view onto
// part of that one result. So every month slice of a term sums back to the
// undivided figure — no คาบ can be paid twice and none can fall between two
// documents. Empty means the whole term.
//
// level ("undergrad" | "graduate", 12/08/2026) is the OTHER split this
// document now has: TA level and month are independent axes, so a course can
// contribute rows to both files in the same run. The sheet a row lands ON
// (curriculum × track) is still keyed by the COURSE's own curriculum, never
// the TA's — a graduate TA helping an undergrad course prints on that
// course's own sheet, just inside the graduate FILE, exactly as staff asked:
// "หลักสูตรของรายวิชา" decides the sheet, level decides the file.
func (s *ExportService) buildTransferCoverSheets(ctx context.Context, termID uuid.UUID, months []string, level string) ([]transferCoverSheet, []string, error) {
	if level != "undergrad" && level != "graduate" {
		return nil, nil, fmt.Errorf("buildTransferCoverSheets: invalid level %q", level)
	}
	printCurricula, warnings, err := s.transferCoverPrintCurricula(ctx, termID)
	if err != nil {
		return nil, nil, err
	}
	inSlice := func(string) bool { return true }
	// uniformMonthShare is the fallback apportionment (equal weight per
	// calendar month) used only when a course has no regular-track schedule
	// to weight the grad-special lump by. 1 when unscoped.
	uniformMonthShare := 1.0
	var selected map[string]bool
	if len(months) > 0 {
		selected = map[string]bool{}
		for _, m := range months {
			selected[m] = true
		}
		inSlice = func(ym string) bool { return selected[ym] }
		all, err := s.TermMonths(ctx, termID)
		if err != nil {
			return nil, nil, err
		}
		if n := len(all); n > 0 {
			hit := 0
			for _, m := range all {
				if selected[m.YearMonth] {
					hit++
				}
			}
			uniformMonthShare = float64(hit) / float64(n)
		}
	}
	// gradLumpShare apportions the flat graduate-special term lump (no คาบ
	// behind it) across a month-scoped document, weighted by that COURSE's own
	// regular-track class-schedule hours per month (2026 meeting: grad-special
	// TAs no longer log anything themselves, so their monthly split is
	// estimated from the regular track's teaching pattern instead). Falls back
	// to an even per-calendar-month share if the course has no regular-track
	// schedule yet to weight by.
	gradLumpShare := func(courseID uuid.UUID) (float64, error) {
		if selected == nil {
			return 1.0, nil
		}
		weights, err := gradSpecialMonthShares(ctx, s.pool, courseID)
		if err != nil {
			return 0, err
		}
		if weights == nil {
			return uniformMonthShare, nil
		}
		var share float64
		for ym, w := range weights {
			if selected[ym] {
				share += w
			}
		}
		return share, nil
	}

	var pr PayRate
	if err := s.pool.QueryRow(ctx, `
		SELECT undergrad_regular, undergrad_special, graduate_regular_hourly,
		       graduate_special_lumpsum, grad_special_term_cap, term_months
		FROM pay_rates ORDER BY effective_from DESC LIMIT 1`).Scan(
		&pr.UndergradRegular, &pr.UndergradSpecial, &pr.GraduateRegularHourly,
		&pr.GraduateSpecialLumpsum, &pr.GradSpecialTermCap, &pr.TermMonths); err != nil {
		return nil, nil, err
	}

	accum := map[transferCoverKey]*transferCoverAcc{}
	get := func(cur, track string, ta uuid.UUID, name string) *transferCoverAcc {
		k := transferCoverKey{cur, track, ta}
		a, ok := accum[k]
		if !ok {
			a = &transferCoverAcc{name: name, courses: map[string]bool{}}
			accum[k] = a
		}
		return a
	}

	for courseID, cur := range printCurricula {
		var courseCode string
		if err := s.pool.QueryRow(ctx, `
			SELECT tc.code FROM teaching_courses tc WHERE tc.id = $1`, courseID).Scan(&courseCode); err != nil {
			return nil, nil, err
		}

		settlement, err := s.SettleCourse(ctx, courseID)
		if err != nil {
			return nil, nil, err
		}
		costs, err := s.claimCostByTASlot(ctx, courseID, pr, mergedSittingsCTE)
		if err != nil {
			return nil, nil, err
		}
		names, err := s.taNamesByCourse(ctx, courseID)
		if err != nil {
			return nil, nil, err
		}

		for _, c := range costs {
			if !inSlice(c.YearMonth) {
				continue
			}
			rowLevel := "undergrad"
			if c.gradLevel() {
				rowLevel = "graduate"
			}
			if rowLevel != level {
				continue
			}
			a := get(cur, c.Track, c.TA, names[c.TA])
			a.courses[courseCode] = true
			trackSettle := settlement.Regular
			if c.Track == "special" {
				trackSettle = settlement.Special
			}
			if !trackSettle.unpaidFrom(c.Date, c.StartTime) {
				a.baht += c.Baht
			}
		}

		// The เหมาจ่าย lump belongs to graduate TAs only — never printed on the
		// undergrad file.
		if level != "graduate" {
			continue
		}

		gradTAs, err := s.gradSpecialTAIDs(ctx, courseID)
		if err != nil {
			return nil, nil, err
		}
		// graduate_special_lumpsum IS the whole-term-per-course figure (2026
		// meeting correction) — no month multiplication.
		gradLump := pr.GraduateSpecialLumpsum
		if pr.GradSpecialTermCap > 0 && gradLump > pr.GradSpecialTermCap {
			gradLump = pr.GradSpecialTermCap
		}
		// The graduate-special lump is a flat TERM figure with no คาบ behind it,
		// so slicing by month cannot filter it — it is apportioned instead, by
		// this course's own regular-track class-schedule share of the selected
		// months. Pro-rating rather than assigning it whole to the first slice
		// keeps the slice-sum equal to the undivided total and stops a TA's
		// October document reading 0.00 for work done that month.
		share, err := gradLumpShare(courseID)
		if err != nil {
			return nil, nil, err
		}
		gradLump *= share
		for _, taID := range gradTAs {
			a := get(cur, "special", taID, names[taID])
			a.courses[courseCode] = true
			a.baht += gradLump
		}
	}

	curricula, err := s.teaching.ListCurricula(ctx)
	if err != nil {
		return nil, nil, err
	}

	type sheetKey struct{ cur, track string }
	grouped := map[sheetKey][]transferCoverRow{}
	for k, a := range accum {
		// A row that nets to zero (every คาบ it touched fell off the budget
		// cutoff) has nothing to transfer — a 0.00 line on a bank instruction
		// is not information, it is noise the finance office would query.
		if a.baht <= 0 {
			continue
		}
		codes := make([]string, 0, len(a.courses))
		for code := range a.courses {
			codes = append(codes, code)
		}
		sort.Strings(codes)
		seniority, err := s.users.TASeniority(ctx, k.ta, termID)
		if err != nil {
			return nil, nil, err
		}
		seniorityTH := "เก่า"
		if seniority == "new" {
			seniorityTH = "ใหม่"
		}
		grouped[sheetKey{k.curriculum, k.track}] = append(grouped[sheetKey{k.curriculum, k.track}], transferCoverRow{
			TAID: k.ta, Name: a.name, Courses: strings.Join(codes, ", "), Baht: round2(a.baht), Seniority: seniorityTH,
		})
	}

	var sheets []transferCoverSheet
	for _, cur := range curricula {
		for _, track := range []string{"regular", "special"} {
			rows := grouped[sheetKey{cur.Code, track}]
			if len(rows) == 0 {
				continue
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
			trackTH := "ปกติ"
			if track == "special" {
				trackTH = "พิเศษ"
			}
			var total float64
			for _, r := range rows {
				total += r.Baht
			}
			sheets = append(sheets, transferCoverSheet{
				CurriculumCode: cur.Code, CurriculumLabel: cur.SheetName,
				CurriculumFull: cur.FullNameTH, CurriculumLevel: cur.Level,
				SheetName: fmt.Sprintf("%s %s", cur.SheetName, trackTH),
				Track:     track, TrackTH: trackTH, Rows: rows, TotalBaht: round2(total),
			})
		}
	}
	return sheets, warnings, nil
}

// fillPromptPay decrypts each row's citizen ID fresh, via the one audited
// read path (RevealCitizenID) — called at both Build and Reprint, never
// cached, so every appearance of the plaintext number is its own trail entry
// and the number itself never sits in the reprint ledger.
func (s *ExportService) fillPromptPay(ctx context.Context, actor uuid.UUID, sheets []transferCoverSheet) []string {
	var warnings []string
	for si := range sheets {
		for ri := range sheets[si].Rows {
			r := &sheets[si].Rows[ri]
			plain, err := s.docs.RevealCitizenID(ctx, actor, r.TAID, "ปะหน้าจ่ายตรง")
			switch {
			case err == nil:
				r.PromptPay = plain
			case errors.Is(err, ErrNotFound):
				warnings = append(warnings, fmt.Sprintf("%s: ไม่มีเลขบัตรประชาชนในระบบ ช่องพร้อมเพย์จะว่าง", r.Name))
			default:
				warnings = append(warnings, fmt.Sprintf("%s: ถอดรหัสเลขบัตรประชาชนไม่สำเร็จ", r.Name))
			}
		}
	}
	return warnings
}

/* -------------------------------------------------------------------------- */
/* Workbook rendering                                                         */
/* -------------------------------------------------------------------------- */

type transferCoverStyles struct {
	title, memo, subtitle, colHeader int
	body, bodyCenter, money          int
	totalLabel, totalMoney           int
	sign                             int
}

func buildTransferCoverStyles(f *excelize.File) (*transferCoverStyles, error) {
	st := &transferCoverStyles{}
	font := func(size float64, bold bool) *excelize.Font {
		return &excelize.Font{Family: "TH Sarabun New", Size: size, Bold: bold}
	}
	center := &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true}
	left := &excelize.Alignment{Horizontal: "left", Vertical: "center", WrapText: true}
	right := &excelize.Alignment{Horizontal: "right", Vertical: "center"}
	thin := []excelize.Border{
		{Type: "left", Color: "999999", Style: 1}, {Type: "right", Color: "999999", Style: 1},
		{Type: "top", Color: "999999", Style: 1}, {Type: "bottom", Color: "999999", Style: 1},
	}
	const moneyFmt = `_-* #,##0.00_-;\-* #,##0.00_-;_-* "-"??_-;_-@_-`

	var err error
	mk := func(s *excelize.Style) int {
		if err != nil {
			return 0
		}
		var id int
		id, err = f.NewStyle(s)
		return id
	}
	st.title = mk(&excelize.Style{Font: font(18, true), Alignment: center})
	st.memo = mk(&excelize.Style{Font: font(15, false), Alignment: left})
	st.subtitle = mk(&excelize.Style{Font: font(15, false), Alignment: left})
	st.colHeader = mk(&excelize.Style{Font: font(15, true), Alignment: center, Border: thin})
	st.body = mk(&excelize.Style{Font: font(14, false), Alignment: left, Border: thin})
	st.bodyCenter = mk(&excelize.Style{Font: font(14, false), Alignment: center, Border: thin})
	st.money = mk(&excelize.Style{Font: font(14, false), Alignment: right, Border: thin, CustomNumFmt: fmtPtr(moneyFmt)})
	st.totalLabel = mk(&excelize.Style{Font: font(15, true), Alignment: center, Border: thin})
	st.totalMoney = mk(&excelize.Style{Font: font(15, true), Alignment: right, Border: thin, CustomNumFmt: fmtPtr(moneyFmt)})
	st.sign = mk(&excelize.Style{Font: font(14, false), Alignment: center})
	if err != nil {
		return nil, err
	}
	return st, nil
}

func levelHeadingTH(level string) string {
	if level == "graduate" {
		return "ระดับบัณฑิตศึกษา"
	}
	return "ระดับปริญญาตรี"
}

// writeTransferCoverWorkbook renders every sheet already computed (with
// PromptPay already filled by fillPromptPay) into one xlsx. termLine and
// yearLine carry the header text so Build and Reprint always print the exact
// wording that was true when the document was generated, never today's.
func writeTransferCoverWorkbook(
	sheets []transferCoverSheet, headerBySheet map[string]transferCoverHeader,
) ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()
	st, err := buildTransferCoverStyles(f)
	if err != nil {
		return nil, err
	}

	wrote := false
	for _, sh := range sheets {
		name := sh.SheetName
		if !wrote {
			_ = f.SetSheetName("Sheet1", name)
		} else if _, err := f.NewSheet(name); err != nil {
			return nil, err
		}
		wrote = true
		h := headerBySheet[name]
		if err := writeTransferCoverSheet(f, st, sh, h); err != nil {
			return nil, err
		}
	}
	if !wrote {
		_ = f.SetSheetName("Sheet1", "ปะหน้าจ่ายตรง")
		_ = f.SetCellValue("ปะหน้าจ่ายตรง", "A1", "ไม่มีรายการที่ต้องโอนสำหรับภาคเรียนนี้")
	}
	f.SetActiveSheet(0)

	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// transferCoverHeader is the per-sheet header text, resolved once at Build
// time (term line) and frozen into the ledger snapshot so a reprint shows the
// exact wording that was true when it was generated.
type transferCoverHeader struct {
	MemoLine   string `json:"memo_line"`   // static blank template line — see transferCoverBlankMemoLine
	TermLine   string `json:"term_line"`   // "ภาคต้น ปีการศึกษา 2568  (เดือน... - ... 2569)"
	SignerName string `json:"signer_name"` // always blank — see transferCoverBlankMemoLine's own comment
}

// transferCoverBlankMemoLine matches the office's own template
// (docs/ปะหน้าจ่ายตรง-CY.xls) exactly: the memo number, its date, and the
// ผู้แจ้งโอน signature are filled in by hand after printing, never by the
// system. There is deliberately no per-term configuration for these — staff
// asked for the generated file to match the template as-is, not to gain a new
// settings screen.
const transferCoverBlankMemoLine = "เลขที่ ..................................          ลงวันที่ .................................."

func writeTransferCoverSheet(f *excelize.File, st *transferCoverStyles, sh transferCoverSheet, h transferCoverHeader) error {
	sheet := sh.SheetName
	set := func(cell string, style int, v any) error {
		if err := f.SetCellValue(sheet, cell, v); err != nil {
			return err
		}
		return f.SetCellStyle(sheet, cell, cell, style)
	}

	_ = f.MergeCell(sheet, "A1", "F1")
	if err := set("A1", st.title, "แจ้งโอนจ่ายตรงเข้าบัญชีบุคลากร"); err != nil {
		return err
	}
	_ = f.MergeCell(sheet, "A2", "F2")
	if err := set("A2", st.memo, h.MemoLine); err != nil {
		return err
	}
	_ = f.MergeCell(sheet, "A3", "F3")
	subject := fmt.Sprintf("ค่าตอบแทนผู้ช่วยสอนและผู้ช่วยปฏิบัติงาน%s หลักสูตร %s (%s)",
		levelHeadingTH(sh.CurriculumLevel), sh.CurriculumLabel, sh.TrackTH)
	if err := set("A3", st.subtitle, subject); err != nil {
		return err
	}
	_ = f.MergeCell(sheet, "A4", "F4")
	if err := set("A4", st.subtitle, h.TermLine); err != nil {
		return err
	}

	headers := []string{"ลำดับที่", "ชื่อ-สกุล", "รายวิชา", "จำนวนเงิน", "หมายเลขพร้อมเพย์", "หมายเหตุ"}
	cols := []string{"A", "B", "C", "D", "E", "F"}
	for i, label := range headers {
		if err := set(cols[i]+"5", st.colHeader, label); err != nil {
			return err
		}
	}

	row := 6
	for i, r := range sh.Rows {
		if err := set(fmt.Sprintf("A%d", row), st.bodyCenter, i+1); err != nil {
			return err
		}
		if err := set(fmt.Sprintf("B%d", row), st.body, r.Name); err != nil {
			return err
		}
		if err := set(fmt.Sprintf("C%d", row), st.body, r.Courses); err != nil {
			return err
		}
		if err := set(fmt.Sprintf("D%d", row), st.money, r.Baht); err != nil {
			return err
		}
		if err := set(fmt.Sprintf("E%d", row), st.bodyCenter, r.PromptPay); err != nil {
			return err
		}
		if err := set(fmt.Sprintf("F%d", row), st.bodyCenter, r.Seniority); err != nil {
			return err
		}
		row++
	}
	lastDataRow := row - 1
	if lastDataRow < 6 {
		lastDataRow = 6
	}
	row++ // one blank spacer row, matching the office's own template

	totalRow := row
	_ = f.MergeCell(sheet, fmt.Sprintf("B%d", totalRow), fmt.Sprintf("C%d", totalRow))
	if err := set(fmt.Sprintf("A%d", totalRow), st.totalLabel, "รวม"); err != nil {
		return err
	}
	if err := set(fmt.Sprintf("B%d", totalRow), st.totalLabel, BahtText(sh.TotalBaht)); err != nil {
		return err
	}
	sumFormula := fmt.Sprintf("SUM(D6:D%d)", lastDataRow)
	if err := f.SetCellFormula(sheet, fmt.Sprintf("D%d", totalRow), sumFormula); err != nil {
		return err
	}
	if err := f.SetCellStyle(sheet, fmt.Sprintf("D%d", totalRow), fmt.Sprintf("D%d", totalRow), st.totalMoney); err != nil {
		return err
	}

	signRow := totalRow + 3
	_ = f.MergeCell(sheet, fmt.Sprintf("D%d", signRow), fmt.Sprintf("F%d", signRow))
	_ = f.MergeCell(sheet, fmt.Sprintf("D%d", signRow+1), fmt.Sprintf("F%d", signRow+1))
	_ = f.MergeCell(sheet, fmt.Sprintf("D%d", signRow+2), fmt.Sprintf("F%d", signRow+2))
	if err := set(fmt.Sprintf("D%d", signRow), st.sign, "ลงชื่อ ........................................................"); err != nil {
		return err
	}
	signerLine := ""
	if h.SignerName != "" {
		signerLine = fmt.Sprintf("(%s)", h.SignerName)
	}
	if err := set(fmt.Sprintf("D%d", signRow+1), st.sign, signerLine); err != nil {
		return err
	}
	if err := set(fmt.Sprintf("D%d", signRow+2), st.sign, "ผู้แจ้งโอน"); err != nil {
		return err
	}

	widths := []struct {
		col string
		w   float64
	}{{"A", 8}, {"B", 28}, {"C", 26}, {"D", 15}, {"E", 18}, {"F", 12}}
	for _, w := range widths {
		_ = f.SetColWidth(sheet, w.col, w.col, w.w)
	}
	return nil
}

/* -------------------------------------------------------------------------- */
/* Header text + ledger                                                       */
/* -------------------------------------------------------------------------- */

// transferCoverHeaders resolves the header text for every sheet already
// computed: the term line (from the term's own dates, so it reads correctly
// even if generated well after the term closed) and, per curriculum, whatever
// memo number/date/signer staff have configured via term_export_docs — blank
// where nothing has been set yet, never invented.
func (s *ExportService) transferCoverHeaders(
	ctx context.Context, termID uuid.UUID, sheets []transferCoverSheet,
) (map[string]transferCoverHeader, error) {
	var academicYear, semester int
	var startsOn, endsOn *time.Time
	if err := s.pool.QueryRow(ctx, `
		SELECT academic_year, semester, starts_on, ends_on FROM academic_terms WHERE id = $1`,
		termID).Scan(&academicYear, &semester, &startsOn, &endsOn); err != nil {
		return nil, err
	}
	semLabel := "ภาคฤดูร้อน"
	switch semester {
	case 1:
		semLabel = "ภาคต้น"
	case 2:
		semLabel = "ภาคปลาย"
	}
	termLine := fmt.Sprintf("%s ปีการศึกษา %d", semLabel, academicYear)
	if startsOn != nil && endsOn != nil {
		startM := thaiMonths[int(startsOn.Month())-1]
		endM := thaiMonths[int(endsOn.Month())-1]
		endYearBE := endsOn.Year() + 543
		termLine = fmt.Sprintf("%s  (เดือน%s - %s %d)", termLine, startM, endM, endYearBE)
	}

	out := map[string]transferCoverHeader{}
	for _, sh := range sheets {
		out[sh.SheetName] = transferCoverHeader{
			MemoLine:   transferCoverBlankMemoLine,
			TermLine:   termLine,
			SignerName: "",
		}
	}
	return out, nil
}

// transferCoverSnapshot is what's frozen into the ledger row: every sheet's
// rows and resolved header text. PromptPay is excluded at the type level
// (transferCoverRow.PromptPay has `json:"-"`), so it structurally cannot end
// up here even if a caller forgets to strip it.
type transferCoverSnapshot struct {
	Sheets  []transferCoverSheet           `json:"sheets"`
	Headers map[string]transferCoverHeader `json:"headers"`
}

// BuildTransferCoverWorkbook renders ปะหน้าจ่ายตรง for termID. Refuses outright
// if any course in the term has not reached finance_sent (TermExportBlockers)
// — this document IS the finance notice, so there is no partial-file path the
// way ใบ A has one. Records a ledger row before returning bytes so an
// unrecorded generation can never happen; see ReprintTransferCover for how it
// is read back.
// months (Gregorian "YYYY-MM", empty = whole term) issues one fiscal slice of
// the term; both the gate and the money are narrowed to it. level
// ("undergrad" | "graduate", 12/08/2026) issues one of the two separate files
// staff now need — see buildTransferCoverSheets.
func (s *ExportService) BuildTransferCoverWorkbook(ctx context.Context, actor, termID uuid.UUID, months []string, level string) ([]byte, []string, error) {
	if level != "undergrad" && level != "graduate" {
		return nil, nil, Invalid("level ต้องเป็น undergrad หรือ graduate")
	}
	all, err := s.TermMonths(ctx, termID)
	if err != nil {
		return nil, nil, err
	}
	months, err = normalizeMonthSelection(all, months)
	if err != nil {
		return nil, nil, Invalid(err.Error())
	}

	blockers, err := s.TermExportBlockers(ctx, termID, months, level)
	if err != nil {
		return nil, nil, err
	}
	if len(blockers) > 0 {
		return nil, nil, exportBlockedError(blockers)
	}

	sheets, warnings, err := s.buildTransferCoverSheets(ctx, termID, months, level)
	if err != nil {
		return nil, nil, err
	}
	if level == "graduate" {
		// TA เหมาจ่าย (track "special") ไม่มี work_logs ให้ตรวจเลย จึงไม่มีขั้นตอน
		// ใดมาบล็อกยอดของพวกเขาได้ตั้งแต่ต้นเทอม — เตือนไว้เฉย ๆ ไม่บล็อก เพราะนี่
		// คือพฤติกรรมที่ถูกต้องสำหรับ TA เหมาจ่าย แต่ยังคุ้มที่จะเตือนเจ้าหน้าที่ให้
		// ตรวจยอดเองก่อนส่ง. ไม่แตะ track "regular" เพราะกลุ่มนั้นผ่านการตรวจสอบ
		// worklog ตามปกติอยู่แล้ว.
		for _, sh := range sheets {
			if sh.Track == "special" && len(sh.Rows) > 0 {
				warnings = append(warnings, "ไฟล์บัณฑิตศึกษามี TA เหมาจ่ายที่ไม่มีขั้นตอนตรวจสอบก่อนส่งออก (ไม่ต้องลงเวลา) กรุณาตรวจยอดเงินก่อนส่งการเงิน")
				break
			}
		}
	}
	warnings = append(warnings, s.fillPromptPay(ctx, actor, sheets)...)

	headers, err := s.transferCoverHeaders(ctx, termID, sheets)
	if err != nil {
		return nil, nil, err
	}

	body, err := writeTransferCoverWorkbook(sheets, headers)
	if err != nil {
		return nil, nil, err
	}

	var totalBaht float64
	for _, sh := range sheets {
		totalBaht += sh.TotalBaht
	}
	if err := s.recordTransferCoverExport(ctx, actor, termID, months, level, sheets, headers, totalBaht); err != nil {
		return nil, nil, err
	}
	// The file carries decrypted citizen ID numbers — audit who pulled it,
	// same reasoning BuildCourseZip's own PII trail follows. Must not be
	// best-effort: a disclosure with no trail is worse than a retry.
	if err := s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "export.transfer_cover", Entity: "academic_term", EntityID: termID.String(),
		After: map[string]any{"sheet_count": len(sheets), "total_baht": round2(totalBaht), "months": months, "level": level},
	}); err != nil {
		return nil, nil, err
	}
	return body, warnings, nil
}

// recordTransferCoverExport persists the ledger row BEFORE bytes are handed
// back — an unrecorded generation cannot be reprinted, distinguished from a
// staff member who simply never downloaded it.
func (s *ExportService) recordTransferCoverExport(
	ctx context.Context, actor, termID uuid.UUID, months []string, level string,
	sheets []transferCoverSheet, headers map[string]transferCoverHeader, totalBaht float64,
) error {
	raw, err := json.Marshal(transferCoverSnapshot{Sheets: sheets, Headers: headers})
	if err != nil {
		return err
	}
	var by *uuid.UUID
	if actor != uuid.Nil {
		by = &actor
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO transfer_cover_exports (id, term_id, generated_by, total_baht, sheet_count, document, months, level)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7)`,
		termID, by, round2(totalBaht), len(sheets), raw, months, level)
	return err
}

// ReprintTransferCover hands back a copy of a generation already on the
// ledger. Renders from the frozen snapshot rather than re-querying courses —
// a work log corrected after the fact must not silently change a document
// finance may already have acted on. PromptPay is the one field re-derived
// live: it is never stored (see transferCoverRow.PromptPay), so every reprint
// re-decrypts it fresh through the same audited path Build used.
func (s *ExportService) ReprintTransferCover(ctx context.Context, actor, exportID uuid.UUID) ([]byte, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT document FROM transfer_cover_exports WHERE id = $1`, exportID).Scan(&raw); err != nil {
		return nil, ErrNotFound
	}
	var snap transferCoverSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("transfer cover export %s: snapshot unreadable: %w", exportID, err)
	}
	_ = s.fillPromptPay(ctx, actor, snap.Sheets)
	body, err := writeTransferCoverWorkbook(snap.Sheets, snap.Headers)
	if err != nil {
		return nil, err
	}
	if err := s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "export.transfer_cover.reprint",
		Entity: "transfer_cover_export", EntityID: exportID.String(),
	}); err != nil {
		return nil, err
	}
	return body, nil
}

// TransferCoverExportSummary is one row of generation history — who, when,
// how much, and its id for reprinting.
type TransferCoverExportSummary struct {
	ID          uuid.UUID `json:"id"`
	TermID      uuid.UUID `json:"term_id"`
	GeneratedAt string    `json:"generated_at"`
	GeneratedBy string    `json:"generated_by,omitempty"`
	TotalBaht   float64   `json:"total_baht"`
	SheetCount  int       `json:"sheet_count"`
	// Months is the fiscal slice this file covered, Gregorian "YYYY-MM".
	// Empty for rows generated before the split existed — those covered the
	// whole term, and the screen says so rather than showing a blank range.
	Months []string `json:"months,omitempty"`
	// Level is which file this generation was: "undergrad" | "graduate" | ""
	// for rows predating the level split (12/08/2026), which covered both in
	// one file — the screen says so rather than mislabeling it either way.
	Level string `json:"level,omitempty"`
}

// ListTransferCoverExports returns the generation history for a term, newest
// first, scoped to one file's history. level="" (predating the split, kept
// for the CourseExportBlockers-style low-level callers and tests) lists every
// generation regardless of level.
//
// A pre-split row (level NULL — it covered BOTH files at once, back when
// there was only one) is included in EVERY level's history, the same way a
// pre-split months=NULL row counted as covering every month: it genuinely
// did cover this level, it just wasn't recorded as a level-scoped file yet.
func (s *ExportService) ListTransferCoverExports(ctx context.Context, termID uuid.UUID, level string) ([]TransferCoverExportSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT e.id, e.term_id, TO_CHAR(e.generated_at,'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'),
		       COALESCE(u.first_name || ' ' || u.last_name, ''), e.total_baht, e.sheet_count,
		       COALESCE(e.months, '{}'), COALESCE(e.level, '')
		FROM transfer_cover_exports e
		LEFT JOIN users u ON u.id = e.generated_by
		WHERE e.term_id = $1
		  AND ($2 = '' OR e.level IS NULL OR e.level = $2)
		ORDER BY e.generated_at DESC`, termID, level)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TransferCoverExportSummary{}
	for rows.Next() {
		var r TransferCoverExportSummary
		if err := rows.Scan(&r.ID, &r.TermID, &r.GeneratedAt, &r.GeneratedBy, &r.TotalBaht, &r.SheetCount, &r.Months, &r.Level); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TransferCoverCoverage answers "which months of this term have already been
// issued, and which are still outstanding" — the question that decides whether
// staff are about to double-issue October or forget it entirely. Free month
// selection makes both mistakes possible, so the screen shows this before the
// picker rather than leaving it to memory.
type TransferCoverCoverage struct {
	Months []TransferCoverMonthStatus `json:"months"`
	Split  FiscalSplit                `json:"fiscal_split"`
}

type TransferCoverMonthStatus struct {
	TermMonth
	// Issued is true once any generation covered this month. Rows predating
	// the months column covered the whole term, so they mark every month.
	Issued bool `json:"issued"`
}

// level ("undergrad" | "graduate") scopes coverage to one file: issuing the
// undergrad file must not make the graduate screen believe those same months
// are already covered, and vice versa — the two files are on independent
// schedules.
func (s *ExportService) TransferCoverCoverage(ctx context.Context, termID uuid.UUID, level string) (*TransferCoverCoverage, error) {
	if level != "undergrad" && level != "graduate" {
		return nil, Invalid("level ต้องเป็น undergrad หรือ graduate")
	}
	all, err := s.TermMonths(ctx, termID)
	if err != nil {
		return nil, err
	}
	history, err := s.ListTransferCoverExports(ctx, termID, level)
	if err != nil {
		return nil, err
	}
	issued := map[string]bool{}
	for _, h := range history {
		if len(h.Months) == 0 {
			// Pre-split generation: it covered everything.
			for _, m := range all {
				issued[m.YearMonth] = true
			}
			continue
		}
		for _, m := range h.Months {
			issued[m] = true
		}
	}
	split, err := fiscalSplit(all)
	if err != nil {
		return nil, err
	}
	out := &TransferCoverCoverage{Split: split, Months: make([]TransferCoverMonthStatus, 0, len(all))}
	for _, m := range all {
		out.Months = append(out.Months, TransferCoverMonthStatus{TermMonth: m, Issued: issued[m.YearMonth]})
	}
	return out, nil
}

/* -------------------------------------------------------------------------- */
/* On-screen preview                                                          */
/* -------------------------------------------------------------------------- */

// TransferCoverPreviewRow is one printed line for the on-screen table.
// PromptPay is deliberately never included here — a preview endpoint gets
// polled/revisited far more casually than a download, and RevealCitizenID's
// own audit trail is meant for genuine document generation, not a page a
// staff member might load a dozen times while checking progress.
type TransferCoverPreviewRow struct {
	Name      string  `json:"name"`
	Courses   string  `json:"courses"`
	Baht      float64 `json:"baht"`
	Seniority string  `json:"seniority"`
}

type TransferCoverPreviewSheet struct {
	SheetName string                    `json:"sheet_name"`
	Track     string                    `json:"track"`
	TrackTH   string                    `json:"track_th"`
	Rows      []TransferCoverPreviewRow `json:"rows"`
	TotalBaht float64                   `json:"total_baht"`
}

// TransferCoverPreview returns ปะหน้าจ่ายตรง as data instead of a workbook.
// Deliberately NOT gated on TermExportBlockers — the money here is already
// settled/actual (SettleCourse's own คาบ cutoff), so it is accurate at any
// point in the pipeline; the gate exists to stop the FILE (an auditable
// financial instrument someone might act on) from leaving the server early,
// not to hide the underlying numbers from staff checking progress.
func (s *ExportService) TransferCoverPreview(ctx context.Context, termID uuid.UUID, months []string, level string) ([]TransferCoverPreviewSheet, []string, error) {
	if level != "undergrad" && level != "graduate" {
		return nil, nil, Invalid("level ต้องเป็น undergrad หรือ graduate")
	}
	all, err := s.TermMonths(ctx, termID)
	if err != nil {
		return nil, nil, err
	}
	months, err = normalizeMonthSelection(all, months)
	if err != nil {
		return nil, nil, Invalid(err.Error())
	}
	sheets, warnings, err := s.buildTransferCoverSheets(ctx, termID, months, level)
	if err != nil {
		return nil, nil, err
	}
	out := make([]TransferCoverPreviewSheet, 0, len(sheets))
	for _, sh := range sheets {
		rows := make([]TransferCoverPreviewRow, 0, len(sh.Rows))
		for _, r := range sh.Rows {
			rows = append(rows, TransferCoverPreviewRow{
				Name: r.Name, Courses: r.Courses, Baht: r.Baht, Seniority: r.Seniority,
			})
		}
		out = append(out, TransferCoverPreviewSheet{
			SheetName: sh.SheetName, Track: sh.Track, TrackTH: sh.TrackTH,
			Rows: rows, TotalBaht: sh.TotalBaht,
		})
	}
	return out, warnings, nil
}

/* -------------------------------------------------------------------------- */
/* Combined download (12/08/2026)                                             */
/* -------------------------------------------------------------------------- */

// BuildTransferCoverBundle issues ปะหน้าจ่ายตรง as ONE zip download containing
// whichever of the two level files (ป.ตรี, บัณฑิตศึกษา) is ready. Staff asked
// for a single button instead of two separate downloads crowding the header —
// the two documents are still built, gated, and ledgered completely
// independently, exactly as BuildTransferCoverWorkbook always has; only the
// DOWNLOAD ACTION is merged.
//
// A level that is not yet ready (TermExportBlockers) is SKIPPED with a warning
// rather than blocking the other level's file — collapsing the two gates back
// into one would recreate the exact problem the level split fixed: a graduate
// course still mid-review holding the undergrad document (or the reverse)
// hostage. The zip only fails outright if BOTH levels are blocked, since there
// would then be nothing to hand back.
func (s *ExportService) BuildTransferCoverBundle(ctx context.Context, actor, termID uuid.UUID, months []string) ([]byte, []string, error) {
	type builtFile struct {
		level string
		body  []byte
	}
	var files []builtFile
	var warnings []string
	var blockedMsgs []string
	for _, level := range []string{"undergrad", "graduate"} {
		body, warn, err := s.BuildTransferCoverWorkbook(ctx, actor, termID, months, level)
		if err != nil {
			var ue *UserError
			if errors.As(err, &ue) {
				// Expected: this level just isn't ready yet. Note it and move on
				// to the other level rather than failing the whole bundle.
				blockedMsgs = append(blockedMsgs, fmt.Sprintf("ไฟล์%s: %s", transferCoverLevelLabelTH(level), ue.Msg))
				continue
			}
			return nil, nil, err
		}
		warnings = append(warnings, warn...)
		files = append(files, builtFile{level: level, body: body})
	}
	if len(files) == 0 {
		return nil, nil, Invalid("ยังสร้างไฟล์ไม่ได้ทั้งสองระดับ:\n• " + strings.Join(blockedMsgs, "\n• "))
	}
	warnings = append(warnings, blockedMsgs...)

	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	for _, f := range files {
		w, err := zw.Create(fmt.Sprintf("ปะหน้าจ่ายตรง-%s.xlsx", transferCoverLevelLabelTH(f.level)))
		if err != nil {
			return nil, nil, err
		}
		if _, err := w.Write(f.body); err != nil {
			return nil, nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), warnings, nil
}

// transferCoverLevelLabelTH names a level for a bundle entry's filename/
// warning — short forms ("ปตรี"/"บัณฑิต") distinct from levelLabelTH's own
// full academic-degree labels ("ปริญญาตรี"/"ปริญญาโท"/"ปริญญาเอก") used
// elsewhere, since a zip entry name reads better short.
func transferCoverLevelLabelTH(level string) string {
	if level == "graduate" {
		return "บัณฑิต"
	}
	return "ปตรี"
}
