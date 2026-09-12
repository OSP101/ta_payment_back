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
	// sortName is the name WITHOUT its คำนำหน้า, used only to order the sheet.
	// Unexported, so it never reaches the reprint snapshot — a reprint writes
	// the rows back in the order they were already stored.
	sortName string
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
//
// Names carry the คำนำหน้า, as the office's own template writes them
// ("นายอภิภัทร คําพุทธ", "นางสาวณัฐนิชา ทะยานรัมย์") and as every other
// document in this package already does. The TA profile's own prefix wins over
// the account title: the profile is what the TA filled in for these documents,
// and the account title may still be whatever the registrar import left.
func (s *ExportService) taNamesByCourse(ctx context.Context, courseID uuid.UUID) (map[uuid.UUID]transferCoverName, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT u.id,
		       COALESCE(NULLIF(tp.prefix,''), NULLIF(u.title,''), '')||
		       u.first_name || ' ' || u.last_name,
		       u.first_name || ' ' || u.last_name
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		JOIN users u       ON u.id = a.ta_id
		LEFT JOIN ta_profiles tp ON tp.user_id = u.id
		JOIN sections sec  ON sec.id = a.section_id
		WHERE sec.teaching_course_id = $1 AND a.state <> 'dropped'`, courseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]transferCoverName{}
	for rows.Next() {
		var id uuid.UUID
		var n transferCoverName
		if err := rows.Scan(&id, &n.display, &n.sort); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// transferCoverName carries a TA's printed name and the key the list is
// ordered by. They differ on purpose: the คำนำหน้า is printed, but ordering on
// it would group the whole sheet into นางสาว then นาย before any given name is
// consulted. Thai name lists are ordered by ชื่อ.
type transferCoverName struct {
	display string
	sort    string
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
		  AND a.level::text IN ('master','phd')`, courseID)
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
	name     string
	sortName string
	baht     float64
	courses  map[string]bool
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
		_ = all
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
	get := func(cur, track string, ta uuid.UUID, n transferCoverName) *transferCoverAcc {
		k := transferCoverKey{cur, track, ta}
		a, ok := accum[k]
		if !ok {
			a = &transferCoverAcc{name: n.display, sortName: n.sort, courses: map[string]bool{}}
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
			a.baht += c.Baht * trackSettle.fundedShare(c.TA, c.Date, c.StartTime)
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
		// so slicing by month cannot filter it — each holder's lump is dated by
		// their own approved special-track hours (gradLumpByMonth) and this
		// document carries the selected months' slices, so the slice-sum equals
		// the undivided total and a TA's October document never reads 0.00 for
		// a month they worked.
		for _, taID := range gradTAs {
			byMonth, err := s.gradLumpByMonth(ctx, courseID, taID, gradLump, true)
			if err != nil {
				return nil, nil, err
			}
			a := get(cur, "special", taID, names[taID])
			a.courses[courseCode] = true
			a.baht += sumMonths(byMonth, months)
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
			TAID: k.ta, Name: a.name, sortName: a.sortName,
			Courses: strings.Join(codes, ", "), Baht: round2(a.baht), Seniority: seniorityTH,
		})
	}

	var sheets []transferCoverSheet
	for _, cur := range curricula {
		for _, track := range []string{"regular", "special"} {
			rows := grouped[sheetKey{cur.Code, track}]
			if len(rows) == 0 {
				continue
			}
			// Ordered by ชื่อ, not by คำนำหน้า — see transferCoverName.
			sort.Slice(rows, func(i, j int) bool {
				a, b := rows[i].sortName, rows[j].sortName
				if a == "" {
					a = rows[i].Name
				}
				if b == "" {
					b = rows[j].Name
				}
				return a < b
			})
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
	subtitleRuled                    int
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
	// Black, as the office's template draws them. The grey these used to be
	// read as a different document next to the real one — the claim book made
	// exactly the same mistake and was corrected the same way.
	//
	// Verticals thin, horizontals hair: the template rules the columns firmly
	// and separates the names lightly, so a long list reads as one block rather
	// than as forty boxes.
	const black = "000000"
	box := []excelize.Border{
		{Type: "left", Color: black, Style: 1}, {Type: "right", Color: black, Style: 1},
		{Type: "top", Color: black, Style: 1}, {Type: "bottom", Color: black, Style: 1},
	}
	rowRules := []excelize.Border{
		{Type: "left", Color: black, Style: 1}, {Type: "right", Color: black, Style: 1},
		{Type: "top", Color: black, Style: 7}, {Type: "bottom", Color: black, Style: 7},
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
	// The four heading lines are ALL centred and bold in the template, at 20
	// for the document's name and 18 for the three lines under it. They used to
	// print left-aligned at 15 and unbolded, which is most of why the file did
	// not read as the same document at a glance.
	st.title = mk(&excelize.Style{Font: font(20, true), Alignment: center})
	st.memo = mk(&excelize.Style{Font: font(18, true), Alignment: center})
	st.subtitle = mk(&excelize.Style{Font: font(18, true), Alignment: center})
	// The last heading line carries the rule that separates the heading block
	// from the table.
	st.subtitleRuled = mk(&excelize.Style{Font: font(18, true), Alignment: center,
		Border: []excelize.Border{{Type: "bottom", Color: black, Style: 1}}})
	st.colHeader = mk(&excelize.Style{Font: font(16, true), Alignment: center, Border: box})
	st.body = mk(&excelize.Style{Font: font(16, false), Alignment: left, Border: rowRules})
	st.bodyCenter = mk(&excelize.Style{Font: font(16, false), Alignment: center, Border: rowRules})
	st.money = mk(&excelize.Style{Font: font(16, false), Alignment: right, Border: rowRules, CustomNumFmt: fmtPtr(moneyFmt)})
	st.totalLabel = mk(&excelize.Style{Font: font(16, true), Alignment: center, Border: box})
	// The grand total is closed with a DOUBLE rule underneath, the accounting
	// convention the template follows.
	st.totalMoney = mk(&excelize.Style{Font: font(16, true), Alignment: right,
		Border: []excelize.Border{
			{Type: "left", Color: black, Style: 1}, {Type: "right", Color: black, Style: 1},
			{Type: "top", Color: black, Style: 1}, {Type: "bottom", Color: black, Style: 6},
		},
		CustomNumFmt: fmtPtr(moneyFmt)})
	st.sign = mk(&excelize.Style{Font: font(16, false), Alignment: center})
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

	// The sheet is FIVE columns wide, A–E, exactly as the office's template
	// (docs/ปะหน้าจ่ายตรง-CY.xls) draws it. It used to carry a sixth,
	// หมายเหตุ, holding the TA's ใหม่/เก่า seniority — a column the template
	// does not have. That reading is still on the preview screen, where it
	// costs the finance office nothing; it does not belong on the paper they
	// key from.
	const lastCol = "E"

	// The four heading lines, each merged across the full width and centred.
	for _, line := range []struct {
		row   int
		style int
		text  string
	}{
		{1, st.title, "แจ้งโอนจ่ายตรงเข้าบัญชีบุคลากร"},
		{2, st.memo, h.MemoLine},
		{3, st.subtitle, fmt.Sprintf("ค่าตอบแทนผู้ช่วยสอนและผู้ช่วยปฏิบัติงาน%s หลักสูตร %s (%s)",
			levelHeadingTH(sh.CurriculumLevel), sh.CurriculumLabel, sh.TrackTH)},
		{4, st.subtitleRuled, h.TermLine},
	} {
		first := fmt.Sprintf("A%d", line.row)
		last := fmt.Sprintf("%s%d", lastCol, line.row)
		_ = f.MergeCell(sheet, first, last)
		if err := set(first, line.style, line.text); err != nil {
			return err
		}
		// Merging keeps only the anchor's style, so without this the rule under
		// the last heading line would stop at column A.
		if err := f.SetCellStyle(sheet, first, last, line.style); err != nil {
			return err
		}
	}

	headers := []string{"ลำดับที่", "ชื่อ-สกุล", "รายวิชา", "จำนวนเงิน", "หมายเลขพร้อมเพย์"}
	cols := []string{"A", "B", "C", "D", "E"}
	for i, label := range headers {
		if err := set(cols[i]+"5", st.colHeader, label); err != nil {
			return err
		}
	}

	// Column styles for the body, in template order: only the name reads from
	// the left; the course codes are centred under their heading.
	bodyStyle := map[string]int{
		"A": st.bodyCenter, "B": st.body, "C": st.bodyCenter,
		"D": st.money, "E": st.bodyCenter,
	}
	row := 6
	for i, r := range sh.Rows {
		values := map[string]any{
			"A": i + 1, "B": r.Name, "C": r.Courses, "D": r.Baht, "E": r.PromptPay,
		}
		for _, col := range cols {
			if err := set(fmt.Sprintf("%s%d", col, row), bodyStyle[col], values[col]); err != nil {
				return err
			}
		}
		row++
	}
	lastDataRow := row - 1
	if lastDataRow < 6 {
		lastDataRow = 6
	}
	// The template closes the list with one empty RULED row before the total,
	// not with a gap: the table stays a single block down to its own total
	// line. (This used to be an unstyled spacer, which tore the grid open right
	// above the figure the office keys.)
	for _, col := range cols {
		cell := fmt.Sprintf("%s%d", col, row)
		if err := f.SetCellStyle(sheet, cell, cell, bodyStyle[col]); err != nil {
			return err
		}
	}
	row++

	totalRow := row
	_ = f.MergeCell(sheet, fmt.Sprintf("B%d", totalRow), fmt.Sprintf("C%d", totalRow))
	if err := set(fmt.Sprintf("A%d", totalRow), st.totalLabel, "รวม"); err != nil {
		return err
	}
	if err := set(fmt.Sprintf("B%d", totalRow), st.totalLabel, BahtText(sh.TotalBaht)); err != nil {
		return err
	}
	if err := f.SetCellStyle(sheet, fmt.Sprintf("B%d", totalRow),
		fmt.Sprintf("C%d", totalRow), st.totalLabel); err != nil {
		return err
	}
	sumFormula := fmt.Sprintf("SUM(D6:D%d)", lastDataRow)
	if err := f.SetCellFormula(sheet, fmt.Sprintf("D%d", totalRow), sumFormula); err != nil {
		return err
	}
	if err := f.SetCellStyle(sheet, fmt.Sprintf("D%d", totalRow), fmt.Sprintf("D%d", totalRow), st.totalMoney); err != nil {
		return err
	}
	if err := f.SetCellStyle(sheet, fmt.Sprintf("E%d", totalRow),
		fmt.Sprintf("E%d", totalRow), st.totalLabel); err != nil {
		return err
	}

	// The signature block sits under the last two columns, as the template
	// places it — not spilling into a sixth column that no longer exists.
	signRow := totalRow + 3
	for i, line := range []string{
		"ลงชื่อ ........................................................",
		signerLineOf(h.SignerName),
		"ผู้แจ้งโอน",
	} {
		r := signRow + i
		first := fmt.Sprintf("D%d", r)
		last := fmt.Sprintf("%s%d", lastCol, r)
		_ = f.MergeCell(sheet, first, last)
		if err := set(first, st.sign, line); err != nil {
			return err
		}
		if err := f.SetCellStyle(sheet, first, last, st.sign); err != nil {
			return err
		}
	}

	// The template's own column widths.
	widths := []struct {
		col string
		w   float64
	}{{"A", 7.9}, {"B", 23.4}, {"C", 23}, {"D", 17.6}, {"E", 27.7}}
	for _, w := range widths {
		_ = f.SetColWidth(sheet, w.col, w.col, w.w)
	}
	return nil
}

// signerLineOf brackets the ผู้แจ้งโอน name, or yields "" so the line prints
// blank for a wet signature rather than as an empty pair of brackets.
func signerLineOf(name string) string {
	if name == "" {
		return ""
	}
	return fmt.Sprintf("(%s)", name)
}

/* -------------------------------------------------------------------------- */
/* Header text + ledger                                                       */
/* -------------------------------------------------------------------------- */

// transferCoverHeaders resolves the header text for every sheet already
// computed: the term line and, per curriculum, whatever memo number/date/signer
// staff have configured via term_export_docs — blank where nothing has been set
// yet, never invented.
//
// months (Gregorian "YYYY-MM", empty = the whole term) is the selection this
// document was issued for, and the heading names EXACTLY those months.
//
// It used to print the term's own starts_on–ends_on span instead, which was
// wrong on every document that is not the whole term — and since งบแผ่นดิน
// closes 30 กันยายน, a ภาคต้น that teaches มิ.ย.–ต.ค. is ALWAYS issued in
// slices. A มิ.ย.–ก.ย. cover headed "(เดือนมิถุนายน - ตุลาคม 2569)" tells the
// finance office it covers a month whose money is not on the sheet, against an
// appropriation that no longer exists.
func (s *ExportService) transferCoverHeaders(
	ctx context.Context, termID uuid.UUID, months []string, sheets []transferCoverSheet,
) (map[string]transferCoverHeader, error) {
	var academicYear, semester int
	if err := s.pool.QueryRow(ctx, `
		SELECT academic_year, semester FROM academic_terms WHERE id = $1`,
		termID).Scan(&academicYear, &semester); err != nil {
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
	// An empty selection means the whole term, so name the term's own months —
	// the same list the picker offers, not a date span that could include a
	// month with no submission period behind it.
	if len(months) == 0 {
		all, err := s.TermMonths(ctx, termID)
		if err != nil {
			return nil, err
		}
		for _, m := range all {
			months = append(months, m.YearMonth)
		}
	}
	if label := thaiSelectedMonthsLabel(months); label != "" {
		termLine = fmt.Sprintf("%s  (เดือน%s)", termLine, label)
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

// thaiSelectedMonthsLabel names exactly the months a document covers, in the
// office's own phrasing:
//
//	one month        มิถุนายน 2569
//	a run            มิถุนายน - กันยายน 2569   (the year is written once)
//	a run of years   ธันวาคม 2568 - มกราคม 2569
//	gaps             มิถุนายน 2569, สิงหาคม 2569
//
// A gap is spelled out rather than collapsed to first–last: a cover headed
// "มิ.ย. - ส.ค." that silently omits July would have the finance office looking
// for a month of payments that is not on the sheet.
func thaiSelectedMonthsLabel(months []string) string {
	type ym struct{ y, m int }
	seen := map[string]bool{}
	keys := make([]string, 0, len(months))
	for _, k := range months {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	parsed := make([]ym, 0, len(keys))
	for _, k := range keys {
		var y, m int
		if _, err := fmt.Sscanf(k, "%d-%d", &y, &m); err != nil || m < 1 || m > 12 {
			continue
		}
		parsed = append(parsed, ym{y, m})
	}
	if len(parsed) == 0 {
		return ""
	}
	named := func(p ym) string { return fmt.Sprintf("%s %d", thaiMonths[p.m-1], p.y+543) }
	if len(parsed) == 1 {
		return named(parsed[0])
	}
	contiguous := true
	for i := 1; i < len(parsed); i++ {
		prev, cur := parsed[i-1], parsed[i]
		if prev.y*12+prev.m+1 != cur.y*12+cur.m {
			contiguous = false
			break
		}
	}
	first, last := parsed[0], parsed[len(parsed)-1]
	if contiguous {
		if first.y == last.y {
			return fmt.Sprintf("%s - %s", thaiMonths[first.m-1], named(last))
		}
		return fmt.Sprintf("%s - %s", named(first), named(last))
	}
	out := make([]string, 0, len(parsed))
	for _, p := range parsed {
		out = append(out, named(p))
	}
	return strings.Join(out, ", ")
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

	if err := assertOneFiscalYear(all, months); err != nil {
		return nil, nil, err
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

	headers, err := s.transferCoverHeaders(ctx, termID, months, sheets)
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
	// Ready is false while anything in the month is still waiting on a TA, a
	// lecturer, or the finance step. The document is keyed straight into the
	// university's ERP, so its figures have to be final — a month that is still
	// moving must not be selectable at all, rather than refused after the press.
	//
	// A POINTER, and omitted when nil, because this struct is shared with
	// CourseExportCoverage, which has no such gate. As a plain bool the zero
	// value shipped `"ready": false` from that endpoint too and the shared month
	// picker greyed out every month on the course export page — a readiness rule
	// that belongs to one document silently disabling another.
	Ready *bool `json:"ready,omitempty"`
}

// level ("undergrad" | "graduate") scopes coverage to one file: issuing the
// undergrad file must not make the graduate screen believe those same months
// are already covered, and vice versa — the two files are on independent
// schedules.
// assertOneFiscalYear refuses a month selection that draws on two budget years.
//
// ปะหน้าจ่ายตรง is not a report — the finance office keys its figures into the
// university's ERP to move the money, against ONE appropriation. A term that
// teaches across 30 September spends from two, so a single sheet covering both
// halves cannot be keyed at all: whichever year it is entered under, the other
// half's months are paid from the wrong budget.
//
// This is refused rather than warned about because the mistake is invisible on
// the sheet itself. The document shows names and amounts; nothing on it says
// which appropriation they belong to, so the officer keying it has no way to
// notice. The month lists in the message are what they need to reissue: one
// document per half.
//
// The finance_sent gate above is deliberately month-scoped and stays that way —
// the closing year's document must be issuable while October is still being
// approved, which is the whole reason a term crossing the boundary is split.
func assertOneFiscalYear(all []TermMonth, months []string) error {
	byYear := map[int][]string{}
	var years []int
	for _, ym := range months {
		fy, err := fiscalYearOf(ym)
		if err != nil {
			return err
		}
		if _, seen := byYear[fy]; !seen {
			years = append(years, fy)
		}
		byYear[fy] = append(byYear[fy], ym)
	}
	if len(years) < 2 {
		return nil
	}
	sort.Ints(years)
	parts := make([]string, 0, len(years))
	for _, fy := range years {
		parts = append(parts, fmt.Sprintf("ปีงบ %d (%s)", fy+543,
			strings.Join(monthLabelsOf(all, byYear[fy]), ", ")))
	}
	return Invalid(
		"เลือกเดือนข้ามปีงบประมาณในเอกสารฉบับเดียวไม่ได้ ต้องแยกออกเป็นคนละฉบับ: " +
			strings.Join(parts, " / "))
}

// monthLabelsOf turns Gregorian keys into the Thai labels staff see on screen,
// so a refusal names the months the way the picker does.
//
// Sorted on the Gregorian KEY, not the label: Thai month names sort
// alphabetically into กรกฎาคม, กันยายน, มิถุนายน, สิงหาคม — an order that reads
// as a jumble to anyone checking a list of months against a calendar.
func monthLabelsOf(all []TermMonth, months []string) []string {
	label := map[string]string{}
	for _, m := range all {
		label[m.YearMonth] = m.Label
	}
	keys := append([]string(nil), months...)
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, ym := range keys {
		if l, ok := label[ym]; ok {
			out = append(out, l)
		} else {
			out = append(out, ym)
		}
	}
	return out
}

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
	notReady, err := s.TermMonthsNotReady(ctx, termID, level)
	if err != nil {
		return nil, err
	}
	out := &TransferCoverCoverage{Split: split, Months: make([]TransferCoverMonthStatus, 0, len(all))}
	for _, m := range all {
		ready := !notReady[m.YearMonth]
		out.Months = append(out.Months, TransferCoverMonthStatus{
			TermMonth: m,
			Issued:    issued[m.YearMonth],
			Ready:     &ready,
		})
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
