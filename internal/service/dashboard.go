package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DashboardService struct {
	pool *pgxpool.Pool
}

type ExecutiveSummary struct {
	TotalCourses    int     `json:"total_courses"`
	CoursesWithTA   int     `json:"courses_with_ta"`
	TotalTAs        int     `json:"total_tas"`
	PendingReviews  int     `json:"pending_reviews"`
	BudgetAllocated float64 `json:"budget_allocated"`
	BudgetUsed      float64 `json:"budget_used"`
}

// TACourseStatus is one row on the TA landing dashboard: for a course the TA
// is assigned to, what stage the workflow is at + estimated pay so far.
type TACourseStatus struct {
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	CourseCode       string    `json:"course_code"`
	CourseNameTH     string    `json:"course_name_th"`
	TermLabel        string    `json:"term_label"`
	Stage            string    `json:"stage"`          // draft/submitted/approved/exported
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
		SELECT tc.id, fc.code, fc.name_th,
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
		JOIN faculty_courses fc ON fc.id=tc.faculty_course_id
		JOIN academic_terms t ON t.id=tc.term_id
		LEFT JOIN work_logs wl ON wl.assignment_id=a.id
		CROSS JOIN latest pr
		WHERE a.ta_id = $1
		GROUP BY tc.id, fc.code, fc.name_th, t.academic_year, t.semester, a.level, sec.track, tc.exported_at
		ORDER BY t.academic_year DESC, t.semester DESC, fc.code`, taID)
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
		SELECT tc.id, fc.code, fc.name_th,
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
		JOIN faculty_courses fc ON fc.id=tc.faculty_course_id
		JOIN academic_terms t ON t.id=tc.term_id
		JOIN teaching_lecturers tl ON tl.teaching_course_id=tc.id
		LEFT JOIN ta_requests r ON r.teaching_course_id=tc.id AND r.status='approved'
		LEFT JOIN ta_request_assignments a ON a.request_id=r.id
		LEFT JOIN sections sec ON sec.id=a.section_id
		LEFT JOIN work_logs wl ON wl.assignment_id=a.id
		CROSS JOIN latest pr
		WHERE tl.lecturer_id = $1`+termFilter+`
		GROUP BY tc.id, fc.code, fc.name_th, t.academic_year, t.semester
		ORDER BY t.academic_year DESC, t.semester DESC, fc.code`, args...)
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

func (s *DashboardService) Executive(ctx context.Context, termID *uuid.UUID) (*ExecutiveSummary, error) {
	sum := &ExecutiveSummary{}
	termFilter := ""
	args := []any{}
	if termID != nil {
		termFilter = " WHERE tc.term_id = $1"
		args = append(args, *termID)
	}
	_ = s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM teaching_courses tc`+termFilter, args...).Scan(&sum.TotalCourses)
	_ = s.pool.QueryRow(ctx, `SELECT COUNT(DISTINCT r.teaching_course_id)
		FROM ta_requests r
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id`+termFilter+` AND r.status='approved'`,
		args...).Scan(&sum.CoursesWithTA)
	_ = s.pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT a.ta_id)
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id=a.request_id
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id`+termFilter, args...).Scan(&sum.TotalTAs)
	_ = s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM ta_profiles WHERE status IN ('submitted','needs_fix')`).Scan(&sum.PendingReviews)
	_ = s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM((SELECT per_course_max FROM budget_caps ORDER BY effective_from DESC LIMIT 1)), 0) FROM teaching_courses tc`+termFilter,
		args...).Scan(&sum.BudgetAllocated)
	_ = s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(wl.hours *
		 CASE WHEN u.study_level='undergrad' AND sec.track='regular' THEN pr.undergrad_regular
		      WHEN u.study_level='undergrad' AND sec.track='special' THEN pr.undergrad_special
		      WHEN u.study_level IN ('master','phd') AND sec.track='regular' THEN pr.graduate_regular
		      ELSE 0 END
		),0)
		FROM work_logs wl JOIN ta_request_assignments a ON a.id=wl.assignment_id
		JOIN ta_requests r ON r.id=a.request_id
		JOIN teaching_courses tc ON tc.id=r.teaching_course_id
		JOIN sections sec ON sec.id=a.section_id
		JOIN users u ON u.id=a.ta_id
		CROSS JOIN LATERAL (SELECT * FROM pay_rates ORDER BY effective_from DESC LIMIT 1) pr
		WHERE wl.status='approved'`+
			func() string { if termID != nil { return " AND tc.term_id = $1" }; return "" }(),
		args...).Scan(&sum.BudgetUsed)
	return sum, nil
}
