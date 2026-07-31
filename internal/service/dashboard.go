package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DashboardService struct {
	pool *pgxpool.Pool
}

// ExecutiveSummary is the staff/admin landing dashboard. Every figure is scoped
// to ONE term — the caller's, or the active one. A cross-term total answers no
// question staff actually have ("how many courses am I running this term?")
// and silently grows every semester.
type ExecutiveSummary struct {
	TermID    *uuid.UUID `json:"term_id"`
	TermLabel string     `json:"term_label"` // "2569/1", empty if no terms exist

	TotalCourses  int `json:"total_courses"`   // วิชาที่เปิดสอนในเทอมนี้
	CoursesWithTA int `json:"courses_with_ta"` // วิชาที่มีการขอใช้ TA (ส่งคำขอแล้ว/อนุมัติแล้ว)
	TotalTAs      int `json:"total_tas"`       // TA ที่ทำงานจริงในเทอมนี้

	// One count per step of the staff workflow, in order. These drive both the
	// sidebar badges and the dashboard's to-do panel: before this, nothing on
	// screen distinguished "no work waiting" from "nobody has looked", so a
	// queue could sit untouched for a week without anyone noticing.
	PendingTARequests    int `json:"pending_ta_requests"`    // ขั้นที่ 1 — คำร้องขอ TA (/staff/approvals)
	PendingReviews       int `json:"pending_reviews"`        // ขั้นที่ 2 — แบบฟอร์มใบแจ้งหนี้ (/staff/review)
	PendingPayoutReviews int `json:"pending_payout_reviews"` // ขั้นที่ 3 — เบิกจ่ายค่าตอบแทน
	ReadyToExport        int `json:"ready_to_export"`        // ขั้นที่ 4 — ตรวจแล้วรอส่งออก

	// PayoutCoursesActionable counts COURSES the officer can move right now —
	// either a month is ready to review or the package is ready to download.
	//
	// The two counts above are (month × TA) tallies of two different things and
	// were shown as two sidebar badges on two menus. Nobody could add them, and
	// neither answered the only question the sidebar is asked: how much is on my
	// desk? Since 31/07/2026 review and export are one screen, so the badge is
	// one number in the unit that screen lists — courses.
	PayoutCoursesActionable int `json:"payout_courses_actionable"`

	// Budget covers only the courses that actually asked for a TA. Courses
	// with no request commit no money, so counting all 127 courses in the term
	// produced a denominator that could never be approached.
	BudgetAllocated float64 `json:"budget_allocated"`
	BudgetUsed      float64 `json:"budget_used"`
	BudgetCourses   int     `json:"budget_courses"` // = CoursesWithTA; makes the card's basis explicit

	// Courses in this term whose enrolled student count is still blank. The
	// budget formula is driven by that count, so an export is blocked until it
	// is filled — the dashboard warns up front instead of letting staff find
	// out at export time.
	MissingStudentCounts int `json:"missing_student_counts"`
}

// TACourseStatus is one row on the TA landing dashboard: for a course the TA
// is assigned to, what stage the workflow is at + estimated pay so far.
type TACourseStatus struct {
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	CourseCode       string    `json:"course_code"`
	CourseNameTH     string    `json:"course_name_th"`
	TermLabel        string    `json:"term_label"`
	Stage            string    `json:"stage"` // draft/submitted/approved/exported
	HoursApproved    float64   `json:"hours_approved"`
	HoursPending     float64   `json:"hours_pending"`
	EstimatedBaht    float64   `json:"estimated_baht"` // approved-hours × per-track rate (grad-special = flat)
	Level            string    `json:"level"`
	Track            string    `json:"track"`
}

