package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

type TARequestService struct {
	pool   *pgxpool.Pool
	aud    *audit.Auditor
	budget *BudgetService
	notify *NotifyService
}

// querier is the read subset both *pgxpool.Pool and pgx.Tx satisfy. All
// eligibility helpers accept it so they can run inside an auto-decide tx (to
// see advisory locks) or on the pool (for read-only projections such as
// Detail's per-assignment warning banner).
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DecisionCheck is one line of the "การตรวจสอบของระบบ" checklist that the
// staff accordion renders. Rows are appended in a deterministic order per TA;
// callers should not mutate this list post-decision.
type DecisionCheck struct {
	Rule   string `json:"rule"`
	TAName string `json:"ta,omitempty"`
	Passed bool   `json:"passed"`
	// Warning marks a failing check as informational-only: staff still sees
	// it in the checklist, and reject_reason still lists it, but the overall
	// verdict is not flipped to 'rejected' just because of it. Used for
	// docs-not-approved so the lecturer can submit and staff can follow up.
	Warning bool   `json:"warning,omitempty"`
	Message string `json:"message"`
}

// CreateResult is what Create returns to the handler. Every submission
// resolves to either 'approved' or 'rejected' inside the tx; there is no
// 'submitted' state at rest under the auto-decide model.
type CreateResult struct {
	ID           uuid.UUID       `json:"id"`
	Status       string          `json:"status"`
	Checks       []DecisionCheck `json:"decision_checks"`
	RejectReason string          `json:"reject_reason,omitempty"`
}

// WorkloadInput's Hrs fields carry validate:"gte=0,lte=99" mirroring
// validateWorkloadFields below — each is stored as NUMERIC(4,2), so >99 is
// already rejected there; the tag just catches it before the DB round trip.
// Desc fields have no DB length limit (TEXT columns) but are free text a TA
// types, so max=500 bounds them to something sane.
type WorkloadInput struct {
	HelpTeachHrs  float64 `json:"help_teach_hrs" validate:"gte=0,lte=99"`
	HelpTeachDesc string  `json:"help_teach_desc" validate:"omitempty,max=500"`
	PrepHrs       float64 `json:"prep_hrs" validate:"gte=0,lte=99"`
	PrepDesc      string  `json:"prep_desc" validate:"omitempty,max=500"`
	GradeHrs      float64 `json:"grade_hrs" validate:"gte=0,lte=99"`
	GradeDesc     string  `json:"grade_desc" validate:"omitempty,max=500"`
	OtherHrs      float64 `json:"other_hrs" validate:"gte=0,lte=99"`
	OtherDesc     string  `json:"other_desc" validate:"omitempty,max=500"`
	CheckWorkHrs  float64 `json:"check_work_hrs" validate:"gte=0,lte=99"`
	AttendanceHrs float64 `json:"attendance_hrs" validate:"gte=0,lte=99"`
	UGOtherHrs    float64 `json:"ug_other_hrs" validate:"gte=0,lte=99"`
	UGOtherDesc   string  `json:"ug_other_desc" validate:"omitempty,max=500"`
	LabHrs        float64 `json:"lab_hrs" validate:"gte=0,lte=99"`
	// Support for a lab section done OUTSIDE the slot — prep, materials,
	// marking lab sheets. Mutually exclusive with LabHrs: a TA either runs the
	// session or supports it, never both for the same group.
	LabOtherHrs  float64 `json:"lab_other_hrs" validate:"gte=0,lte=99"`
	LabOtherDesc string  `json:"lab_other_desc" validate:"omitempty,max=500"`
}

type CreateTARequestInput struct {
	TeachingCourseID uuid.UUID `json:"teaching_course_id" validate:"required"`
	// oneof mirrors the exact check in Create below (lecture/lab/both).
	ReimburseScope string `json:"reimburse_scope" validate:"required,oneof=lecture lab both"`
	// Counts has no length floor in Create — a request can reference sections
	// purely through Assignments — so only its elements are validated (dive).
	Counts []struct {
		SectionID      uuid.UUID `json:"section_id" validate:"required"`
		UndergradCount int       `json:"undergrad_count" validate:"gte=0"`
		GraduateCount  int       `json:"graduate_count" validate:"gte=0"`
	} `json:"counts" validate:"omitempty,dive"`
	// required: Create rejects an empty Assignments list outright.
	Assignments []AssignmentInput `json:"assignments" validate:"required,dive"`
}

// AssignmentInput is one TA's placement within a request. Named rather than
// anonymous so callers and tests can build one without restating the whole
// shape — the previous inline struct had to be copied verbatim everywhere and
// broke at every field addition.
type AssignmentInput struct {
	// SectionIDs lists every section this TA is assigned to within the
	// request. Each becomes its own ta_request_assignments row so worklog can
	// attribute time to the specific section taught.
	SectionIDs []uuid.UUID `json:"section_ids" validate:"required,dive,required"`
	TAID       uuid.UUID   `json:"ta_id" validate:"required"`
	// Level is only a fallback: validateTA prefers the TA's own on-file study
	// level and only falls back to this value when the DB has none. Left
	// omittable for that reason; when present it must still be one of the
	// three levels the resolved value is checked against.
	Level string `json:"level" validate:"omitempty,oneof=undergrad master phd"`
	// Workload is the fallback declaration applied to every section that has
	// no entry in SectionWorkloads. Kept so a client that declares one figure
	// for the whole assignment keeps working.
	Workload WorkloadInput `json:"workload"`
	// SectionWorkloads carries the per-section hours the lecturers asked for:
	// each group gets its own figure, bounded by the course's weekly contact
	// hours for that kind — the numbers in the 3(3-0-6) notation. Optional:
	// resolveSectionWorkloads falls back to Workload above for any section
	// missing here.
	SectionWorkloads []SectionWorkload `json:"section_workloads" validate:"omitempty,dive"`
}

// SectionWorkload is one section's slice of a TA's declared weekly workload.
type SectionWorkload struct {
	SectionID uuid.UUID     `json:"section_id" validate:"required"`
	Workload  WorkloadInput `json:"workload"`
}

