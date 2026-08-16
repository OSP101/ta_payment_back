package demo

import (
	"context"

	"ta-payment-back/internal/service"
)

// ScenarioEventStatus adds a cheap "already done?" read to ScenarioEvent, so
// the simulator panel can show a checkmark instead of making staff guess
// whether they already clicked a step in an earlier visit. Deliberately NOT
// a "can I run this yet" gate — every step already explains what it's
// missing in its own error message when actually run (see scenario_steps.go
// docs), which is simpler than maintaining a second, parallel description of
// the same prerequisites here.
type ScenarioEventStatus struct {
	ScenarioEvent
	Done bool `json:"done"`
}

// ScenarioStatus reports Done for every event in one pass. Each check
// mirrors the "already did this" guard the matching step function in
// scenario_steps.go runs for itself — kept as its own small query per event
// rather than factored into a single shared helper, because what "done"
// means is a different shape for almost every step (a row exists vs. a
// count threshold vs. no rows left in a particular state).
func ScenarioStatus(ctx context.Context, svc *service.Container) ([]ScenarioEventStatus, error) {
	termID, _ := activeTermID(ctx, svc) // uuid.Nil if not created yet — every query below tolerates that

	done := map[string]bool{}
	scan := func(key, query string, args ...any) error {
		var b bool
		if err := svc.Pool.QueryRow(ctx, query, args...).Scan(&b); err != nil {
			return err
		}
		done[key] = b
		return nil
	}

	if err := scan("term", `SELECT EXISTS(SELECT 1 FROM academic_terms WHERE is_active)`); err != nil {
		return nil, err
	}
	if err := scan("courses",
		`SELECT COUNT(*) >= 3 FROM teaching_courses WHERE term_id=$1 AND code = ANY($2)`,
		termID, courseCodes()); err != nil {
		return nil, err
	}
	if err := scan("submission_periods",
		`SELECT EXISTS(SELECT 1 FROM submission_periods WHERE term_id=$1 AND year_month=$2)`,
		termID, currentYearMonth()); err != nil {
		return nil, err
	}
	if err := scan("ta_schedules",
		`SELECT COUNT(DISTINCT user_id) >= 4 FROM ta_class_schedules
		 WHERE term_id=$1 AND user_id IN (SELECT id FROM users WHERE email LIKE 'ta%@demo.local')`,
		termID); err != nil {
		return nil, err
	}
	if err := scan("ta_requests",
		`SELECT COUNT(*) >= 3 FROM ta_requests r
		 JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		 WHERE tc.term_id=$1 AND tc.code = ANY($2)`,
		termID, courseCodes()); err != nil {
		return nil, err
	}
	if err := scan("appointment_order",
		`SELECT EXISTS(SELECT 1 FROM appointment_orders WHERE term_id=$1)`, termID); err != nil {
		return nil, err
	}
	if err := scan("ta_docs",
		`SELECT COUNT(*) >= 3 FROM ta_profiles
		 WHERE user_id IN (SELECT id FROM users WHERE email = ANY($1)) AND completed_at IS NOT NULL`,
		taEmails()); err != nil {
		return nil, err
	}
	if err := scan("docs_approve",
		`SELECT COUNT(*) >= 3 FROM ta_profiles
		 WHERE user_id IN (SELECT id FROM users WHERE email = ANY($1)) AND status = 'approved'`,
		taEmails()); err != nil {
		return nil, err
	}
	if err := scan("worklog_submit",
		`SELECT COUNT(*) > 0 FROM work_logs wl
		 JOIN ta_request_assignments a ON a.id = wl.assignment_id
		 JOIN ta_requests r ON r.id = a.request_id
		 JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		 WHERE tc.term_id=$1 AND tc.code = ANY($2) AND wl.status IN ('submitted','approved')`,
		termID, courseCodes()); err != nil {
		return nil, err
	}
	if err := scan("worklog_approve",
		`SELECT (COUNT(*) FILTER (WHERE wl.status = 'approved')) > 0
		        AND (COUNT(*) FILTER (WHERE wl.status = 'submitted')) = 0
		 FROM work_logs wl
		 JOIN ta_request_assignments a ON a.id = wl.assignment_id
		 JOIN ta_requests r ON r.id = a.request_id
		 JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		 WHERE tc.term_id=$1 AND tc.code = ANY($2)`,
		termID, courseCodes()); err != nil {
		return nil, err
	}
	if err := scan("staff_review_export",
		`SELECT COUNT(*) >= 3 FROM teaching_courses WHERE term_id=$1 AND code = ANY($2) AND exported_at IS NOT NULL`,
		termID, courseCodes()); err != nil {
		return nil, err
	}
	if err := scan("transfer_cover",
		`SELECT EXISTS(SELECT 1 FROM transfer_cover_exports WHERE term_id=$1)`, termID); err != nil {
		return nil, err
	}

	out := make([]ScenarioEventStatus, len(scenarioEvents))
	for i, ev := range scenarioEvents {
		out[i] = ScenarioEventStatus{ScenarioEvent: ev, Done: done[ev.Key]}
	}
	return out, nil
}

func courseCodes() []string {
	codes := make([]string, len(courseSeeds))
	for i, cs := range courseSeeds {
		codes[i] = cs.code
	}
	return codes
}

func taEmails() []string {
	emails := make([]string, len(courseSeeds))
	for i, cs := range courseSeeds {
		emails[i] = cs.taEmail
	}
	return emails
}
