package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/testutil"
)

// The executive dashboard shipped with two figures that agreed with no other
// page in the system:
//
//   - Every count spanned every term ever imported, so "วิชาที่เปิดสอน" grew
//     by ~130 each semester and never described the term staff were working on.
//   - งบทั้งหมด was budget_caps.per_course_max × every course in the database.
//     budget_caps stopped being the source of truth when the per-course ceiling
//     became formula-derived (BudgetService.Compute), and courses with no TA
//     request commit no money at all. The card read 2,540,000฿ where the courses
//     that had actually asked for a TA totalled 40,800฿.
//
// Both are the kind of number nobody re-derives by hand, so they are pinned here.

func TestExecutive_ScopesToTermAndRequestedCourses(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	dash := &DashboardService{pool: pool}
	budget := &BudgetService{pool: pool}
	seedWorkbookRates(t, pool)

	active := insertTerm(t, pool, 2569, 1, true)
	old := insertTerm(t, pool, 2568, 2, false)

	// Active term: one course that asked for a TA, one that did not.
	// 3(3-0-6) with 60 students → 3 × 3 × (60/60) = 9 hr/wk × 300฿ × 4 = 10,800฿.
	requested := insertDashCourse(t, pool, active, "TERM-REQ", 3, 0, 60)
	insertDashCourse(t, pool, active, "TERM-NOREQ", 3, 0, 60)
	// Same shape in a past term — must not leak into any figure.
	otherTerm := insertDashCourse(t, pool, old, "OLD-REQ", 3, 0, 60)

	lecturer := insertDashUser(t, pool, "lecturer")
	ta := insertDashUser(t, pool, "ta")
	dropped := insertDashUser(t, pool, "ta")

	reqID := insertDashRequest(t, pool, requested, lecturer, "approved")
	sec := insertDashSection(t, pool, requested)
	insertDashAssignment(t, pool, reqID, sec, ta, "active")
	// A dropped assignment is a TA the clash rule already removed from the
	// course; counting them overstated how many people were working.
	insertDashAssignment(t, pool, reqID, sec, dropped, "dropped")

	// The past term's request must not raise this term's counts or budget.
	oldReq := insertDashRequest(t, pool, otherTerm, lecturer, "approved")
	oldSec := insertDashSection(t, pool, otherTerm)
	insertDashAssignment(t, pool, oldReq, oldSec, ta, "active")

	sum, err := dash.Executive(ctx, nil, budget)
	if err != nil {
		t.Fatalf("Executive: %v", err)
	}

	if sum.TermLabel != "2569/1" {
		t.Errorf("TermLabel = %q, want \"2569/1\" (ควรเลือกเทอมที่ is_active)", sum.TermLabel)
	}
	if sum.TotalCourses != 2 {
		t.Errorf("TotalCourses = %d, want 2 — วิชาของเทอมเก่าไม่ควรถูกนับ", sum.TotalCourses)
	}
	if sum.CoursesWithTA != 1 {
		t.Errorf("CoursesWithTA = %d, want 1", sum.CoursesWithTA)
	}
	if sum.TotalTAs != 1 {
		t.Errorf("TotalTAs = %d, want 1 — assignment ที่ state='dropped' ไม่ได้ทำงานแล้ว", sum.TotalTAs)
	}
	if sum.BudgetCourses != 1 {
		t.Errorf("BudgetCourses = %d, want 1", sum.BudgetCourses)
	}
	if sum.MissingStudentCounts != 0 {
		t.Errorf("MissingStudentCounts = %d, want 0 — ทุกวิชากรอกจำนวน นศ. แล้ว", sum.MissingStudentCounts)
	}
	// One requested course only: the un-requested course and the past term's
	// course must contribute nothing.
	if sum.BudgetAllocated != 10800 {
		t.Errorf("BudgetAllocated = %.0f฿, want 10800฿ "+
			"(เฉพาะวิชาที่ขอใช้ TA: 3นก. × 3ชม. × 60/60 × 300฿ × 4เดือน)", sum.BudgetAllocated)
	}
}

