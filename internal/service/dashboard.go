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

	// PendingAppointments counts รายชื่อ (TA × course) approved but not yet on
	// any appointment order of this term — the next round's print list. A TA
	// without an order cannot pass payout review, so leaving this invisible
	// let approved requests pile up until the payout screen mysteriously
	// stalled. Same unit and predicate as the appointments page's preview.
	PendingAppointments int `json:"pending_appointments"`

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
//
// One row per COURSE. It used to be one per (course, track), and a TA helping
// both ภาคปกติ and ภาคพิเศษ of one course then reached the screen as two rows
// that a lookup by course id collapsed to one — half their hours simply gone
// (SC362005, 11/09/2026). The split is carried as fields instead, so the
// screen can say "ปกติ 144 · พิเศษ 12" rather than a total that mixes two rates
// and two budgets.
type TACourseStatus struct {
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	CourseCode       string    `json:"course_code"`
	CourseNameTH     string    `json:"course_name_th"`
	TermLabel        string    `json:"term_label"`
	Stage            string    `json:"stage"` // draft/submitted/approved/exported
	// Hours are counted as SITTINGS: a คาบ logged against two co-taught
	// sections is one คาบ of work, and one shared with a special section is
	// regular work (rule B2 bills it once, on the regular side).
	HoursApproved        float64 `json:"hours_approved"`
	HoursApprovedRegular float64 `json:"hours_approved_regular"`
	HoursApprovedSpecial float64 `json:"hours_approved_special"`
	HoursPending         float64 `json:"hours_pending"`
	HoursPendingRegular  float64 `json:"hours_pending_regular"`
	HoursPendingSpecial  float64 `json:"hours_pending_special"`
	// approved-hours × per-track rate (grad-special = flat, not counted here).
	EstimatedBaht        float64 `json:"estimated_baht"`
	EstimatedBahtRegular float64 `json:"estimated_baht_regular"`
	EstimatedBahtSpecial float64 `json:"estimated_baht_special"`
	Level                string  `json:"level"`
}