// TaOverview aggregates every course the TA is on. Read-only. Meant for the
// TA's landing card grid.
func (s *DashboardService) TaOverview(ctx context.Context, taID uuid.UUID) ([]TACourseStatus, error) {
	rows, err := s.pool.Query(ctx, `
		WITH latest AS (SELECT * FROM pay_rates ORDER BY effective_from DESC LIMIT 1)
		SELECT tc.id, tc.code, tc.name_th,
		       t.academic_year || '/' || t.semester,
		       a.level, sec.track,
		       COALESCE(SUM(CASE WHEN wl.status='approved' THEN wl.hours END),0) AS hrs_approved,
		       COALESCE(SUM(CASE WHEN wl.status IN ('draft','submitted') THEN wl.hours END),0) AS hrs_pending,
		       COALESCE(SUM(CASE WHEN wl.status='approved' THEN
		           wl.hours * CASE
		               WHEN a.level='undergrad' AND sec.track='regular' THEN pr.undergrad_regular
		               WHEN a.level='undergrad' AND sec.track='special' THEN pr.undergrad_special
		               WHEN a.level IN ('master','phd') AND sec.track='regular' THEN pr.graduate_regular_hourly
		               ELSE 0
		           END
		       END),0) AS baht_approved,
		       COALESCE(BOOL_OR(wl.status='approved'),  FALSE) AS any_approved,
		       COALESCE(BOOL_OR(wl.status='submitted'), FALSE) AS any_submitted,
		       tc.exported_at IS NOT NULL AS exported
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id=a.request_id AND r.status='approved'
		JOIN sections sec ON sec.id=a.section_id
		JOIN teaching_courses tc ON tc.id=sec.teaching_course_id
		JOIN academic_terms t ON t.id=tc.term_id
		LEFT JOIN work_logs wl ON wl.assignment_id=a.id
		CROSS JOIN latest pr
		WHERE a.ta_id = $1
		GROUP BY tc.id, tc.code, tc.name_th, t.academic_year, t.semester, a.level, sec.track, tc.exported_at
		ORDER BY t.academic_year DESC, t.semester DESC, tc.code`, taID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TACourseStatus{}
	for rows.Next() {
		var r TACourseStatus
		var anyApproved, anySubmitted, exported bool
		if err := rows.Scan(&r.TeachingCourseID, &r.CourseCode, &r.CourseNameTH,
			&r.TermLabel, &r.Level, &r.Track,
			&r.HoursApproved, &r.HoursPending, &r.EstimatedBaht,
			&anyApproved, &anySubmitted, &exported); err != nil {
			return nil, err
		}
		switch {
		case exported:
			r.Stage = "exported"
		case anyApproved && !anySubmitted:
			r.Stage = "approved"
		case anySubmitted:
			r.Stage = "submitted"
		default:
			r.Stage = "draft"
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LecturerCourseStatus is a per-course summary for the lecturer landing page:
// how many TAs, pending vs approved hours, projected pay, budget snapshot.
type LecturerCourseStatus struct {
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	CourseCode       string    `json:"course_code"`
	CourseNameTH     string    `json:"course_name_th"`
	TermLabel        string    `json:"term_label"`
	TACount          int       `json:"ta_count"`
	TAsPending       int       `json:"ta_pending_count"`
	HoursPending     float64   `json:"hours_pending_approval"`
	HoursApproved    float64   `json:"hours_approved"`
	EstimatedBaht    float64   `json:"estimated_baht"`
	BudgetMax        float64   `json:"budget_max"`
	BudgetUsed       float64   `json:"budget_used"`
}

// LecturerOverview lists every course the lecturer teaches this term with
// TA counts + hour totals + budget snapshot.
func (s *DashboardService) LecturerOverview(ctx context.Context, lecturerID uuid.UUID, termID *uuid.UUID, budget *BudgetService) ([]LecturerCourseStatus, error) {
	termFilter := ""
	args := []any{lecturerID}
	if termID != nil {
		termFilter = " AND tc.term_id = $2"
		args = append(args, *termID)
	}
	rows, err := s.pool.Query(ctx, `
		WITH latest AS (SELECT * FROM pay_rates ORDER BY effective_from DESC LIMIT 1)
		SELECT tc.id, tc.code, tc.name_th,
		       t.academic_year || '/' || t.semester,
		       COUNT(DISTINCT a.ta_id) FILTER (WHERE a.ta_id IS NOT NULL),
		       COUNT(DISTINCT a.ta_id) FILTER (WHERE wl.status='submitted'),
		       COALESCE(SUM(CASE WHEN wl.status='submitted' THEN wl.hours END),0),
		       COALESCE(SUM(CASE WHEN wl.status='approved' THEN wl.hours END),0),
		       COALESCE(SUM(CASE WHEN wl.status='approved' THEN
		           wl.hours * CASE
		               WHEN a.level='undergrad' AND sec.track='regular' THEN pr.undergrad_regular
		               WHEN a.level='undergrad' AND sec.track='special' THEN pr.undergrad_special
		               WHEN a.level IN ('master','phd') AND sec.track='regular' THEN pr.graduate_regular_hourly
		               ELSE 0
		           END
		       END),0)
		FROM teaching_courses tc
		JOIN academic_terms t ON t.id=tc.term_id
		JOIN teaching_lecturers tl ON tl.teaching_course_id=tc.id
		LEFT JOIN ta_requests r ON r.teaching_course_id=tc.id AND r.status='approved'
		LEFT JOIN ta_request_assignments a ON a.request_id=r.id
		LEFT JOIN sections sec ON sec.id=a.section_id
		LEFT JOIN work_logs wl ON wl.assignment_id=a.id
		CROSS JOIN latest pr
		WHERE tl.lecturer_id = $1`+termFilter+`
		GROUP BY tc.id, tc.code, tc.name_th, t.academic_year, t.semester
		ORDER BY t.academic_year DESC, t.semester DESC, tc.code`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LecturerCourseStatus{}
	for rows.Next() {
		var r LecturerCourseStatus
		if err := rows.Scan(&r.TeachingCourseID, &r.CourseCode, &r.CourseNameTH,
			&r.TermLabel, &r.TACount, &r.TAsPending,
			&r.HoursPending, &r.HoursApproved, &r.EstimatedBaht); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	for i := range out {
		if snap, err := budget.Compute(ctx, out[i].TeachingCourseID); err == nil {
			out[i].BudgetMax = snap.PerCourseMaxBaht
			out[i].BudgetUsed = snap.UsedBaht
		}
	}
	return out, nil
}

// Executive builds the staff/admin summary for one term. termID may be nil, in
// which case the active term is used.
//
// budget is required: the per-course ceiling is a derived formula living in
// BudgetService, not a stored number. This used to sum budget_caps.per_course_max
// across every course in the database — a flat 20,000฿ × every course ever
// imported. budget_caps stopped being the source of truth when the ceiling
// became formula-derived (see BudgetService.Compute), so the dashboard was
// reporting a figure no other page agreed with.
func (s *DashboardService) Executive(ctx context.Context, termID *uuid.UUID, budget *BudgetService) (*ExecutiveSummary, error) {
	sum := &ExecutiveSummary{}

	// Resolve the term once. Fall back to the newest term so a database with no
	// active term still shows something rather than five zeroes.
	var (
		tid  uuid.UUID
		year int
		sem  int
	)
	resolve := `SELECT id, academic_year, semester FROM academic_terms
	            ORDER BY is_active DESC, academic_year DESC, semester DESC LIMIT 1`
	resolveArgs := []any{}
	if termID != nil {
		resolve = `SELECT id, academic_year, semester FROM academic_terms WHERE id = $1`
		resolveArgs = append(resolveArgs, *termID)
	}
	if err := s.pool.QueryRow(ctx, resolve, resolveArgs...).Scan(&tid, &year, &sem); err != nil {
		// No terms at all (fresh install) — an empty summary is the honest answer.
		return sum, nil
	}
	sum.TermID = &tid
	sum.TermLabel = fmt.Sprintf("%d/%d", year, sem)

	_ = s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM teaching_courses WHERE term_id = $1`, tid).Scan(&sum.TotalCourses)

	// Same predicate the dashboard used to evaluate in the browser, which meant
	// fetching all ~130 course rows (57 KB) on every load and on every SWR
	// revalidation just to show one integer.
	_ = s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM teaching_courses tc
		WHERE tc.term_id = $1
		  AND (tc.num_students_regular = 0
		       OR (tc.num_students_special = 0
		           AND EXISTS (SELECT 1 FROM sections sx
		                       WHERE sx.teaching_course_id = tc.id AND sx.track = 'special')))`,
		tid).Scan(&sum.MissingStudentCounts)

	// "มีการขอใช้ TA" counts requests that are still alive: submitted requests
	// have already reserved a TA's quota (see reservedCourseCount), so a course
	// waiting on staff approval is as committed as an approved one.
	courseIDs := []uuid.UUID{}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT r.teaching_course_id
		FROM ta_requests r
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		WHERE tc.term_id = $1 AND r.status IN ('submitted','approved')`, tid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		courseIDs = append(courseIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sum.CoursesWithTA = len(courseIDs)
	sum.BudgetCourses = len(courseIDs)

	// TAs actually working: approved request, assignment not dropped. Counting
	// every assignment row included TAs whose request was rejected and TAs the
	// clash rule had already dropped.
	_ = s.pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT a.ta_id)
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		WHERE tc.term_id = $1 AND a.state <> 'dropped'`, tid).Scan(&sum.TotalTAs)

	// ขั้นที่ 1 — คำร้องที่อาจารย์ส่งมาแล้วแต่ยังไม่มีใครตัดสิน. Same predicate as
	// TARequestService.ListPending.
	_ = s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM ta_requests r
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		WHERE tc.term_id = $1 AND r.status = 'submitted'`, tid).Scan(&sum.PendingTARequests)

	// ขั้นที่ 2 — same bucket DocsService.ListReview calls "pending", so the card
	// and the page it links to can never disagree. That includes the "all three
	// documents are in" test: the page hides TAs who have only saved the profile
	// form, so a card counting them would send officers to an empty list.
	_ = s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ta_profiles p
		 WHERE p.status IN ('submitted','needs_fix') AND `+ProfileDocsInSQL("p.user_id")+` = 3`).Scan(&sum.PendingReviews)

	// ขั้นที่ 3 — (period, TA, course) months with approved work that staff have
	// not signed off yet. Mirrors ListReviewQueue's shape; see that query for
	// why the month match is on RIGHT(year_month, 2).
	_ = s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM (
		    SELECT sp.id AS period_id, a.ta_id, tc.id AS tc_id
		    FROM teaching_courses tc
		    JOIN submission_periods sp    ON sp.term_id = tc.term_id
		    JOIN sections sec             ON sec.teaching_course_id = tc.id
		    JOIN ta_request_assignments a ON a.section_id = sec.id AND a.state <> 'dropped'
		    JOIN ta_requests r            ON r.id = a.request_id AND r.status = 'approved'
		    JOIN work_logs wl             ON wl.assignment_id = a.id
		                                 AND to_char(wl.work_date, 'MM') = RIGHT(sp.year_month, 2)
		                                 AND wl.status = 'approved'
		    LEFT JOIN submission_period_status st
		           ON st.submission_period_id = sp.id
		          AND st.ta_id = a.ta_id
		          AND st.teaching_course_id = tc.id
		    WHERE tc.term_id = $1 AND COALESCE(st.status, 'pending') = 'pending'
		      -- Same appointment gate as ListReviewQueue. Without it the card
		      -- counts work the page it links to refuses to show, and the officer
		      -- clicks a badge saying 12 to land on an empty queue.
		      AND `+AppointedSQL("tc.id", "a.ta_id")+`
		    GROUP BY sp.id, a.ta_id, tc.id
		) q`, tid).Scan(&sum.PendingPayoutReviews)

	// ขั้นที่ 4 — staff have signed the month off but the payout documents have
	// not been generated. This is the step that strands money: nothing else on
	// screen distinguishes "reviewed and waiting for me" from "already sent".
	_ = s.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM submission_period_status st
		JOIN submission_periods sp ON sp.id = st.submission_period_id
		WHERE sp.term_id = $1 AND st.status = 'staff_reviewed'`, tid).Scan(&sum.ReadyToExport)

	// Courses with at least one month an officer can act on today: a month
	// settled by the lecturer and awaiting sign-off, OR a month signed off and
	// not yet exported. Counted as DISTINCT courses because that is the unit the
	// screen lists — one row per course, whatever the month count behind it.
	//
	// Months still open with the TA or lecturer are deliberately excluded: they
	// belong to the "waiting on someone else" group, and counting them made the
	// badge promise work that could not be done.
	_ = s.pool.QueryRow(ctx, `
		WITH months AS (
		    SELECT tc.id AS tc_id, sp.id AS period_id, a.ta_id,
		           COALESCE(st.status, 'pending') AS status,
		           COUNT(*) FILTER (WHERE wl.status <> 'approved') AS open_rows
		    FROM teaching_courses tc
		    JOIN submission_periods sp    ON sp.term_id = tc.term_id
		    JOIN sections sec             ON sec.teaching_course_id = tc.id
		    JOIN ta_request_assignments a ON a.section_id = sec.id AND a.state <> 'dropped'
		    JOIN ta_requests r            ON r.id = a.request_id AND r.status = 'approved'
		    JOIN work_logs wl             ON wl.assignment_id = a.id
		                                 AND to_char(wl.work_date, 'MM') = RIGHT(sp.year_month, 2)
		    LEFT JOIN submission_period_status st
		           ON st.submission_period_id = sp.id
		          AND st.ta_id = a.ta_id
		          AND st.teaching_course_id = tc.id
		    WHERE tc.term_id = $1
		      AND `+AppointedSQL("tc.id", "a.ta_id")+`
		    GROUP BY tc.id, sp.id, a.ta_id, COALESCE(st.status, 'pending')
		)
		SELECT COUNT(DISTINCT tc_id) FROM months
		 WHERE (status = 'pending' AND open_rows = 0)
		    OR status = 'staff_reviewed'`, tid).Scan(&sum.PayoutCoursesActionable)

	// Budget over exactly the courses counted above. Delegating to
	// BudgetService.Compute rather than re-deriving the formula here is what
	// keeps this card equal to the sum of the per-course pages — the previous
	// hand-rolled SUM used graduate_regular (a 3,000฿/month lump sum) as if it
	// were an hourly rate and ignored the ป.ตรี ภาคพิเศษ monthly cap entirely.
	for _, id := range courseIDs {
		snap, err := budget.Compute(ctx, id)
		if err != nil {
			continue
		}
		sum.BudgetAllocated += snap.PerCourseMaxBaht
		sum.BudgetUsed += snap.UsedBaht
	}
	return sum, nil
}