// Create runs input integrity checks upfront and then auto-decides the
// request atomically inside a single tx. Structural failures (unknown TA,
// wrong section, negative workload, etc.) return an error and never persist a
// row; business-rule failures (docs not approved, 3-course cap, time
// conflicts) persist as a 'rejected' row so the staff accordion can display
// the checklist that produced the verdict.
func (s *TARequestService) Create(ctx context.Context, lecturerID uuid.UUID, in CreateTARequestInput) (*CreateResult, error) {
	if in.TeachingCourseID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	if in.ReimburseScope != "lecture" && in.ReimburseScope != "lab" && in.ReimburseScope != "both" {
		return nil, errors.New("ประเภทการเบิกไม่ถูกต้อง")
	}
	if len(in.Assignments) == 0 {
		return nil, errors.New("ต้องระบุรายชื่อ TA อย่างน้อย 1 คน")
	}

	// The lecturer must actually teach this course.
	var teaches bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM teaching_lecturers WHERE teaching_course_id = $1 AND lecturer_id = $2)
	`, in.TeachingCourseID, lecturerID).Scan(&teaches); err != nil {
		return nil, err
	}
	if !teaches {
		return nil, errors.New("คุณไม่ได้เป็นผู้สอนของรายวิชานี้")
	}

	var termID uuid.UUID
	if err := s.pool.QueryRow(ctx, `
		SELECT tc.term_id FROM teaching_courses tc WHERE tc.id = $1
	`, in.TeachingCourseID).Scan(&termID); err != nil {
		return nil, errors.New("ไม่พบรายวิชาที่เลือก")
	}

	// WBA gate: a course whose section(s) still have no class schedule (the
	// registrar file said "will be arranged") cannot accept a TA request —
	// downstream worklog validation, budget math, and time-clock checks all
	// read the timetable. The lecturer/staff must fill the schedule first.
	var missingSchedule bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM sections sx
			WHERE sx.teaching_course_id = $1
			  AND NOT EXISTS(SELECT 1 FROM section_schedules ss WHERE ss.section_id = sx.id))
	`, in.TeachingCourseID).Scan(&missingSchedule); err != nil {
		return nil, err
	}
	if missingSchedule {
		return nil, Invalid("รายวิชานี้ยังไม่ระบุเวลาเรียน (WBA) กรุณากรอกตารางเรียนของทุก section ให้ครบก่อน จึงจะส่งคำขอ TA ได้")
	}

	// Capture which window admitted this request so window deletion can honour
	// the "in use" guard (M3).
	//
	// There is NO "closed" state: a TA request is never blocked by the window
	// (per the lecturers' request — deadlines inform, they don't gate). The
	// window only decides ONE thing: on-time vs late. Late requests carry
	// `is_late` so staff (and the lecturer) know the payout will be delayed.
	// When the term has no window configured at all, there is no deadline to
	// miss, so the request is on time and window_id stays NULL.
	var windowID *uuid.UUID
	var isLate bool
	err := s.pool.QueryRow(ctx, `
		SELECT w.id, (NOW() > w.closes_at) AS is_late
		FROM ta_request_windows w
		WHERE w.term_id = $1
		ORDER BY w.closes_at DESC LIMIT 1
	`, termID).Scan(&windowID, &isLate)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	// Guard against the same TA being listed twice within one request. The UI
	// prevents this today, but the API is public and mistakes upstream would
	// otherwise leak into workload/audit rows.
	seen := map[uuid.UUID]struct{}{}
	for _, a := range in.Assignments {
		if _, dup := seen[a.TAID]; dup {
			return nil, fmt.Errorf("มี %s ปรากฏซ้ำในคำขอเดียวกัน", s.taName(ctx, a.TAID))
		}
		seen[a.TAID] = struct{}{}
	}

	// Guard against a TA who already has an ACTIVE request on this same course
	// from an earlier round. autoDecide's own "duplicate" rule (below, inside
	// the tx) already catches this and auto-rejects the whole request — but
	// only after every row is written and the transaction runs, so from the
	// lecturer's side it read as "the system let me send it" followed by a
	// confusing rejected row cluttering their history (10/08/2026 feedback).
	// Failing here instead means the attempt is refused immediately, with no
	// rows ever written — same predicate, faster and clearer. The autoDecide
	// check stays in place as a safety net for a race between two submissions
	// that both pass this check before either commits.
	for _, a := range in.Assignments {
		var alreadyActive bool
		if err := s.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM ta_request_assignments a2
				JOIN ta_requests r2 ON r2.id = a2.request_id
				WHERE a2.ta_id = $1 AND r2.teaching_course_id = $2
				  AND r2.status IN ('submitted', 'approved')
				  AND a2.state <> 'dropped'
			)`, a.TAID, in.TeachingCourseID).Scan(&alreadyActive); err != nil {
			return nil, err
		}
		if alreadyActive {
			return nil, fmt.Errorf(
				"%s มีคำขอ TA ของวิชานี้ที่ยังดำเนินการอยู่แล้ว (จากรอบก่อนหน้า) ไม่สามารถส่งคำขอซ้ำได้ "+
					"หากต้องการแก้ไข กรุณายกเลิกคำขอเดิมก่อน แล้วจึงส่งคำขอใหม่",
				s.taName(ctx, a.TAID))
		}
	}

	// All referenced sections must belong to this course.
	sectionIDs := map[uuid.UUID]struct{}{}
	for _, c := range in.Counts {
		sectionIDs[c.SectionID] = struct{}{}
		if c.UndergradCount < 0 || c.GraduateCount < 0 {
			return nil, errors.New("จำนวน TA ต้องไม่ติดลบ")
		}
	}
	for _, a := range in.Assignments {
		if len(a.SectionIDs) == 0 {
			return nil, errors.New("ต้องเลือก section อย่างน้อย 1 กลุ่มให้ TA แต่ละคน")
		}
		seenSec := map[uuid.UUID]struct{}{}
		for _, sid := range a.SectionIDs {
			if _, dup := seenSec[sid]; dup {
				return nil, errors.New("มี section ซ้ำกันในรายการของ TA คนเดียว")
			}
			seenSec[sid] = struct{}{}
			sectionIDs[sid] = struct{}{}
		}
	}
	for secID := range sectionIDs {
		var ok bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM sections WHERE id = $1 AND teaching_course_id = $2)`,
			secID, in.TeachingCourseID).Scan(&ok); err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("พบ section ที่ไม่ได้อยู่ในรายวิชานี้")
		}
	}

	// Per-TA integrity validation only. Business rules (docs, schedule, cap,
	// time conflicts, workload totals) are evaluated by autoDecide below and
	// materialise as ✓/✗ checklist entries — not blocking errors — so the
	// resulting rejected request stays visible in the staff accordion.
	levels := make([]string, len(in.Assignments))
	// perSection[i][sectionID] is the workload row that assignment will carry.
	perSection := make([]map[uuid.UUID]WorkloadInput, len(in.Assignments))
	// coGroup[i][sectionID] groups sections this TA covers simultaneously, so
	// their hours are counted once rather than N times.
	coGroup := make([]map[uuid.UUID]int, len(in.Assignments))

	for i, a := range in.Assignments {
		name, level, err := s.validateTA(ctx, a.TAID, a.Level)
		if err != nil {
			return nil, err
		}
		levels[i] = level

		perSection[i] = resolveSectionWorkloads(a.SectionIDs, a.Workload, a.SectionWorkloads)
		coGroup[i], err = s.detectCotaughtGroups(ctx, a.SectionIDs)
		if err != nil {
			return nil, err
		}

		// The ceiling per section comes from the timetable, not the credits.
		weekly, err := s.sectionWeeklyHours(ctx, a.SectionIDs)
		if err != nil {
			return nil, err
		}

		for _, secID := range a.SectionIDs {
			w := perSection[i][secID]
			secNo := s.sectionLabel(ctx, secID)
			// Reject impossibly-shaped numeric input before it can overflow the
			// underlying NUMERIC(4,2) column or invalidate the total-hours rule
			// through cancellation. Not a "check" — this is corrupt data.
			if err := validateWorkloadFields(w, name); err != nil {
				return nil, err
			}
			// Each group's hours are bounded by how long that group actually
			// meets. Undergrad only; grad hours follow the 10–12 total rule
			// inside autoDecide.
			if level == "undergrad" {
				if err := validateUndergradSectionCaps(w, name, secNo, weekly[secID]); err != nil {
					return nil, err
				}
			}
		}

		// ประกาศคุมเป็น "ชม./วัน" แต่ฟอร์มกรอกเป็น "ชม./สัปดาห์" — ตรวจว่าที่กรอก
		// กระจายลงบันทึกเวลาจริงได้โดยไม่ชนเพดานรายวัน (ไม่งั้น auto-generate
		// จะข้ามคาบเงียบ ๆ และ TA เสียชั่วโมงที่ควรได้). Co-taught sections are
		// worked in a single sitting, so their hours count once here too.
		//
		// Undergrad only (2026 meeting correction): the request-submission gate
		// used to also refuse a grad TA whose sections' real timetable exceeded
		// grad_regular_daily_hour_cap (6 ชม./วัน), but graduate students are
		// bound only by the 10–12 ชม./สัปดาห์ total (checked in autoDecide) —
		// the daily-hour cap belongs to the WORKLOG entry point instead
		// (worklog.go's validateGradRegularClassWindow), not request submission.
		if level == "undergrad" {
			if err := s.enforceDailyHourFeasibility(
				ctx, a.SectionIDs, level, name,
				billableWeeklyTotal(a.SectionIDs, perSection[i], coGroup[i], level),
			); err != nil {
				return nil, err
			}
		}
	}

	// Case 2 of the deferred-decision model is NOT handled here. It used to be:
	// a knowable clash refused the whole submission on the spot. That made the
	// verdict depend on WHEN the TA filed their timetable — file it first and a
	// single clashing lab cost you the entire section; file it after the lecturer
	// submitted and applyClashOutcome trimmed just that session and kept the rest.
	// Same TA, same timetable, two different answers.
	//
	// Create now runs the same applyClashOutcome the deferred path runs (below),
	// so there is one clash rule in the system instead of two. See 3ก, 31/07/2026.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rid := uuid.New()
	if _, err := tx.Exec(ctx,
		`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at, window_id, is_late)
		 VALUES ($1,$2,$3,$4::reimburse_scope,'submitted',NOW(),$5,$6)`,
		rid, in.TeachingCourseID, lecturerID, in.ReimburseScope, windowID, isLate); err != nil {
		return nil, err
	}
	for _, c := range in.Counts {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ta_request_counts (request_id, section_id, undergrad_count, graduate_count)
			 VALUES ($1,$2,$3,$4)`, rid, c.SectionID, c.UndergradCount, c.GraduateCount); err != nil {
			return nil, err
		}
	}
	// One TA can cover multiple sections. Each section becomes its own
	// ta_request_assignments row (so worklog can attribute time per section
	// and the schedule-conflict checks continue to operate per section), but
	// the workload declaration is shared: only the first assignment carries
	// the ta_workload_forms row. Downstream reads LEFT JOIN the form and
	// COALESCE numeric fields to 0, so the aggregate (which sums across all
	// of a TA's assignments in the request) matches the declared total.
	// Every section now carries its OWN workload row (0041). Before that only
	// the first one did, which made the hours mean "all sections combined" and
	// left worklog unable to tell how much time belonged to which group.
	for i, a := range in.Assignments {
		for _, secID := range a.SectionIDs {
			aid := uuid.New()
			group := coGroup[i][secID]
			// A singleton group carries no meaning downstream, so store NULL
			// and reserve non-null for genuinely co-taught sets.
			var groupVal any
			if countInGroup(coGroup[i], group) > 1 {
				groupVal = group
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level, cotaught_group)
				 VALUES ($1,$2,$3,$4,$5::study_level,$6)`,
				aid, rid, secID, a.TAID, levels[i], groupVal); err != nil {
				return nil, err
			}
			wl := perSection[i][secID]
			if _, err := tx.Exec(ctx, `
				INSERT INTO ta_workload_forms (id, assignment_id, help_teach_hrs, help_teach_desc, prep_hrs, prep_desc,
					grade_hrs, grade_desc, other_hrs, other_desc, check_work_hrs, attendance_hrs, ug_other_hrs, ug_other_desc,
					lab_hrs, lab_other_hrs, lab_other_desc)
				VALUES (gen_random_uuid(), $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
				aid, wl.HelpTeachHrs, wl.HelpTeachDesc, wl.PrepHrs, wl.PrepDesc, wl.GradeHrs, wl.GradeDesc,
				wl.OtherHrs, wl.OtherDesc, wl.CheckWorkHrs, wl.AttendanceHrs, wl.UGOtherHrs, wl.UGOtherDesc,
				wl.LabHrs, wl.LabOtherHrs, wl.LabOtherDesc); err != nil {
				return nil, err
			}
		}
	}

	// Case 1: someone on this request has not filed a timetable, so there is
	// nothing to judge yet. The row RESTS in 'submitted' — which already means
	// "handed in, not yet judged" — and ReevaluateForTA finishes it the moment
	// the last timetable arrives. The quota these assignments reserve is held
	// throughout, which is the point: submitting books the slot.
	waiting, err := tasMissingSchedule(ctx, tx, rid, termID)
	if err != nil {
		return nil, err
	}
	if len(waiting) > 0 {
		checks := []DecisionCheck{{
			Rule:    "schedule",
			Passed:  false,
			Warning: true,
			Message: fmt.Sprintf(
				"รอตารางเรียนของ %s ระบบจะตัดสินคำขอนี้ให้อัตโนมัติเมื่อครบทุกคน "+
					"หากคาบใดตรงกับตารางเรียนของเขา คาบนั้นจะถูกตัดออก",
				strings.Join(waiting, ", ")),
		}}
		checksJSON, err := json.Marshal(checks)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE ta_requests SET decision_checks = $1::jsonb, updated_at = NOW() WHERE id = $2`,
			checksJSON, rid); err != nil {
			return nil, err
		}
		if err := s.aud.LogTx(ctx, tx, audit.Entry{
			ActorID: &lecturerID, Action: "ta_request.pending_schedule",
			Entity: "ta_request", EntityID: rid.String(),
			Note: strings.Join(waiting, ", "), After: in,
		}); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &CreateResult{ID: rid, Status: "submitted", Checks: checks}, nil
	}

	// Everyone has a timetable, so the clash verdict is knowable now. Decide it
	// exactly as tryFinalize does: trim the sessions that collide, drop only the
	// sections where nothing workable is left, and keep the rest.
	notices, err := s.applyClashOutcome(ctx, tx, rid)
	if err != nil {
		return nil, err
	}

	checks, passed, err := s.autoDecide(ctx, tx, rid)
	if err != nil {
		return nil, err
	}

	// What was trimmed has to reach the lecturer at the moment they submit —
	// silently handing back a thinner assignment than they asked for is how they
	// end up counting on hours nobody will work. Warning, not a failure: the
	// remaining sessions are still worth approving.
	for taID, lines := range notices {
		checks = append(checks, DecisionCheck{
			Rule:    "clash_trimmed",
			TAName:  s.taName(ctx, taID),
			Passed:  false,
			Warning: true,
			Message: strings.Join(lines, "\n"),
		})
	}

	// A TA whose every section was dropped is off the request; if that empties it,
	// there is nobody left to teach. Mirrors tryFinalize.
	var surviving int
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM ta_request_assignments WHERE request_id = $1 AND state <> 'dropped'`,
		rid).Scan(&surviving); err != nil {
		return nil, err
	}
	verdict := "rejected"
	var reason string
	switch {
	case surviving == 0:
		reason = "ผู้ช่วยสอนทุกคนในคำขอนี้ติดตารางเรียนทุกคาบ จึงไม่มีใครสอนได้"
	case passed:
		verdict = "approved"
	default:
		reason = joinRejectMessages(checks)
	}
	checksJSON, err := json.Marshal(checks)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ta_requests SET
		  status          = $1::ta_request_status,
		  decided_at      = NOW(),
		  decided_by      = NULL,
		  reject_reason   = NULLIF($2, ''),
		  decision_checks = $3::jsonb,
		  updated_at      = NOW()
		WHERE id = $4`, verdict, reason, checksJSON, rid); err != nil {
		return nil, err
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{
		ActorID:  &lecturerID,
		Action:   "ta_request.auto_decide",
		Entity:   "ta_request",
		EntityID: rid.String(),
		Note:     verdict,
		After:    in,
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.notifyClashOutcome(ctx, rid, notices)
	s.notifyDecision(ctx, rid, verdict, reason)
	return &CreateResult{ID: rid, Status: verdict, Checks: checks, RejectReason: reason}, nil
}

// joinRejectMessages produces a single-line reject_reason from the failed
// checks. Kept compact (500 chars) so it renders cleanly inside notification
// bodies and legacy admin tables that show reject_reason as a plain string.
func joinRejectMessages(checks []DecisionCheck) string {
	msgs := make([]string, 0, len(checks))
	for _, c := range checks {
		if c.Passed {
			continue
		}
		// Warnings don't cause rejection, so they don't belong in
		// reject_reason (which is shown to lecturers as the rejection cause).
		if c.Warning {
			continue
		}
		msgs = append(msgs, c.Message)
	}
	joined := strings.Join(msgs, " • ")
	if len(joined) > 500 {
		// Byte-slicing a UTF-8 string mid-rune produces invalid bytes and
		// Postgres rejects the INSERT with SQLSTATE 22021 ("invalid byte
		// sequence for encoding UTF8"). Back up to the nearest rune start
		// before truncating so the appended "…" isn't preceded by a dangling
		// continuation byte.
		end := 497
		for end > 0 && !utf8.RuneStart(joined[end]) {
			end--
		}
		joined = joined[:end] + "…"
	}
	return joined
}

// cancellableStatuses is where a lecturer's own request may still be
// withdrawn from. Not 'rejected' (nothing active to withdraw) and not already
// 'cancelled' (idempotent refusal reads clearer than a silent no-op).
var cancellableStatuses = map[string]bool{"submitted": true, "approved": true}

// statusLabelTH names a ta_requests.status value for an error message.
var statusLabelTH = map[string]string{
	"draft": "ฉบับร่าง", "submitted": "รอตัดสิน", "approved": "อนุมัติแล้ว",
	"rejected": "ปฏิเสธ", "cancelled": "ยกเลิกแล้ว",
}

// Cancel withdraws a lecturer's own request while nothing downstream depends
// on it yet — the answer to "แก้ไข/ยกเลิกคำขอที่ส่งไปแล้วได้ไหม" (10/08/2026
// feedback): there is no separate edit path, because editing a request that
// has already been auto-decided means re-running every rule (docs, schedule,
// clashes, quota) from scratch, which is exactly what Create already does.
// Cancel + resubmit reuses that machinery instead of duplicating it.
//
// Allowed from 'submitted' (still resting on a missing timetable — nothing
// has happened yet) or 'approved', but ONLY when no TA has logged any hours
// against it (draft, submitted, or approved work_logs all count): those rows
// are real work already performed, and pulling the request out from under
// them would orphan that record from every downstream query that reads it
// through r.status = 'approved'. That case needs staff, not a self-service
// button.
//
// Reuses the existing 'cancelled' enum value (present since migration 0001,
// never reachable until now — see fixture_test.go's own long-standing
// comment about it) and the decided_at/decided_by/reject_reason columns
// rather than adding schema. decided_by is set to the lecturer themselves,
// distinguishing a self-cancel from a NULL (system auto-decide).
func (s *TARequestService) Cancel(ctx context.Context, lecturerID, reqID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var owner uuid.UUID
	var status string
	if err := tx.QueryRow(ctx,
		`SELECT lecturer_id, status::text FROM ta_requests WHERE id = $1 FOR UPDATE`, reqID,
	).Scan(&owner, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("ไม่พบคำขอนี้")
		}
		return err
	}
	if owner != lecturerID {
		return errors.New("คุณไม่ใช่เจ้าของคำขอนี้ จึงยกเลิกไม่ได้")
	}
	if !cancellableStatuses[status] {
		label := statusLabelTH[status]
		if label == "" {
			label = status
		}
		return fmt.Errorf("ยกเลิกคำขอนี้ไม่ได้ (สถานะปัจจุบัน: %s)", label)
	}

	var hasWorkLogs bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM work_logs wl
			JOIN ta_request_assignments a ON a.id = wl.assignment_id
			WHERE a.request_id = $1
		)`, reqID).Scan(&hasWorkLogs); err != nil {
		return err
	}
	if hasWorkLogs {
		return errors.New(
			"ยกเลิกไม่ได้ เพราะมีการบันทึกเวลาทำงานของ TA ในคำขอนี้แล้ว กรุณาติดต่อเจ้าหน้าที่เพื่อดำเนินการ")
	}

	if _, err := tx.Exec(ctx, `
		UPDATE ta_requests SET
		  status        = 'cancelled',
		  decided_at    = NOW(),
		  decided_by    = $1,
		  reject_reason = 'ยกเลิกโดยอาจารย์ผู้สอน',
		  updated_at    = NOW()
		WHERE id = $2`, lecturerID, reqID); err != nil {
		return err
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{
		ActorID: &lecturerID, Action: "ta_request.cancel", Entity: "ta_request", EntityID: reqID.String(),
		Note: "สถานะเดิม: " + status,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// notifyDecision fans out approval / rejection notifications. Best-effort:
// failures here must not roll back the decision itself.
func (s *TARequestService) notifyDecision(ctx context.Context, reqID uuid.UUID, verdict, reason string) {
	if s.notify == nil {
		return
	}
	var courseID, lecturerID uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT teaching_course_id, lecturer_id FROM ta_requests WHERE id = $1`, reqID).Scan(&courseID, &lecturerID); err != nil {
		return
	}
	code, nameTH := s.courseLabel(ctx, courseID)
	if verdict == "approved" {
		s.notify.Send(ctx, lecturerID, "คำขอ TA ได้รับการอนุมัติ",
			fmt.Sprintf("คำขอผู้ช่วยสอนวิชา %s %s ได้รับการอนุมัติจากระบบอัตโนมัติแล้ว", code, nameTH), "/lecturer")
		rows, err := s.pool.Query(ctx,
			`SELECT DISTINCT a.ta_id FROM ta_request_assignments a WHERE a.request_id = $1`, reqID)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var taID uuid.UUID
			if err := rows.Scan(&taID); err != nil {
				return
			}
			s.notify.Send(ctx, taID, "คุณได้รับมอบหมายเป็นผู้ช่วยสอน",
				fmt.Sprintf("คุณได้รับอนุมัติเป็นผู้ช่วยสอนวิชา %s %s", code, nameTH), "/ta")
		}
		return
	}
	s.notify.Send(ctx, lecturerID, "คำขอ TA ถูกปฏิเสธ",
		fmt.Sprintf("คำขอผู้ช่วยสอนวิชา %s %s ถูกระบบปฏิเสธอัตโนมัติ: %s", code, nameTH, reason), "/lecturer")
	// The named TAs deserve to hear the outcome too — previously only the
	// lecturer was told and a rejected TA never learned.
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT a.ta_id FROM ta_request_assignments a WHERE a.request_id = $1`, reqID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var taID uuid.UUID
		if err := rows.Scan(&taID); err != nil {
			return
		}
		s.notify.Send(ctx, taID, "คำขอแต่งตั้งผู้ช่วยสอนไม่ผ่านการอนุมัติ",
			fmt.Sprintf("คำขอผู้ช่วยสอนวิชา %s %s ที่ระบุชื่อคุณไม่ผ่านการอนุมัติ: %s", code, nameTH, reason), "/ta")
	}
}

// validateWorkloadFields rejects negative or absurd hour components. Each field
// is stored as NUMERIC(4,2), so the hard ceiling is < 100.
func validateWorkloadFields(w WorkloadInput, name string) error {
	fields := []float64{
		w.HelpTeachHrs, w.PrepHrs, w.GradeHrs, w.OtherHrs,
		w.CheckWorkHrs, w.AttendanceHrs, w.UGOtherHrs, w.LabHrs, w.LabOtherHrs,
	}
	for _, v := range fields {
		if v < 0 {
			return fmt.Errorf("ภาระงานของ %s มีค่าติดลบ", name)
		}
		if v > 99 {
			return fmt.Errorf("ภาระงานของ %s มีค่าเกินกว่าที่กำหนด (สูงสุด 99 ชม.)", name)
		}
	}
	return nil
}

// sectionTotalMultiplier bounds the SUM of one side's declared hours against the
// group's real teaching time. Attendance happens inside the session; grading and
// other work happen outside it, so the total legitimately exceeds the class
// hours — but not without limit.
const sectionTotalMultiplier = 2

// sectionWeekly is one section's actual weekly contact time, split by kind.
type sectionWeekly struct{ Lecture, Lab float64 }

// sectionWeeklyHours reads how long each section ACTUALLY meets per week, from
// the timetable rather than from the credit notation.
//
// The two disagree in practice. A course carrying 1 credit-hour of lab can meet
// for 3 real hours, and the ceiling that matters to a TA is the one they are
// standing in the room for. Deriving it from teaching_courses.lecture_hrs /
// lab_hrs — the 3(2-2-5) figures — capped that TA at 1 hour for a 3-hour job.
//
// Callers reach here only after the WBA gate above, so every section is
// guaranteed to have at least one row; a kind with no sessions is legitimately
// zero and blocks that kind's fields.
func (s *TARequestService) sectionWeeklyHours(ctx context.Context, sectionIDs []uuid.UUID) (map[uuid.UUID]sectionWeekly, error) {
	out := map[uuid.UUID]sectionWeekly{}
	if len(sectionIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT section_id, kind,
		       SUM(EXTRACT(EPOCH FROM (end_time - start_time)) / 3600)
		FROM section_schedules
		WHERE section_id = ANY($1)
		GROUP BY section_id, kind`, sectionIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var kind string
		var hrs float64
		if err := rows.Scan(&id, &kind, &hrs); err != nil {
			return nil, err
		}
		w := out[id]
		switch kind {
		case "lecture":
			w.Lecture = hrs
		case "lab":
			w.Lab = hrs
		}
		out[id] = w
	}
	return out, rows.Err()
}

