package service

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

/* -------------------------------------------------------------------------- */
/* Fixture — self-contained, like csFixture: this export spans several       */
/* courses and needs finance_sent/citizen-id machinery csFixture has no use  */
/* for.                                                                       */
/* -------------------------------------------------------------------------- */

type tcFixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	svc    *ExportService
	docs   *DocsService
	termID uuid.UUID
	lectID uuid.UUID
	// ym is "2569-MM" for the current calendar month — the same convention
	// fixture_test.go's addSubmissionPeriod uses. Only the MM half is ever
	// joined against (to_char(wl.work_date,'MM')), so the BE year prefix is
	// just a label.
	ym string
}

func newTCFixture(t *testing.T) *tcFixture {
	t.Helper()
	pool := testutil.NewPool(t)
	ctx := context.Background()
	cipher := testPIICipher(t)
	f := &tcFixture{t: t, ctx: ctx, pool: pool, ym: "2569-" + day(1)[5:7]}
	teaching := &TeachingService{pool: pool, aud: audit.New(pool)}
	f.docs = &DocsService{pool: pool, aud: audit.New(pool), store: newMemStore(), pii: cipher}
	f.svc = &ExportService{
		pool: pool, aud: audit.New(pool),
		budget: &BudgetService{pool: pool}, teaching: teaching,
		users: &UserService{pool: pool, aud: audit.New(pool)}, docs: f.docs,
	}
	// A small, fully-known rate table: every constant below is chosen so the
	// tests can predict exact baht figures by hand, not just "some cutoff
	// happened".
	f.exec(`INSERT INTO pay_rates (
	            effective_from, undergrad_regular, undergrad_special,
	            graduate_regular, graduate_special_lumpsum, graduate_regular_hourly,
	            grad_special_term_cap, term_months,
	            daily_pay_cap_baht, ug_regular_daily_hour_cap, ug_special_daily_hour_cap,
	            grad_regular_daily_hour_cap,
	            ug_lecture_hours_per_credit, ug_lab_hours_per_credit,
	            baseline_students_lecture, baseline_students_lab, ug_workload_rate_regular)
	        VALUES ('2020-01-01', 50, 60, 300, 1000, 50,
	                12000, 4,
	                100000, 24, 24, 24,
	                1, 1, 30, 30, 10)`)
	f.termID = uuid.New()
	f.exec(`INSERT INTO academic_terms (id, academic_year, semester, starts_on, ends_on, is_active, months)
	        VALUES ($1, 2569, 1, '2026-06-01', '2026-10-31', TRUE, 4)`, f.termID)
	f.exec(`INSERT INTO curricula (code, sheet_name, full_name_th, level, sort_order)
	        VALUES ('CY','CY','ทดสอบหลักสูตร CY','undergrad',1)
	        ON CONFLICT (code) DO NOTHING`)
	f.lectID = f.insertUser("lecturer", "lecturer")
	return f
}

func (f *tcFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec: %v\nSQL: %s", err, sql)
	}
}

func (f *tcFixture) insertUser(role, tag string) uuid.UUID {
	id := uuid.New()
	f.exec(`INSERT INTO users (id, email, first_name, last_name, is_active, profile_completed, study_level)
	        VALUES ($1, $2, $3, 'ทดสอบ', TRUE, TRUE, 'undergrad')`,
		id, fmt.Sprintf("%s-%s@example.test", tag, id), tag)
	f.exec(`INSERT INTO user_roles (user_id, role) VALUES ($1, $2::role_code)`, id, role)
	return id
}

func (f *tcFixture) actor() uuid.UUID { return f.insertUser("staff", "staff") }

type tcCourseOpts struct {
	Code, Curriculum string
	LectureHrs       int // drives the regular-track budget cap; see newTCFixture's rate table
	NumRegular       int
	// Level defaults to "undergrad" when empty — set to "graduate" to test
	// Phase 4 routing (printCurriculumCode).
	Level string
}

