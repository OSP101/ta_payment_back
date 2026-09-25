package service

import (
	"testing"
	"time"
)

// The executive analytics promises one thing above all: its numbers are the
// SETTLEMENT's numbers — the same pricing-with-cutoff that prices the printed
// claim. These tests pin the wiring: per-course spend equals SettleCourse,
// the monthly series equals the settlement's month rollup, and the curriculum
// rollup adds up to the term total (a course serving two programmes counts
// once, under its main one).

func analyticsWorld(t *testing.T) (*fixture, *DashboardService, *BudgetService, *ExportService) {
	t.Helper()
	f := newFixture(t, fixtureOpts{})
	f.exec(`UPDATE sections SET curriculum = 'IT' WHERE id = $1`, f.SectionID)
	dash := &DashboardService{pool: f.Pool}
	budget := &BudgetService{pool: f.Pool}
	export := &ExportService{pool: f.Pool, budget: budget}
	return f, dash, budget, export
}

// approveHours writes n approved one-hour rows on distinct dates inside the
// fixture term, starting from the term's first Monday.
func approveHours(f *fixture, n int) {
	day := monthStart()
	for day.Weekday() != time.Monday {
		day = day.AddDate(0, 0, 1)
	}
	for i := 0; i < n; i++ {
		f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status, approved_at)
		        VALUES (gen_random_uuid(), $1, $2, '09:00', '10:00', 1, 'lecture', 'approved', NOW())`,
			f.AssignmentID, day.AddDate(0, 0, i*7).Format("2006-01-02"))
	}
}

func TestAnalyticsSpendEqualsSettlement(t *testing.T) {
	f, dash, budget, export := analyticsWorld(t)
	approveHours(f, 4)

	a, err := dash.Analytics(f.ctx, &f.TermID, budget, export)
	if err != nil {
		t.Fatalf("Analytics: %v", err)
	}
	settle, err := export.SettleCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatalf("SettleCourse: %v", err)
	}
	want := settle.Regular.PaidBaht + settle.Regular.Committed +
		settle.Special.PaidBaht + settle.Special.Committed
	if want <= 0 {
		t.Fatal("fixture produced no settled money — the test would pass vacuously")
	}

	if len(a.Courses) != 1 {
		t.Fatalf("want 1 course row, got %d", len(a.Courses))
	}
	if a.Courses[0].SpentBaht != round2(want) {
		t.Fatalf("course spend %.2f ≠ settlement %.2f — the dashboard drifted off the claim pricing",
			a.Courses[0].SpentBaht, want)
	}
	if a.BudgetUsed != round2(want) {
		t.Fatalf("term total %.2f ≠ settlement %.2f", a.BudgetUsed, want)
	}

	// The monthly series must add back up to the slot-based part of the total
	// (the graduate lump sum has no month and stays out of the series).
	var monthSum float64
	for _, m := range a.Monthly {
		monthSum += m.Baht
	}
	slotPaid := settle.Regular.PaidBaht + settle.Special.PaidBaht
	if round2(monthSum) != round2(slotPaid) {
		t.Fatalf("Σmonthly %.2f ≠ settled slot pay %.2f", monthSum, slotPaid)
	}

	if a.ApprovedHours != 4 {
		t.Fatalf("approved hours = %.1f, want 4", a.ApprovedHours)
	}
	if a.Courses[0].Curriculum != "IT" {
		t.Fatalf("course curriculum = %q, want IT", a.Courses[0].Curriculum)
	}
}

func TestAnalyticsCurriculumRollupAddsUp(t *testing.T) {
	f, dash, budget, export := analyticsWorld(t)
	approveHours(f, 3)
	// A second course in the term with no TA request, curriculum CS: it must
	// appear in the CS group's open-course count and nowhere in the money.
	f.exec(`INSERT INTO teaching_courses (id, term_id, code, name_th, level, credits, lecture_hrs, num_students)
	        VALUES (gen_random_uuid(), $1, 'ZZ900', 'วิชาไม่มี TA', 'undergrad', 3, 3, 25)`, f.TermID)
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track, num_students, curriculum)
	        SELECT gen_random_uuid(), id, '1', 'regular', 25, 'CS' FROM teaching_courses WHERE code = 'ZZ900'`)

	a, err := dash.Analytics(f.ctx, &f.TermID, budget, export)
	if err != nil {
		t.Fatalf("Analytics: %v", err)
	}

	byCur := map[string]CurriculumStat{}
	var groupSpend float64
	for _, cu := range a.Curricula {
		byCur[cu.Curriculum] = cu
		groupSpend += cu.SpentBaht
	}
	if round2(groupSpend) != a.BudgetUsed {
		t.Fatalf("Σcurriculum spend %.2f ≠ term total %.2f — a course was double-counted or lost",
			groupSpend, a.BudgetUsed)
	}
	it, ok := byCur["IT"]
	if !ok || it.SpentBaht <= 0 || it.CoursesWithTA != 1 || it.TAs != 1 {
		t.Fatalf("IT group wrong: %+v", it)
	}
	cs, ok := byCur["CS"]
	if !ok || cs.CoursesOpen != 1 || cs.CoursesWithTA != 0 || cs.SpentBaht != 0 {
		t.Fatalf("CS group should be open-but-unspent: %+v", cs)
	}
}