// validateUndergradSectionCaps bounds ONE section's declared weekly hours by
// how long that section actually meets each week, per kind.
//
// The bound is per section, not per assignment: the lecturers asked to declare
// hours "ต่อกลุ่มเรียน (section)", and a TA cannot spend more time on a group
// than that group actually meets. Before per-section declarations existed this
// function multiplied the ceiling by the number of sections, because one
// figure covered them all; that multiplication is now wrong and gone.
//
// A collision with the TA's own class is deliberately NOT judged here. Refusing
// the submission would make the verdict depend on whether the TA filed their
// timetable before or after the lecturer pressed Send — the same order-dependence
// applyClashOutcome was written to remove. The clash is settled there instead, by
// trimming the sessions the TA cannot cover (or dropping the assignment when
// nothing off-slot was declared), and the request form disables the affected duty
// up front using PreviewConflicts.BlockedKinds.
//
// secLabel names the group in the error so a lecturer filling several knows
// which one to fix.
func validateUndergradSectionCaps(w WorkloadInput, name, secLabel string, hrs sectionWeekly) error {
	// ปฏิบัติการ is either-or: a TA either runs the lab session or supports it
	// from outside (prep, marking lab sheets). Declaring both would bill the
	// same lab twice over, and the TA cannot be in two places for one slot.
	if w.LabHrs > 0.001 && w.LabOtherHrs > 0.001 {
		return fmt.Errorf(
			"ภาระงานปฏิบัติการของ %s (Sec %s) เลือกได้อย่างใดอย่างหนึ่ง: 'สอนปฏิบัติการ' หรือ 'อื่น ๆ (ปฏิบัติการ)'",
			name, secLabel)
	}
	checks := []struct {
		v     float64
		cap   float64
		label string
	}{
		{w.CheckWorkHrs, hrs.Lecture, "ช่วยตรวจงาน"},
		{w.AttendanceHrs, hrs.Lecture, "เช็คชื่อ/เก็บใบงาน"},
		{w.UGOtherHrs, hrs.Lecture, "อื่น ๆ (บรรยาย)"},
		{w.LabHrs, hrs.Lab, "ปฏิบัติการ"},
		{w.LabOtherHrs, hrs.Lab, "อื่น ๆ (ปฏิบัติการ)"},
	}
	for _, c := range checks {
		if c.v > c.cap+0.001 {
			if c.cap == 0 {
				return fmt.Errorf(
					"ภาระงาน '%s' ของ %s (Sec %s) กรอกไม่ได้ กลุ่มนี้ไม่มีคาบประเภทนี้ในตารางสอน",
					c.label, name, secLabel)
			}
			return fmt.Errorf(
				"ภาระงาน '%s' ของ %s (Sec %s) เกินชั่วโมงสอนจริงของกลุ่มนี้ (%.2f ชม./สัปดาห์)",
				c.label, name, secLabel, c.cap)
		}
	}
	// Each field alone fitting the class hours is not enough: three lecture-side
	// fields at the ceiling declared three times the group's real teaching time.
	// The total is bounded at sectionTotalMultiplier × the class hours.
	//
	// The multiplier is 2, not 1, because the second hour is real work that
	// happens outside the room: every approved assignment in service today
	// declares 2h เช็คชื่อ during a 2h lecture plus 2h ตรวจงาน after it. A ×1
	// ceiling would describe a practice nobody follows and refuse every request.
	totals := []struct {
		v     float64
		hrs   float64
		label string
	}{
		{w.CheckWorkHrs + w.AttendanceHrs + w.UGOtherHrs, hrs.Lecture, "บรรยาย"},
		{w.LabHrs + w.LabOtherHrs, hrs.Lab, "ปฏิบัติการ"},
	}
	for _, t := range totals {
		limit := t.hrs * sectionTotalMultiplier
		if t.hrs > 0 && t.v > limit+0.001 {
			return fmt.Errorf(
				"ภาระงาน%sรวมของ %s (Sec %s) อยู่ที่ %.2f ชม./สัปดาห์ เกินเพดานรวม %.2f ชม. "+
					"(%.0f เท่าของชั่วโมงสอนจริง %.2f ชม.)",
				t.label, name, secLabel, t.v, limit, float64(sectionTotalMultiplier), t.hrs)
		}
	}
	return nil
}

