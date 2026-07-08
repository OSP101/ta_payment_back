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
