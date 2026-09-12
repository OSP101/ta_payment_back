// export_course_summary.go builds "สรุปรายวิชาที่ขอใช้ TA" — the budget-request
// workbook staff currently assemble by hand at the start of every term, after
// student counts are in and lecturers' TA requests are approved.
//
// One workbook, one sheet per curriculum (docs/ค่าตอบแทนTAภาคต้น-2569.xlsx is
// the college's own example this was built against — see the plan doc at
// docs/PLAN-เอกสารสรุปงบและปะหน้าจ่ายตรง.md). Each sheet lists every course in
// that curriculum as a block: one header row per course (code, name, credits,
// lecturer, student counts, budget figures) followed by one row per approved
// TA (student id, name, level). A course that is the same class under two
// registrar codes (a confirmed course_groups row — see course_group.go) prints
// as ONE block: codes joined with "/", money from every member ADDED, never
// recomputed from combined student counts.
//
// Built from code rather than a template, unlike the claim-form exports:
// the number of course/TA rows is not known ahead of time.
package service

import (
	"bytes"
	"context"
	"encoding/json"
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

// courseSummaryCourse is the raw per-teaching_course data the sheet needs,
// before dual-code grouping folds several of these into one printed block.
type courseSummaryCourse struct {
	ID                 uuid.UUID
	Code               string
	NameTH             string
	Credits            int
	LectureHrs         int
	LabHrs             int
	SelfHrs            int
	NumStudentsRegular int
	NumStudentsSpecial int
	Lecturer           string
	// Curriculum is derived from sections.curriculum (see migration 0068/0073)
	// — empty when not yet known, which prints under no sheet at all rather
	// than being guessed into one.
	Curriculum string
	// Level is teaching_courses.level ("undergrad" | "graduate") — combined
	// with Curriculum via printCurriculumCode to pick the FINAL curricula.code
	// a course prints under. One Curriculum value routes to two different rows
	// depending on Level (migration 0073's own comment): a graduate course
	// tagged "CS" or "IT" prints under CS_GRAD (CS&IT), not the undergrad CS/IT
	// sheet.
	Level string
}

type courseSummaryTA struct {
	UserID    uuid.UUID
	StudentID string
	Name      string
	LevelTH   string
}

func studyLevelTH(level string) string {
	switch level {
	case "master":
		return "ป.โท"
	case "phd":
		return "ป.เอก"
	default:
		return "ป.ตรี"
	}
}

// creditText renders "3 (2-2-5)" the way the registrar does.
func creditText(credits, lecture, lab, self int) string {
	return fmt.Sprintf("%d (%d-%d-%d)", credits, lecture, lab, self)
}

// claimKindLabel says what a course's approved TAs actually claim hours for —
// "(Lec.+Lab)", "(Lec.)", "(Lab.)", or blank when nobody has declared either
// (e.g. every hour so far is ตรวจงาน/อื่นๆ, not in-class time).
func claimKindLabel(attendanceHrs, labHrs float64) string {
	switch {
	case attendanceHrs > 0 && labHrs > 0:
		return "(Lec.+Lab)"
	case labHrs > 0:
		return "(Lab.)"
	case attendanceHrs > 0:
		return "(Lec.)"
	default:
		return ""
	}
}

func (s *ExportService) courseSummaryCourses(ctx context.Context, termID uuid.UUID) ([]courseSummaryCourse, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tc.id, tc.code, tc.name_th, tc.credits, tc.lecture_hrs, tc.lab_hrs, tc.self_hrs,
		       tc.num_students_regular, tc.num_students_special, tc.level,
		       COALESCE((
		           SELECT COALESCE(NULLIF(u.title,''),'') || u.first_name || ' ' || u.last_name
		           FROM teaching_lecturers tl JOIN users u ON u.id = tl.lecturer_id
		           WHERE tl.teaching_course_id = tc.id
		           ORDER BY tl.is_primary DESC LIMIT 1), '') AS lecturer,
		       COALESCE((
		           SELECT s.curriculum FROM sections s
		           WHERE s.teaching_course_id = tc.id AND s.curriculum IS NOT NULL
		           ORDER BY s.sec_no LIMIT 1), '') AS curriculum
		FROM teaching_courses tc
		WHERE tc.term_id = $1
		ORDER BY tc.code`, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []courseSummaryCourse
	for rows.Next() {
		var c courseSummaryCourse
		if err := rows.Scan(&c.ID, &c.Code, &c.NameTH, &c.Credits, &c.LectureHrs, &c.LabHrs, &c.SelfHrs,
			&c.NumStudentsRegular, &c.NumStudentsSpecial, &c.Level, &c.Lecturer, &c.Curriculum); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// courseSummaryTAs returns, per teaching_course_id, every DISTINCT approved TA
// (a TA assisting both a regular and a special section of the same course
// prints once, not twice) ordered by name.
func (s *ExportService) courseSummaryTAs(ctx context.Context, termID uuid.UUID) (map[uuid.UUID][]courseSummaryTA, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (sec.teaching_course_id, u.id)
		       sec.teaching_course_id, u.id, COALESCE(a.student_id_snapshot, u.student_id, ''),
		       u.first_name || ' ' || u.last_name, a.level::text
		FROM ta_request_assignments a
		JOIN ta_requests r  ON r.id = a.request_id AND r.status = 'approved'
		JOIN users u        ON u.id = a.ta_id
		JOIN sections sec   ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		WHERE tc.term_id = $1 AND a.state <> 'dropped'
		ORDER BY sec.teaching_course_id, u.id, u.first_name`, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]courseSummaryTA{}
	type row struct {
		tcID  uuid.UUID
		ta    courseSummaryTA
		level string
	}
	var collected []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.tcID, &r.ta.UserID, &r.ta.StudentID, &r.ta.Name, &r.level); err != nil {
			return nil, err
		}
		r.ta.LevelTH = studyLevelTH(r.level)
		collected = append(collected, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Sort by name within each course — DISTINCT ON's own ordering is only
	// guaranteed for the columns it distincts on, not the tie-break we want.
	sort.Slice(collected, func(i, j int) bool {
		if collected[i].tcID != collected[j].tcID {
			return collected[i].tcID.String() < collected[j].tcID.String()
		}
		return collected[i].ta.Name < collected[j].ta.Name
	})
	for _, r := range collected {
		out[r.tcID] = append(out[r.tcID], r.ta)
	}
	return out, nil
}