// resolveSectionWorkloads maps every assigned section to the hours it should
// carry. A section named in SectionWorkloads uses its own figure; anything left
// over falls back to the assignment-wide Workload, which is how older clients
// (and the request tests) declare one figure for the whole assignment.
func resolveSectionWorkloads(sectionIDs []uuid.UUID, fallback WorkloadInput, per []SectionWorkload) map[uuid.UUID]WorkloadInput {
	byID := make(map[uuid.UUID]WorkloadInput, len(sectionIDs))
	for _, sw := range per {
		byID[sw.SectionID] = sw.Workload
	}
	out := make(map[uuid.UUID]WorkloadInput, len(sectionIDs))
	for _, id := range sectionIDs {
		if w, ok := byID[id]; ok {
			out[id] = w
			continue
		}
		out[id] = fallback
	}
	return out
}

// detectCotaughtGroups partitions the sections a TA covers into groups that
// meet at the same time, so their hours are billed once.
//
// The meeting was explicit that co-teaching is the normal case at CP KKU (sec
// 01–02 ภาคปกติ plus sec 03 โครงการพิเศษ in one room, one TA) and that such a
// TA "เบิกได้แค่ก้อนเดียว". The system already knows every section's timetable,
// so it derives the grouping instead of asking the lecturer to declare it —
// one less thing to get wrong, and it cannot drift from the real schedule.
//
// Sections with no timetable rows form their own singleton groups.
func (s *TARequestService) detectCotaughtGroups(ctx context.Context, sectionIDs []uuid.UUID) (map[uuid.UUID]int, error) {
	groups := make(map[uuid.UUID]int, len(sectionIDs))
	next := 0
	for _, id := range sectionIDs {
		if _, done := groups[id]; done {
			continue
		}
		next++
		groups[id] = next
		// Anything overlapping this section joins its group. Overlap is
		// transitive enough in practice (co-taught sections share one slot),
		// and a wrong grouping can only ever under-count, never over-bill.
		for _, other := range sectionIDs {
			if other == id {
				continue
			}
			if _, done := groups[other]; done {
				continue
			}
			var overlaps bool
			if err := s.pool.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM section_schedules a
					JOIN section_schedules b ON b.section_id = $2
					WHERE a.section_id = $1
					  AND a.day_of_week = b.day_of_week
					  AND a.start_time < b.end_time
					  AND b.start_time < a.end_time)`, id, other).Scan(&overlaps); err != nil {
				return nil, err
			}
			if overlaps {
				groups[other] = next
			}
		}
	}
	return groups, nil
}

// countInGroup returns how many sections share the given co-taught group.
func countInGroup(groups map[uuid.UUID]int, group int) int {
	n := 0
	for _, g := range groups {
		if g == group {
			n++
		}
	}
	return n
}

// billableWeeklyTotal sums a TA's declared hours across their sections, counting
// each co-taught group once. Used for the daily-feasibility check and for the
// graduate 10–12 hour rule, both of which describe time the person actually
// spends — not time multiplied by how many groups were in the room.
func billableWeeklyTotal(sectionIDs []uuid.UUID, per map[uuid.UUID]WorkloadInput, groups map[uuid.UUID]int, level string) float64 {
	seen := map[int]bool{}
	var total float64
	for _, id := range sectionIDs {
		g := groups[id]
		if g != 0 && seen[g] {
			continue
		}
		seen[g] = true
		total += weeklyWorkloadTotal(per[id], level)
	}
	return total
}

// weeklyWorkloadTotal sums the fields that actually apply to this TA's level —
// undergrad and grad use two disjoint sets of columns on the same form.
func weeklyWorkloadTotal(w WorkloadInput, level string) float64 {
	if level == "master" || level == "phd" {
		return w.HelpTeachHrs + w.PrepHrs + w.GradeHrs + w.OtherHrs
	}
	return w.CheckWorkHrs + w.AttendanceHrs + w.UGOtherHrs + w.LabHrs + w.LabOtherHrs
}

var thaiDayNames = [7]string{"อาทิตย์", "จันทร์", "อังคาร", "พุธ", "พฤหัสบดี", "ศุกร์", "เสาร์"}

// จำนวนวันทำการต่อสัปดาห์ที่ใช้เป็นฐานแปลง "เพดาน ชม./วัน" → "เพดาน ชม./สัปดาห์".
// งานนอกคาบ (ตรวจงาน/อื่น ๆ) ลงวันไหนก็ได้ จึงไม่ผูกกับวันที่มีคาบเรียน แต่ต้อง
// มีเพดานไม่งั้นภาระงานที่กรอกจะกระจายลงบันทึกเวลาจริงไม่ได้.
const workingDaysPerWeek = 5

// enforceDailyHourFeasibility rejects a request whose declared workload could
// never be recorded as legal work logs under ประกาศ 731/2565 + 1080/2565
// (ป.ตรี ปกติ ≤7 ชม./วัน, ป.ตรี พิเศษ ≤6, บัณฑิต ปกติ ≤6).
//
// Two things must hold, because the auto-generated work logs SILENTLY DROP any
// occurrence that would breach the daily cap — the TA would just lose payable
// hours with no error anywhere:
//  1. per class day: the scheduled hours of every section this TA covers on the
//     same weekday must fit inside one day's cap;
//  2. per week: total declared hours ≤ daily cap × working days.
func (s *TARequestService) enforceDailyHourFeasibility(
	ctx context.Context, sectionIDs []uuid.UUID, level, name string, declaredWeekly float64,
) error {
	if len(sectionIDs) == 0 {
		return nil
	}
	// Without pay_rates there are no caps to enforce, and the query below would
	// return no rows — indistinguishable from "this course has no timetable".
	// Silently approving in that state is the wrong way to be wrong: the caps
	// are the announcement, and a request waved through without them cannot be
	// unwound once the TA has worked the hours. Refuse and say why.
	var haveRates bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pay_rates)`).Scan(&haveRates); err != nil {
		return err
	}
	if !haveRates {
		return Invalid("ยังไม่ได้ตั้งอัตราค่าตอบแทนในระบบ ตรวจเพดานชั่วโมงตามประกาศไม่ได้ จึงยังส่งคำขอไม่ได้")
	}
	isGrad := level == "master" || level == "phd"
	// Hours per weekday must be the UNION of the occupied time, not the sum of
	// each section's length. Co-taught sections meet in one room at one time —
	// summing them would report a TA as working 12 hours for a single 6-hour
	// sitting and refuse a request that is perfectly legal. The CTE chain below
	// is the standard merge-intervals: order the slots, start a new "island"
	// whenever a slot begins after every previous one has ended, then measure
	// each island once.
	rows, err := s.pool.Query(ctx, `
		WITH latest AS (SELECT * FROM pay_rates ORDER BY effective_from DESC LIMIT 1),
		slots AS (
		    SELECT ss.day_of_week AS dow, ss.start_time AS st, ss.end_time AS en
		    FROM section_schedules ss
		    WHERE ss.section_id = ANY($1)
		),
		ordered AS (
		    SELECT dow, st, en,
		           MAX(en) OVER (PARTITION BY dow ORDER BY st, en
		                         ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) AS prev_end
		    FROM slots
		),
		islands AS (
		    SELECT dow, st, en,
		           SUM(CASE WHEN prev_end IS NULL OR st > prev_end THEN 1 ELSE 0 END)
		               OVER (PARTITION BY dow ORDER BY st, en ROWS UNBOUNDED PRECEDING) AS island
		    FROM ordered
		),
		merged AS (
		    SELECT dow, island, MIN(st) AS st, MAX(en) AS en
		    FROM islands GROUP BY dow, island
		),
		day_hours AS (
		    SELECT dow, SUM(EXTRACT(EPOCH FROM (en - st)) / 3600.0) AS hrs
		    FROM merged GROUP BY dow
		),
		day_caps AS (
		    SELECT ss.day_of_week AS dow,
		           MIN(CASE
		               WHEN NOT $2 AND sec.track = 'regular' THEN pr.ug_regular_daily_hour_cap
		               WHEN NOT $2 AND sec.track = 'special' THEN pr.ug_special_daily_hour_cap
		               WHEN $2 AND sec.track = 'regular' THEN pr.grad_regular_daily_hour_cap
		               ELSE 24
		           END) AS day_cap
		    FROM section_schedules ss
		    JOIN sections sec ON sec.id = ss.section_id
		    CROSS JOIN latest pr
		    WHERE ss.section_id = ANY($1)
		    GROUP BY ss.day_of_week
		)
		SELECT h.dow, h.hrs, c.day_cap
		FROM day_hours h JOIN day_caps c ON c.dow = h.dow`, sectionIDs, isGrad)
	if err != nil {
		return err
	}
	defer rows.Close()

	minCap := 0.0
	for rows.Next() {
		var dow int
		var hrs, dayCap float64
		if err := rows.Scan(&dow, &hrs, &dayCap); err != nil {
			return err
		}
		if minCap == 0 || dayCap < minCap {
			minCap = dayCap
		}
		if hrs > dayCap+0.01 {
			day := "—"
			if dow >= 0 && dow < 7 {
				day = thaiDayNames[dow]
			}
			return Invalid(fmt.Sprintf(
				"%s รับ section ที่เรียนวัน%sรวม %.1f ชม. เกินเพดาน %.1f ชม./วัน ตามประกาศ "+
					"กรุณาลด section ของวันนั้น มิฉะนั้นบันทึกเวลาจะลงได้ไม่ครบ",
				name, day, hrs, dayCap))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if minCap <= 0 {
		return nil // ไม่มีตารางเรียน (WBA) — ถูกบล็อกไปแล้วก่อนหน้านี้
	}
	weeklyCap := minCap * workingDaysPerWeek
	if declaredWeekly > weeklyCap+0.01 {
		return Invalid(fmt.Sprintf(
			"ภาระงานของ %s รวม %.2f ชม./สัปดาห์ เกินที่จะลงบันทึกเวลาได้จริง "+
				"เพดาน %.1f ชม./วัน × %d วันทำการ = %.1f ชม./สัปดาห์",
			name, declaredWeekly, minCap, workingDaysPerWeek, weeklyCap))
	}
	return nil
}

// validateTA checks the user is an active TA and returns their display name
// and authoritative study level (DB wins over client input).
func (s *TARequestService) validateTA(ctx context.Context, taID uuid.UUID, clientLevel string) (name, level string, err error) {
	var first, last string
	var dbLevel *string
	var isActive, hasRole bool
	err = s.pool.QueryRow(ctx, `
		SELECT u.first_name, u.last_name, u.study_level::text, u.is_active AND u.deleted_at IS NULL,
		       EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role = 'ta')
		FROM users u WHERE u.id = $1
	`, taID).Scan(&first, &last, &dbLevel, &isActive, &hasRole)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", errors.New("ไม่พบบัญชี TA ที่เลือก")
		}
		return "", "", err
	}
	name = first + " " + last
	if !isActive {
		return "", "", fmt.Errorf("บัญชีของ %s ถูกปิดใช้งาน", name)
	}
	if !hasRole {
		return "", "", fmt.Errorf("%s ไม่ได้มีสิทธิ์เป็น TA ในระบบ", name)
	}
	level = clientLevel
	if dbLevel != nil && *dbLevel != "" {
		level = *dbLevel
	}
	if level != "undergrad" && level != "master" && level != "phd" {
		return "", "", fmt.Errorf("ยังไม่ได้ระบุระดับการศึกษาของ %s", name)
	}
	return name, level, nil
}