// A course whose sections carry two curricula counts once, under the majority
// one — the group totals must keep adding up to the term total.
func TestAnalyticsMixedCourseCountsOnce(t *testing.T) {
	f, dash, budget, export := analyticsWorld(t)
	approveHours(f, 2)
	// Give the fixture course a second section in another programme; the
	// original (IT) section outnumbers nothing — 1 vs 1 tie breaks
	// alphabetically → CY before IT.
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track, num_students, curriculum)
	        VALUES (gen_random_uuid(), $1, '9', 'regular', 10, 'CY')`, f.CourseID)

	a, err := dash.Analytics(f.ctx, &f.TermID, budget, export)
	if err != nil {
		t.Fatalf("Analytics: %v", err)
	}
	if len(a.Courses) != 1 {
		t.Fatalf("course must appear exactly once, got %d rows", len(a.Courses))
	}
	var withMoney int
	for _, cu := range a.Curricula {
		if cu.SpentBaht > 0 {
			withMoney++
		}
	}
	if withMoney != 1 {
		t.Fatalf("exactly one curriculum group should carry the money, got %d", withMoney)
	}
}

// The figure that separates this dashboard from the old card: when the budget
// cuts a month off, the dashboard reports what will actually be PAID, not what
// the approved hours would have been worth. Without an over-budget case both
// numbers coincide and a wiring mistake (earned instead of paid, Baht instead
// of PaidBaht in the monthly series) passes every other test here.
func TestAnalyticsReportsPaidNotEarnedWhenOverBudget(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.exec(`UPDATE sections SET curriculum = 'IT' WHERE id = $1`, f.SectionID)
	// Two months of identical approved work; budget buys ~1.4 months.
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	next := monthStart().AddDate(0, 1, 9).Format("2006-01-02")
	f.mustUpsert(f.entry(next, "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1`, f.AssignmentID)
	squeezeBudget(t, f, 1.4, 2)

	dash := &DashboardService{pool: f.Pool}
	budget := &BudgetService{pool: f.Pool}
	export := exportSvcFor(f)

	settle, err := export.SettleCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if !settle.OverBudget || settle.DroppedBaht <= 0 {
		t.Fatalf("fixture must actually overrun for this test to mean anything: %+v", settle.Regular)
	}

	a, err := dash.Analytics(f.ctx, &f.TermID, budget, export)
	if err != nil {
		t.Fatal(err)
	}
	earned := settle.EarnedBaht()
	paid := settle.Regular.PaidBaht + settle.Regular.Committed +
		settle.Special.PaidBaht + settle.Special.Committed
	if a.BudgetUsed >= earned {
		t.Fatalf("dashboard shows %.2f but only %.2f will be paid (earned %.2f) — it is reporting pre-cutoff money",
			a.BudgetUsed, paid, earned)
	}
	if a.BudgetUsed != round2(paid) {
		t.Fatalf("BudgetUsed = %.2f, want the settled paid figure %.2f", a.BudgetUsed, paid)
	}
	// The dropped month must not appear at full value in the monthly series.
	var monthSum float64
	for _, m := range a.Monthly {
		monthSum += m.Baht
	}
	if round2(monthSum) != round2(settle.Regular.PaidBaht+settle.Special.PaidBaht) {
		t.Fatalf("Σmonthly %.2f ≠ paid slot money %.2f unpaid คาบ leaked into the chart", monthSum,
			settle.Regular.PaidBaht+settle.Special.PaidBaht)
	}
}