// coursesWithTARequest reports which courses a lecturer has actually asked to
// use a TA for this term — a course the registrar opened but nobody ever
// requested a TA for has nothing to summarize on this document and must not
// appear on it at all (staff's own instruction). 'submitted'/'approved' only:
// a rejected or cancelled request is not a live ask, same convention
// dashboard.go's "มีการขอใช้ TA" figure already uses.
func (s *ExportService) coursesWithTARequest(ctx context.Context, termID uuid.UUID) (map[uuid.UUID]bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT r.teaching_course_id
		FROM ta_requests r
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		WHERE tc.term_id = $1 AND r.status IN ('submitted','approved')`, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// courseSummaryClaimKinds returns, per teaching_course_id, the SUM of
// attendance_hrs and lab_hrs declared across every one of its live
// assignments — what claimKindLabel reads to decide "(Lec.+Lab)" vs "(Lec.)"
// vs "(Lab.)".
func (s *ExportService) courseSummaryClaimKinds(ctx context.Context, termID uuid.UUID) (map[uuid.UUID][2]float64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sec.teaching_course_id, COALESCE(SUM(wf.attendance_hrs),0), COALESCE(SUM(wf.lab_hrs),0)
		FROM ta_workload_forms wf
		JOIN ta_request_assignments a ON a.id = wf.assignment_id AND a.state <> 'dropped'
		JOIN sections sec ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		WHERE tc.term_id = $1
		GROUP BY sec.teaching_course_id`, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][2]float64{}
	for rows.Next() {
		var id uuid.UUID
		var attendance, lab float64
		if err := rows.Scan(&id, &attendance, &lab); err != nil {
			return nil, err
		}
		out[id] = [2]float64{attendance, lab}
	}
	return out, rows.Err()
}

/* -------------------------------------------------------------------------- */
/* Row assembly — one printed block per course, or per confirmed group        */
/* -------------------------------------------------------------------------- */

// courseSummaryBlock is one printed course block: header row + its TAs.
//
// ApplyRegular/ApplySpecial are the course's BUDGET per track (the snapshot's
// term pay), not money anyone has spent. PaidRegularByMonth/PaidSpecialByMonth
// (added back 11/09/2026, reversing the 10/08/2026 decision to drop them —
// and changed same-day from one cumulative figure to one figure PER selected
// month, per staff's follow-up request) are what SettleCourse says has
// actually cleared, keyed by YearMonth ("2026-06") for every month the caller
// selected — never summed here, so a course with several months shown prints
// each month's own figure side by side rather than one combined total.
type courseSummaryBlock struct {
	Code, NameTH, CreditText, Lecturer, ClaimKind string
	NumRegular, NumSpecial                        int
	ApplyRegular, ApplySpecial                    float64            // ขออนุมัติเบิกจ่าย
	PaidRegularByMonth, PaidSpecialByMonth        map[string]float64 // เบิกจ่ายเดือน, keyed by YearMonth
	// CommittedRegular/CommittedSpecial is the graduate-special lump sum
	// (reserved off the top of the pool at term start, not tied to any one
	// month — grad-special TAs no longer log work_logs at all). It is folded
	// into the earliest SELECTED month's own PaidRegularByMonth/
	// PaidSpecialByMonth entry by buildCourseSummaryBlocks once every
	// member's committed amount is known, rather than kept as a separate
	// invisible figure — a reader adding up the printed month columns must
	// land on the same total the total row does.
	CommittedRegular, CommittedSpecial float64
	TAs                                []courseSummaryTA
}

// courseSummaryPaidByMonth returns what SettleCourse says has actually
// cleared for one course, per track, broken out by month — never summed.
// SettleCourse already prices approved work and applies the college's budget
// cutoff (07/09/2026 rule); this is pure aggregation of that, not a new
// pricing path, so it can never print a number the claim documents would
// disagree with. Also returns each track's Committed lump sum separately —
// see courseSummaryBlock's own comment for why the caller folds it into a
// month rather than this function doing so.
func (s *ExportService) courseSummaryPaidByMonth(
	ctx context.Context, courseID uuid.UUID, months []string,
) (paidRegular, paidSpecial map[string]float64, committedRegular, committedSpecial float64, err error) {
	settled, err := s.SettleCourse(ctx, courseID)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	regByMonth := map[string]float64{}
	for _, m := range settled.Regular.Months {
		regByMonth[m.YearMonth] = m.PaidBaht
	}
	specByMonth := map[string]float64{}
	for _, m := range settled.Special.Months {
		specByMonth[m.YearMonth] = m.PaidBaht
	}
	paidRegular = map[string]float64{}
	paidSpecial = map[string]float64{}
	for _, ym := range months {
		paidRegular[ym] = regByMonth[ym]
		paidSpecial[ym] = specByMonth[ym]
	}
	// The graduate-special lump is dated by each holder's own hours
	// (CommittedByMonth, 11/09/2026); the selected months carry their own
	// slices, and the rest of the lump is claimed on the other months' sheet.
	// Nothing is left for the caller to fold, so the committed figures return
	// 0 — the fields survive for the block's own bookkeeping.
	for _, ym := range months {
		paidRegular[ym] += settled.Regular.CommittedByMonth[ym]
		paidSpecial[ym] += settled.Special.CommittedByMonth[ym]
	}
	return paidRegular, paidSpecial, 0, 0, nil
}

