package service

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

/* -------------------------------------------------------------------------- */
/* Fixture — deliberately separate from fixture_test.go's `fixture`, which is */
/* built around ONE course/assignment; this export needs several courses.    */
/* -------------------------------------------------------------------------- */

type csFixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	svc    *ExportService
	termID uuid.UUID
	lectID uuid.UUID
}

func newCSFixture(t *testing.T) *csFixture {
	t.Helper()
	pool := testutil.NewPool(t)
	ctx := context.Background()
	f := &csFixture{t: t, ctx: ctx, pool: pool}
	f.svc = &ExportService{
		pool:     pool,
		aud:      audit.New(pool),
		budget:   &BudgetService{pool: pool},
		teaching: &TeachingService{pool: pool, aud: audit.New(pool)},
	}
	f.exec(`INSERT INTO pay_rates (
	            effective_from, undergrad_regular, undergrad_special,
	            graduate_regular, graduate_special_lumpsum, graduate_regular_hourly,
	            daily_pay_cap_baht, ug_regular_daily_hour_cap, ug_special_daily_hour_cap,
	            grad_regular_daily_hour_cap)
	        VALUES ('2020-01-01', 300, 300, 300, 12000, 50, 100000, 7, 6, 6)`)
	f.termID = uuid.New()
	f.exec(`INSERT INTO academic_terms (id, academic_year, semester, starts_on, ends_on, is_active, months)
	        VALUES ($1, 2569, 1, '2026-06-01', '2026-10-31', TRUE, 4)`, f.termID)
	f.lectID = f.insertUser("lecturer", "lecturer")
	return f
}

// addSubmissionPeriod gives the fixture's term a claimable month — needed for
// anything reading TermMonths (the เบิกจ่ายเดือน/คงเหลือ month picker), which
// otherwise sees an empty term and treats every course as having settled
// nothing, no matter what work_logs say.
func (f *csFixture) addSubmissionPeriod(yearMonth, startsOn, dueDate, label string) {
	f.exec(`INSERT INTO submission_periods (id, term_id, year_month, starts_on, due_date, label)
	        VALUES (gen_random_uuid(), $1, $2, $3, $4, $5)`, f.termID, yearMonth, startsOn, dueDate, label)
}

func (f *csFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec: %v\nSQL: %s", err, sql)
	}
}

func (f *csFixture) insertUser(role, tag string) uuid.UUID {
	id := uuid.New()
	f.exec(`INSERT INTO users (id, email, first_name, last_name, is_active, profile_completed, study_level)
	        VALUES ($1, $2, $3, 'ทดสอบ', TRUE, TRUE, 'undergrad')`,
		id, fmt.Sprintf("%s-%s@example.test", tag, id), tag)
	f.exec(`INSERT INTO user_roles (user_id, role) VALUES ($1, $2::role_code)`, id, role)
	return id
}

type csCourseOpts struct {
	Code                                 string
	NameTH                               string
	Credits, LectureHrs, LabHrs, SelfHrs int
	NumRegular, NumSpecial               int
	Curriculum                           string
	// Level defaults to "undergrad" when empty — set to "graduate" to test
	// Phase 4 routing (printCurriculumCode).
	Level string
}

// insertCourse builds a course with one regular section carrying the given
// curriculum, and wires the fixture's lecturer as primary.
func (f *csFixture) insertCourse(o csCourseOpts) uuid.UUID {
	id := uuid.New()
	level := o.Level
	if level == "" {
		level = "undergrad"
	}
	f.exec(`INSERT INTO teaching_courses
	          (id, term_id, code, name_th, level, credits, lecture_hrs, lab_hrs, self_hrs,
	           num_students, num_students_regular, num_students_special, starts_on, ends_on)
	        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'2026-06-01','2026-10-31')`,
		id, f.termID, o.Code, o.NameTH, level, o.Credits, o.LectureHrs, o.LabHrs, o.SelfHrs,
		o.NumRegular+o.NumSpecial, o.NumRegular, o.NumSpecial)
	f.exec(`INSERT INTO teaching_lecturers (teaching_course_id, lecturer_id, is_primary)
	        VALUES ($1, $2, TRUE)`, id, f.lectID)
	secID := uuid.New()
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track, curriculum)
	        VALUES ($1, $2, '1', 'regular', $3)`, secID, id, o.Curriculum)
	return id
}

// addApprovedTA assigns a TA to courseID's one section with an approved
// request, and declares attendanceHrs/labHrs on the workload form (what
// claimKindLabel reads).
func (f *csFixture) addApprovedTA(courseID uuid.UUID, name, level string, attendanceHrs, labHrs float64) uuid.UUID {
	taID := f.insertUser("ta", "ta")
	f.exec(`UPDATE users SET first_name = $2, last_name = 'ทดสอบ', student_id = $3, study_level = $4::study_level WHERE id = $1`,
		taID, name, "65"+taID.String()[:9], level)
	var secID uuid.UUID
	if err := f.pool.QueryRow(f.ctx, `SELECT id FROM sections WHERE teaching_course_id = $1 LIMIT 1`, courseID).Scan(&secID); err != nil {
		f.t.Fatalf("find section: %v", err)
	}
	reqID := uuid.New()
	f.exec(`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at, decided_at)
	        VALUES ($1,$2,$3,'both'::reimburse_scope,'approved'::ta_request_status, NOW(), NOW())`,
		reqID, courseID, f.lectID)
	assignID := uuid.New()
	f.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
	        VALUES ($1,$2,$3,$4,$5::study_level)`, assignID, reqID, secID, taID, level)
	f.exec(`INSERT INTO ta_workload_forms (id, assignment_id, attendance_hrs, lab_hrs)
	        VALUES (gen_random_uuid(), $1, $2, $3)`, assignID, attendanceHrs, labHrs)
	return taID
}

