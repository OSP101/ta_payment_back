package demo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	// SubSteps is only populated for the 8 events that create more than one
	// record: the 3 staff-actor ones (courses, docs_approve,
	// staff_review_export) plus the 5 TA/lecturer-actor ones
	// (ta_schedules, ta_requests, ta_docs, worklog_submit,
	// worklog_approve) — every event key EXCEPT "term", "submission_periods",
	// "appointment_order", "transfer_cover", which create exactly one
	// record and so have nothing finer than their own top-level Done to
	// report. It lets the guided panel tick off "รายวิชา CP100002 ยังไม่เสร็จ"
	// individually instead of forcing an all-or-nothing checkmark on a step
	// that's really 3-4 independent pieces of work, each possibly done by a
	// DIFFERENT seeded account (see SubStep.Actor below).
	SubSteps []SubStep `json:"sub_steps,omitempty"`
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

	courseSubs, err := courseSubSteps(ctx, svc, termID)
	if err != nil {
		return nil, err
	}
	docsSubs, err := docsApproveSubSteps(ctx, svc)
	if err != nil {
		return nil, err
	}
	exportSubs, err := staffReviewExportSubSteps(ctx, svc, termID)
	if err != nil {
		return nil, err
	}
	scheduleSubs, err := taScheduleSubSteps(ctx, svc, termID)
	if err != nil {
		return nil, err
	}
	requestSubs, err := taRequestSubSteps(ctx, svc, termID)
	if err != nil {
		return nil, err
	}
	taDocSubs, err := taDocsSubSteps(ctx, svc)
	if err != nil {
		return nil, err
	}
	worklogSubmitSubs, err := worklogSubmitSubSteps(ctx, svc, termID)
	if err != nil {
		return nil, err
	}
	worklogApproveSubs, err := worklogApproveSubSteps(ctx, svc, termID)
	if err != nil {
		return nil, err
	}
	subSteps := map[string][]SubStep{
		"courses":             courseSubs,
		"docs_approve":        docsSubs,
		"staff_review_export": exportSubs,
		"ta_schedules":        scheduleSubs,
		"ta_requests":         requestSubs,
		"ta_docs":             taDocSubs,
		"worklog_submit":      worklogSubmitSubs,
		"worklog_approve":     worklogApproveSubs,
	}

	out := make([]ScenarioEventStatus, len(scenarioEvents))
	for i, ev := range scenarioEvents {
		out[i] = ScenarioEventStatus{ScenarioEvent: ev, Done: done[ev.Key], SubSteps: subSteps[ev.Key]}
	}
	return out, nil
}

// SubStep is one DB-verifiable record within a multi-record scenario event —
// e.g. one of the 3 seeded courses, or one TA's document approval. Key
// matches the identifier the frontend's static field-level guide content
// (app/lib/demoStepGuides.ts) is keyed by (a course code or a TA email), so
// the two sides can be edited independently without drifting apart: this
// file owns "is it verifiably done", the frontend owns "what exactly should
// staff click/type to get there".
type SubStep struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Done  bool   `json:"done"`
	// Actor is the specific seeded account email that must perform this
	// record — set only for the TA/lecturer-actor events (ta_schedules,
	// ta_requests, ta_docs, worklog_submit, worklog_approve), where
	// different records genuinely need different logins (course
	// CP100002's nomination can only be done as lecturer2, not lecturer1).
	// Empty for the staff-actor events' sub-steps (courses, docs_approve,
	// staff_review_export) — those are all done from the one shared staff
	// login already active for the rest of the guided walkthrough.
	Actor string `json:"actor,omitempty"`
	// ActorPath is this record's own real page for Actor to act on —
	// distinct from ScenarioEvent.ActorPath (which only covers events with
	// no per-course URL). Set only for the 3 course-scoped TA/lecturer
	// events (ta_requests, worklog_submit, worklog_approve), each resolved
	// via the course's actual teaching_courses.id — empty (falls back to
	// the parent event's own ActorPath) for ta_schedules/ta_docs, and
	// unused entirely for the 3 staff-actor multi-record events.
	ActorPath string `json:"actor_path,omitempty"`
}

// courseIDByCode resolves a course code to its real DB id, needed to build a
// SubStep's per-course ActorPath (e.g. "/lecturer/courses/{id}/request").
// Returns uuid.Nil (no error) when the course doesn't exist yet — callers
// treat that as "not done, no real page to offer yet" rather than a query
// failure, since this runs before the "courses" step exists on a fresh
// workspace and ScenarioStatus must still return a full status list then.
func courseIDByCode(ctx context.Context, svc *service.Container, termID uuid.UUID, code string) (uuid.UUID, error) {
	var id uuid.UUID
	err := svc.Pool.QueryRow(ctx,
		`SELECT id FROM teaching_courses WHERE term_id=$1 AND code=$2`, termID, code,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil
	}
	return id, err
}