// buildBlocks turns the term's courses into printed blocks, keyed by which
// curriculum sheet each block prints under. A confirmed course_groups member
// that is not the group's primary contributes its money and TAs to the
// primary's block and produces no block of its own — this is the ONLY place
// a course's money is added rather than read once, and it sums each member's
// OWN BudgetSnapshot/CourseSettlement, never a value recomputed from merged
// student counts (the staff interview's explicit instruction).
func (s *ExportService) buildCourseSummaryBlocks(
	ctx context.Context, termID uuid.UUID, months []string,
) (map[string][]courseSummaryBlock, []string, error) {
	courses, err := s.courseSummaryCourses(ctx, termID)
	if err != nil {
		return nil, nil, err
	}
	byID := map[uuid.UUID]courseSummaryCourse{}
	for _, c := range courses {
		byID[c.ID] = c
	}
	tasByCourse, err := s.courseSummaryTAs(ctx, termID)
	if err != nil {
		return nil, nil, err
	}
	claimKinds, err := s.courseSummaryClaimKinds(ctx, termID)
	if err != nil {
		return nil, nil, err
	}
	groups, err := s.teaching.ListConfirmedCourseGroups(ctx, termID)
	if err != nil {
		return nil, nil, err
	}
	requested, err := s.coursesWithTARequest(ctx, termID)
	if err != nil {
		return nil, nil, err
	}

	var warnings []string
	bySheet := map[string][]courseSummaryBlock{}
	handled := map[uuid.UUID]bool{}

	for _, c := range courses {
		if handled[c.ID] {
			continue
		}
		memberIDs := []uuid.UUID{c.ID}
		sheetCode := printCurriculumCode(c.Curriculum, c.Level)
		codes := []string{c.Code}
		names := []string{c.NameTH}

		if grp, ok := groups[c.ID]; ok {
			if c.ID != grp.PrimaryCourseID {
				// Reached a non-primary member before its primary (course
				// order is by code, which does not guarantee this) — defer
				// entirely to when the primary is processed.
				continue
			}
			memberIDs = append([]uuid.UUID(nil), grp.MemberCourseIDs...)
			sort.Slice(memberIDs, func(i, j int) bool {
				return byID[memberIDs[i]].Code < byID[memberIDs[j]].Code
			})
			codes = codes[:0]
			names = names[:0]
			for _, id := range memberIDs {
				codes = append(codes, byID[id].Code)
				names = append(names, byID[id].NameTH)
			}
			sheetCode = grp.CurriculumCode
		}
		for _, id := range memberIDs {
			handled[id] = true
		}

		// Nobody asked to use a TA for this course (or, for a merged group,
		// for any of its registrar codes) — quietly skip it. This is the
		// ordinary case (most courses run without a TA), not a problem to
		// warn about.
		anyRequested := false
		for _, id := range memberIDs {
			if requested[id] {
				anyRequested = true
				break
			}
		}
		if !anyRequested {
			continue
		}

		block := courseSummaryBlock{
			Code:               strings.Join(codes, "/"),
			NameTH:             strings.Join(dedupStrings(names), " / "),
			CreditText:         creditText(c.Credits, c.LectureHrs, c.LabHrs, c.SelfHrs),
			Lecturer:           c.Lecturer,
			PaidRegularByMonth: map[string]float64{},
			PaidSpecialByMonth: map[string]float64{},
		}

		var attendance, lab float64
		var taSeen = map[uuid.UUID]bool{}
		for _, id := range memberIDs {
			mc := byID[id]
			block.NumRegular += mc.NumStudentsRegular
			block.NumSpecial += mc.NumStudentsSpecial
			if mc.NumStudentsRegular == 0 && mc.NumStudentsSpecial == 0 {
				warnings = append(warnings, fmt.Sprintf("%s (%s): ยังไม่ได้กรอกจำนวนนักศึกษา", mc.Code, mc.NameTH))
			}

			snap, err := s.budget.Compute(ctx, id)
			if err != nil {
				return nil, nil, err
			}
			block.ApplyRegular += snap.TermPayRegular
			block.ApplySpecial += snap.TermPaySpecial

			paidRegular, paidSpecial, committedReg, committedSpec, err := s.courseSummaryPaidByMonth(ctx, id, months)
			if err != nil {
				return nil, nil, err
			}
			for _, ym := range months {
				block.PaidRegularByMonth[ym] += paidRegular[ym]
				block.PaidSpecialByMonth[ym] += paidSpecial[ym]
			}
			block.CommittedRegular += committedReg
			block.CommittedSpecial += committedSpec

			if ck, ok := claimKinds[id]; ok {
				attendance += ck[0]
				lab += ck[1]
			}
			// A TA helping a class that runs under two registrar codes may
			// hold a separate assignment under EACH code (one request per
			// lecturer, same person) — dedup by user id so a merged block
			// lists them once, not once per member course.
			for _, ta := range tasByCourse[id] {
				if taSeen[ta.UserID] {
					continue
				}
				taSeen[ta.UserID] = true
				block.TAs = append(block.TAs, ta)
			}
		}
		block.ClaimKind = claimKindLabel(attendance, lab)
		sort.Slice(block.TAs, func(i, j int) bool { return block.TAs[i].Name < block.TAs[j].Name })

		// The graduate-special lump is already inside each month's figure
		// (courseSummaryPaidByMonth adds CommittedByMonth), so the committed
		// totals here are 0 and this fold is a no-op kept for the shape.
		if len(months) > 0 {
			block.PaidRegularByMonth[months[0]] += block.CommittedRegular
			block.PaidSpecialByMonth[months[0]] += block.CommittedSpecial
		}
		for ym := range block.PaidRegularByMonth {
			block.PaidRegularByMonth[ym] = round2(block.PaidRegularByMonth[ym])
		}
		for ym := range block.PaidSpecialByMonth {
			block.PaidSpecialByMonth[ym] = round2(block.PaidSpecialByMonth[ym])
		}

		if len(block.TAs) == 0 {
			warnings = append(warnings, fmt.Sprintf("%s (%s): ยังไม่มี TA ที่อนุมัติ", block.Code, block.NameTH))
		}
		if sheetCode == "" {
			warnings = append(warnings, fmt.Sprintf("%s (%s): ยังไม่ทราบหลักสูตร ไม่ได้ลงชีตใด", block.Code, block.NameTH))
			continue
		}
		bySheet[sheetCode] = append(bySheet[sheetCode], block)
	}
	return bySheet, warnings, nil
}

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

/* -------------------------------------------------------------------------- */
/* Workbook rendering                                                         */
/* -------------------------------------------------------------------------- */

type courseSummaryStyles struct {
	title, subtitle, sheetHeading int
	colHeader, colHeaderCenter    int
	body, bodyCenter              int
	money, moneyBold              int
	totalLabel                    int
}

// Palette lifted from the college's own ค่าตอบแทนTA workbook (docs/
// ค่าตอบแทนTAภาคต้น-2569.xlsx), which stores these as Office theme colours +
// tints; resolved to literal RGB here so the output does not depend on
// whatever theme the reader's Excel happens to apply:
//
//	headerFill  = accent2 tint 0.60  — title bar, column headers, the money cells
//	sectionFill = accent6 tint 0.40  — the curriculum name row
//	totalFill   = accent2 tint 0.00  — the grand-total row
//
// Borders are plain black ("auto" in the reference), NOT the grey this file
// used to draw: next to the reference the grey read as a different document.
const (
	csHeaderFill  = "F8CBAD"
	csSectionFill = "A9D18E"
	csTotalFill   = "ED7D31"
	csTitleFont   = "0000CC"
)