// insertCourse builds a course with BOTH a regular and a special section, so
// a single TA can be assigned to either (or both) independently.
func (f *tcFixture) insertCourse(o tcCourseOpts) (courseID uuid.UUID, regularSec, specialSec uuid.UUID) {
	courseID = uuid.New()
	numReg := o.NumRegular
	if numReg == 0 {
		numReg = 30
	}
	level := o.Level
	if level == "" {
		level = "undergrad"
	}
	f.exec(`INSERT INTO teaching_courses
	          (id, term_id, code, name_th, level, credits, lecture_hrs, lab_hrs, self_hrs,
	           num_students, num_students_regular, num_students_special, starts_on, ends_on)
	        VALUES ($1,$2,$3,$4,$5,3,$6,0,0,$7,$7,0,'2026-06-01','2026-10-31')`,
		courseID, f.termID, o.Code, o.Code+" ทดสอบ", level, o.LectureHrs, numReg)
	f.exec(`INSERT INTO teaching_lecturers (teaching_course_id, lecturer_id, is_primary)
	        VALUES ($1, $2, TRUE)`, courseID, f.lectID)
	regularSec = uuid.New()
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track, curriculum)
	        VALUES ($1, $2, '1', 'regular', $3)`, regularSec, courseID, o.Curriculum)
	specialSec = uuid.New()
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track, curriculum)
	        VALUES ($1, $2, '2', 'special', $3)`, specialSec, courseID, o.Curriculum)
	return courseID, regularSec, specialSec
}

// assignTA creates (or reuses) a TA and gives them an approved assignment on
// sectionID, then logs one approved 1-hour work_log per day offset in days —
// each becomes its own คาบ for the settlement cutoff to walk.
func (f *tcFixture) assignTA(taID uuid.UUID, courseID, sectionID uuid.UUID, level string, days []int) uuid.UUID {
	f.t.Helper()
	reqID := uuid.New()
	f.exec(`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at, decided_at)
	        VALUES ($1,$2,$3,'both'::reimburse_scope,'approved'::ta_request_status, NOW(), NOW())`,
		reqID, courseID, f.lectID)
	assignID := uuid.New()
	f.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
	        VALUES ($1,$2,$3,$4,$5::study_level)`, assignID, reqID, sectionID, taID, level)
	for _, d := range days {
		start := fmt.Sprintf("%02d:00", 8+d%10)
		end := fmt.Sprintf("%02d:00", 9+d%10)
		f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
		        VALUES (gen_random_uuid(), $1, $2::date, $3, $4, 1, 'lecture', 'approved')`,
			assignID, day(d), start, end)
	}
	return assignID
}

func (f *tcFixture) newTA(name, level string) uuid.UUID {
	id := f.insertUser("ta", "ta")
	f.exec(`UPDATE users SET first_name = $2, last_name = 'ทดสอบ', student_id = $3, study_level = $4::study_level WHERE id = $1`,
		id, name, "65"+id.String()[:9], level)
	return id
}

// ensurePeriod creates the submission_periods row for f.ym if it does not
// already exist — staff create these at the start of a term regardless of
// finance status, so a "not yet finance_sent" test needs one to exist without
// financeSend also flipping every status to finance_sent.
func (f *tcFixture) ensurePeriod() uuid.UUID {
	f.t.Helper()
	var spID uuid.UUID
	err := f.pool.QueryRow(f.ctx, `SELECT id FROM submission_periods WHERE term_id=$1 AND year_month=$2`,
		f.termID, f.ym).Scan(&spID)
	if err != nil {
		spID = uuid.New()
		f.exec(`INSERT INTO submission_periods (id, term_id, year_month, due_date, label, starts_on, is_closed)
		        VALUES ($1, $2, $3, '2026-12-31'::date, $4, '2026-06-01'::date, FALSE)`,
			spID, f.termID, f.ym, "รอบ "+f.ym)
	}
	return spID
}

func (f *tcFixture) financeSend(courseID uuid.UUID) {
	f.t.Helper()
	spID := f.ensurePeriod()
	rows, err := f.pool.Query(f.ctx, `
		SELECT DISTINCT a.ta_id
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		JOIN sections sec ON sec.id = a.section_id
		WHERE sec.teaching_course_id = $1`, courseID)
	if err != nil {
		f.t.Fatalf("query tas: %v", err)
	}
	var taIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			f.t.Fatal(err)
		}
		taIDs = append(taIDs, id)
	}
	rows.Close()
	for _, taID := range taIDs {
		f.exec(`INSERT INTO submission_period_status (id, submission_period_id, ta_id, teaching_course_id, status)
		        VALUES (gen_random_uuid(), $1, $2, $3, 'finance_sent')
		        ON CONFLICT (submission_period_id, ta_id, teaching_course_id)
		        DO UPDATE SET status = 'finance_sent'`, spID, taID, courseID)
	}
}