// taScheduleSubSteps mirrors the "ta_schedules" event's per-TA EXISTS
// condition, one row per TA instead of a >=4 distinct-user count. Covers
// all 4 seeded TAs (allTAEmails), not just the 3 in courseSeeds — see that
// slice's own doc comment.
func taScheduleSubSteps(ctx context.Context, svc *service.Container, termID uuid.UUID) ([]SubStep, error) {
	out := make([]SubStep, len(allTAEmails))
	for i, email := range allTAEmails {
		var exists bool
		if err := svc.Pool.QueryRow(ctx,
			`SELECT EXISTS(
			   SELECT 1 FROM ta_class_schedules
			   WHERE term_id=$1 AND user_id = (SELECT id FROM users WHERE email=$2)
			 )`,
			termID, email,
		).Scan(&exists); err != nil {
			return nil, err
		}
		out[i] = SubStep{
			Key:   email,
			Label: fmt.Sprintf("ตารางเรียนของ TA คนที่ %d (%s)", i+1, email),
			Done:  exists,
			Actor: email,
		}
	}
	return out, nil
}

// taRequestSubSteps mirrors the "ta_requests" event's per-course EXISTS
// condition, one row per course/lecturer instead of a >=3 count.
func taRequestSubSteps(ctx context.Context, svc *service.Container, termID uuid.UUID) ([]SubStep, error) {
	out := make([]SubStep, len(courseSeeds))
	for i, cs := range courseSeeds {
		courseID, err := courseIDByCode(ctx, svc, termID, cs.code)
		if err != nil {
			return nil, err
		}
		var exists bool
		if courseID != uuid.Nil {
			if err := svc.Pool.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM ta_requests WHERE teaching_course_id=$1)`, courseID,
			).Scan(&exists); err != nil {
				return nil, err
			}
		}
		sub := SubStep{
			Key:   cs.code,
			Label: fmt.Sprintf("วิชา %s — อาจารย์ผู้สอน %d เสนอชื่อ TA", cs.code, i+1),
			Done:  exists,
			Actor: cs.lecturerEmail,
		}
		if courseID != uuid.Nil {
			sub.ActorPath = fmt.Sprintf("/lecturer/courses/%s/request", courseID)
		}
		out[i] = sub
	}
	return out, nil
}

// taDocsSubSteps mirrors the "ta_docs" event's per-TA completed_at
// condition — the TA's OWN submission, not staff's later approval of it
// (that's docsApproveSubSteps, a different event with the same shape).
func taDocsSubSteps(ctx context.Context, svc *service.Container) ([]SubStep, error) {
	out := make([]SubStep, len(courseSeeds))
	for i, cs := range courseSeeds {
		var completed bool
		if err := svc.Pool.QueryRow(ctx,
			`SELECT COALESCE(
			   (SELECT completed_at IS NOT NULL FROM ta_profiles
			    WHERE user_id = (SELECT id FROM users WHERE email = $1)),
			   FALSE)`,
			cs.taEmail,
		).Scan(&completed); err != nil {
			return nil, err
		}
		out[i] = SubStep{
			Key:   cs.taEmail,
			Label: fmt.Sprintf("TA คนที่ %d ส่งเอกสารของตัวเอง (%s)", i+1, cs.taEmail),
			Done:  completed,
			Actor: cs.taEmail,
		}
	}
	return out, nil
}

// worklogSubmitSubSteps mirrors the "worklog_submit" event's per-course
// status IN ('submitted','approved') condition, one row per TA/course
// instead of a plain COUNT(*) > 0 across all of them.
func worklogSubmitSubSteps(ctx context.Context, svc *service.Container, termID uuid.UUID) ([]SubStep, error) {
	out := make([]SubStep, len(courseSeeds))
	for i, cs := range courseSeeds {
		courseID, err := courseIDByCode(ctx, svc, termID, cs.code)
		if err != nil {
			return nil, err
		}
		var submitted bool
		if courseID != uuid.Nil {
			if err := svc.Pool.QueryRow(ctx,
				`SELECT EXISTS(
				   SELECT 1 FROM work_logs wl
				   JOIN ta_request_assignments a ON a.id = wl.assignment_id
				   JOIN ta_requests r ON r.id = a.request_id
				   WHERE r.teaching_course_id = $1
				     AND a.ta_id = (SELECT id FROM users WHERE email = $2)
				     AND a.state <> 'dropped'
				     AND wl.status IN ('submitted','approved')
				 )`,
				courseID, cs.taEmail,
			).Scan(&submitted); err != nil {
				return nil, err
			}
		}
		sub := SubStep{
			Key:   cs.taEmail,
			Label: fmt.Sprintf("TA คนที่ %d ส่งบันทึกเวลา วิชา %s", i+1, cs.code),
			Done:  submitted,
			Actor: cs.taEmail,
		}
		if courseID != uuid.Nil {
			sub.ActorPath = fmt.Sprintf("/ta/courses/%s/worklog", courseID)
		}
		out[i] = sub
	}
	return out, nil
}

// worklogApproveSubSteps mirrors the "worklog_approve" event's per-course
// "approved>0 and nothing still submitted" condition, one row per
// lecturer/course instead of aggregated across all of them.
func worklogApproveSubSteps(ctx context.Context, svc *service.Container, termID uuid.UUID) ([]SubStep, error) {
	out := make([]SubStep, len(courseSeeds))
	for i, cs := range courseSeeds {
		courseID, err := courseIDByCode(ctx, svc, termID, cs.code)
		if err != nil {
			return nil, err
		}
		var done bool
		if courseID != uuid.Nil {
			if err := svc.Pool.QueryRow(ctx,
				`SELECT (COUNT(*) FILTER (WHERE wl.status = 'approved')) > 0
				        AND (COUNT(*) FILTER (WHERE wl.status = 'submitted')) = 0
				 FROM work_logs wl
				 JOIN ta_request_assignments a ON a.id = wl.assignment_id
				 JOIN ta_requests r ON r.id = a.request_id
				 WHERE r.teaching_course_id = $1
				   AND a.ta_id = (SELECT id FROM users WHERE email = $2)
				   AND a.state <> 'dropped'`,
				courseID, cs.taEmail,
			).Scan(&done); err != nil {
				return nil, err
			}
		}
		sub := SubStep{
			Key:   cs.lecturerEmail,
			Label: fmt.Sprintf("อาจารย์ผู้สอน %d อนุมัติบันทึกเวลา วิชา %s", i+1, cs.code),
			Done:  done,
			Actor: cs.lecturerEmail,
		}
		if courseID != uuid.Nil {
			sub.ActorPath = fmt.Sprintf("/lecturer/courses/%s/reports", courseID)
		}
		out[i] = sub
	}
	return out, nil
}

// courseSubSteps reports the same per-course EXISTS check stepCourses itself
// runs to decide what to skip (see scenario_steps.go) — exposed here per
// course instead of folded into the "courses" event's >=3 threshold, so a
// guided staff member sees which specific course is still missing.
func courseSubSteps(ctx context.Context, svc *service.Container, termID uuid.UUID) ([]SubStep, error) {
	out := make([]SubStep, len(courseSeeds))
	for i, cs := range courseSeeds {
		var exists bool
		if err := svc.Pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM teaching_courses WHERE term_id=$1 AND code=$2)`,
			termID, cs.code,
		).Scan(&exists); err != nil {
			return nil, err
		}
		out[i] = SubStep{Key: cs.code, Label: fmt.Sprintf("รายวิชา %s — %s", cs.code, cs.nameTH), Done: exists}
	}
	return out, nil
}