func buildCourseSummaryStyles(f *excelize.File) (*courseSummaryStyles, error) {
	st := &courseSummaryStyles{}
	font := func(size float64, bold bool) *excelize.Font {
		return &excelize.Font{Family: "TH Sarabun New", Size: size, Bold: bold}
	}
	center := &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true}
	left := &excelize.Alignment{Horizontal: "left", Vertical: "center", WrapText: true}
	right := &excelize.Alignment{Horizontal: "right", Vertical: "center"}
	thin := []excelize.Border{
		{Type: "left", Color: "000000", Style: 1}, {Type: "right", Color: "000000", Style: 1},
		{Type: "top", Color: "000000", Style: 1}, {Type: "bottom", Color: "000000", Style: 1},
	}
	fill := func(rgb string) excelize.Fill {
		return excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{rgb}}
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
	st.title = mk(&excelize.Style{
		Font:      &excelize.Font{Family: "TH Sarabun New", Size: 24, Bold: true, Color: csTitleFont},
		Alignment: center, Fill: fill(csHeaderFill),
		Border: []excelize.Border{{Type: "bottom", Color: "000000", Style: 1}},
	})
	st.subtitle = mk(&excelize.Style{Font: font(16, false), Alignment: center})
	st.sheetHeading = mk(&excelize.Style{Font: font(16, true), Alignment: center, Border: thin, Fill: fill(csSectionFill)})
	st.colHeader = mk(&excelize.Style{Font: font(16, true), Alignment: left, Border: thin, Fill: fill(csHeaderFill)})
	st.colHeaderCenter = mk(&excelize.Style{Font: font(16, true), Alignment: center, Border: thin, Fill: fill(csHeaderFill)})
	st.body = mk(&excelize.Style{Font: font(14, false), Alignment: left, Border: thin})
	st.bodyCenter = mk(&excelize.Style{Font: font(14, false), Alignment: center, Border: thin})
	// Money cells carry the header fill down the whole column in the reference —
	// the ขออนุมัติเบิกจ่าย pair is the figure the document exists to report, so
	// it is tinted for the reader's eye rather than left plain like the rest.
	st.money = mk(&excelize.Style{Font: font(14, true), Alignment: right, Border: thin, Fill: fill(csHeaderFill), CustomNumFmt: fmtPtr(moneyFmt)})
	st.moneyBold = mk(&excelize.Style{Font: font(16, true), Alignment: right, Border: thin, Fill: fill(csTotalFill), CustomNumFmt: fmtPtr(moneyFmt)})
	st.totalLabel = mk(&excelize.Style{Font: font(20, true), Alignment: center, Border: thin, Fill: fill(csTotalFill)})
	if err != nil {
		return nil, err
	}
	return st, nil
}

func fmtPtr(s string) *string { return &s }

// summaryBodyColumns is the sheet's body grid, column by column, in the styles
// the reference gives them: names read from the left, everything countable is
// centred, and the money columns carry the header tint down the whole
// column.
//
// summaryCol/colWidth are named (rather than anonymous struct literals)
// because summaryBodyColumns and the column-width table below now both grow
// a variable, per-term number of entries — one เบิกจ่ายเดือน pair per
// selected month — built with append() in a loop, which anonymous struct
// types make awkward to spell out repeatedly.
type summaryCol struct {
	col   string
	style int
}

// excelCol turns a 1-based column INDEX (12 → "L", 27 → "AA") into the
// letter excelize's cell-name API expects. Needed because the sheet's money
// columns from ขออนุมัติเบิกจ่าย onward no longer sit at a fixed letter — the
// number of เบิกจ่ายเดือน pairs between them and คงเหลือ depends on how many
// months staff selected.
func excelCol(n int) string {
	s := ""
	for n > 0 {
		n--
		s = string(rune('A'+n%26)) + s
		n /= 26
	}
	return s
}

// summaryBodyColumns lists every ruled column of the body grid for a sheet
// with numMonths เบิกจ่ายเดือน pairs: the fixed A:M prefix, then one money
// pair per selected month, then the final คงเหลือ pair.
func summaryBodyColumns(st *courseSummaryStyles, numMonths int) []summaryCol {
	cols := []summaryCol{
		{"A", st.bodyCenter}, {"B", st.bodyCenter}, {"C", st.body},
		{"D", st.bodyCenter}, {"E", st.body}, {"F", st.bodyCenter}, {"G", st.body},
		{"H", st.bodyCenter}, {"I", st.bodyCenter}, {"J", st.bodyCenter},
		{"K", st.bodyCenter}, {"L", st.money}, {"M", st.money},
	}
	idx := 14 // first เบิกจ่ายเดือน column
	for i := 0; i < numMonths; i++ {
		cols = append(cols, summaryCol{excelCol(idx), st.money}, summaryCol{excelCol(idx + 1), st.money})
		idx += 2
	}
	cols = append(cols, summaryCol{excelCol(idx), st.money}, summaryCol{excelCol(idx + 1), st.money}) // คงเหลือ
	return cols
}

// ruleSummaryBlock draws the grid for one course: one row per TA, or a single
// row when the course has none on file yet.
func ruleSummaryBlock(f *excelize.File, st *courseSummaryStyles, sheet string, top, tas, numMonths int) error {
	if tas < 1 {
		tas = 1
	}
	for r := top; r < top+tas; r++ {
		for _, c := range summaryBodyColumns(st, numMonths) {
			cell := fmt.Sprintf("%s%d", c.col, r)
			if err := f.SetCellStyle(sheet, cell, cell, c.style); err != nil {
				return err
			}
		}
	}
	return nil
}

// courseSummarySheetSnapshot is one curriculum sheet's worth of frozen blocks
// — CurriculumFullNameTH and SheetName are copied from curricula at generation
// time (not re-looked-up on reprint) because that table is staff-editable
// (settings page, migration 0073's own comment): a later rename must not
// silently reword a document already handed to someone.
type courseSummarySheetSnapshot struct {
	CurriculumCode string               `json:"curriculum_code"`
	SheetName      string               `json:"sheet_name"`
	FullNameTH     string               `json:"full_name_th"`
	Blocks         []courseSummaryBlock `json:"blocks"`
}