// storeCitizenID encrypts nationalID for taID via the one write path
// (DocsService.storeCitizenID), inside its own transaction — ta_profiles must
// exist first (UpsertProfile's own precondition in production).
func (f *tcFixture) storeCitizenID(taID uuid.UUID, nationalID string) {
	f.t.Helper()
	f.exec(`INSERT INTO ta_profiles (user_id, status, current_round) VALUES ($1,'pending',1)
	        ON CONFLICT (user_id) DO NOTHING`, taID)
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if err := f.docs.storeCitizenID(f.ctx, tx, taID, nationalID); err != nil {
		f.t.Fatalf("storeCitizenID: %v", err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		f.t.Fatal(err)
	}
}

func openWorkbookBytes(t *testing.T, body []byte) *excelize.File {
	t.Helper()
	f, err := excelize.OpenReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

/* -------------------------------------------------------------------------- */
/* Tests                                                                      */
/* -------------------------------------------------------------------------- */

// A TA who works the regular section of one course and the special section of
// another (same curriculum) must appear on BOTH sheets, each with only that
// track's own money — never merged into one row the way a single-course claim
// would.
func TestBuildTransferCoverSheets_DualTrackSplitsAcrossSheets(t *testing.T) {
	f := newTCFixture(t)
	courseA, regA, _ := f.insertCourse(tcCourseOpts{Code: "CP111", Curriculum: "CY", LectureHrs: 100})
	courseB, _, specB := f.insertCourse(tcCourseOpts{Code: "CP222", Curriculum: "CY", LectureHrs: 100})

	ta := f.newTA("สมชาย รวยดี", "undergrad")
	f.assignTA(ta, courseA, regA, "undergrad", []int{1})  // 1 hour regular @ 50 = 50 baht
	f.assignTA(ta, courseB, specB, "undergrad", []int{2}) // 1 hour special @ 60 = 60 baht
	f.financeSend(courseA)
	f.financeSend(courseB)

	sheets, warnings, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID, nil, "undergrad")
	if err != nil {
		t.Fatalf("buildTransferCoverSheets: %v", err)
	}
	_ = warnings

	var regSheet, specSheet *transferCoverSheet
	for i := range sheets {
		if sheets[i].Track == "regular" {
			regSheet = &sheets[i]
		} else {
			specSheet = &sheets[i]
		}
	}
	if regSheet == nil || specSheet == nil {
		t.Fatalf("expected both a regular and a special sheet, got %d sheets", len(sheets))
	}
	if len(regSheet.Rows) != 1 || regSheet.Rows[0].Baht != 50 {
		t.Fatalf("regular sheet = %+v, want one row of 50", regSheet.Rows)
	}
	if regSheet.Rows[0].Courses != "CP111" {
		t.Errorf("regular sheet course list = %q, want CP111", regSheet.Rows[0].Courses)
	}
	if len(specSheet.Rows) != 1 || specSheet.Rows[0].Baht != 60 {
		t.Fatalf("special sheet = %+v, want one row of 60", specSheet.Rows)
	}
	if specSheet.Rows[0].Courses != "CP222" {
		t.Errorf("special sheet course list = %q, want CP222", specSheet.Rows[0].Courses)
	}
}

// The money on ใบ B must come from the SAME คาบ cutoff SettleCourse applies —
// never the raw, uncapped hourly total. A mutation that summed claimCostByTASlot
// directly (skipping unpaidFrom) would pay 150 here instead of the true 50.
func TestBuildTransferCoverSheets_UsesSettledCutoffNotRawTotal(t *testing.T) {
	f := newTCFixture(t)
	// LectureHrs=7, students=30=baseline → weekly=7×1×1=7; monthly=7×10=70;
	// term_months=4 → regular cap = 70×4 = 280... too big for a small demo,
	// so months is overridden to 1 for THIS course's term via a dedicated term.
	f.termID = uuid.New()
	f.exec(`INSERT INTO academic_terms (id, academic_year, semester, starts_on, ends_on, is_active, months)
	        VALUES ($1, 2569, 2, '2026-06-01', '2026-10-31', FALSE, 1)`, f.termID)
	f.exec(`INSERT INTO curricula (code, sheet_name, full_name_th, level, sort_order)
	        VALUES ('CY','CY','ทดสอบหลักสูตร CY','undergrad',1) ON CONFLICT (code) DO NOTHING`)
	courseA, regA, _ := f.insertCourse(tcCourseOpts{Code: "CP111", Curriculum: "CY", LectureHrs: 7})
	// cap = weekly(7) × rate(10) × months(1) = 70.

	ta := f.newTA("สมหญิง เก่งมาก", "undergrad")
	// Three 1-hour คาบ at 50 baht each: first fits (remaining 70→20), second
	// (50) does not (20 < 50.01) → cutoff there, third also unpaid.
	f.assignTA(ta, courseA, regA, "undergrad", []int{1, 2, 3})
	f.financeSend(courseA)

	sheets, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID, nil, "undergrad")
	if err != nil {
		t.Fatalf("buildTransferCoverSheets: %v", err)
	}
	if len(sheets) != 1 {
		t.Fatalf("got %d sheets, want 1 (regular only)", len(sheets))
	}
	if len(sheets[0].Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(sheets[0].Rows))
	}
	got := sheets[0].Rows[0].Baht
	if got != 50 {
		t.Errorf("baht = %v, want 50 (only the first คาบ fit the 70-baht cap; raw total would be 150)", got)
	}
}