// docsApproveSubSteps mirrors the "docs_approve" event's per-TA
// status='approved' condition, one row per TA instead of a >=3 count.
func docsApproveSubSteps(ctx context.Context, svc *service.Container) ([]SubStep, error) {
	out := make([]SubStep, len(courseSeeds))
	for i, cs := range courseSeeds {
		var approved bool
		if err := svc.Pool.QueryRow(ctx,
			`SELECT COALESCE(
			   (SELECT status = 'approved' FROM ta_profiles
			    WHERE user_id = (SELECT id FROM users WHERE email = $1)),
			   FALSE)`,
			cs.taEmail,
		).Scan(&approved); err != nil {
			return nil, err
		}
		out[i] = SubStep{
			Key:   cs.taEmail,
			Label: fmt.Sprintf("เอกสารของ TA คนที่ %d (%s)", i+1, cs.taEmail),
			Done:  approved,
		}
	}
	return out, nil
}

// staffReviewExportSubSteps mirrors the "staff_review_export" event's
// per-course exported_at IS NOT NULL condition, one row per course instead
// of a >=3 count. Note this does NOT reflect the finance-sent flag
// stepStaffReviewExport also sets (see that function's doc comment) — there
// is no staff-facing UI action for that half, so it stays fully automatic
// and isn't something a guided sub-step could ever show as "done by hand".
func staffReviewExportSubSteps(ctx context.Context, svc *service.Container, termID uuid.UUID) ([]SubStep, error) {
	out := make([]SubStep, len(courseSeeds))
	for i, cs := range courseSeeds {
		var exported bool
		if err := svc.Pool.QueryRow(ctx,
			`SELECT COALESCE(
			   (SELECT exported_at IS NOT NULL FROM teaching_courses WHERE term_id=$1 AND code=$2),
			   FALSE)`,
			termID, cs.code,
		).Scan(&exported); err != nil {
			return nil, err
		}
		out[i] = SubStep{Key: cs.code, Label: fmt.Sprintf("รายวิชา %s — %s", cs.code, cs.nameTH), Done: exported}
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