// insertSubmittedRequest records a lecturer's TA request with no decision and
// no assignment yet — the "submitted, still waiting" state, distinct from a
// course that never had a request submitted at all.
func (f *csFixture) insertSubmittedRequest(courseID uuid.UUID) {
	f.exec(`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at)
	        VALUES (gen_random_uuid(),$1,$2,'both'::reimburse_scope,'submitted'::ta_request_status, NOW())`,
		courseID, f.lectID)
}

func (f *csFixture) actor() uuid.UUID {
	return f.insertUser("staff", "staff")
}

func (f *csFixture) build() ([]byte, []string) {
	f.t.Helper()
	body, warnings, err := f.svc.BuildCourseSummaryWorkbook(f.ctx, f.actor(), f.termID, nil)
	if err != nil {
		f.t.Fatalf("BuildCourseSummaryWorkbook: %v", err)
	}
	return body, warnings
}

func openWorkbook(t *testing.T, body []byte) *excelize.File {
	t.Helper()
	f, err := excelize.OpenReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestBuildCourseSummaryWorkbook_BasicCourseWithMoney(t *testing.T) {
	f := newCSFixture(t)
	courseID := f.insertCourse(csCourseOpts{
		Code: "CP100001", NameTH: "Test Course", Credits: 3, LectureHrs: 3, LabHrs: 3, SelfHrs: 6,
		NumRegular: 60, NumSpecial: 30, Curriculum: "CS",
	})
	f.addSubmissionPeriod("2569-06", "2026-06-01", "2026-07-31", "มิถุนายน 2569")
	f.addApprovedTA(courseID, "สมชาย", "undergrad", 3, 0)

	snap, err := f.svc.budget.Compute(f.ctx, courseID)
	if err != nil {
		t.Fatal(err)
	}

	body, warnings := f.build()
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	wb := openWorkbook(t, body)
	defer wb.Close()

	if got := wb.GetSheetList(); len(got) != 1 || got[0] != "CS" {
		t.Fatalf("sheets = %v, want exactly [CS]", got)
	}
	if got, _ := wb.GetCellValue("CS", "B5"); got != "CP100001" {
		t.Errorf("B5 (code) = %q, want CP100001", got)
	}
	if got, _ := wb.GetCellValue("CS", "C5"); got != "Test Course" {
		t.Errorf("C5 (name) = %q, want Test Course", got)
	}
	if got, _ := wb.GetCellValue("CS", "D5"); got != "3 (3-3-6)" {
		t.Errorf("D5 (credit text) = %q, want 3 (3-3-6)", got)
	}
	if got, _ := wb.GetCellValue("CS", "K5"); got != "(Lec.)" {
		t.Errorf("K5 (claim kind) = %q, want (Lec.) — only attendance_hrs was declared", got)
	}
	if got, _ := wb.GetCellValue("CS", "F5"); got == "" {
		t.Error("F5 (TA student id) is empty")
	}
	if got, _ := wb.GetCellValue("CS", "G5"); got != "สมชาย ทดสอบ" {
		t.Errorf("G5 (TA name) = %q, want สมชาย ทดสอบ", got)
	}

	// ขออนุมัติเบิกจ่าย — the course's BUDGET per track.
	assertMoneyCell(t, wb, "CS", "L5", snap.TermPayRegular)
	assertMoneyCell(t, wb, "CS", "M5", snap.TermPaySpecial)
	// เบิกจ่ายเดือน/คงเหลือ (added back 11/09/2026): no work has been approved
	// yet for this TA, so nothing has cleared — เบิกจ่ายเดือน reads 0 and
	// คงเหลือ reads the full budget, unchanged from L/M.
	assertMoneyCell(t, wb, "CS", "N5", 0)
	assertMoneyCell(t, wb, "CS", "O5", 0)
	assertFormulaCell(t, wb, "CS", "P5", "L5-N5")
	assertFormulaCell(t, wb, "CS", "Q5", "M5-O5")
}

func assertMoneyCell(t *testing.T, wb *excelize.File, sheet, cell string, want float64) {
	t.Helper()
	v, err := wb.GetCellValue(sheet, cell)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseFloatLoose(v)
	if err != nil {
		t.Fatalf("%s!%s = %q, not a number: %v", sheet, cell, v, err)
	}
	if diff := got - want; diff > 0.01 || diff < -0.01 {
		t.Errorf("%s!%s = %v, want %v", sheet, cell, got, want)
	}
}

// assertFormulaCell checks that a cell holds a live formula (excelize does
// not compute a cached result for a cell it wrote itself, so GetCellValue
// on Q/R reads back empty — the formula text is what's checkable) that
// matches wantExpr exactly.
func assertFormulaCell(t *testing.T, wb *excelize.File, sheet, cell, wantExpr string) {
	t.Helper()
	got, err := wb.GetCellFormula(sheet, cell)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantExpr {
		t.Errorf("%s!%s formula = %q, want %q", sheet, cell, got, wantExpr)
	}
}

// parseFloatLoose parses what GetCellValue renders through the accounting
// number format (money cells carry one, e.g. "21,600.00") — the thousands
// separator would otherwise stop fmt.Sscanf's %g at the first comma.
func parseFloatLoose(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(strings.ReplaceAll(s, ",", ""), "%g", &f)
	return f, err
}

// The whole point of "รวมทั้งหมด" is that it recomputes if a cell above it
// changes — a literal value copied in at build time would silently go stale.
func TestBuildCourseSummaryWorkbook_TotalRowIsARealFormula(t *testing.T) {
	f := newCSFixture(t)
	courseID := f.insertCourse(csCourseOpts{
		Code: "CP100002", NameTH: "X", Credits: 3, LectureHrs: 3, LabHrs: 3, SelfHrs: 6,
		NumRegular: 40, Curriculum: "CS",
	})
	f.addApprovedTA(courseID, "TA1", "undergrad", 3, 0)

	body, _ := f.build()
	wb := openWorkbook(t, body)
	defer wb.Close()

	// One course block occupies rows 5-5 (single TA), so the total row is 6.
	formula, err := wb.GetCellFormula("CS", "L6")
	if err != nil {
		t.Fatal(err)
	}
	if formula == "" || formula[:3] != "SUM" {
		t.Errorf("L6 formula = %q, want a SUM(...) formula", formula)
	}
}

// The staff interview's explicit rule: a confirmed dual-code group's money is
// the SUM of each member's own figures, never a value recomputed from merged
// student counts.
func TestBuildCourseSummaryWorkbook_DualCodeGroupSumsMoney(t *testing.T) {
	f := newCSFixture(t)
	courseA := f.insertCourse(csCourseOpts{
		Code: "CP200001", NameTH: "Internetworking", Credits: 3, LectureHrs: 2, LabHrs: 2, SelfHrs: 5,
		NumRegular: 35, NumSpecial: 35, Curriculum: "CS",
	})
	courseB := f.insertCourse(csCourseOpts{
		Code: "SC200001", NameTH: "Internetworking", Credits: 3, LectureHrs: 2, LabHrs: 2, SelfHrs: 5,
		NumRegular: 20, NumSpecial: 10, Curriculum: "IT",
	})
	f.addApprovedTA(courseA, "TA-A", "undergrad", 2, 2)
	f.addApprovedTA(courseB, "TA-B", "undergrad", 2, 2)

	snapA, err := f.svc.budget.Compute(f.ctx, courseA)
	if err != nil {
		t.Fatal(err)
	}
	snapB, err := f.svc.budget.Compute(f.ctx, courseB)
	if err != nil {
		t.Fatal(err)
	}
	wantRegular := snapA.TermPayRegular + snapB.TermPayRegular
	wantSpecial := snapA.TermPaySpecial + snapB.TermPaySpecial

	if _, err := f.svc.teaching.ConfirmCourseGroup(f.ctx, f.actor(), f.termID, courseA, []uuid.UUID{courseA, courseB}, "CS"); err != nil {
		t.Fatalf("ConfirmCourseGroup: %v", err)
	}

	body, _ := f.build()
	wb := openWorkbook(t, body)
	defer wb.Close()

	if got := wb.GetSheetList(); len(got) != 1 || got[0] != "CS" {
		t.Fatalf("sheets = %v, want exactly [CS] — the group must print under its confirmed curriculum, not under IT too", got)
	}
	if got, _ := wb.GetCellValue("CS", "B5"); got != "CP200001/SC200001" {
		t.Errorf("B5 (merged code) = %q, want CP200001/SC200001", got)
	}
	assertMoneyCell(t, wb, "CS", "L5", wantRegular)
	assertMoneyCell(t, wb, "CS", "M5", wantSpecial)

	// Both TAs must appear — merging money must not merge away roster rows.
	f5, _ := wb.GetCellValue("CS", "F5")
	f6, _ := wb.GetCellValue("CS", "F6")
	if f5 == "" || f6 == "" {
		t.Errorf("expected two TA rows under the merged block, got F5=%q F6=%q", f5, f6)
	}
}

// A TA who (for whatever reason) holds a separate approved assignment under
// EACH code in a merged group must still print once per group, not twice.
func TestBuildCourseSummaryWorkbook_DedupsSameTAAcrossGroupMembers(t *testing.T) {
	f := newCSFixture(t)
	courseA := f.insertCourse(csCourseOpts{
		Code: "CP300001", NameTH: "Shared Class", Credits: 3, LectureHrs: 2, LabHrs: 2, SelfHrs: 5,
		NumRegular: 30, Curriculum: "CS",
	})
	courseB := f.insertCourse(csCourseOpts{
		Code: "SC300001", NameTH: "Shared Class", Credits: 3, LectureHrs: 2, LabHrs: 2, SelfHrs: 5,
		NumRegular: 15, Curriculum: "IT",
	})
	// Same TA, two separate assignments (one per registrar code).
	taID := f.insertUser("ta", "shared-ta")
	f.exec(`UPDATE users SET first_name = 'SharedTA', student_id = $2, study_level = 'undergrad' WHERE id = $1`,
		taID, "65"+taID.String()[:9])
	for _, courseID := range []uuid.UUID{courseA, courseB} {
		var secID uuid.UUID
		if err := f.pool.QueryRow(f.ctx, `SELECT id FROM sections WHERE teaching_course_id=$1`, courseID).Scan(&secID); err != nil {
			t.Fatal(err)
		}
		reqID := uuid.New()
		f.exec(`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at, decided_at)
		        VALUES ($1,$2,$3,'both'::reimburse_scope,'approved'::ta_request_status,NOW(),NOW())`,
			reqID, courseID, f.lectID)
		assignID := uuid.New()
		f.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
		        VALUES ($1,$2,$3,$4,'undergrad'::study_level)`, assignID, reqID, secID, taID)
		f.exec(`INSERT INTO ta_workload_forms (id, assignment_id, attendance_hrs) VALUES (gen_random_uuid(),$1,2)`, assignID)
	}

	if _, err := f.svc.teaching.ConfirmCourseGroup(f.ctx, f.actor(), f.termID, courseA, []uuid.UUID{courseA, courseB}, "CS"); err != nil {
		t.Fatal(err)
	}

	body, _ := f.build()
	wb := openWorkbook(t, body)
	defer wb.Close()

	f5, _ := wb.GetCellValue("CS", "F5")
	if f5 == "" {
		t.Fatal("expected at least one TA row")
	}
	// A single deduped TA row puts the total at row 6 (row 6's "รวมทั้งหมด"
	// merge is why F6 isn't checked directly — it would read the merge's
	// anchor value regardless). If dedup failed and the TA printed twice, the
	// total would land at row 7 instead, and L6 would carry no formula at all.
	formula, err := wb.GetCellFormula("CS", "L6")
	if err != nil {
		t.Fatal(err)
	}
	if formula == "" {
		t.Error("expected the total row at L6 (one deduped TA row) — got no formula there, suggesting the TA printed twice")
	}
}

func TestBuildCourseSummaryWorkbook_WarnsOnZeroStudents(t *testing.T) {
	f := newCSFixture(t)
	courseID := f.insertCourse(csCourseOpts{
		Code: "CP400001", NameTH: "Empty", Credits: 3, LectureHrs: 3, LabHrs: 3, SelfHrs: 6,
		NumRegular: 0, NumSpecial: 0, Curriculum: "CS",
	})
	f.addApprovedTA(courseID, "TA1", "undergrad", 3, 0)

	_, warnings := f.build()
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "CP400001") && strings.Contains(w, "จำนวนนักศึกษา") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a missing-student-count warning for CP400001, got: %v", warnings)
	}
}

// Staff's own instruction: this document collects only courses a lecturer
// actually asked to use a TA for. A course that was opened but never had any
// ta_requests row at all must not appear on it — not even as a warning, since
// most courses legitimately run without a TA and that is not a problem to flag.
func TestBuildCourseSummaryWorkbook_ExcludesCourseWithNoTARequest(t *testing.T) {
	f := newCSFixture(t)
	f.insertCourse(csCourseOpts{
		Code: "CP400002", NameTH: "No TA request at all", Credits: 3, LectureHrs: 3, LabHrs: 3, SelfHrs: 6,
		NumRegular: 40, Curriculum: "CS",
	})

	body, warnings := f.build()
	wb := openWorkbook(t, body)
	defer wb.Close()
	for _, w := range warnings {
		if strings.Contains(w, "CP400002") {
			t.Errorf("CP400002 never had a TA request — it must not appear even as a warning, got: %v", warnings)
		}
	}
	for _, sheet := range wb.GetSheetList() {
		if v, _ := wb.GetCellValue(sheet, "B5"); v == "CP400002" {
			t.Errorf("CP400002 must not print anywhere — sheet %s has it at B5", sheet)
		}
	}
}

// A course a lecturer DID submit a request for, but that has not yet been
// approved (so no TA is assigned), is a genuinely different case from one
// with no request at all — it still belongs on the document, flagged so
// staff know to chase the approval.
func TestBuildCourseSummaryWorkbook_WarnsOnNoApprovedTA(t *testing.T) {
	f := newCSFixture(t)
	courseID := f.insertCourse(csCourseOpts{
		Code: "CP400003", NameTH: "Requested, not yet approved", Credits: 3, LectureHrs: 3, LabHrs: 3, SelfHrs: 6,
		NumRegular: 40, Curriculum: "CS",
	})
	f.insertSubmittedRequest(courseID)

	_, warnings := f.build()
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "CP400003") && strings.Contains(w, "TA") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a no-approved-TA warning for CP400003, got: %v", warnings)
	}
}

// ขออนุมัติเบิกจ่าย reports the course's BUDGET — what the term allots it —
// and must not drift toward spend as work is logged and approved. The sheet
// used to print เบิกจ่ายจริง/คงเหลือ beside it, and with those columns gone
// (10/08/2026) this is the guard that the surviving pair still answers "how
// much was this course given", not "how much has it used".
func TestBuildCourseSummaryWorkbook_ApplyColumnsAreBudgetNotSpend(t *testing.T) {
	f := newCSFixture(t)
	courseID := f.insertCourse(csCourseOpts{
		Code: "CP500001", NameTH: "Paid Test", Credits: 3, LectureHrs: 2, LabHrs: 0, SelfHrs: 4,
		NumRegular: 40, Curriculum: "CS",
	})
	f.insertSubmittedRequest(courseID)
	f.addSubmissionPeriod("2569-06", "2026-06-01", "2026-07-31", "มิถุนายน 2569")
	taID := f.addApprovedTA(courseID, "TA1", "undergrad", 2, 0)

	snap, err := f.svc.budget.Compute(f.ctx, courseID)
	if err != nil {
		t.Fatal(err)
	}

	var assignID uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`SELECT id FROM ta_request_assignments WHERE ta_id = $1`, taID).Scan(&assignID); err != nil {
		t.Fatal(err)
	}
	// Real settled money on the course: an approved log, plus a submitted one.
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status, approved_at)
	        VALUES (gen_random_uuid(),$1,'2026-06-15','09:00','11:00',2,'lecture','approved',NOW())`, assignID)
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status, submitted_at)
	        VALUES (gen_random_uuid(),$1,'2026-06-16','09:00','11:00',2,'lecture','submitted',NOW())`, assignID)

	settlement, err := f.svc.SettleCourse(f.ctx, courseID)
	if err != nil {
		t.Fatal(err)
	}
	if settlement.Regular.PaidBaht <= 0 {
		t.Fatal("fixture bug: expected the approved log to settle to a nonzero amount")
	}

	body, _ := f.build()
	wb := openWorkbook(t, body)
	defer wb.Close()
	// Still the full budget, even though part of it has now been spent.
	assertMoneyCell(t, wb, "CS", "L5", snap.TermPayRegular)
	// เบิกจ่ายเดือน (N) is the actual settled spend — same number
	// SettleCourse itself reports, not a re-derived one — and คงเหลือ (P)
	// is what's left of the budget after it.
	assertMoneyCell(t, wb, "CS", "N5", settlement.Regular.PaidBaht)
	assertFormulaCell(t, wb, "CS", "P5", "L5-N5")
}

// Phase 4: a graduate-level course tagged curriculum "CS" or "IT" prints
// under CS_GRAD's sheet ("CS&IT"), not the undergrad CS/IT sheet — migration
// 0073's own comment says this combined routing is application logic in the
// export builder, since one sections.curriculum value routes to two
// different curricula rows depending on teaching_courses.level.
func TestBuildCourseSummaryWorkbook_GraduateCourseRoutesToGradSheet(t *testing.T) {
	f := newCSFixture(t)
	courseID := f.insertCourse(csCourseOpts{
		Code: "CP388001", NameTH: "Graduate Test Course", Credits: 3, LectureHrs: 3,
		NumRegular: 10, Curriculum: "CS", Level: "graduate",
	})
	f.addApprovedTA(courseID, "บัณฑิต", "master", 3, 0)

	body, warnings := f.build()
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	wb := openWorkbook(t, body)
	defer wb.Close()

	if got := wb.GetSheetList(); len(got) != 1 || got[0] != "CS&IT" {
		t.Fatalf("sheets = %v, want exactly [CS&IT] — a graduate CS course must route to CS_GRAD, not the undergrad CS sheet", got)
	}
	if got, _ := wb.GetCellValue("CS&IT", "A4"); got != "สาขาวิชาวิทยาการคอมพิวเตอร์และเทคโนโลยีสารสนเทศ" {
		t.Errorf("A4 (curriculum heading) = %q, want the CS_GRAD full name", got)
	}
	if got, _ := wb.GetCellValue("CS&IT", "B5"); got != "CP388001" {
		t.Errorf("B5 (code) = %q, want CP388001", got)
	}
}

// A graduate course tagged a curriculum with no graduate row yet (GIS/CY/
// OTHER) has nowhere to print and must warn, exactly like an unrecognised
// undergrad token — never guessed onto an unrelated sheet.
func TestBuildCourseSummaryWorkbook_GraduateCourseWithNoGradSheetWarns(t *testing.T) {
	f := newCSFixture(t)
	courseID := f.insertCourse(csCourseOpts{
		Code: "CP388002", NameTH: "Graduate GIS Course", Credits: 3, LectureHrs: 3,
		NumRegular: 10, Curriculum: "GIS", Level: "graduate",
	})
	f.insertSubmittedRequest(courseID)

	body, warnings := f.build()
	if len(warnings) == 0 {
		t.Fatal("expected a warning: no graduate curriculum row exists for GIS yet")
	}
	wb := openWorkbook(t, body)
	defer wb.Close()
	for _, sheet := range wb.GetSheetList() {
		if sheet == "GIS" {
			t.Error("a graduate course must never print under the undergrad GIS sheet")
		}
	}
}

// summaryStyle reads back the effective style of one cell on a summary sheet.
func summaryStyle(t *testing.T, wb *excelize.File, sheet, cell string) *excelize.Style {
	t.Helper()
	id, err := wb.GetCellStyle(sheet, cell)
	if err != nil {
		t.Fatal(err)
	}
	s, err := wb.GetStyle(id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A course with several TAs takes one row per TA, and only the first of them
// carries the course's own columns. The reference (docs/ค่าตอบแทนTAภาคต้น-2569)
// still rules A–N on every one of those rows, so the sheet reads as one grid
// with the course spanning it.
//
// We used to style only the cells we wrote a value into, which meant that from
// the second TA down everything except รหัสนักศึกษา/ชื่อ TA/ระดับ printed with
// no rules — the table fell apart down the middle of every course with more
// than one TA, which is most of them.
func TestBuildCourseSummaryWorkbook_GridIsUnbrokenUnderEveryTA(t *testing.T) {
	f := newCSFixture(t)
	courseID := f.insertCourse(csCourseOpts{
		Code: "CP100002", NameTH: "Grid Course", Credits: 3, LectureHrs: 3, LabHrs: 3, SelfHrs: 6,
		NumRegular: 60, NumSpecial: 30, Curriculum: "CS",
	})
	f.addSubmissionPeriod("2569-06", "2026-06-01", "2026-07-31", "มิถุนายน 2569")
	f.addApprovedTA(courseID, "หนึ่ง", "undergrad", 3, 0)
	f.addApprovedTA(courseID, "สอง", "undergrad", 3, 0)
	f.addApprovedTA(courseID, "สาม", "undergrad", 3, 0)

	body, _ := f.build()
	wb := openWorkbook(t, body)
	defer wb.Close()

	// Three TAs, so rows 5, 6 and 7 — the last two being the ones that used to
	// print bare.
	for r := 5; r <= 7; r++ {
		for _, col := range []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K", "L", "M", "N", "O", "P", "Q"} {
			cell := fmt.Sprintf("%s%d", col, r)
			s := summaryStyle(t, wb, "CS", cell)
			for _, side := range []string{"left", "right", "top", "bottom"} {
				if got := borderOf(s, side); got != "thin" {
					t.Errorf("%s has no %s rule (%q) — the grid is torn open",
						cell, side, got)
				}
			}
		}
	}
}

// The ขออนุมัติเบิกจ่าย pair carries the header tint down the WHOLE column in
// the reference, not only beside the first TA.
func TestBuildCourseSummaryWorkbook_MoneyColumnsKeepTheirTintOnEveryRow(t *testing.T) {
	f := newCSFixture(t)
	courseID := f.insertCourse(csCourseOpts{
		Code: "CP100003", NameTH: "Tint Course", Credits: 3, LectureHrs: 3, LabHrs: 3, SelfHrs: 6,
		NumRegular: 60, NumSpecial: 30, Curriculum: "CS",
	})
	f.addApprovedTA(courseID, "หนึ่ง", "undergrad", 3, 0)
	f.addApprovedTA(courseID, "สอง", "undergrad", 3, 0)

	body, _ := f.build()
	wb := openWorkbook(t, body)
	defer wb.Close()

	for _, cell := range []string{"L5", "M5", "L6", "M6"} {
		s := summaryStyle(t, wb, "CS", cell)
		if s.Fill.Type != "pattern" || len(s.Fill.Color) == 0 || s.Fill.Color[0] != csHeaderFill {
			t.Errorf("%s is not on the header tint (type=%q color=%v)",
				cell, s.Fill.Type, s.Fill.Color)
		}
	}
	// The figure itself still belongs to the course, not to each TA: only the
	// first row of the block carries it.
	// Read raw: the accounting format renders an empty cell as padding, so a
	// formatted read cannot tell a blank apart from a value.
	raw := excelize.Options{RawCellValue: true}
	if got, _ := wb.GetCellValue("CS", "L6", raw); got != "" {
		t.Errorf("L6 = %q — the course's budget was repeated against a second TA", got)
	}
	if got, _ := wb.GetCellValue("CS", "L5", raw); got == "" {
		t.Error("L5 is empty — the block's own budget figure went missing")
	}
}

// findCourseSummaryRow returns the first preview row for courseCode across
// every sheet — เบิกจ่ายเดือน/คงเหลือ are the same on every one of a block's
// rows (one per TA), so the first match is enough.
func findCourseSummaryRow(t *testing.T, sheets []CourseSummaryPreviewSheet, courseCode string) CourseSummaryPreviewRow {
	t.Helper()
	for _, sh := range sheets {
		for _, r := range sh.Rows {
			if r.CourseCode == courseCode {
				return r
			}
		}
	}
	t.Fatalf("no preview row found for course %s", courseCode)
	return CourseSummaryPreviewRow{}
}

// เบิกจ่ายเดือน/คงเหลือ were added back 11/09/2026, reversing the 10/08/2026
// decision to cut the sheet at N — see the package doc comment on
// courseSummaryBlock. Same day, staff followed up asking that months NOT be
// pooled into one cumulative figure but shown separately, one column pair
// per selected month — these tests pin that second revision.

// findMonthAmount returns row's PaidByMonth entry for yearMonth, or fails
// the test if that month isn't among the ones the row carries.
func findMonthAmount(t *testing.T, row CourseSummaryPreviewRow, yearMonth string) CourseSummaryMonthAmount {
	t.Helper()
	for _, m := range row.PaidByMonth {
		if m.YearMonth == yearMonth {
			return m
		}
	}
	t.Fatalf("row for %s has no PaidByMonth entry for %s (got %+v)", row.CourseCode, yearMonth, row.PaidByMonth)
	return CourseSummaryMonthAmount{}
}

func TestCourseSummaryPreview_EachSelectedMonthGetsItsOwnFigureNeverSummed(t *testing.T) {
	f := newCSFixture(t)
	courseID := f.insertCourse(csCourseOpts{
		Code: "CP600001", NameTH: "Per-Month Test", Credits: 3, LectureHrs: 2, LabHrs: 0, SelfHrs: 4,
		NumRegular: 40, Curriculum: "CS",
	})
	f.addSubmissionPeriod("2569-06", "2026-06-01", "2026-07-31", "มิถุนายน 2569")
	f.addSubmissionPeriod("2569-07", "2026-07-01", "2026-08-31", "กรกฎาคม 2569")
	f.addSubmissionPeriod("2569-08", "2026-08-01", "2026-09-30", "สิงหาคม 2569")
	taID := f.addApprovedTA(courseID, "TA1", "undergrad", 2, 0)

	var assignID uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`SELECT id FROM ta_request_assignments WHERE ta_id = $1`, taID).Scan(&assignID); err != nil {
		t.Fatal(err)
	}
	// Approved work in June and August, nothing in July.
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status, approved_at)
	        VALUES (gen_random_uuid(),$1,'2026-06-15','09:00','11:00',2,'lecture','approved',NOW())`, assignID)
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status, approved_at)
	        VALUES (gen_random_uuid(),$1,'2026-08-15','09:00','11:00',2,'lecture','approved',NOW())`, assignID)

	settled, err := f.svc.SettleCourse(f.ctx, courseID)
	if err != nil {
		t.Fatal(err)
	}
	var junePaid, augPaid float64
	for _, m := range settled.Regular.Months {
		switch m.YearMonth {
		case "2026-06":
			junePaid = m.PaidBaht
		case "2026-08":
			augPaid = m.PaidBaht
		}
	}
	if junePaid <= 0 || augPaid <= 0 {
		t.Fatalf("fixture bug: expected June and August to both settle nonzero, got june=%v aug=%v", junePaid, augPaid)
	}

	// Selecting June and August (skipping July on purpose) must return
	// exactly those two months, each with its OWN figure — not a July-less
	// sum of the two.
	sheets, _, months, _, err := f.svc.CourseSummaryPreview(f.ctx, f.termID, []string{"2026-06", "2026-08"})
	if err != nil {
		t.Fatal(err)
	}
	wantMonths := []string{"2026-06", "2026-08"}
	if len(months) != 2 || months[0] != wantMonths[0] || months[1] != wantMonths[1] {
		t.Errorf("months echoed back = %v, want %v", months, wantMonths)
	}
	row := findCourseSummaryRow(t, sheets, "CP600001")
	if len(row.PaidByMonth) != 2 {
		t.Fatalf("PaidByMonth = %+v, want exactly 2 entries (June, August) — no July, no pooling", row.PaidByMonth)
	}
	if diff := findMonthAmount(t, row, "2026-06").Regular - junePaid; diff > 0.01 || diff < -0.01 {
		t.Errorf("June figure = %v, want %v", findMonthAmount(t, row, "2026-06").Regular, junePaid)
	}
	if diff := findMonthAmount(t, row, "2026-08").Regular - augPaid; diff > 0.01 || diff < -0.01 {
		t.Errorf("August figure = %v, want %v", findMonthAmount(t, row, "2026-08").Regular, augPaid)
	}

	// An empty request means every term month — still separate, so July
	// (which settled nothing) must show up as its own zero entry, not be
	// absorbed into June or August.
	sheetsAll, _, monthsAll, _, err := f.svc.CourseSummaryPreview(f.ctx, f.termID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(monthsAll) != 3 {
		t.Fatalf("months with no request = %v, want all 3 term months", monthsAll)
	}
	rowAll := findCourseSummaryRow(t, sheetsAll, "CP600001")
	if len(rowAll.PaidByMonth) != 3 {
		t.Fatalf("PaidByMonth with no request = %+v, want 3 entries (June, July, August)", rowAll.PaidByMonth)
	}
	if got := findMonthAmount(t, rowAll, "2026-07").Regular; got != 0 {
		t.Errorf("July figure = %v, want 0 — nothing was approved that month", got)
	}
}

func TestCourseSummaryReprint_PreservesOriginalMonthFigures(t *testing.T) {
	f := newCSFixture(t)
	courseID := f.insertCourse(csCourseOpts{
		Code: "CP600002", NameTH: "Reprint Test", Credits: 3, LectureHrs: 2, LabHrs: 0, SelfHrs: 4,
		NumRegular: 40, Curriculum: "CS",
	})
	f.addSubmissionPeriod("2569-06", "2026-06-01", "2026-07-31", "มิถุนายน 2569")
	taID := f.addApprovedTA(courseID, "TA1", "undergrad", 2, 0)
	var assignID uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`SELECT id FROM ta_request_assignments WHERE ta_id = $1`, taID).Scan(&assignID); err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status, approved_at)
	        VALUES (gen_random_uuid(),$1,'2026-06-15','09:00','11:00',2,'lecture','approved',NOW())`, assignID)

	actor := f.actor()
	body1, _, err := f.svc.BuildCourseSummaryWorkbook(f.ctx, actor, f.termID, nil)
	if err != nil {
		t.Fatal(err)
	}
	wb1 := openWorkbook(t, body1)
	defer wb1.Close()
	// One submission period (June) means exactly one เบิกจ่ายเดือน pair, at N:O.
	orig, _ := wb1.GetCellValue("CS", "N5")
	if orig == "" || orig == "0.00" {
		t.Fatalf("fixture bug: expected a nonzero original N5, got %q", orig)
	}

	// More work is approved AFTER the export — a reprint must not pick it up.
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status, approved_at)
	        VALUES (gen_random_uuid(),$1,'2026-06-20','09:00','11:00',2,'lecture','approved',NOW())`, assignID)

	var exportID uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`SELECT id FROM course_summary_exports WHERE term_id = $1`, f.termID).Scan(&exportID); err != nil {
		t.Fatal(err)
	}
	body2, err := f.svc.ReprintCourseSummary(f.ctx, actor, exportID)
	if err != nil {
		t.Fatal(err)
	}
	wb2 := openWorkbook(t, body2)
	defer wb2.Close()
	reprinted, _ := wb2.GetCellValue("CS", "N5")
	if reprinted != orig {
		t.Errorf("reprint N5 = %q, want original %q — a reprint must not recompute เบิกจ่ายเดือน from today's work_logs",
			reprinted, orig)
	}
}

// Graduate-special TAs (master/phd on a track='special' section) are paid a
// flat term lump (pay_rates.graduate_special_lumpsum), reserved off the top of
// the special pool — but paid MONTHLY, dated by the holder's own approved
// special-track hours (11/09/2026), so the summary's เบิกจ่ายเดือน carries each
// month's own share rather than the whole lump on the earliest month.
func TestCourseSummaryPreview_DatesTheGradSpecialLumpByTheHoldersOwnHours(t *testing.T) {
	f := newCSFixture(t)
	courseID := f.insertCourse(csCourseOpts{
		Code: "CP600003", NameTH: "Grad Special Test", Credits: 3, LectureHrs: 2, LabHrs: 0, SelfHrs: 4,
		NumSpecial: 20, Curriculum: "CS",
	})
	f.addSubmissionPeriod("2569-06", "2026-06-01", "2026-07-31", "มิถุนายน 2569")
	f.addSubmissionPeriod("2569-07", "2026-07-01", "2026-08-31", "กรกฎาคม 2569")

	specialSecID := uuid.New()
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track, curriculum)
	        VALUES ($1, $2, '2', 'special', 'CS')`, specialSecID, courseID)

	taID := f.insertUser("ta", "gradta")
	f.exec(`UPDATE users SET first_name='GradTA', last_name='ทดสอบ', study_level='master'::study_level WHERE id=$1`, taID)
	reqID := uuid.New()
	f.exec(`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at, decided_at)
	        VALUES ($1,$2,$3,'both'::reimburse_scope,'approved'::ta_request_status, NOW(), NOW())`,
		reqID, courseID, f.lectID)
	assignID := uuid.New()
	f.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
	        VALUES ($1,$2,$3,$4,'master'::study_level)`, assignID, reqID, specialSecID, taID)
	for _, d := range []string{"2026-06-08", "2026-06-15", "2026-07-06"} {
		f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
		        VALUES (gen_random_uuid(), $1, $2::date, '09:00', '10:00', 1, 'lecture', 'approved')`, assignID, d)
	}

	sheets, _, _, _, err := f.svc.CourseSummaryPreview(f.ctx, f.termID, nil)
	if err != nil {
		t.Fatal(err)
	}
	row := findCourseSummaryRow(t, sheets, "CP600003")
	if len(row.PaidByMonth) != 2 {
		t.Fatalf("PaidByMonth = %+v, want 2 entries (June, July)", row.PaidByMonth)
	}
	// pay_rates.graduate_special_lumpsum is 12000 in this fixture. Two
	// approved hours in June and one in July: 2/3 of the lump is June's.
	if got := findMonthAmount(t, row, "2026-06").Special; got != 8000 {
		t.Errorf("June Special = %v, want 8000 (two of the three hours)", got)
	}
	if got := findMonthAmount(t, row, "2026-07").Special; got != 4000 {
		t.Errorf("July Special = %v, want 4000 (one of the three hours)", got)
	}
}