// A graduate TA's special-track pay is a flat term lump (never priced hourly
// by claimCostByTASlot), so it must still show up on the special sheet even
// though every hourly คาบ they logged prices at zero.
// graduate_special_lumpsum is the whole-term-per-course figure (2026 meeting
// correction) — it must NOT be multiplied by term_months. With the fixture's
// rate table (lumpsum=1000, cap=12000) the flat amount is just 1000, not
// 1000×4=4000 the way the old (buggy) formula computed it.
func TestBuildTransferCoverSheets_GradSpecialLumpIsFlatNotMultipliedByMonths(t *testing.T) {
	f := newTCFixture(t)
	courseID, _, specSec := f.insertCourse(tcCourseOpts{Code: "CP888", Curriculum: "CY", LectureHrs: 100})

	ta := f.newTA("นายบัณฑิต เรียนดี", "master")
	// No work_logs at all — grad-special TAs no longer log anything themselves;
	// eligibility must be the approved assignment alone.
	f.assignTA(ta, courseID, specSec, "master", nil)
	f.financeSend(courseID)

	sheets, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID, nil, "graduate")
	if err != nil {
		t.Fatalf("buildTransferCoverSheets: %v", err)
	}
	if len(sheets) != 1 || sheets[0].Track != "special" {
		t.Fatalf("expected exactly one special sheet, got %+v", sheets)
	}
	want := 1000.0 // graduate_special_lumpsum, flat — not × term_months
	if sheets[0].Rows[0].Baht != want {
		t.Errorf("baht = %v, want the flat lump %v", sheets[0].Rows[0].Baht, want)
	}
}

// Two special-track courses in the same term must each pay their own flat
// lump independently — there is no cross-course aggregate cap (a TA on 2
// courses gets 2×lump, not a shared 12,000 split between them).
func TestBuildTransferCoverSheets_GradSpecialLumpIsPerCourseNotAggregated(t *testing.T) {
	f := newTCFixture(t)
	c1, _, spec1 := f.insertCourse(tcCourseOpts{Code: "CP888", Curriculum: "CY", LectureHrs: 100})
	c2, _, spec2 := f.insertCourse(tcCourseOpts{Code: "CP889", Curriculum: "CY", LectureHrs: 100})

	ta := f.newTA("นายบัณฑิต สองวิชา", "master")
	f.assignTA(ta, c1, spec1, "master", nil)
	f.assignTA(ta, c2, spec2, "master", nil)
	f.financeSend(c1)
	f.financeSend(c2)

	sheets, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID, nil, "graduate")
	if err != nil {
		t.Fatalf("buildTransferCoverSheets: %v", err)
	}
	if len(sheets) != 1 || sheets[0].Track != "special" {
		t.Fatalf("expected exactly one special sheet, got %+v", sheets)
	}
	// One row for the TA, courses joined — but the money must be 2×1000, each
	// course's own flat lump, not a shared/capped total.
	want := 2000.0
	if sheets[0].Rows[0].Baht != want {
		t.Errorf("baht = %v, want %v (2 independent per-course lumps)", sheets[0].Rows[0].Baht, want)
	}
}