// courseSummarySnapshot is what's frozen into the ledger row: the term-line
// text plus every sheet's blocks, exactly as BuildCourseSummaryWorkbook
// resolved them. No PII to exclude here (unlike ปะหน้าจ่ายตรง's PromptPay
// column), so reprint needs no live re-derivation step at all.
//
// Months/MonthLabels are frozen too, same reasoning as FullNameTH/SheetName
// below: a reprint must reproduce the exact เบิกจ่ายเดือน/คงเหลือ figures and
// column headers the original generation showed, never recompute against
// whatever months or work have been approved since.
type courseSummarySnapshot struct {
	AcademicYear string                       `json:"academic_year"`
	SemLabel     string                       `json:"sem_label"`
	Months       []string                     `json:"months"`
	MonthLabels  map[string]string            `json:"month_labels"`
	Sheets       []courseSummarySheetSnapshot `json:"sheets"`
}

// resolveMonths validates a caller's requested month selection against the
// term's own months (normalizeMonthSelection's usual contract — empty means
// every month, an unknown month is an error rather than a silent drop) and
// returns, alongside the resolved list, a short single-word Thai label per
// month ("มิถุนายน") for a narrow column header — distinct from
// thaiSelectedMonthsLabel's combined range phrasing, which named one pooled
// "เบิกจ่ายเดือน" column before this became one column PER month.
func (s *ExportService) resolveMonths(
	ctx context.Context, termID uuid.UUID, requested []string,
) (months []string, monthLabels map[string]string, err error) {
	all, err := s.TermMonths(ctx, termID)
	if err != nil {
		return nil, nil, err
	}
	months, err = normalizeMonthSelection(all, requested)
	if err != nil {
		return nil, nil, err
	}
	monthLabels = map[string]string{}
	for _, m := range all {
		var y, mm int
		if _, serr := fmt.Sscanf(m.YearMonth, "%d-%d", &y, &mm); serr == nil && mm >= 1 && mm <= 12 {
			monthLabels[m.YearMonth] = thaiMonths[mm-1]
		} else {
			monthLabels[m.YearMonth] = m.Label
		}
	}
	return months, monthLabels, nil
}

// renderCourseSummarySnapshot gathers everything writeCourseSummaryWorkbook
// needs, live from today's tables — the read side shared by a fresh Build and
// (indirectly, via the ledger) a later Reprint's Build.
func (s *ExportService) renderCourseSummarySnapshot(
	ctx context.Context, termID uuid.UUID, requestedMonths []string,
) (courseSummarySnapshot, []string, error) {
	var academicYear, semLabel string
	if err := s.pool.QueryRow(ctx, `
		SELECT academic_year::text,
		       CASE semester WHEN 1 THEN 'ภาคต้น' WHEN 2 THEN 'ภาคปลาย' ELSE 'ภาคฤดูร้อน' END
		FROM academic_terms WHERE id = $1`, termID).Scan(&academicYear, &semLabel); err != nil {
		return courseSummarySnapshot{}, nil, err
	}

	months, monthLabels, err := s.resolveMonths(ctx, termID, requestedMonths)
	if err != nil {
		return courseSummarySnapshot{}, nil, err
	}

	curricula, err := s.teaching.ListCurricula(ctx)
	if err != nil {
		return courseSummarySnapshot{}, nil, err
	}
	bySheet, warnings, err := s.buildCourseSummaryBlocks(ctx, termID, months)
	if err != nil {
		return courseSummarySnapshot{}, nil, err
	}

	var sheets []courseSummarySheetSnapshot
	for _, cur := range curricula {
		blocks := bySheet[cur.Code]
		if len(blocks) == 0 {
			continue
		}
		sheets = append(sheets, courseSummarySheetSnapshot{
			CurriculumCode: cur.Code, SheetName: cur.SheetName, FullNameTH: cur.FullNameTH, Blocks: blocks,
		})
	}
	return courseSummarySnapshot{
		AcademicYear: academicYear, SemLabel: semLabel,
		Months: months, MonthLabels: monthLabels,
		Sheets: sheets,
	}, warnings, nil
}

// writeCourseSummaryWorkbook renders an already-resolved snapshot into an
// xlsx — shared by a fresh Build and ReprintCourseSummary, so a reprint always
// produces byte-for-byte the same layout a fresh build would have, just from
// frozen data.
func writeCourseSummaryWorkbook(snap courseSummarySnapshot) ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()
	st, err := buildCourseSummaryStyles(f)
	if err != nil {
		return nil, err
	}

	wrote := false
	for _, sh := range snap.Sheets {
		sheetName := sh.SheetName
		if !wrote {
			// The default sheet excelize creates ("Sheet1") is renamed for the
			// first curriculum instead of deleted-then-recreated.
			_ = f.SetSheetName("Sheet1", sheetName)
		} else if _, err := f.NewSheet(sheetName); err != nil {
			return nil, err
		}
		wrote = true
		if err := writeCourseSummarySheet(f, st, sheetName, snap.SemLabel, snap.AcademicYear, sh.FullNameTH, snap.Months, snap.MonthLabels, sh.Blocks); err != nil {
			return nil, err
		}
	}
	if !wrote {
		// No curriculum had any course at all — still hand back a valid,
		// openable (if empty) workbook rather than error out.
		_ = f.SetSheetName("Sheet1", "สรุป")
		_ = f.SetCellValue("สรุป", "A1", "ไม่มีข้อมูลรายวิชาสำหรับภาคเรียนนี้")
	}
	f.SetActiveSheet(0)

	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// CourseSummaryWarnings answers "what should staff double check" without
// rendering (and discarding) a whole workbook — the staff screen just needs
// the count before the download button is even pressed.
func (s *ExportService) CourseSummaryWarnings(ctx context.Context, termID uuid.UUID, requestedMonths []string) ([]string, error) {
	months, _, err := s.resolveMonths(ctx, termID, requestedMonths)
	if err != nil {
		return nil, err
	}
	_, warnings, err := s.buildCourseSummaryBlocks(ctx, termID, months)
	return warnings, err
}

// BuildCourseSummaryWorkbook renders "สรุปรายวิชาที่ขอใช้ TA": one sheet per
// curriculum that has at least one course this term, one block per course (or
// confirmed course_group), one row per approved TA. Returns the xlsx bytes and
// any warnings — missing student counts or a course with no approved TA —
// which do NOT block generation (this is an estimate document, made before
// everything is settled), only flag what staff should double check.
//
// Records a ledger row (course_summary_exports, 27/08/2026) before returning
// bytes, mirroring transfer_cover_exports: a student count or TA approval
// corrected after this file went out must not make the original numbers
// unrecoverable — see ReprintCourseSummary.
func (s *ExportService) BuildCourseSummaryWorkbook(
	ctx context.Context, actor, termID uuid.UUID, requestedMonths []string,
) ([]byte, []string, error) {
	snap, warnings, err := s.renderCourseSummarySnapshot(ctx, termID, requestedMonths)
	if err != nil {
		return nil, nil, err
	}
	body, err := writeCourseSummaryWorkbook(snap)
	if err != nil {
		return nil, nil, err
	}
	if err := s.recordCourseSummaryExport(ctx, actor, termID, snap); err != nil {
		return nil, nil, err
	}
	if err := s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "export.course_summary", Entity: "academic_term", EntityID: termID.String(),
	}); err != nil {
		return nil, nil, err
	}
	return body, warnings, nil
}