// A submitted-but-not-yet-approved request has already reserved the TA's quota
// (see reservedCourseCount), so the money it commits belongs on the dashboard
// too — otherwise งบทั้งหมด jumps every time staff click approve.
func TestExecutive_CountsSubmittedRequests(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	dash := &DashboardService{pool: pool}
	budget := &BudgetService{pool: pool}
	seedWorkbookRates(t, pool)

	term := insertTerm(t, pool, 2569, 1, true)
	pending := insertDashCourse(t, pool, term, "SUBMITTED", 3, 0, 60)
	rejected := insertDashCourse(t, pool, term, "REJECTED", 3, 0, 60)
	lecturer := insertDashUser(t, pool, "lecturer")

	insertDashRequest(t, pool, pending, lecturer, "submitted")
	insertDashRequest(t, pool, rejected, lecturer, "rejected")

	sum, err := dash.Executive(ctx, nil, budget)
	if err != nil {
		t.Fatalf("Executive: %v", err)
	}
	if sum.CoursesWithTA != 1 {
		t.Errorf("CoursesWithTA = %d, want 1 — นับ submitted แต่ไม่นับ rejected", sum.CoursesWithTA)
	}
	if sum.BudgetAllocated != 10800 {
		t.Errorf("BudgetAllocated = %.0f฿, want 10800฿", sum.BudgetAllocated)
	}
}

// The sidebar badges and the dashboard's to-do panel both read these counts, so
// a wrong one does not merely look odd — it tells staff a queue is empty when
// it is not, which is the exact failure the badges were added to prevent.
func TestExecutive_StepQueueCounts(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	dash := &DashboardService{pool: pool}
	budget := &BudgetService{pool: pool}
	seedWorkbookRates(t, pool)

	term := insertTerm(t, pool, 2569, 1, true)
	other := insertTerm(t, pool, 2568, 2, false)
	lecturer := insertDashUser(t, pool, "lecturer")
	ta := insertDashUser(t, pool, "ta")

	// ขั้นที่ 1 — two submitted requests waiting, plus decided ones that must not
	// count, plus a submitted one in a different term.
	insertDashRequest(t, pool, insertDashCourse(t, pool, term, "WAIT-A", 3, 0, 60), lecturer, "submitted")
	insertDashRequest(t, pool, insertDashCourse(t, pool, term, "WAIT-B", 3, 0, 60), lecturer, "submitted")
	insertDashRequest(t, pool, insertDashCourse(t, pool, term, "DONE", 3, 0, 60), lecturer, "approved")
	insertDashRequest(t, pool, insertDashCourse(t, pool, term, "NO", 3, 0, 60), lecturer, "rejected")
	insertDashRequest(t, pool, insertDashCourse(t, pool, other, "OLD", 3, 0, 60), lecturer, "submitted")

	// ขั้นที่ 4 — one month signed off by staff and awaiting export, alongside
	// months at every other stage. Only 'staff_reviewed' is work for step 4.
	exportCourse := insertDashCourse(t, pool, term, "EXPORTABLE", 3, 0, 60)
	period := insertDashPeriod(t, pool, term, "2569-06")
	for _, st := range []string{"staff_reviewed", "pending", "exported", "finance_sent"} {
		insertDashPeriodStatus(t, pool, period, insertDashUser(t, pool, "ta"), exportCourse, st)
	}
	// A reviewed month in another term must not leak in.
	insertDashPeriodStatus(t, pool,
		insertDashPeriod(t, pool, other, "2568-11"), ta,
		insertDashCourse(t, pool, other, "OLD-EXPORTABLE", 3, 0, 60), "staff_reviewed")

	sum, err := dash.Executive(ctx, nil, budget)
	if err != nil {
		t.Fatalf("Executive: %v", err)
	}
	if sum.PendingTARequests != 2 {
		t.Errorf("PendingTARequests = %d, want 2 — นับเฉพาะ submitted ในเทอมนี้", sum.PendingTARequests)
	}
	if sum.ReadyToExport != 1 {
		t.Errorf("ReadyToExport = %d, want 1 — นับเฉพาะ staff_reviewed ในเทอมนี้", sum.ReadyToExport)
	}
}