// A row that nets to zero (every คาบ it touched fell off the budget cutoff)
// must not appear at all — a 0.00 line is not something finance transfers.
func TestBuildTransferCoverSheets_ZeroNetRowExcluded(t *testing.T) {
	f := newTCFixture(t)
	f.termID = uuid.New()
	f.exec(`INSERT INTO academic_terms (id, academic_year, semester, starts_on, ends_on, is_active, months)
	        VALUES ($1, 2569, 2, '2026-06-01', '2026-10-31', FALSE, 1)`, f.termID)
	f.exec(`INSERT INTO curricula (code, sheet_name, full_name_th, level, sort_order)
	        VALUES ('CY','CY','ทดสอบหลักสูตร CY','undergrad',1) ON CONFLICT (code) DO NOTHING`)
	// weekly = 1×1×1 = 1; monthly = 1×10 = 10; cap = 10×1 = 10 — smaller than
	// even the first 50-baht คาบ, so nothing at all is paid.
	courseA, regA, _ := f.insertCourse(tcCourseOpts{Code: "CP333", Curriculum: "CY", LectureHrs: 1})
	ta := f.newTA("นายไม่ได้เงิน สักบาท", "undergrad")
	f.assignTA(ta, courseA, regA, "undergrad", []int{1})
	f.financeSend(courseA)

	sheets, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID, nil, "undergrad")
	if err != nil {
		t.Fatalf("buildTransferCoverSheets: %v", err)
	}
	if len(sheets) != 0 {
		t.Fatalf("expected no sheets (the only row nets to zero), got %+v", sheets)
	}
}

// The workbook's total row must be a real SUM formula over exactly the printed
// rows, not a hardcoded figure — and the rendered cell values must match what
// buildTransferCoverSheets computed.
func TestBuildTransferCoverWorkbook_TotalIsARealFormula(t *testing.T) {
	f := newTCFixture(t)
	courseA, regA, _ := f.insertCourse(tcCourseOpts{Code: "CP111", Curriculum: "CY", LectureHrs: 100})
	// Named so alphabetical sort is unambiguous: ก precedes ย in Thai order,
	// so ta1 must land on row 6 regardless of insertion order.
	ta1 := f.newTA("กมล ทดสอบ", "undergrad")
	ta2 := f.newTA("ยุพา ทดสอบ", "undergrad")
	f.assignTA(ta1, courseA, regA, "undergrad", []int{1})
	f.assignTA(ta2, courseA, regA, "undergrad", []int{2})
	f.financeSend(courseA)
	f.storeCitizenID(ta1, "1234567890123")

	actor := f.actor()
	body, warnings, err := f.svc.BuildTransferCoverWorkbook(f.ctx, actor, f.termID, nil, "undergrad")
	if err != nil {
		t.Fatalf("BuildTransferCoverWorkbook: %v\nwarnings: %v", err, warnings)
	}
	wb := openWorkbookBytes(t, body)
	defer wb.Close()

	sheetName := "CY ปกติ"
	// header(5) + 2 data rows(6,7) + 1 blank spacer(8) → total at row 9.
	formula, err := wb.GetCellFormula(sheetName, "D9")
	if err != nil {
		t.Fatal(err)
	}
	if formula != "SUM(D6:D7)" {
		t.Errorf("total formula = %q, want SUM(D6:D7)", formula)
	}
	name, _ := wb.GetCellValue(sheetName, "B6")
	promptpay, _ := wb.GetCellValue(sheetName, "E6")
	if name != "กมล ทดสอบ ทดสอบ" {
		t.Errorf("B6 = %q", name)
	}
	if promptpay != "1234567890123" {
		t.Errorf("promptpay column = %q, want the decrypted citizen id", promptpay)
	}
	promptpay2, _ := wb.GetCellValue(sheetName, "E7")
	if promptpay2 != "" {
		t.Errorf("TA without a stored citizen id must print a blank promptpay cell, got %q", promptpay2)
	}
}