// recordCourseSummaryExport persists the ledger row BEFORE bytes are handed
// back, same reasoning as recordTransferCoverExport: an unrecorded generation
// can never be reprinted.
func (s *ExportService) recordCourseSummaryExport(ctx context.Context, actor, termID uuid.UUID, snap courseSummarySnapshot) error {
	raw, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	var by *uuid.UUID
	if actor != uuid.Nil {
		by = &actor
	}
	courseCount := 0
	for _, sh := range snap.Sheets {
		courseCount += len(sh.Blocks)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO course_summary_exports (id, term_id, generated_by, course_count, document)
		VALUES (gen_random_uuid(), $1, $2, $3, $4)`,
		termID, by, courseCount, raw)
	return err
}

// ReprintCourseSummary hands back a copy of a generation already on the
// ledger, rendered from the frozen snapshot rather than re-querying courses —
// a student count or TA approval corrected after the fact must not silently
// change a document already handed to someone.
func (s *ExportService) ReprintCourseSummary(ctx context.Context, actor, exportID uuid.UUID) ([]byte, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT document FROM course_summary_exports WHERE id = $1`, exportID).Scan(&raw); err != nil {
		return nil, ErrNotFound
	}
	var snap courseSummarySnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("course summary export %s: snapshot unreadable: %w", exportID, err)
	}
	body, err := writeCourseSummaryWorkbook(snap)
	if err != nil {
		return nil, err
	}
	if err := s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "export.course_summary.reprint",
		Entity: "course_summary_export", EntityID: exportID.String(),
	}); err != nil {
		return nil, err
	}
	return body, nil
}

// CourseSummaryExportSummary is one row of generation history — who, when,
// how many courses, and its id for reprinting.
type CourseSummaryExportSummary struct {
	ID          uuid.UUID `json:"id"`
	TermID      uuid.UUID `json:"term_id"`
	GeneratedAt string    `json:"generated_at"`
	GeneratedBy string    `json:"generated_by,omitempty"`
	CourseCount int       `json:"course_count"`
	// MonthLabel summarizes which months this generation's เบิกจ่ายเดือน
	// columns covered — empty for rows generated before 11/09/2026, which
	// covered the whole term the same way an empty month selection still
	// does.
	MonthLabel string `json:"month_label,omitempty"`
}