func (s *TARequestService) taName(ctx context.Context, taID uuid.UUID) string {
	var first, last string
	if err := s.pool.QueryRow(ctx, `SELECT first_name, last_name FROM users WHERE id = $1`, taID).Scan(&first, &last); err != nil {
		return "TA"
	}
	return first + " " + last
}

// SectionConflict is one section's preview verdict for a specific TA.
type SectionConflict struct {
	SectionID uuid.UUID `json:"section_id"`
	Messages  []string  `json:"messages"`
	// BlockedKinds names the session kinds ("lecture" / "lab") whose every
	// meeting collides with this TA's own timetable. The form disables only the
	// matching in-class duty — เช็คชื่อ for lecture, สอนปฏิบัติการ for lab —
	// and leaves grading and other work available, because those are done
	// outside the session.
	//
	// A section may appear in the result with an empty BlockedKinds: a partial
	// collision is worth warning about but costs the TA nothing.
	BlockedKinds []string `json:"blocked_kinds"`
	// CrossConflict marks an overlap with a DIFFERENT course the TA already
	// assists. Unlike a collision with their own class, this one still blocks
	// the request: both sides are teaching commitments, so it cannot be
	// resolved by dropping sessions from this request. The form keeps refusing
	// to submit on this — and only this.
	CrossConflict bool `json:"cross_conflict"`
}

// PreviewConflicts checks the given TA against every section of the given
// teaching course and returns the sections that conflict (own class schedule
// or already-approved TA duties elsewhere). Only sections with at least one
// conflict appear in the result — an empty slice means the TA can safely be
// assigned to any section of this course.
//
// This mirrors the checks that autoDecide performs at submit time, so the
// lecturer sees the same verdict inline while picking sections instead of
// discovering it only after clicking Send.
func (s *TARequestService) PreviewConflicts(ctx context.Context, taID, teachingCourseID uuid.UUID) ([]SectionConflict, error) {
	name := s.taName(ctx, taID)
	var termID uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT term_id FROM teaching_courses WHERE id = $1`, teachingCourseID).Scan(&termID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM sections WHERE teaching_course_id = $1`, teachingCourseID)
	if err != nil {
		return nil, err
	}
	var secIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		secIDs = append(secIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]SectionConflict, 0, len(secIDs))
	for _, sid := range secIDs {
		var msgs []string
		var cross bool
		if err := s.checkOwnClassConflict(ctx, s.pool, taID, sid, name); err != nil {
			msgs = append(msgs, err.Error())
		}
		// Passing uuid.Nil for excludeRequestID is fine here — we're not
		// filing a request yet, so no ID to exclude.
		if err := s.checkCrossRequestConflict(ctx, s.pool, taID, sid, termID, teachingCourseID, uuid.Nil, []string{"approved"}, name); err != nil {
			msgs = append(msgs, err.Error())
			cross = true
		}
		byKind, err := sectionClashByKind(ctx, s.pool, taID, sid)
		if err != nil {
			return nil, err
		}
		var blocked []string
		for _, k := range []string{"lecture", "lab"} {
			if byKind.fullyBlocked(k) {
				blocked = append(blocked, k)
			}
		}
		if len(msgs) > 0 || len(blocked) > 0 {
			out = append(out, SectionConflict{
				SectionID: sid, Messages: msgs, BlockedKinds: blocked, CrossConflict: cross,
			})
		}
	}
	return out, nil
}

// checkTAEligibility enforces requirement 1: documents approved by staff and a
// class schedule recorded for the term (a WBA/year-4 row counts).
// checkTAEligibility enforces requirement 1 (docs + schedule). Called both
// inside the auto-decide tx (q = tx) and from Detail's read-only warnings
// path (q = pool).
func (s *TARequestService) checkTAEligibility(ctx context.Context, q querier, taID, termID uuid.UUID, name string) error {
	if err := s.checkTADocs(ctx, q, taID, name); err != nil {
		return err
	}
	return s.checkTASchedule(ctx, q, taID, termID, name)
}

// checkTADocs verifies the profile is staff-approved AND the three mandatory
// documents each have a current approved row. Split out from checkTAEligibility
// so autoDecide can evaluate docs and schedule INDEPENDENTLY — a docs failure
// must not mask (or fake a pass on) the schedule gate.
func (s *TARequestService) checkTADocs(ctx context.Context, q querier, taID uuid.UUID, name string) error {
	var profileStatus *string
	if err := q.QueryRow(ctx,
		`SELECT status::text FROM ta_profiles WHERE user_id = $1`, taID).Scan(&profileStatus); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	if profileStatus == nil || *profileStatus != "approved" {
		return fmt.Errorf("%s ยังไม่ผ่านการอนุมัติเอกสารจากเจ้าหน้าที่", name)
	}
	// The three required documents must each exist as a current (not superseded)
	// row with status='approved'. A profile can be approved independently of the
	// documents, so this gate must be checked explicitly (rule C6).
	var approvedDocKinds int
	if err := q.QueryRow(ctx, `
		SELECT COUNT(DISTINCT kind) FROM ta_documents
		WHERE user_id = $1 AND superseded_at IS NULL AND status = 'approved'
		  AND kind IN ('national_id','bank_book','creditor_form')`,
		taID).Scan(&approvedDocKinds); err != nil {
		return err
	}
	if approvedDocKinds < 3 {
		return fmt.Errorf("%s ยังมีเอกสารบังคับที่ไม่ครบหรือยังไม่ผ่านการอนุมัติ (บัตรประชาชน/สมุดบัญชี/แบบฟอร์มเจ้าหนี้)", name)
	}
	return nil
}