// The หมายเหตุ (ใหม่/เก่า) column must reflect TASeniority — not print blank
// for every row, which is what happened before this test existed (the field
// was declared on transferCoverRow but nothing ever set it).
func TestBuildTransferCoverWorkbook_SeniorityColumnPopulated(t *testing.T) {
	f := newTCFixture(t)
	courseA, regA, _ := f.insertCourse(tcCourseOpts{Code: "CP111", Curriculum: "CY", LectureHrs: 100})
	newTA := f.newTA("กมล ใหม่เอี่ยม", "undergrad")
	oldTA := f.newTA("ยุพา เก่าแก่", "undergrad")
	f.exec(`UPDATE users SET ta_first_term_id = $2 WHERE id = $1`, newTA, f.termID)
	f.assignTA(newTA, courseA, regA, "undergrad", []int{1})
	f.assignTA(oldTA, courseA, regA, "undergrad", []int{2})
	f.financeSend(courseA)

	body, warnings, err := f.svc.BuildTransferCoverWorkbook(f.ctx, f.actor(), f.termID, nil, "undergrad")
	if err != nil {
		t.Fatalf("BuildTransferCoverWorkbook: %v\nwarnings: %v", err, warnings)
	}
	wb := openWorkbookBytes(t, body)
	defer wb.Close()

	sheetName := "CY ปกติ"
	// Sorted by name: "กมล..." (row 6) before "ยุพา..." (row 7).
	newRow, _ := wb.GetCellValue(sheetName, "F6")
	oldRow, _ := wb.GetCellValue(sheetName, "F7")
	if newRow != "ใหม่" {
		t.Errorf("F6 (seniority for a TA whose ta_first_term_id is this term) = %q, want ใหม่", newRow)
	}
	if oldRow != "เก่า" {
		t.Errorf("F7 (seniority for a TA with no ta_first_term_id stamp) = %q, want เก่า", oldRow)
	}
}