// ListCourseSummaryExports returns the generation history for a term, newest
// first.
func (s *ExportService) ListCourseSummaryExports(ctx context.Context, termID uuid.UUID) ([]CourseSummaryExportSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT e.id, e.term_id, TO_CHAR(e.generated_at,'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'),
		       COALESCE(u.first_name || ' ' || u.last_name, ''), e.course_count, e.document
		FROM course_summary_exports e
		LEFT JOIN users u ON u.id = e.generated_by
		WHERE e.term_id = $1
		ORDER BY e.generated_at DESC`, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CourseSummaryExportSummary{}
	for rows.Next() {
		var r CourseSummaryExportSummary
		var raw []byte
		if err := rows.Scan(&r.ID, &r.TermID, &r.GeneratedAt, &r.GeneratedBy, &r.CourseCount, &raw); err != nil {
			return nil, err
		}
		var snap courseSummarySnapshot
		if err := json.Unmarshal(raw, &snap); err == nil {
			r.MonthLabel = thaiSelectedMonthsLabel(snap.Months)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func writeCourseSummarySheet(
	f *excelize.File, st *courseSummaryStyles, sheet, semLabel, academicYear, curriculumFullName string,
	months []string, monthLabels map[string]string,
	blocks []courseSummaryBlock,
) error {
	set := func(cell string, style int, v any) error {
		if str, ok := v.(string); ok && strings.HasPrefix(str, "=") {
			if err := f.SetCellFormula(sheet, cell, strings.TrimPrefix(str, "=")); err != nil {
				return err
			}
		} else if err := f.SetCellValue(sheet, cell, v); err != nil {
			return err
		}
		return f.SetCellStyle(sheet, cell, cell, style)
	}

	_ = f.MergeCell(sheet, "A1", "M1")
	if err := set("A1", st.title, fmt.Sprintf("สรุปรายวิชาที่ขอใช้ TA  ประจำ%s ปีการศึกษา %s", semLabel, academicYear)); err != nil {
		return err
	}
	_ = f.SetRowHeight(sheet, 1, 36)

	// Column layout, 1-indexed — the fixed A:M prefix is always the same
	// (รายวิชา is B:C — the reference's own template ruled an empty filler
	// column D under that header, but nothing was ever written to it and
	// nobody could say what it was for, so it was dropped 11/09/2026, staff's
	// own call), then one เบิกจ่ายเดือน pair PER selected month (11/09/2026,
	// reversing the SAME DAY's earlier decision to pool every selected month
	// into one cumulative pair — staff asked to see each month on its own
	// instead), then a final คงเหลือ pair.
	const monthStartCol = 14 // N, if no month were ever removed — see excelCol
	numMonths := len(months)
	remainCol0 := excelCol(monthStartCol + 2*numMonths)
	remainCol1 := excelCol(monthStartCol + 2*numMonths + 1)

	headers := []struct {
		cell, mergeTo, label string
	}{
		{"A2", "A3", "ลำดับที่"},
		{"B2", "C3", "รายวิชา"},
		{"D2", "D3", "หน่วยกิต"},
		{"E2", "E3", "ชื่ออาจารย์"},
		{"F2", "F3", "รหัสนักศึกษา"},
		{"G2", "G3", "ชื่อ TA"},
		{"H2", "H3", "ระดับ"},
		{"I2", "J2", "จำนวน นศ."},
		{"K2", "", "เบิกจ่าย"},
		{"L2", "M3", "ขออนุมัติเบิกจ่าย"},
	}
	for i, ym := range months {
		c0, c1 := excelCol(monthStartCol+2*i), excelCol(monthStartCol+2*i+1)
		label := monthLabels[ym]
		if label == "" {
			label = ym
		}
		headers = append(headers, struct{ cell, mergeTo, label string }{c0 + "2", c1 + "3", "เบิกจ่ายเดือน " + label})
	}
	headers = append(headers, struct{ cell, mergeTo, label string }{remainCol0 + "2", remainCol1 + "3", "คงเหลือ"})

	for _, h := range headers {
		if err := set(h.cell, st.colHeaderCenter, h.label); err != nil {
			return err
		}
		if h.mergeTo != "" {
			_ = f.MergeCell(sheet, h.cell, h.mergeTo)
			// Merging keeps only the anchor's style, so the covered cells would
			// print unfilled and unbordered where the merge is wider than one
			// column (B2:C3, I2:J2, L2:M3, and every เบิกจ่ายเดือน/คงเหลือ pair).
			if err := f.SetCellStyle(sheet, h.cell, h.mergeTo, st.colHeaderCenter); err != nil {
				return err
			}
		}
	}
	subheaders := map[string]string{
		"I3": "ปกติ", "J3": "พิเศษ", "K3": "Lec./Lab",
		"L4": "ปกติ", "M4": "พิเศษ",
	}
	for i := range months {
		subheaders[excelCol(monthStartCol+2*i)+"4"] = "ปกติ"
		subheaders[excelCol(monthStartCol+2*i+1)+"4"] = "พิเศษ"
	}
	subheaders[remainCol0+"4"] = "ปกติ"
	subheaders[remainCol1+"4"] = "พิเศษ"
	for cell, label := range subheaders {
		if err := set(cell, st.colHeaderCenter, label); err != nil {
			return err
		}
	}
	_ = f.MergeCell(sheet, "A4", "K4")
	if err := set("A4", st.sheetHeading, curriculumFullName); err != nil {
		return err
	}

	row := 5
	for i, b := range blocks {
		top := row
		// EVERY cell of the block is ruled first, then the values are written
		// on top of the rules.
		//
		// A course with more than one TA occupies one row per TA, and only the
		// first of them carries the course's own columns — but the reference
		// still rules the whole grid on every one of those rows, so the table
		// reads as a single grid with the course spanning it. Styling only the
		// cells that take a value tore that grid open: from the second TA down,
		// everything outside รหัสนักศึกษา/ชื่อ TA/ระดับ printed with no rules at
		// all.
		if err := ruleSummaryBlock(f, st, sheet, top, len(b.TAs), numMonths); err != nil {
			return err
		}
		if err := set(fmt.Sprintf("A%d", top), st.bodyCenter, i+1); err != nil {
			return err
		}
		// Course code is centred and wrapped in the reference — a merged pair
		// prints as "SC313302/CP353301" over two lines rather than overflowing.
		if err := set(fmt.Sprintf("B%d", top), st.bodyCenter, b.Code); err != nil {
			return err
		}
		if err := set(fmt.Sprintf("C%d", top), st.body, b.NameTH); err != nil {
			return err
		}
		if err := set(fmt.Sprintf("D%d", top), st.bodyCenter, b.CreditText); err != nil {
			return err
		}
		if err := set(fmt.Sprintf("E%d", top), st.body, b.Lecturer); err != nil {
			return err
		}
		if err := set(fmt.Sprintf("I%d", top), st.bodyCenter, b.NumRegular); err != nil {
			return err
		}
		if err := set(fmt.Sprintf("J%d", top), st.bodyCenter, b.NumSpecial); err != nil {
			return err
		}
		if err := set(fmt.Sprintf("K%d", top), st.bodyCenter, b.ClaimKind); err != nil {
			return err
		}
		if err := set(fmt.Sprintf("L%d", top), st.money, b.ApplyRegular); err != nil {
			return err
		}
		if err := set(fmt.Sprintf("M%d", top), st.money, b.ApplySpecial); err != nil {
			return err
		}
		regCells := make([]string, 0, numMonths)
		specCells := make([]string, 0, numMonths)
		for i, ym := range months {
			regCell := fmt.Sprintf("%s%d", excelCol(monthStartCol+2*i), top)
			specCell := fmt.Sprintf("%s%d", excelCol(monthStartCol+2*i+1), top)
			if err := set(regCell, st.money, b.PaidRegularByMonth[ym]); err != nil {
				return err
			}
			if err := set(specCell, st.money, b.PaidSpecialByMonth[ym]); err != nil {
				return err
			}
			regCells = append(regCells, regCell)
			specCells = append(specCells, specCell)
		}
		// คงเหลือ is a live formula, ขออนุมัติเบิกจ่าย minus every visible
		// month's own printed cell, so it keeps agreeing with them even if
		// someone edits a cell by hand after the file is handed over.
		remainReg := fmt.Sprintf("=L%d", top)
		for _, c := range regCells {
			remainReg += "-" + c
		}
		remainSpec := fmt.Sprintf("=M%d", top)
		for _, c := range specCells {
			remainSpec += "-" + c
		}
		if err := set(fmt.Sprintf("%s%d", remainCol0, top), st.money, remainReg); err != nil {
			return err
		}
		if err := set(fmt.Sprintf("%s%d", remainCol1, top), st.money, remainSpec); err != nil {
			return err
		}

		if len(b.TAs) == 0 {
			row++
		}
		for j, ta := range b.TAs {
			r := top + j
			if err := set(fmt.Sprintf("F%d", r), st.bodyCenter, ta.StudentID); err != nil {
				return err
			}
			if err := set(fmt.Sprintf("G%d", r), st.body, ta.Name); err != nil {
				return err
			}
			if err := set(fmt.Sprintf("H%d", r), st.bodyCenter, ta.LevelTH); err != nil {
				return err
			}
			row = r + 1
		}
	}
	lastDataRow := row - 1
	if lastDataRow < 5 {
		lastDataRow = 5
	}

	totalRow := row
	_ = f.MergeCell(sheet, fmt.Sprintf("A%d", totalRow), fmt.Sprintf("K%d", totalRow))
	if err := set(fmt.Sprintf("A%d", totalRow), st.totalLabel, "รวมทั้งหมด"); err != nil {
		return err
	}
	sumCols := []string{"L", "M"}
	for i := range months {
		sumCols = append(sumCols, excelCol(monthStartCol+2*i), excelCol(monthStartCol+2*i+1))
	}
	for _, col := range sumCols {
		cell := fmt.Sprintf("%s%d", col, totalRow)
		formula := fmt.Sprintf("=SUM(%s5:%s%d)", col, col, lastDataRow)
		if err := set(cell, st.moneyBold, formula); err != nil {
			return err
		}
	}
	remainRegTotal := fmt.Sprintf("=L%d", totalRow)
	remainSpecTotal := fmt.Sprintf("=M%d", totalRow)
	for i := range months {
		remainRegTotal += "-" + fmt.Sprintf("%s%d", excelCol(monthStartCol+2*i), totalRow)
		remainSpecTotal += "-" + fmt.Sprintf("%s%d", excelCol(monthStartCol+2*i+1), totalRow)
	}
	if err := set(fmt.Sprintf("%s%d", remainCol0, totalRow), st.moneyBold, remainRegTotal); err != nil {
		return err
	}
	if err := set(fmt.Sprintf("%s%d", remainCol1, totalRow), st.moneyBold, remainSpecTotal); err != nil {
		return err
	}

	type colWidth struct {
		from, to string
		w        float64
	}
	widths := []colWidth{
		{"A", "A", 6.83}, {"B", "B", 22.33}, {"C", "C", 49.33},
		{"D", "D", 9.33}, {"E", "E", 25.83}, {"F", "F", 13.33}, {"G", "G", 26.33},
		{"H", "H", 6.33}, {"I", "J", 11}, {"K", "K", 11.33}, {"L", "L", 13.16}, {"M", "M", 15.0},
	}
	for i := range months {
		widths = append(widths,
			colWidth{excelCol(monthStartCol + 2*i), excelCol(monthStartCol + 2*i), 12.66},
			colWidth{excelCol(monthStartCol + 2*i + 1), excelCol(monthStartCol + 2*i + 1), 10.83},
		)
	}
	widths = append(widths, colWidth{remainCol0, remainCol0, 11.66}, colWidth{remainCol1, remainCol1, 11.5})
	for _, w := range widths {
		_ = f.SetColWidth(sheet, w.from, w.to, w.w)
	}
	return nil
}

/* -------------------------------------------------------------------------- */
/* On-screen preview — the same data flattened into rows a web table can sort */
/* -------------------------------------------------------------------------- */

// CourseSummaryPreviewRow is one printed line, flattened for a data table: a
// course's own header fields are repeated on every one of its TA rows (or
// once, blank, if it has no approved TA yet) rather than merged, since a
// sortable table has no cell-merge equivalent.
// CourseSummaryMonthAmount is one course's เบิกจ่ายเดือน figure for ONE
// selected month — never summed with any other month (11/09/2026, staff's
// explicit request: show each month on its own, not pooled into a range).
type CourseSummaryMonthAmount struct {
	YearMonth string  `json:"year_month"`
	Label     string  `json:"label"` // "มิถุนายน"
	Regular   float64 `json:"regular"`
	Special   float64 `json:"special"`
}

type CourseSummaryPreviewRow struct {
	CourseCode   string                     `json:"course_code"`
	CourseNameTH string                     `json:"course_name_th"`
	CreditText   string                     `json:"credit_text"`
	Lecturer     string                     `json:"lecturer"`
	ClaimKind    string                     `json:"claim_kind"`
	NumRegular   int                        `json:"num_regular"`
	NumSpecial   int                        `json:"num_special"`
	ApplyRegular float64                    `json:"apply_regular"`
	ApplySpecial float64                    `json:"apply_special"`
	PaidByMonth  []CourseSummaryMonthAmount `json:"paid_by_month"`
	TAStudentID  string                     `json:"ta_student_id,omitempty"`
	TAName       string                     `json:"ta_name,omitempty"`
	TALevelTH    string                     `json:"ta_level_th,omitempty"`
}

// CourseSummaryPreviewSheet is one curriculum's worth of rows — the web
// equivalent of one sheet in the .xlsx.
type CourseSummaryPreviewSheet struct {
	CurriculumCode string                    `json:"curriculum_code"`
	Sheet          string                    `json:"sheet"` // curriculum full name, e.g. "สาขาวิชาความมั่นคงปลอดภัยไซเบอร์"
	Rows           []CourseSummaryPreviewRow `json:"rows"`
}

// CourseSummaryPreview returns "สรุปรายวิชาที่ขอใช้ TA" as data instead of a
// workbook — the on-screen table staff asked for so they can check the
// numbers without downloading a file first. Same source as
// BuildCourseSummaryWorkbook (buildCourseSummaryBlocks), so the screen and
// the file can never show different figures. Returns months/monthLabels too
// so the frontend can render one column pair per month without re-deriving
// the resolved set (an empty request) or the short labels itself.
func (s *ExportService) CourseSummaryPreview(
	ctx context.Context, termID uuid.UUID, requestedMonths []string,
) (sheets []CourseSummaryPreviewSheet, warnings []string, months []string, monthLabels map[string]string, err error) {
	curricula, err := s.teaching.ListCurricula(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	months, monthLabels, err = s.resolveMonths(ctx, termID, requestedMonths)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	bySheet, warnings, err := s.buildCourseSummaryBlocks(ctx, termID, months)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	var out []CourseSummaryPreviewSheet
	for _, cur := range curricula {
		blocks := bySheet[cur.Code]
		if len(blocks) == 0 {
			continue
		}
		var rows []CourseSummaryPreviewRow
		for _, b := range blocks {
			paidByMonth := make([]CourseSummaryMonthAmount, 0, len(months))
			for _, ym := range months {
				label := monthLabels[ym]
				if label == "" {
					label = ym
				}
				paidByMonth = append(paidByMonth, CourseSummaryMonthAmount{
					YearMonth: ym, Label: label,
					Regular: b.PaidRegularByMonth[ym], Special: b.PaidSpecialByMonth[ym],
				})
			}
			base := CourseSummaryPreviewRow{
				CourseCode: b.Code, CourseNameTH: b.NameTH, CreditText: b.CreditText,
				Lecturer: b.Lecturer, ClaimKind: b.ClaimKind,
				NumRegular: b.NumRegular, NumSpecial: b.NumSpecial,
				ApplyRegular: b.ApplyRegular, ApplySpecial: b.ApplySpecial,
				PaidByMonth: paidByMonth,
			}
			if len(b.TAs) == 0 {
				rows = append(rows, base)
				continue
			}
			for _, ta := range b.TAs {
				r := base
				r.TAStudentID, r.TAName, r.TALevelTH = ta.StudentID, ta.Name, ta.LevelTH
				rows = append(rows, r)
			}
		}
		out = append(out, CourseSummaryPreviewSheet{CurriculumCode: cur.Code, Sheet: cur.FullNameTH, Rows: rows})
	}
	return out, warnings, months, monthLabels, nil
}