// checkTASchedule verifies the TA recorded a class schedule for the term (a
// WBA/year-4 row counts). Evaluated independently of docs.
func (s *TARequestService) checkTASchedule(ctx context.Context, q querier, taID, termID uuid.UUID, name string) error {
	var hasSchedule bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM ta_class_schedules WHERE user_id = $1 AND term_id = $2)`,
		taID, termID).Scan(&hasSchedule); err != nil {
		return err
	}
	if !hasSchedule {
		return fmt.Errorf("%s ยังไม่ได้บันทึกตารางเรียนของภาคการศึกษานี้", name)
	}
	return nil
}

// sectionLabel names a section for an error message. Moved here from
// ta_request_decide.go when assertNoKnownClash was removed — its only remaining
// caller is the workload-cap validator above.
func (s *TARequestService) sectionLabel(ctx context.Context, sectionID uuid.UUID) string {
	var secNo string
	if err := s.pool.QueryRow(ctx,
		`SELECT sec_no FROM sections WHERE id = $1`, sectionID).Scan(&secNo); err != nil {
		return "—"
	}
	return secNo
}

func (s *TARequestService) checkOwnClassConflict(ctx context.Context, q querier, taID, sectionID uuid.UUID, name string) error {
	var conflicts int
	err := q.QueryRow(ctx, `
		SELECT COUNT(*) FROM section_schedules ss
		JOIN sections sec ON sec.id = ss.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		JOIN ta_class_schedules cs ON cs.user_id = $1 AND cs.term_id = tc.term_id AND NOT cs.is_wba
		WHERE ss.section_id = $2
		  -- Lab periods only. A lab puts the TA in a room with students at a
		  -- fixed hour, so it cannot share a slot with their own class; lecture
		  -- duty does not, and blocking on it ruled out TAs who were perfectly
		  -- able to do the job (31/07/2026 change).
		  AND `+BlockingSessionSQL("ss")+`
		  AND ss.day_of_week = cs.day_of_week
		  AND ss.start_time < cs.end_time
		  AND ss.end_time > cs.start_time`,
		taID, sectionID).Scan(&conflicts)
	if err != nil {
		return err
	}
	if conflicts > 0 {
		return fmt.Errorf("คาบปฏิบัติการของ section นี้ทับซ้อนกับตารางเรียนของ %s", name)
	}
	return nil
}

func (s *TARequestService) checkCrossRequestConflict(ctx context.Context, q querier, taID, sectionID, termID, courseID, excludeRequestID uuid.UUID, statuses []string, name string) error {
	var conflicts int
	err := q.QueryRow(ctx, `
		SELECT COUNT(*) FROM section_schedules ss
		JOIN ta_request_assignments oa ON oa.ta_id = $1
		JOIN ta_requests orq ON orq.id = oa.request_id
			AND orq.status::text = ANY($5)
			AND orq.teaching_course_id <> $4
			AND orq.id <> $6
		JOIN teaching_courses otc ON otc.id = orq.teaching_course_id AND otc.term_id = $3
		JOIN section_schedules oss ON oss.section_id = oa.section_id
		WHERE ss.section_id = $2
		  -- Blocking when EITHER side is a lab, not only the requested one. A lab
		  -- is a fixed commitment whichever course it belongs to, so a lecture
		  -- duty here that lands on a lab the TA already runs elsewhere is still
		  -- a double booking — exempting lectures on both sides would have let
		  -- exactly that through.
		  AND ('lab' IN (ss.kind, oss.kind))
		  AND oss.day_of_week = ss.day_of_week
		  AND oss.start_time < ss.end_time
		  AND ss.start_time < oss.end_time`,
		taID, sectionID, termID, courseID, statuses, excludeRequestID).Scan(&conflicts)
	if err != nil {
		return err
	}
	if conflicts > 0 {
		return fmt.Errorf("เวลาสอนของ section นี้ทับซ้อนกับวิชาอื่นที่ %s เป็นผู้ช่วยสอนอยู่", name)
	}
	return nil
}

// maxApprovedCoursesPerTerm is the per-term cap on how many courses one TA may
// be approved for (Q&A rule). Enforced at auto-decide; the request UI also uses
// it to keep already-full TAs out of the picker.
const maxApprovedCoursesPerTerm = 3

// TACandidate is one selectable TA for the request form, carrying how many
// courses they are already approved for this term so the UI can hide/flag those
// at the cap.
type TACandidate struct {
	ID                  uuid.UUID `json:"id"`
	FirstName           string    `json:"first_name"`
	LastName            string    `json:"last_name"`
	Email               string    `json:"email"`
	StudyLevel          string    `json:"study_level"`
	ApprovedCourseCount int       `json:"approved_course_count"` // other courses this term (excl. this one)
	AtQuota             bool      `json:"at_quota"`              // adding THIS course would exceed the cap
	AlreadyInCourse     bool      `json:"already_in_course"`     // already an approved TA of THIS course
	// HasSchedule reports whether this TA has filed a class timetable for the
	// term. False means the request can still be submitted, but the verdict
	// waits for the timetable and any clashing session will be dropped — the
	// form shows that warning before the lecturer commits.
	HasSchedule bool `json:"has_schedule"`
}

// Candidates lists every TA with their approved-course count in the given
// course's term (excluding this course), flagging those who have already hit
// the per-term cap. Used by the request form to keep full TAs unselectable.
func (s *TARequestService) Candidates(ctx context.Context, tcID uuid.UUID) ([]TACandidate, error) {
	var termID uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT term_id FROM teaching_courses WHERE id=$1`, tcID).Scan(&termID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, COALESCE(u.first_name,''), COALESCE(u.last_name,''), u.email,
		       COALESCE(u.study_level, 'undergrad'),
		       COALESCE((
		         SELECT COUNT(DISTINCT r.teaching_course_id)
		         FROM ta_request_assignments a
		         JOIN ta_requests r ON r.id = a.request_id
		         JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		         WHERE a.ta_id = u.id AND tc.term_id = $2
		           AND r.status IN ('submitted', 'approved')
		           AND a.state <> 'dropped'
		           AND r.teaching_course_id <> $1), 0),
		       EXISTS (
		         SELECT 1 FROM ta_request_assignments a
		         JOIN ta_requests r ON r.id = a.request_id
		         WHERE a.ta_id = u.id AND r.teaching_course_id = $1
		           AND r.status IN ('submitted', 'approved')
		           AND a.state <> 'dropped'),
		       EXISTS (
		         SELECT 1 FROM ta_class_schedules cs
		         WHERE cs.user_id = u.id AND cs.term_id = $2)
		FROM users u
		WHERE EXISTS (SELECT 1 FROM user_roles ur WHERE ur.user_id = u.id AND ur.role::text = 'ta')
		ORDER BY u.first_name, u.last_name`, tcID, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TACandidate{}
	for rows.Next() {
		var c TACandidate
		if err := rows.Scan(&c.ID, &c.FirstName, &c.LastName, &c.Email, &c.StudyLevel,
			&c.ApprovedCourseCount, &c.AlreadyInCourse, &c.HasSchedule); err != nil {
			return nil, err
		}
		c.AtQuota = c.ApprovedCourseCount >= maxApprovedCoursesPerTerm
		out = append(out, c)
	}
	return out, rows.Err()
}

// reservedCourseCount counts distinct courses (other than courseID) in the term
// that this TA's quota is currently committed to.
//
// The meeting changed what "committed" means: putting a name on a request
// BOOKS the slot, whether or not the request has been judged yet. Otherwise a
// TA sitting in three pending requests looks free to a fourth lecturer, and
// all four approve at once.
//
// Two exclusions release the booking again:
//   - a request that ends up 'rejected' or 'cancelled' never counted;
//   - an assignment dropped because every session clashed with the TA's own
//     timetable frees that course, exactly as the meeting asked ("กรณีสอนวิชานั้น
//     ไม่ได้เลย … ก็ให้คืนโควต้า").
func (s *TARequestService) reservedCourseCount(ctx context.Context, q querier, taID, termID, courseID uuid.UUID) (int, error) {
	var count int
	err := q.QueryRow(ctx, `
		SELECT COUNT(DISTINCT r.teaching_course_id) FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		WHERE a.ta_id = $1 AND tc.term_id = $2
		  AND r.status IN ('submitted', 'approved')
		  AND a.state <> 'dropped'
		  AND r.teaching_course_id <> $3`,
		taID, termID, courseID).Scan(&count)
	return count, err
}

// autoDecide runs every business rule for the given request inside tx and
// returns the per-rule checklist plus the overall verdict. It never mutates
// state — the caller composes the UPDATE that flips status.
//
// Concurrency: obtains a per-TA advisory lock (sorted by TA UUID to avoid
// deadlocks) so two racing lecturers submitting for the same TA can't both
// slip past the 3-course cap.
func (s *TARequestService) autoDecide(ctx context.Context, tx pgx.Tx, reqID uuid.UUID) ([]DecisionCheck, bool, error) {
	var courseID uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT teaching_course_id FROM ta_requests WHERE id = $1 FOR UPDATE`, reqID).Scan(&courseID); err != nil {
		return nil, false, err
	}
	var termID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT term_id FROM teaching_courses WHERE id = $1`, courseID).Scan(&termID); err != nil {
		return nil, false, err
	}

	type taRow struct {
		id    uuid.UUID
		name  string
		level string
	}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT a.ta_id, u.first_name || ' ' || u.last_name, a.level::text
		FROM ta_request_assignments a JOIN users u ON u.id = a.ta_id
		WHERE a.request_id = $1`, reqID)
	if err != nil {
		return nil, false, err
	}
	var tas []taRow
	for rows.Next() {
		var t taRow
		if err := rows.Scan(&t.id, &t.name, &t.level); err != nil {
			rows.Close()
			return nil, false, err
		}
		tas = append(tas, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	sort.Slice(tas, func(i, j int) bool { return tas[i].id.String() < tas[j].id.String() })

	type asgRow struct {
		taID, secID uuid.UUID
		name        string
		level       string
		wl          WorkloadInput
		// coGroup is non-zero when this section is taught in the same sitting
		// as others in the request. Their hours are the SAME hours, so the
		// weekly total must count the group once.
		coGroup int
	}
	arows, err := tx.Query(ctx, `
		SELECT a.ta_id, a.section_id, u.first_name || ' ' || u.last_name, a.level::text,
		       COALESCE(w.help_teach_hrs,0), COALESCE(w.prep_hrs,0),
		       COALESCE(w.grade_hrs,0),      COALESCE(w.other_hrs,0),
		       COALESCE(w.check_work_hrs,0), COALESCE(w.attendance_hrs,0),
		       COALESCE(w.ug_other_hrs,0),   COALESCE(w.lab_hrs,0),
		       COALESCE(w.lab_other_hrs,0),
		       COALESCE(a.cotaught_group, 0)
		FROM ta_request_assignments a
		JOIN users u ON u.id = a.ta_id
		LEFT JOIN ta_workload_forms w ON w.assignment_id = a.id
		WHERE a.request_id = $1
		ORDER BY a.ta_id, a.section_id`, reqID)
	if err != nil {
		return nil, false, err
	}
	var asgs []asgRow
	taSections := map[uuid.UUID][]uuid.UUID{}
	for arows.Next() {
		var a asgRow
		if err := arows.Scan(&a.taID, &a.secID, &a.name, &a.level,
			&a.wl.HelpTeachHrs, &a.wl.PrepHrs, &a.wl.GradeHrs, &a.wl.OtherHrs,
			&a.wl.CheckWorkHrs, &a.wl.AttendanceHrs, &a.wl.UGOtherHrs, &a.wl.LabHrs,
			&a.wl.LabOtherHrs,
			&a.coGroup); err != nil {
			arows.Close()
			return nil, false, err
		}
		asgs = append(asgs, a)
		taSections[a.taID] = append(taSections[a.taID], a.secID)
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return nil, false, err
	}

	checks := make([]DecisionCheck, 0, 8*len(tas))
	add := func(rule, ta, msgOK, msgFail string, ok bool) {
		msg := msgOK
		if !ok {
			msg = msgFail
		}
		checks = append(checks, DecisionCheck{Rule: rule, TAName: ta, Passed: ok, Message: msg})
	}
	// addWarn records a failing check that surfaces to staff but does not
	// flip the verdict to rejected. Used for docs-not-approved so lecturers
	// can still submit while staff follows up with the TA.
	addWarn := func(rule, ta, msgOK, msgFail string, ok bool) {
		msg := msgOK
		if !ok {
			msg = msgFail
		}
		checks = append(checks, DecisionCheck{Rule: rule, TAName: ta, Passed: ok, Warning: !ok, Message: msg})
	}

	// Section belongs to course — catches upstream data corruption (client
	// bypassing Create's integrity check).
	for _, a := range asgs {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM sections WHERE id = $1 AND teaching_course_id = $2)`,
			a.secID, courseID).Scan(&ok); err != nil {
			return nil, false, err
		}
		if !ok {
			add("section", a.name, "", "section ที่มอบหมายไม่อยู่ในรายวิชานี้", false)
		}
	}

	// Per-TA rules (docs, schedule, duplicate, cap, workload total).
	for _, t := range tas {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, t.id); err != nil {
			return nil, false, err
		}

		// Docs and schedule are evaluated INDEPENDENTLY so a docs failure can no
		// longer fake a schedule pass (previously the helper short-circuited on
		// docs, marking schedule green even for a TA with no schedule at all, and
		// the request auto-approved because docs is only a warning). Docs stays a
		// warning (staff sees it, payout gates catch it at export); schedule is
		// blocking.
		if err := s.checkTADocs(ctx, tx, t.id, t.name); err != nil {
			addWarn("docs", t.name, "", err.Error(), false)
		} else {
			addWarn("docs", t.name, fmt.Sprintf("%s เอกสารครบและได้รับการอนุมัติ", t.name), "", true)
		}
		if err := s.checkTASchedule(ctx, tx, t.id, termID, t.name); err != nil {
			add("schedule", t.name, "", err.Error(), false)
		} else {
			add("schedule", t.name, fmt.Sprintf("%s บันทึกตารางเรียนของภาคเรียนแล้ว", t.name), "", true)
		}

		var dupSameCourse bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM ta_request_assignments a
				JOIN ta_requests r ON r.id = a.request_id
				WHERE r.teaching_course_id = $1
				  AND r.id <> $2
				  AND r.status IN ('submitted', 'approved')
				  AND a.state <> 'dropped'
				  AND a.ta_id = $3
			)`, courseID, reqID, t.id).Scan(&dupSameCourse); err != nil {
			return nil, false, err
		}
		add("duplicate", t.name,
			fmt.Sprintf("%s ไม่ซ้ำในวิชานี้", t.name),
			fmt.Sprintf("%s อยู่ในคำขออื่นของวิชานี้แล้ว", t.name),
			!dupSameCourse)

		count, err := s.reservedCourseCount(ctx, tx, t.id, termID, courseID)
		if err != nil {
			return nil, false, err
		}
		add("cap", t.name,
			fmt.Sprintf("%s เป็นผู้ช่วยสอนอยู่ %d วิชา (ยังไม่เกินขีดจำกัด 3 วิชา)", t.name, count),
			fmt.Sprintf("%s เป็นผู้ช่วยสอนครบ 3 วิชาในภาคการศึกษานี้แล้ว", t.name),
			count < 3)

		// Workload total range.
		var totOK bool
		var totMsg string
		// Co-taught sections describe one sitting, so their declared hours are
		// the same hours seen from several groups. Counting each row would
		// inflate a graduate TA past the 10–12 rule for doing nothing extra.
		countedGroups := map[int]bool{}
		countOnce := func(a asgRow) bool {
			if a.coGroup == 0 {
				return true
			}
			if countedGroups[a.coGroup] {
				return false
			}
			countedGroups[a.coGroup] = true
			return true
		}
		if t.level == "master" || t.level == "phd" {
			// Sum this TA's grad hours across all sections in this request.
			var tot float64
			for _, a := range asgs {
				if a.taID != t.id || !countOnce(a) {
					continue
				}
				tot += a.wl.HelpTeachHrs + a.wl.PrepHrs + a.wl.GradeHrs + a.wl.OtherHrs
			}
			totOK = tot >= 10 && tot <= 12
			if totOK {
				totMsg = fmt.Sprintf("%s ภาระงานรวม %.2f ชม./สัปดาห์ (อยู่ในช่วง 10–12)", t.name, tot)
			} else {
				totMsg = fmt.Sprintf("%s ภาระงาน %.2f ชม./สัปดาห์ ไม่อยู่ในช่วง 10–12 ตามระเบียบบัณฑิตศึกษา", t.name, tot)
			}
		} else {
			var tot float64
			for _, a := range asgs {
				if a.taID != t.id || !countOnce(a) {
					continue
				}
				tot += a.wl.CheckWorkHrs + a.wl.AttendanceHrs + a.wl.UGOtherHrs + a.wl.LabHrs + a.wl.LabOtherHrs
			}
			totOK = tot > 0
			if totOK {
				totMsg = fmt.Sprintf("%s ระบุภาระงาน %.2f ชม./สัปดาห์", t.name, tot)
			} else {
				totMsg = fmt.Sprintf("%s ยังไม่ได้ระบุภาระงาน", t.name)
			}
		}
		checks = append(checks, DecisionCheck{Rule: "workload", TAName: t.name, Passed: totOK, Message: totMsg})
	}

	// Per-(TA, section) time-conflict rules.
	for _, a := range asgs {
		// Informational only since 0041. A clash with the TA's OWN timetable no
		// longer sinks the request: applyClashOutcome has already marked the
		// affected assignment 'trimmed' or 'dropped', which is the outcome the
		// lecturers asked for ("ทับแค่บางคาบก็สอนได้บางคาบ"). Rejecting here as
		// well would throw away the sections that are still perfectly workable.
		//
		// cross_conflict below stays blocking — an overlap with a DIFFERENT
		// course the TA already assists cannot be resolved by dropping sessions
		// from this request, because both sides are teaching commitments.
		if err := s.checkOwnClassConflict(ctx, tx, a.taID, a.secID, a.name); err != nil {
			addWarn("own_conflict", a.name, "", err.Error(), false)
		} else {
			add("own_conflict", a.name, fmt.Sprintf("%s เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง", a.name), "", true)
		}
		if err := s.checkCrossRequestConflict(ctx, tx, a.taID, a.secID, termID, courseID, reqID, []string{"approved"}, a.name); err != nil {
			add("cross_conflict", a.name, "", err.Error(), false)
		} else {
			add("cross_conflict", a.name, fmt.Sprintf("%s เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่", a.name), "", true)
		}
	}

	// Sections within this request that overlap in time for the same TA are
	// almost always intentional at CP KKU: one course is taught to several
	// sections simultaneously (e.g. sec 01–02 ภาคปกติ + sec 03 โครงการพิเศษ
	// meeting in the same slot), and one TA covers them together. Every section
	// in a request belongs to the SAME teaching_course, so a same-slot overlap
	// here is co-teaching, not a real double-booking. Surface it as a Warning
	// (staff-visible) instead of auto-rejecting. Genuine cross-course clashes
	// are still caught by cross_conflict above.
	for taID, secs := range taSections {
		if len(secs) < 2 {
			continue
		}
		var clash int
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM section_schedules a
			JOIN section_schedules b ON b.section_id = ANY($1) AND b.section_id <> a.section_id
			WHERE a.section_id = ANY($1)
			  AND a.day_of_week = b.day_of_week
			  AND a.start_time < b.end_time AND b.start_time < a.end_time`, secs).Scan(&clash); err != nil {
			return nil, false, err
		}
		name := "TA"
		for _, t := range tas {
			if t.id == taID {
				name = t.name
				break
			}
		}
		addWarn("intra_conflict", name,
			fmt.Sprintf("%s section ในคำขอเดียวกันไม่ทับซ้อน", name),
			fmt.Sprintf("%s คุมหลาย section ที่สอนพร้อมกัน (เวลาทับซ้อน) อนุญาตสำหรับวิชาที่สอนรวมกัน", name),
			clash == 0)
	}

	passed := true
	for _, c := range checks {
		if !c.Passed && !c.Warning {
			passed = false
			break
		}
	}
	return checks, passed, nil
}

// The startup sweep that used to force-decide every 'submitted' row lived
// here. Under the deferred-decision model (0041) 'submitted' is a legitimate
// resting state — the request is waiting for a TA's timetable — so blindly
// deciding those rows would defeat the whole mechanism. Its replacement,
// SweepPendingRequests in ta_request_decide.go, only finalises requests whose
// TAs have all filed timetables.

func (s *TARequestService) courseLabel(ctx context.Context, courseID uuid.UUID) (code, nameTH string) {
	_ = s.pool.QueryRow(ctx, `
		SELECT tc.code, tc.name_th FROM teaching_courses tc
		WHERE tc.id = $1`,
		courseID).Scan(&code, &nameTH)
	return code, nameTH
}

type TARequestSummary struct {
	ID               uuid.UUID       `json:"id"`
	Code             string          `json:"course_code"`
	NameTH           string          `json:"course_name"`
	Status           string          `json:"status"`
	SubmittedAt      *time.Time      `json:"submitted_at,omitempty"`
	DecidedAt        *time.Time      `json:"decided_at,omitempty"`
	DecidedBy        *uuid.UUID      `json:"decided_by,omitempty"`
	RejectReason     *string         `json:"reject_reason,omitempty"`
	TeachingCourseID uuid.UUID       `json:"teaching_course_id"`
	LecturerName     string          `json:"lecturer_name"`
	TACount          int             `json:"ta_count"`
	TermID           uuid.UUID       `json:"term_id"`
	AcademicYear     int             `json:"academic_year"`
	Semester         int             `json:"semester"`
	IsLate           bool            `json:"is_late"`
	DecisionChecks   []DecisionCheck `json:"decision_checks"`
	// TrimmedCount / DroppedCount summarise the deferred decision's per-section
	// outcome. An approved request can still contain sections the TA cannot
	// fully work, and the summary row is where the lecturer looks first — a
	// green "อนุมัติแล้ว" with no other signal would hide that entirely.
	TrimmedCount int `json:"trimmed_count"`
	DroppedCount int `json:"dropped_count"`
	// CanCancel is the server's own verdict on whether Cancel would currently
	// succeed for this row — status is submitted/approved AND no work_logs
	// exist yet. Computed here rather than left for the client to infer, so
	// the button's enabled state can never drift from what Cancel itself
	// checks (the same reasoning ExportPreview.CanExport already follows).
	CanCancel bool `json:"can_cancel"`
}

const requestSummarySelect = `
	SELECT r.id, tc.code, tc.name_th, r.status::text, r.submitted_at, r.decided_at, r.decided_by, r.reject_reason, r.teaching_course_id,
	       u.first_name || ' ' || u.last_name,
	       (SELECT COUNT(DISTINCT a.ta_id) FROM ta_request_assignments a WHERE a.request_id = r.id),
	       tc.term_id, at.academic_year, at.semester, r.is_late, r.decision_checks,
	       (SELECT COUNT(*) FROM ta_request_assignments a WHERE a.request_id = r.id AND a.state = 'trimmed'),
	       (SELECT COUNT(*) FROM ta_request_assignments a WHERE a.request_id = r.id AND a.state = 'dropped'),
	       r.status::text IN ('submitted','approved') AND NOT EXISTS (
	           SELECT 1 FROM work_logs wl
	           JOIN ta_request_assignments a ON a.id = wl.assignment_id
	           WHERE a.request_id = r.id)
	FROM ta_requests r
	JOIN teaching_courses tc ON tc.id = r.teaching_course_id
	JOIN academic_terms at ON at.id = tc.term_id
	JOIN users u ON u.id = r.lecturer_id`

func scanRequestSummaries(rows pgx.Rows) ([]TARequestSummary, error) {
	defer rows.Close()
	out := []TARequestSummary{}
	for rows.Next() {
		var t TARequestSummary
		var checksRaw []byte
		if err := rows.Scan(&t.ID, &t.Code, &t.NameTH, &t.Status, &t.SubmittedAt, &t.DecidedAt, &t.DecidedBy, &t.RejectReason, &t.TeachingCourseID, &t.LecturerName, &t.TACount, &t.TermID, &t.AcademicYear, &t.Semester, &t.IsLate, &checksRaw, &t.TrimmedCount, &t.DroppedCount, &t.CanCancel); err != nil {
			return nil, err
		}
		if len(checksRaw) > 0 {
			_ = json.Unmarshal(checksRaw, &t.DecisionChecks)
		}
		if t.DecisionChecks == nil {
			t.DecisionChecks = []DecisionCheck{}
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *TARequestService) ListForLecturer(ctx context.Context, lecturerID uuid.UUID) ([]TARequestSummary, error) {
	rows, err := s.pool.Query(ctx,
		requestSummarySelect+` WHERE r.lecturer_id = $1 ORDER BY COALESCE(r.submitted_at, r.created_at) DESC`, lecturerID)
	if err != nil {
		return nil, err
	}
	return scanRequestSummaries(rows)
}

func (s *TARequestService) ListPending(ctx context.Context) ([]TARequestSummary, error) {
	rows, err := s.pool.Query(ctx,
		requestSummarySelect+` WHERE r.status = 'submitted' ORDER BY r.submitted_at`)
	if err != nil {
		return nil, err
	}
	return scanRequestSummaries(rows)
}

// ListAll returns every request across every lecturer. Intended for the staff
// approvals dashboard so decided requests remain visible as history rather than
// disappearing from view once acted on.
func (s *TARequestService) ListAll(ctx context.Context) ([]TARequestSummary, error) {
	rows, err := s.pool.Query(ctx,
		requestSummarySelect+` ORDER BY COALESCE(r.submitted_at, r.created_at) DESC`)
	if err != nil {
		return nil, err
	}
	return scanRequestSummaries(rows)
}

// ---------------------------------------------------------------------------
// Detail (staff review view)
// ---------------------------------------------------------------------------

type TARequestAssignmentDetail struct {
	SectionNo           string    `json:"section_no"`
	TAID                uuid.UUID `json:"ta_id"`
	TAName              string    `json:"ta_name"`
	Email               string    `json:"email"`
	StudentID           *string   `json:"student_id,omitempty"`
	Level               string    `json:"level"`
	TotalHrs            float64   `json:"total_hrs"`
	ProfileStatus       string    `json:"profile_status"`
	HasSchedule         bool      `json:"has_schedule"`
	ApprovedCourseCount int       `json:"approved_course_count"`
	Warnings            []string  `json:"warnings"`
	// State and StateReason expose the per-section outcome of the deferred
	// decision: 'active', 'trimmed' (some sessions clash with the TA's own
	// timetable) or 'dropped' (all of them do). Without surfacing these the
	// lecturer would see an approved request and never learn that some
	// sessions are unworkable — the TA would discover it only when the work
	// log refused the entry.
	State       string  `json:"state"`
	StateReason *string `json:"state_reason,omitempty"`
}

type TARequestCountDetail struct {
	SectionNo      string `json:"section_no"`
	UndergradCount int    `json:"undergrad_count"`
	GraduateCount  int    `json:"graduate_count"`
}

type TARequestDetail struct {
	TARequestSummary
	ReimburseScope string                      `json:"reimburse_scope"`
	Counts         []TARequestCountDetail      `json:"counts"`
	Assignments    []TARequestAssignmentDetail `json:"assignments"`
}

func (s *TARequestService) Detail(ctx context.Context, reqID uuid.UUID) (*TARequestDetail, error) {
	var d TARequestDetail
	var checksRaw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT r.id, tc.code, tc.name_th, r.status::text, r.submitted_at, r.decided_at, r.decided_by, r.reject_reason, r.teaching_course_id,
		       u.first_name || ' ' || u.last_name,
		       (SELECT COUNT(DISTINCT a.ta_id) FROM ta_request_assignments a WHERE a.request_id = r.id),
		       tc.term_id, at.academic_year, at.semester,
		       r.decision_checks, r.reimburse_scope::text
		FROM ta_requests r
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		JOIN academic_terms at ON at.id = tc.term_id
		JOIN users u ON u.id = r.lecturer_id
		WHERE r.id = $1`, reqID).Scan(
		&d.ID, &d.Code, &d.NameTH, &d.Status, &d.SubmittedAt, &d.DecidedAt, &d.DecidedBy, &d.RejectReason,
		&d.TeachingCourseID, &d.LecturerName, &d.TACount,
		&d.TermID, &d.AcademicYear, &d.Semester,
		&checksRaw, &d.ReimburseScope)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errors.New("ไม่พบคำขอนี้")
		}
		return nil, err
	}
	if len(checksRaw) > 0 {
		_ = json.Unmarshal(checksRaw, &d.DecisionChecks)
	}
	if d.DecisionChecks == nil {
		d.DecisionChecks = []DecisionCheck{}
	}

	var termID uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT term_id FROM teaching_courses WHERE id = $1`, d.TeachingCourseID).Scan(&termID); err != nil {
		return nil, err
	}

	crows, err := s.pool.Query(ctx, `
		SELECT sec.sec_no, c.undergrad_count, c.graduate_count
		FROM ta_request_counts c JOIN sections sec ON sec.id = c.section_id
		WHERE c.request_id = $1 ORDER BY sec.sec_no`, reqID)
	if err != nil {
		return nil, err
	}
	for crows.Next() {
		var c TARequestCountDetail
		if err := crows.Scan(&c.SectionNo, &c.UndergradCount, &c.GraduateCount); err != nil {
			crows.Close()
			return nil, err
		}
		d.Counts = append(d.Counts, c)
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return nil, err
	}

	arows, err := s.pool.Query(ctx, `
		SELECT sec.sec_no, a.ta_id, u.first_name || ' ' || u.last_name, u.email, u.student_id, a.level::text,
		       COALESCE(w.help_teach_hrs,0)+COALESCE(w.prep_hrs,0)+COALESCE(w.grade_hrs,0)+COALESCE(w.other_hrs,0)
		       +COALESCE(w.check_work_hrs,0)+COALESCE(w.attendance_hrs,0)+COALESCE(w.ug_other_hrs,0)+COALESCE(w.lab_hrs,0)
		       +COALESCE(w.lab_other_hrs,0),
		       COALESCE(p.status::text, 'pending'),
		       EXISTS (SELECT 1 FROM ta_class_schedules cs WHERE cs.user_id = a.ta_id AND cs.term_id = $2),
		       a.section_id, a.state::text, a.state_reason
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		JOIN users u ON u.id = a.ta_id
		LEFT JOIN ta_workload_forms w ON w.assignment_id = a.id
		LEFT JOIN ta_profiles p ON p.user_id = a.ta_id
		WHERE a.request_id = $1 ORDER BY sec.sec_no, u.first_name`, reqID, termID)
	if err != nil {
		return nil, err
	}
	type pendingCheck struct {
		idx   int
		taID  uuid.UUID
		secID uuid.UUID
	}
	var checks []pendingCheck
	for arows.Next() {
		var a TARequestAssignmentDetail
		var secID uuid.UUID
		if err := arows.Scan(&a.SectionNo, &a.TAID, &a.TAName, &a.Email, &a.StudentID, &a.Level, &a.TotalHrs,
			&a.ProfileStatus, &a.HasSchedule, &secID, &a.State, &a.StateReason); err != nil {
			arows.Close()
			return nil, err
		}
		a.Warnings = []string{}
		d.Assignments = append(d.Assignments, a)
		checks = append(checks, pendingCheck{idx: len(d.Assignments) - 1, taID: a.TAID, secID: secID})
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return nil, err
	}

	for _, c := range checks {
		a := &d.Assignments[c.idx]
		count, err := s.reservedCourseCount(ctx, s.pool, c.taID, termID, d.TeachingCourseID)
		if err != nil {
			return nil, err
		}
		a.ApprovedCourseCount = count
		if a.ProfileStatus != "approved" {
			a.Warnings = append(a.Warnings, "เอกสารยังไม่ผ่านการอนุมัติ")
		}
		if !a.HasSchedule {
			a.Warnings = append(a.Warnings, "ยังไม่ได้บันทึกตารางเรียนของภาคการศึกษานี้")
		}
		if count >= 3 {
			a.Warnings = append(a.Warnings, "เป็นผู้ช่วยสอนครบ 3 วิชาในภาคการศึกษานี้แล้ว อนุมัติเพิ่มไม่ได้")
		}
		if err := s.checkOwnClassConflict(ctx, s.pool, c.taID, c.secID, a.TAName); err != nil {
			a.Warnings = append(a.Warnings, "เวลาสอนทับซ้อนกับตารางเรียนของ TA")
		}
		if err := s.checkCrossRequestConflict(ctx, s.pool, c.taID, c.secID, termID, d.TeachingCourseID, reqID, []string{"approved"}, a.TAName); err != nil {
			a.Warnings = append(a.Warnings, "เวลาสอนทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่")
		}
	}
	return &d, nil
}

// Windows
type Window struct {
	// ID is intentionally untagged: UpsertWindow treats uuid.Nil as "create
	// new" (isNew := in.ID == uuid.Nil), so a missing/zero id is the normal
	// shape for a create call, not an error.
	ID uuid.UUID `json:"id"`
	// TermID/OpensAt/ClosesAt are all NOT NULL columns (migration 0001).
	TermID   uuid.UUID `json:"term_id" validate:"required"`
	OpensAt  time.Time `json:"opens_at" validate:"required"`
	ClosesAt time.Time `json:"closes_at" validate:"required"`
	IsOpen   bool      `json:"is_open"`
	Note     *string   `json:"note,omitempty" validate:"omitempty,max=500"`
}

func (s *TARequestService) UpsertWindow(ctx context.Context, actor uuid.UUID, in Window) (*Window, error) {
	isNew := in.ID == uuid.Nil
	if isNew {
		in.ID = uuid.New()
	}
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "ta_window.upsert", Entity: "ta_window", EntityID: in.ID.String(), After: in},
		func(tx pgx.Tx) error {
			if isNew {
				_, err := tx.Exec(ctx,
					`INSERT INTO ta_request_windows (id, term_id, opens_at, closes_at, is_open, note)
					 VALUES ($1,$2,$3,$4,$5,$6)`,
					in.ID, in.TermID, in.OpensAt, in.ClosesAt, in.IsOpen, in.Note)
				return err
			}
			_, err := tx.Exec(ctx,
				`UPDATE ta_request_windows SET opens_at=$2, closes_at=$3, is_open=$4, note=$5 WHERE id=$1`,
				in.ID, in.OpensAt, in.ClosesAt, in.IsOpen, in.Note)
			return err
		}); err != nil {
		return nil, err
	}
	return &in, nil
}

func (s *TARequestService) ListWindows(ctx context.Context, termID *uuid.UUID) ([]Window, error) {
	q := `SELECT id, term_id, opens_at, closes_at, is_open, note FROM ta_request_windows`
	args := []any{}
	if termID != nil {
		q += ` WHERE term_id = $1`
		args = append(args, *termID)
	}
	q += ` ORDER BY opens_at DESC`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Window{}
	for rows.Next() {
		var w Window
		if err := rows.Scan(&w.ID, &w.TermID, &w.OpensAt, &w.ClosesAt, &w.IsOpen, &w.Note); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

func (s *TARequestService) DeleteWindow(ctx context.Context, actor, id uuid.UUID) error {
	// Refuse if any live TA request was filed under this window; deleting it
	// would break audit/traceability.
	var used int
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ta_requests WHERE window_id = $1`, id).Scan(&used); err != nil {
		return err
	}
	if used > 0 {
		return errors.New("ช่วงเวลานี้ถูกอ้างอิงจากคำขอ TA แล้ว ไม่สามารถลบได้ (แนะนำให้ปิดชั่วคราวแทน)")
	}
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "ta_window.delete", Entity: "ta_window", EntityID: id.String()},
		func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `DELETE FROM ta_request_windows WHERE id = $1`, id)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return errors.New("ไม่พบช่วงเวลารับสมัครนี้")
			}
			return nil
		})
}