// The gate must be term-wide: one course still short of finance_sent blocks
// the whole document, even if every other course is fully done.
func TestTermExportBlockers_RejectsWhenOneMonthNotFinanceSent(t *testing.T) {
	f := newTCFixture(t)
	courseA, regA, _ := f.insertCourse(tcCourseOpts{Code: "CP111", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("นายค้าง จ่ายเงิน", "undergrad")
	f.assignTA(ta, courseA, regA, "undergrad", []int{1})
	// The period exists (staff always create these up front) but its status
	// was never advanced to finance_sent — the case that must still block.
	f.ensurePeriod()

	blockers, err := f.svc.TermExportBlockers(f.ctx, f.termID, nil, "undergrad")
	if err != nil {
		t.Fatalf("TermExportBlockers: %v", err)
	}
	if len(blockers) == 0 {
		t.Fatal("expected at least one blocker for the un-sent course")
	}
	found := false
	for _, b := range blockers {
		if b.CourseCode == "CP111" && b.Kind == "not_finance_sent" {
			found = true
		}
	}
	if !found {
		t.Errorf("blockers = %+v, want a not_finance_sent entry tagged CP111", blockers)
	}

	if _, _, err := f.svc.BuildTransferCoverWorkbook(f.ctx, f.actor(), f.termID, nil, "undergrad"); err == nil {
		t.Error("BuildTransferCoverWorkbook must refuse while the gate is open")
	}
}

// Once every course reaches finance_sent, the gate opens and the same course
// that blocked above must no longer appear.
func TestTermExportBlockers_ClearsOnceFinanceSent(t *testing.T) {
	f := newTCFixture(t)
	courseA, regA, _ := f.insertCourse(tcCourseOpts{Code: "CP111", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("นายจ่ายครบ แล้ว", "undergrad")
	f.assignTA(ta, courseA, regA, "undergrad", []int{1})
	f.financeSend(courseA)

	blockers, err := f.svc.TermExportBlockers(f.ctx, f.termID, nil, "undergrad")
	if err != nil {
		t.Fatalf("TermExportBlockers: %v", err)
	}
	if len(blockers) != 0 {
		t.Fatalf("expected no blockers once finance_sent, got %+v", blockers)
	}
}

// Phase 4: a graduate-level course tagged curriculum "AI" prints under
// DSAI_GRAD's sheet ("DS&AI ปกติ"), not the undergrad AI sheet — same
// printCurriculumCode routing export_course_summary.go uses, exercised here
// through transferCoverPrintCurricula's own call site.
func TestBuildTransferCoverSheets_GraduateCourseRoutesToGradSheet(t *testing.T) {
	f := newTCFixture(t)
	courseA, regA, _ := f.insertCourse(tcCourseOpts{
		Code: "CP398001", Curriculum: "AI", LectureHrs: 100, Level: "graduate",
	})
	ta := f.newTA("บัณฑิต วิทยาการข้อมูล", "master")
	f.assignTA(ta, courseA, regA, "master", []int{1})
	f.financeSend(courseA)

	sheets, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID, nil, "graduate")
	if err != nil {
		t.Fatalf("buildTransferCoverSheets: %v", err)
	}
	if len(sheets) != 1 {
		t.Fatalf("got %d sheets, want 1", len(sheets))
	}
	if sheets[0].CurriculumCode != "DSAI_GRAD" {
		t.Errorf("curriculum code = %q, want DSAI_GRAD — a graduate AI course must not route to the undergrad AI sheet", sheets[0].CurriculumCode)
	}
	if sheets[0].SheetName != "DS&AI ปกติ" {
		t.Errorf("sheet name = %q, want %q", sheets[0].SheetName, "DS&AI ปกติ")
	}
}

// ป.ตรี and บัณฑิตศึกษา are claimed on SEPARATE documents — the college files
// two different forms, not one form with a level column (their own
// docs/15.CP362104.xlsx vs docs/14. CP363761-บัณฑิต.xls). So the combined book
// carries undergrad TAs and NOBODY else.
//
// A graduate TA appearing here is not a cosmetic slip: this book prices its
// people by the undergrad form's shape, and its หลักฐาน sheet ticks
// ปริญญาตรี/บัณฑิตศึกษา ONCE for the whole sheet, so a mixed course printed the
// wrong level over half its rows — while the same TA was also billed on the
// graduate document, making the course's paperwork claim them twice.
func TestCollectCombinedBook_ExcludesGraduateTAsEntirely(t *testing.T) {
	f := newTCFixture(t)
	courseID, regSec, specSec := f.insertCourse(tcCourseOpts{Code: "CP891", Curriculum: "CY", LectureHrs: 100})

	ug := f.newTA("นายปริญญาตรี ทำงาน", "undergrad")
	f.assignTA(ug, courseID, regSec, "undergrad", []int{10, 11})
	// A grad-regular TA with real logged hours, and a grad-special TA with
	// none at all — neither belongs on the undergrad book.
	gradReg := f.newTA("นายบัณฑิต ลงเวลา", "phd")
	f.assignTA(gradReg, courseID, regSec, "phd", []int{12, 13})
	gradSp := f.newTA("นายบัณฑิต ไม่ลงเวลา", "master")
	f.assignTA(gradSp, courseID, specSec, "master", nil)
	f.financeSend(courseID)

	d, err := f.svc.collectCombinedBook(f.ctx, courseID, nil)
	if err != nil {
		t.Fatalf("collectCombinedBook: %v", err)
	}
	for _, side := range [][]claimant{d.Regular, d.Special} {
		for _, c := range side {
			if c.LevelTH != "ป.ตรี" {
				t.Errorf("graduate claimant %q (%s) is on the undergrad book — "+
					"บัณฑิตศึกษา is claimed on its own form", c.Name, c.LevelTH)
			}
		}
	}
	if len(d.Regular) != 1 {
		t.Fatalf("got %d regular claimants, want the 1 undergrad: %+v", len(d.Regular), d.Regular)
	}
	if len(d.Special) != 0 {
		t.Errorf("got %d special claimants, want 0 — the only special-track TA here is graduate: %+v",
			len(d.Special), d.Special)
	}
}

// The counterweight: a course staffed ONLY by graduate TAs produces no
// undergrad book at all, and that is not an error — its whole claim lives on
// the graduate documents.
func TestBuildCombinedClaimWorkbook_NilForGraduateOnlyCourse(t *testing.T) {
	f := newTCFixture(t)
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP892", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("นายบัณฑิต ลงเวลา", "phd")
	f.assignTA(ta, courseID, regSec, "phd", []int{10, 11})
	f.financeSend(courseID)

	book, err := f.svc.BuildCombinedClaimWorkbook(f.ctx, courseID, nil)
	if err != nil {
		t.Fatalf("a graduate-only course must not error, it must simply have no undergrad book: %v", err)
	}
	if book != nil {
		t.Error("a graduate-only course produced an undergrad claim workbook")
	}
}