// Pace: today inside the fixture term must land strictly between 0 and 100.
func TestAnalyticsElapsedPct(t *testing.T) {
	f, dash, budget, export := analyticsWorld(t)
	a, err := dash.Analytics(f.ctx, &f.TermID, budget, export)
	if err != nil {
		t.Fatalf("Analytics: %v", err)
	}
	if a.ElapsedPct < 0 || a.ElapsedPct > 100 {
		t.Fatalf("elapsed%% = %.1f, want within [0,100]", a.ElapsedPct)
	}
}

// The redesign's staffing question: every course of the term gets a row, the
// requested count is DISTINCT named TAs on live requests, and the verdict is
// read against the shared recommendation (ta_recommend.go).
func TestAnalyticsStaffingVerdict(t *testing.T) {
	f, dash, budget, export := analyticsWorld(t)
	approveHours(f, 2)
	// A second course with students but no request.
	f.exec(`INSERT INTO teaching_courses (id, term_id, code, name_th, level, credits, lecture_hrs, num_students, num_students_regular)
	        VALUES (gen_random_uuid(), $1, 'ZZ901', 'วิชายังไม่ขอ TA', 'undergrad', 3, 3, 40, 40)`, f.TermID)
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track, num_students, curriculum)
	        SELECT gen_random_uuid(), id, '1', 'regular', 40, 'CS' FROM teaching_courses WHERE code = 'ZZ901'`)

	a, err := dash.Analytics(f.ctx, &f.TermID, budget, export)
	if err != nil {
		t.Fatalf("Analytics: %v", err)
	}
	if len(a.Staffing) != 2 {
		t.Fatalf("want a staffing row per course (2), got %d", len(a.Staffing))
	}
	byCode := map[string]CourseStaffing{}
	for _, c := range a.Staffing {
		byCode[c.Code] = c
	}
	none := byCode["ZZ901"]
	if none.Status != StaffingNoRequest || none.Recommended != 2 || none.Ceiling != 3 {
		t.Fatalf("ZZ901 = %+v, want no_request with recommended 2 / ceiling 3 for 40 students", none)
	}
	if a.Pipeline == nil || a.Pipeline.CoursesNoTA != 1 {
		t.Fatalf("pipeline courses_no_ta = %+v, want 1", a.Pipeline)
	}

	var fixtureRow CourseStaffing
	for code, c := range byCode {
		if code != "ZZ901" {
			fixtureRow = c
		}
	}
	if fixtureRow.Requested != 1 || fixtureRow.Approved != 1 {
		t.Fatalf("fixture course requested/approved = %d/%d, want 1/1", fixtureRow.Requested, fixtureRow.Approved)
	}
	if fixtureRow.Status != staffingStatus(fixtureRow) || fixtureRow.Status == StaffingNoRequest {
		t.Fatalf("fixture course status %q is not the verdict for %+v", fixtureRow.Status, fixtureRow)
	}
	if fixtureRow.SpentBaht != a.Courses[0].SpentBaht {
		t.Fatalf("staffing spend %.2f ≠ course spend %.2f — two tables on one screen disagree",
			fixtureRow.SpentBaht, a.Courses[0].SpentBaht)
	}
	if a.ActiveTAs != 1 {
		t.Fatalf("active TAs = %d, want 1 (the fixture TA has approved hours)", a.ActiveTAs)
	}
	if a.BudgetForecast < a.BudgetUsed {
		t.Fatalf("forecast %.2f below settled %.2f", a.BudgetForecast, a.BudgetUsed)
	}
}