// TaOverview aggregates every course the TA is on. Read-only. Meant for the
// TA's landing card grid.
//
// enrollmentID, when non-nil, scopes this to one ta_enrollments period
// (migration 0094/0096 — the login-time "which period am I viewing" picker,
// EnrollmentScopeModal on the frontend) — same optional-filter shape as
// LecturerOverview's termFilter below. Assignment rows with no snapshot yet
// (a.enrollment_id IS NULL — pre-migration-0095 rows, or an ambiguous
// backfill case) are kept in EVERY period's view rather than silently
// dropped from all of them: hiding real hours/pay from a TA because of a
// missing snapshot would be a worse failure than occasionally showing an old
// row under a period it may not exactly belong to.
func (s *DashboardService) TaOverview(ctx context.Context, taID uuid.UUID, enrollmentID *uuid.UUID) ([]TACourseStatus, error) {
	filter := ""
	args := []any{taID}
	if enrollmentID != nil {
		filter = " AND (a.enrollment_id = $2 OR a.enrollment_id IS NULL)"
		args = append(args, *enrollmentID)
	}
	rows, err := s.pool.Query(ctx, `
		WITH latest AS (SELECT * FROM pay_rates ORDER BY effective_from DESC LIMIT 1),
		assign AS (
		    SELECT tc.id AS tc_id, tc.code, tc.name_th, tc.exported_at,
		           t.academic_year, t.semester,
		           a.id AS assignment_id, a.level::text AS level,
		           COALESCE(a.cotaught_group::text, a.id::text) AS grp,
		           sec.track::text AS track
		    FROM ta_request_assignments a
		    JOIN ta_requests r ON r.id=a.request_id AND r.status='approved'
		    JOIN sections sec ON sec.id=a.section_id
		    JOIN teaching_courses tc ON tc.id=sec.teaching_course_id
		    JOIN academic_terms t ON t.id=tc.term_id
		    WHERE a.ta_id = $1`+filter+`
		),
		-- One sitting per co-taught group: the same คาบ written against two
		-- sections is one คาบ. Regular if any regular section shares it.
		sitting AS (
		    SELECT s.tc_id, s.level, s.grp, wl.status, wl.work_date, wl.start_time, wl.end_time,
		           MAX(wl.hours) AS hours,
		           BOOL_OR(s.track = 'regular') AS regular
		    FROM assign s
		    JOIN work_logs wl ON wl.assignment_id = s.assignment_id
		    GROUP BY 1, 2, 3, 4, 5, 6, 7
		),
		priced AS (
		    SELECT st.*,
		           st.hours * CASE
		               WHEN st.level='undergrad' AND st.regular     THEN pr.undergrad_regular
		               WHEN st.level='undergrad' AND NOT st.regular THEN pr.undergrad_special
		               WHEN st.level IN ('master','phd') AND st.regular THEN pr.graduate_regular_hourly
		               ELSE 0
		           END AS baht
		    FROM sitting st CROSS JOIN latest pr
		)
		SELECT c.tc_id, c.code, c.name_th,
		       c.academic_year || '/' || c.semester,
		       c.level,
		       COALESCE(SUM(p.hours) FILTER (WHERE p.status='approved'),0),
		       COALESCE(SUM(p.hours) FILTER (WHERE p.status='approved' AND p.regular),0),
		       COALESCE(SUM(p.hours) FILTER (WHERE p.status='approved' AND NOT p.regular),0),
		       COALESCE(SUM(p.hours) FILTER (WHERE p.status IN ('draft','submitted')),0),
		       COALESCE(SUM(p.hours) FILTER (WHERE p.status IN ('draft','submitted') AND p.regular),0),
		       COALESCE(SUM(p.hours) FILTER (WHERE p.status IN ('draft','submitted') AND NOT p.regular),0),
		       COALESCE(SUM(p.baht) FILTER (WHERE p.status='approved'),0),
		       COALESCE(SUM(p.baht) FILTER (WHERE p.status='approved' AND p.regular),0),
		       COALESCE(SUM(p.baht) FILTER (WHERE p.status='approved' AND NOT p.regular),0),
		       COALESCE(BOOL_OR(p.status='approved'),  FALSE) AS any_approved,
		       COALESCE(BOOL_OR(p.status='submitted'), FALSE) AS any_submitted,
		       c.exported_at IS NOT NULL AS exported
		FROM (SELECT DISTINCT tc_id, code, name_th, exported_at, academic_year, semester,
		             -- A TA holds one level per course; MIN only settles ties
		             -- that cannot happen.
		             MIN(level) OVER (PARTITION BY tc_id) AS level
		      FROM assign) c
		LEFT JOIN priced p ON p.tc_id = c.tc_id
		GROUP BY c.tc_id, c.code, c.name_th, c.academic_year, c.semester, c.level, c.exported_at
		ORDER BY c.academic_year DESC, c.semester DESC, c.code`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TACourseStatus{}
	for rows.Next() {
		var r TACourseStatus
		var anyApproved, anySubmitted, exported bool
		if err := rows.Scan(&r.TeachingCourseID, &r.CourseCode, &r.CourseNameTH,
			&r.TermLabel, &r.Level,
			&r.HoursApproved, &r.HoursApprovedRegular, &r.HoursApprovedSpecial,
			&r.HoursPending, &r.HoursPendingRegular, &r.HoursPendingSpecial,
			&r.EstimatedBaht, &r.EstimatedBahtRegular, &r.EstimatedBahtSpecial,
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
	// Hours are sittings, split by the track they are billed on — the same
	// counting as the TA's own card (TaOverview) and the approval queue.
	HoursPending         float64 `json:"hours_pending_approval"`
	HoursPendingRegular  float64 `json:"hours_pending_regular"`
	HoursPendingSpecial  float64 `json:"hours_pending_special"`
	HoursApproved        float64 `json:"hours_approved"`
	HoursApprovedRegular float64 `json:"hours_approved_regular"`
	HoursApprovedSpecial float64 `json:"hours_approved_special"`
	EstimatedBaht        float64 `json:"estimated_baht"`
	BudgetMax            float64 `json:"budget_max"`
	BudgetUsed           float64 `json:"budget_used"`
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
		WITH latest AS (SELECT * FROM pay_rates ORDER BY effective_from DESC LIMIT 1),
		course AS (
		    SELECT tc.id, tc.code, tc.name_th, t.academic_year, t.semester
		    FROM teaching_courses tc
		    JOIN academic_terms t ON t.id=tc.term_id
		    JOIN teaching_lecturers tl ON tl.teaching_course_id=tc.id
		    WHERE tl.lecturer_id = $1`+termFilter+`
		),
		assign AS (
		    SELECT sec.teaching_course_id AS tc_id, a.id AS assignment_id, a.ta_id,
		           a.level::text AS level, sec.track::text AS track,
		           COALESCE(a.cotaught_group::text, a.id::text) AS grp
		    FROM ta_requests r
		    JOIN ta_request_assignments a ON a.request_id=r.id
		    JOIN sections sec ON sec.id=a.section_id
		    WHERE r.status='approved' AND sec.teaching_course_id IN (SELECT id FROM course)
		),
		-- One sitting per co-taught group (a คาบ written against two sections
		-- is one คาบ), regular if any regular section shares it — rule B2.
		sitting AS (
		    SELECT s.tc_id, s.ta_id, s.level, s.grp, wl.status, wl.work_date, wl.start_time, wl.end_time,
		           MAX(wl.hours) AS hours,
		           BOOL_OR(s.track = 'regular') AS regular
		    FROM assign s
		    JOIN work_logs wl ON wl.assignment_id = s.assignment_id
		    -- Grad-special (master/phd on a special-track section) no longer
		    -- logs work_logs at all — pay is computed automatically from the
		    -- regular track's class schedule (2026 meeting). Same exclusion as
		    -- worklog.go's ListPending/ListByCourse and every other queue this
		    -- lecturer sees: without it, a leftover 'submitted' row from before
		    -- that change creates a "ต้องดำเนินการ" card on the homepage that can
		    -- never be resolved — nothing ever approves or rejects it.
		    WHERE (s.level NOT IN ('master','phd') OR s.track <> 'special')
		    GROUP BY 1, 2, 3, 4, 5, 6, 7, 8
		),
		priced AS (
		    SELECT st.*,
		           st.hours * CASE
		               WHEN st.level='undergrad' AND st.regular     THEN pr.undergrad_regular
		               WHEN st.level='undergrad' AND NOT st.regular THEN pr.undergrad_special
		               WHEN st.level IN ('master','phd') AND st.regular THEN pr.graduate_regular_hourly
		               ELSE 0
		           END AS baht
		    FROM sitting st CROSS JOIN latest pr
		)
		SELECT c.id, c.code, c.name_th,
		       c.academic_year || '/' || c.semester,
		       (SELECT COUNT(DISTINCT ta_id) FROM assign WHERE tc_id = c.id),
		       COUNT(DISTINCT p.ta_id) FILTER (WHERE p.status='submitted'),
		       COALESCE(SUM(p.hours) FILTER (WHERE p.status='submitted'),0),
		       COALESCE(SUM(p.hours) FILTER (WHERE p.status='submitted' AND p.regular),0),
		       COALESCE(SUM(p.hours) FILTER (WHERE p.status='submitted' AND NOT p.regular),0),
		       COALESCE(SUM(p.hours) FILTER (WHERE p.status='approved'),0),
		       COALESCE(SUM(p.hours) FILTER (WHERE p.status='approved' AND p.regular),0),
		       COALESCE(SUM(p.hours) FILTER (WHERE p.status='approved' AND NOT p.regular),0),
		       COALESCE(SUM(p.baht) FILTER (WHERE p.status='approved'),0)
		FROM course c
		LEFT JOIN priced p ON p.tc_id = c.id
		GROUP BY c.id, c.code, c.name_th, c.academic_year, c.semester
		ORDER BY c.academic_year DESC, c.semester DESC, c.code`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LecturerCourseStatus{}
	for rows.Next() {
		var r LecturerCourseStatus
		if err := rows.Scan(&r.TeachingCourseID, &r.CourseCode, &r.CourseNameTH,
			&r.TermLabel, &r.TACount, &r.TAsPending,
			&r.HoursPending, &r.HoursPendingRegular, &r.HoursPendingSpecial,
			&r.HoursApproved, &r.HoursApprovedRegular, &r.HoursApprovedSpecial,
			&r.EstimatedBaht); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
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
func (s *DashboardService) Executive(ctx context.Context, termID *uuid.UUID, budget *BudgetService, appointments *AppointmentOrderService) (*ExecutiveSummary, error) {
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
		    JOIN academic_terms trm       ON trm.id = tc.term_id
		    JOIN submission_periods sp    ON sp.term_id = tc.term_id
		    JOIN sections sec             ON sec.teaching_course_id = tc.id
		    JOIN ta_request_assignments a ON a.section_id = sec.id AND a.state <> 'dropped'
		    JOIN ta_requests r            ON r.id = a.request_id AND r.status = 'approved'
		    JOIN work_logs wl             ON wl.assignment_id = a.id
		                                 AND `+workLogInPeriodSQL("wl", "trm", "sp")+`
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
		      -- Grad-special no longer logs work_logs and is excluded from
		      -- ListReviewQueue — leftover approved rows from before that change
		      -- must not inflate this badge with work no queue will ever show.
		      AND (a.level::text NOT IN ('master','phd') OR sec.track <> 'special')
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
		    JOIN academic_terms trm       ON trm.id = tc.term_id
		    JOIN submission_periods sp    ON sp.term_id = tc.term_id
		    JOIN sections sec             ON sec.teaching_course_id = tc.id
		    JOIN ta_request_assignments a ON a.section_id = sec.id AND a.state <> 'dropped'
		    JOIN ta_requests r            ON r.id = a.request_id AND r.status = 'approved'
		    JOIN work_logs wl             ON wl.assignment_id = a.id
		                                 AND `+workLogInPeriodSQL("wl", "trm", "sp")+`
		    LEFT JOIN submission_period_status st
		           ON st.submission_period_id = sp.id
		          AND st.ta_id = a.ta_id
		          AND st.teaching_course_id = tc.id
		    WHERE tc.term_id = $1
		      AND `+AppointedSQL("tc.id", "a.ta_id")+`
		      -- Same grad-special exclusion as above: leftover rows from before
		      -- TAs stopped logging must not make a course look actionable when
		      -- the review queue behind the badge would show nothing for it.
		      AND (a.level::text NOT IN ('master','phd') OR sec.track <> 'special')
		    GROUP BY tc.id, sp.id, a.ta_id, COALESCE(st.status, 'pending')
		)
		SELECT COUNT(DISTINCT tc_id) FROM months
		 WHERE (status = 'pending' AND open_rows = 0)
		    OR status = 'staff_reviewed'`, tid).Scan(&sum.PayoutCoursesActionable)

	if appointments != nil {
		if n, err := appointments.PendingCount(ctx, tid); err == nil {
			sum.PendingAppointments = n
		}
	}

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