// The "กรอกจำนวนนักศึกษา" banner gates every export, so its count has to catch
// both shapes: a course with no regular students at all, and a course that has
// a ภาคพิเศษ section but no ภาคพิเศษ head-count.
func TestExecutive_CountsCoursesMissingStudentCounts(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	dash := &DashboardService{pool: pool}
	budget := &BudgetService{pool: pool}
	seedWorkbookRates(t, pool)

	term := insertTerm(t, pool, 2569, 1, true)
	insertDashCourse(t, pool, term, "FILLED", 3, 0, 60)
	insertDashCourse(t, pool, term, "NO-STUDENTS", 3, 0, 0)

	// Regular count filled, but it runs a ภาคพิเศษ section with no head-count.
	specialGap := insertDashCourse(t, pool, term, "SPECIAL-GAP", 3, 0, 60)
	if _, err := pool.Exec(ctx,
		`INSERT INTO sections (id, teaching_course_id, sec_no, track)
		 VALUES (gen_random_uuid(), $1, '90', 'special'::section_track)`, specialGap); err != nil {
		t.Fatalf("insert special section: %v", err)
	}

	// A ภาคพิเศษ head-count of zero is only a gap when such a section exists —
	// this one has none, so it must not be flagged.
	insertDashSection(t, pool, insertDashCourse(t, pool, term, "REGULAR-ONLY", 3, 0, 60))

	sum, err := dash.Executive(ctx, nil, budget)
	if err != nil {
		t.Fatalf("Executive: %v", err)
	}
	if sum.MissingStudentCounts != 2 {
		t.Errorf("MissingStudentCounts = %d, want 2 (NO-STUDENTS + SPECIAL-GAP)", sum.MissingStudentCounts)
	}
}

/* ---------------------------- fixture helpers ---------------------------- */

func insertTerm(t *testing.T, pool *pgxpool.Pool, year, sem int, active bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO academic_terms (id, academic_year, semester, is_active, months)
		 VALUES ($1,$2,$3,$4,4)`, id, year, sem, active); err != nil {
		t.Fatalf("insert term: %v", err)
	}
	return id
}

func insertDashCourse(t *testing.T, pool *pgxpool.Pool, term uuid.UUID, code string, lec, lab, students int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO teaching_courses
		    (id, term_id, code, name_th, level, credits, lecture_hrs, lab_hrs,
		     num_students, num_students_regular)
		VALUES ($1,$2,$3,'วิชาทดสอบ','undergrad',$4,$5,$6,$7,$7)`,
		id, term, code+"-"+id.String()[:4], lec+lab, lec, lab, students); err != nil {
		t.Fatalf("insert course: %v", err)
	}
	return id
}

func insertDashUser(t *testing.T, pool *pgxpool.Pool, role string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, first_name, last_name, is_active, profile_completed, study_level)
		 VALUES ($1, $2, $3, 'Test', TRUE, TRUE, 'undergrad')`,
		id, role+"-"+id.String()+"@example.test", role); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO user_roles (user_id, role) VALUES ($1,$2::role_code)`, id, role); err != nil {
		t.Fatalf("insert role: %v", err)
	}
	return id
}

func insertDashRequest(t *testing.T, pool *pgxpool.Pool, course, lecturer uuid.UUID, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at)
		 VALUES ($1,$2,$3,'both'::reimburse_scope,$4::ta_request_status, NOW())`,
		id, course, lecturer, status); err != nil {
		t.Fatalf("insert request: %v", err)
	}
	return id
}

func insertDashSection(t *testing.T, pool *pgxpool.Pool, course uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO sections (id, teaching_course_id, sec_no, track)
		 VALUES ($1,$2,'01','regular'::section_track)`, id, course); err != nil {
		t.Fatalf("insert section: %v", err)
	}
	return id
}

func insertDashPeriod(t *testing.T, pool *pgxpool.Pool, term uuid.UUID, yearMonth string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		// year_month is char(7) and label is text — one placeholder for both
		// leaves Postgres unable to deduce a single type (42P08).
		`INSERT INTO submission_periods (id, term_id, year_month, starts_on, due_date, label)
		 VALUES ($1,$2,$3, DATE '2026-06-01', DATE '2026-07-05', $4)`,
		id, term, yearMonth, yearMonth); err != nil {
		t.Fatalf("insert period: %v", err)
	}
	return id
}

func insertDashPeriodStatus(t *testing.T, pool *pgxpool.Pool, period, ta, course uuid.UUID, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO submission_period_status
		     (id, submission_period_id, ta_id, teaching_course_id, status)
		 VALUES (gen_random_uuid(),$1,$2,$3,$4)`, period, ta, course, status); err != nil {
		t.Fatalf("insert period status: %v", err)
	}
}

func insertDashAssignment(t *testing.T, pool *pgxpool.Pool, req, sec, ta uuid.UUID, state string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level, state)
		 VALUES (gen_random_uuid(),$1,$2,$3,'undergrad'::study_level,$4::ta_assignment_state)`,
		req, sec, ta, state); err != nil {
		t.Fatalf("insert assignment: %v", err)
	}
}
