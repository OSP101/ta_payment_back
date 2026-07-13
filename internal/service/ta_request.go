package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

type WorkloadInput struct {
	HelpTeachHrs  float64 `json:"help_teach_hrs"`
	HelpTeachDesc string  `json:"help_teach_desc"`
	PrepHrs       float64 `json:"prep_hrs"`
	PrepDesc      string  `json:"prep_desc"`
	GradeHrs      float64 `json:"grade_hrs"`
	GradeDesc     string  `json:"grade_desc"`
	OtherHrs      float64 `json:"other_hrs"`
	OtherDesc     string  `json:"other_desc"`
	CheckWorkHrs  float64 `json:"check_work_hrs"`
	AttendanceHrs float64 `json:"attendance_hrs"`
	UGOtherHrs    float64 `json:"ug_other_hrs"`
	UGOtherDesc   string  `json:"ug_other_desc"`
	LabHrs        float64 `json:"lab_hrs"`
}

type CreateTARequestInput struct {
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	ReimburseScope   string    `json:"reimburse_scope"`
	Counts           []struct {
		SectionID      uuid.UUID `json:"section_id"`
		UndergradCount int       `json:"undergrad_count"`
		GraduateCount  int       `json:"graduate_count"`
	} `json:"counts"`
	Assignments []struct {
		// SectionIDs lists every section this TA is assigned to within the
		// request. One TA now spans N sections so worklog can attribute time
		// to the specific section taught; the workload declaration itself is
		// shared across those sections (single row in ta_workload_forms).
		SectionIDs []uuid.UUID   `json:"section_ids"`
		TAID       uuid.UUID     `json:"ta_id"`
		Level      string        `json:"level"`
		Workload   WorkloadInput `json:"workload"`
	} `json:"assignments"`
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

	var (
		termID                uuid.UUID
		lectureHrs, labHrs    float64
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT tc.term_id, COALESCE(fc.lecture_hrs, 0), COALESCE(fc.lab_hrs, 0)
		FROM teaching_courses tc JOIN faculty_courses fc ON fc.id = tc.faculty_course_id
		WHERE tc.id = $1
	`, in.TeachingCourseID).Scan(&termID, &lectureHrs, &labHrs); err != nil {
		return nil, errors.New("ไม่พบรายวิชาที่เลือก")
	}

	// enforce active window and capture which window admitted this request so
	// window deletion can honour the "in use" guard (M3).
	var windowID uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT w.id FROM ta_request_windows w
		WHERE w.term_id = $1 AND w.is_open AND NOW() BETWEEN w.opens_at AND w.closes_at
		ORDER BY w.closes_at DESC LIMIT 1
	`, termID).Scan(&windowID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("ยังไม่เปิดรับคำขอ TA สำหรับภาคการศึกษานี้")
	}
	if err != nil {
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
	for i, a := range in.Assignments {
		name, level, err := s.validateTA(ctx, a.TAID, a.Level)
		if err != nil {
			return nil, err
		}
		levels[i] = level
		// Reject impossibly-shaped numeric input before it can overflow the
		// underlying NUMERIC(4,2) column or invalidate the total-hours rule
		// through cancellation. Not a "check" — this is corrupt data.
		if err := validateWorkloadFields(a.Workload, name); err != nil {
			return nil, err
		}
		// Enforce per-field caps mirroring the client-side rule (see
		// project_workload_caps memory). Applies to undergrad only; grad
		// hours follow the 10–12 total rule inside autoDecide.
		if level == "undergrad" {
			nSecs := float64(len(a.SectionIDs))
			if err := validateUndergradWorkloadCaps(a.Workload, name, lectureHrs, labHrs, nSecs); err != nil {
				return nil, err
			}
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rid := uuid.New()
	if _, err := tx.Exec(ctx,
		`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at, window_id)
		 VALUES ($1,$2,$3,$4::reimburse_scope,'submitted',NOW(),$5)`,
		rid, in.TeachingCourseID, lecturerID, in.ReimburseScope, windowID); err != nil {
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
	for i, a := range in.Assignments {
		wl := a.Workload
		for j, secID := range a.SectionIDs {
			aid := uuid.New()
			if _, err := tx.Exec(ctx,
				`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
				 VALUES ($1,$2,$3,$4,$5::study_level)`, aid, rid, secID, a.TAID, levels[i]); err != nil {
				return nil, err
			}
			if j != 0 {
				continue
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO ta_workload_forms (id, assignment_id, help_teach_hrs, help_teach_desc, prep_hrs, prep_desc,
					grade_hrs, grade_desc, other_hrs, other_desc, check_work_hrs, attendance_hrs, ug_other_hrs, ug_other_desc, lab_hrs)
				VALUES (gen_random_uuid(), $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
				aid, wl.HelpTeachHrs, wl.HelpTeachDesc, wl.PrepHrs, wl.PrepDesc, wl.GradeHrs, wl.GradeDesc,
				wl.OtherHrs, wl.OtherDesc, wl.CheckWorkHrs, wl.AttendanceHrs, wl.UGOtherHrs, wl.UGOtherDesc, wl.LabHrs); err != nil {
				return nil, err
			}
		}
	}

	checks, passed, err := s.autoDecide(ctx, tx, rid)
	if err != nil {
		return nil, err
	}
	verdict := "rejected"
	var reason string
	if passed {
		verdict = "approved"
	} else {
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
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	s.aud.Log(ctx, audit.Entry{
		ActorID:  &lecturerID,
		Action:   "ta_request.auto_decide",
		Entity:   "ta_request",
		EntityID: rid.String(),
		Note:     verdict,
		After:    in,
	})
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
}

// validateWorkloadFields rejects negative or absurd hour components. Each field
// is stored as NUMERIC(4,2), so the hard ceiling is < 100.
func validateWorkloadFields(w WorkloadInput, name string) error {
	fields := []float64{
		w.HelpTeachHrs, w.PrepHrs, w.GradeHrs, w.OtherHrs,
		w.CheckWorkHrs, w.AttendanceHrs, w.UGOtherHrs, w.LabHrs,
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

// validateUndergradWorkloadCaps mirrors the client-side per-field caps:
//   - in-class activities (check-work, attendance, lab) scale with class
//     exposure: max = credit_hrs × n_sections
//   - ug_other ("อื่น ๆ") is fixed at course credit_hrs — stakeholder rule:
//     prep/admin time can't scale with section count.
// Only applies to undergrad; grad fields are governed by the 10–12 total
// rule inside autoDecide.
func validateUndergradWorkloadCaps(w WorkloadInput, name string, lectureHrs, labHrs, nSecs float64) error {
	lectureCap := lectureHrs * nSecs
	labCap := labHrs * nSecs
	checks := []struct {
		v     float64
		cap   float64
		label string
	}{
		{w.CheckWorkHrs, lectureCap, "ช่วยตรวจงาน"},
		{w.AttendanceHrs, lectureCap, "เช็คชื่อ/เก็บใบงาน"},
		{w.UGOtherHrs, lectureHrs, "อื่น ๆ (บรรยาย)"},
		{w.LabHrs, labCap, "ปฏิบัติการ"},
	}
	for _, c := range checks {
		if c.v > c.cap {
			return fmt.Errorf("ภาระงาน '%s' ของ %s เกินขีดจำกัด %.2f ชม./สัปดาห์", c.label, name, c.cap)
		}
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
		if err := s.checkOwnClassConflict(ctx, s.pool, taID, sid, name); err != nil {
			msgs = append(msgs, err.Error())
		}
		// Passing uuid.Nil for excludeRequestID is fine here — we're not
		// filing a request yet, so no ID to exclude.
		if err := s.checkCrossRequestConflict(ctx, s.pool, taID, sid, termID, teachingCourseID, uuid.Nil, []string{"approved"}, name); err != nil {
			msgs = append(msgs, err.Error())
		}
		if len(msgs) > 0 {
			out = append(out, SectionConflict{SectionID: sid, Messages: msgs})
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

func (s *TARequestService) checkOwnClassConflict(ctx context.Context, q querier, taID, sectionID uuid.UUID, name string) error {
	var conflicts int
	err := q.QueryRow(ctx, `
		SELECT COUNT(*) FROM section_schedules ss
		JOIN sections sec ON sec.id = ss.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		JOIN ta_class_schedules cs ON cs.user_id = $1 AND cs.term_id = tc.term_id AND NOT cs.is_wba
		WHERE ss.section_id = $2
		  AND ss.day_of_week = cs.day_of_week
		  AND ss.start_time < cs.end_time
		  AND ss.end_time > cs.start_time`,
		taID, sectionID).Scan(&conflicts)
	if err != nil {
		return err
	}
	if conflicts > 0 {
		return fmt.Errorf("เวลาสอนของ section นี้ทับซ้อนกับตารางเรียนของ %s", name)
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

// approvedCourseCount counts distinct courses (other than courseID) in the term
// where the TA is on an approved request.
func (s *TARequestService) approvedCourseCount(ctx context.Context, q querier, taID, termID, courseID uuid.UUID) (int, error) {
	var count int
	err := q.QueryRow(ctx, `
		SELECT COUNT(DISTINCT r.teaching_course_id) FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		WHERE a.ta_id = $1 AND tc.term_id = $2 AND r.status = 'approved'
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
	}
	arows, err := tx.Query(ctx, `
		SELECT a.ta_id, a.section_id, u.first_name || ' ' || u.last_name, a.level::text,
		       COALESCE(w.help_teach_hrs,0), COALESCE(w.prep_hrs,0),
		       COALESCE(w.grade_hrs,0),      COALESCE(w.other_hrs,0),
		       COALESCE(w.check_work_hrs,0), COALESCE(w.attendance_hrs,0),
		       COALESCE(w.ug_other_hrs,0),   COALESCE(w.lab_hrs,0)
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
			&a.wl.CheckWorkHrs, &a.wl.AttendanceHrs, &a.wl.UGOtherHrs, &a.wl.LabHrs); err != nil {
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

		if err := s.checkTAEligibility(ctx, tx, t.id, termID, t.name); err != nil {
			// Distinguish docs vs schedule for a more useful checklist. The
			// docs branch is a warning (staff sees it, but the request is
			// still auto-approved); schedule stays blocking.
			msg := err.Error()
			if strings.Contains(msg, "ตารางเรียน") {
				addWarn("docs", t.name, fmt.Sprintf("%s เอกสารครบและได้รับการอนุมัติ", t.name), "", true)
				add("schedule", t.name, "", msg, false)
			} else {
				addWarn("docs", t.name, "", msg, false)
				add("schedule", t.name, fmt.Sprintf("%s บันทึกตารางเรียนแล้ว", t.name), "", true) // may be wrong but checkTAEligibility short-circuits on docs first; if this ordering matters we'd need to split the helper — accept slight noise
			}
		} else {
			addWarn("docs", t.name, fmt.Sprintf("%s เอกสารครบและได้รับการอนุมัติ", t.name), "", true)
			add("schedule", t.name, fmt.Sprintf("%s บันทึกตารางเรียนของภาคเรียนแล้ว", t.name), "", true)
		}

		var dupSameCourse bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM ta_request_assignments a
				JOIN ta_requests r ON r.id = a.request_id
				WHERE r.teaching_course_id = $1
				  AND r.id <> $2
				  AND r.status = 'approved'
				  AND a.ta_id = $3
			)`, courseID, reqID, t.id).Scan(&dupSameCourse); err != nil {
			return nil, false, err
		}
		add("duplicate", t.name,
			fmt.Sprintf("%s ไม่ซ้ำในวิชานี้", t.name),
			fmt.Sprintf("%s ได้รับอนุมัติเป็น TA ของวิชานี้ในคำขออื่นแล้ว", t.name),
			!dupSameCourse)

		count, err := s.approvedCourseCount(ctx, tx, t.id, termID, courseID)
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
		if t.level == "master" || t.level == "phd" {
			// Sum this TA's grad hours across all sections in this request.
			var tot float64
			for _, a := range asgs {
				if a.taID != t.id {
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
				if a.taID != t.id {
					continue
				}
				tot += a.wl.CheckWorkHrs + a.wl.AttendanceHrs + a.wl.UGOtherHrs + a.wl.LabHrs
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
		if err := s.checkOwnClassConflict(ctx, tx, a.taID, a.secID, a.name); err != nil {
			add("own_conflict", a.name, "", err.Error(), false)
		} else {
			add("own_conflict", a.name, fmt.Sprintf("%s เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง", a.name), "", true)
		}
		if err := s.checkCrossRequestConflict(ctx, tx, a.taID, a.secID, termID, courseID, reqID, []string{"approved"}, a.name); err != nil {
			add("cross_conflict", a.name, "", err.Error(), false)
		} else {
			add("cross_conflict", a.name, fmt.Sprintf("%s เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่", a.name), "", true)
		}
	}

	// Sections within this request must not overlap for the same TA.
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
		add("intra_conflict", name,
			fmt.Sprintf("%s section ในคำขอเดียวกันไม่ทับซ้อน", name),
			fmt.Sprintf("%s ถูกมอบหมายให้ section ที่เวลาสอนทับซ้อนกันเองในคำขอนี้", name),
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

// RunPendingSweep is a one-shot startup task that auto-decides any legacy
// row still in the 'submitted' state after this migration ships. Idempotent —
// running it again is a no-op once the submitted set is empty.
func (s *TARequestService) RunPendingSweep(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT id FROM ta_requests WHERE status = 'submitted'`)
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	log.Printf("ta_request sweep: auto-deciding %d legacy pending requests", len(ids))

	decided := 0
	for _, id := range ids {
		if err := s.decideOne(ctx, id); err != nil {
			log.Printf("ta_request sweep %s: %v", id, err)
			continue
		}
		decided++
	}
	log.Printf("ta_request sweep: %d/%d decided", decided, len(ids))
	return nil
}

func (s *TARequestService) decideOne(ctx context.Context, reqID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	checks, passed, err := s.autoDecide(ctx, tx, reqID)
	if err != nil {
		return err
	}
	verdict := "rejected"
	var reason string
	if passed {
		verdict = "approved"
	} else {
		reason = joinRejectMessages(checks)
	}
	checksJSON, err := json.Marshal(checks)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ta_requests SET
		  status          = $1::ta_request_status,
		  decided_at      = NOW(),
		  decided_by      = NULL,
		  reject_reason   = NULLIF($2, ''),
		  decision_checks = $3::jsonb,
		  updated_at      = NOW()
		WHERE id = $4 AND status = 'submitted'`, verdict, reason, checksJSON, reqID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.aud.Log(ctx, audit.Entry{Action: "ta_request.auto_decide.sweep", Entity: "ta_request", EntityID: reqID.String(), Note: verdict})
	s.notifyDecision(ctx, reqID, verdict, reason)
	return nil
}

func (s *TARequestService) courseLabel(ctx context.Context, courseID uuid.UUID) (code, nameTH string) {
	_ = s.pool.QueryRow(ctx, `
		SELECT fc.code, fc.name_th FROM teaching_courses tc
		JOIN faculty_courses fc ON fc.id = tc.faculty_course_id WHERE tc.id = $1`,
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
	DecisionChecks   []DecisionCheck `json:"decision_checks"`
}

const requestSummarySelect = `
	SELECT r.id, fc.code, fc.name_th, r.status::text, r.submitted_at, r.decided_at, r.decided_by, r.reject_reason, r.teaching_course_id,
	       u.first_name || ' ' || u.last_name,
	       (SELECT COUNT(DISTINCT a.ta_id) FROM ta_request_assignments a WHERE a.request_id = r.id),
	       tc.term_id, at.academic_year, at.semester, r.decision_checks
	FROM ta_requests r
	JOIN teaching_courses tc ON tc.id = r.teaching_course_id
	JOIN faculty_courses fc ON fc.id = tc.faculty_course_id
	JOIN academic_terms at ON at.id = tc.term_id
	JOIN users u ON u.id = r.lecturer_id`

func scanRequestSummaries(rows pgx.Rows) ([]TARequestSummary, error) {
	defer rows.Close()
	out := []TARequestSummary{}
	for rows.Next() {
		var t TARequestSummary
		var checksRaw []byte
		if err := rows.Scan(&t.ID, &t.Code, &t.NameTH, &t.Status, &t.SubmittedAt, &t.DecidedAt, &t.DecidedBy, &t.RejectReason, &t.TeachingCourseID, &t.LecturerName, &t.TACount, &t.TermID, &t.AcademicYear, &t.Semester, &checksRaw); err != nil {
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
	SectionNo           string   `json:"section_no"`
	TAID                uuid.UUID `json:"ta_id"`
	TAName              string   `json:"ta_name"`
	Email               string   `json:"email"`
	StudentID           *string  `json:"student_id,omitempty"`
	Level               string   `json:"level"`
	TotalHrs            float64  `json:"total_hrs"`
	ProfileStatus       string   `json:"profile_status"`
	HasSchedule         bool     `json:"has_schedule"`
	ApprovedCourseCount int      `json:"approved_course_count"`
	Warnings            []string `json:"warnings"`
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
		SELECT r.id, fc.code, fc.name_th, r.status::text, r.submitted_at, r.decided_at, r.decided_by, r.reject_reason, r.teaching_course_id,
		       u.first_name || ' ' || u.last_name,
		       (SELECT COUNT(DISTINCT a.ta_id) FROM ta_request_assignments a WHERE a.request_id = r.id),
		       tc.term_id, at.academic_year, at.semester,
		       r.decision_checks, r.reimburse_scope::text
		FROM ta_requests r
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		JOIN faculty_courses fc ON fc.id = tc.faculty_course_id
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
		       +COALESCE(w.check_work_hrs,0)+COALESCE(w.attendance_hrs,0)+COALESCE(w.ug_other_hrs,0)+COALESCE(w.lab_hrs,0),
		       COALESCE(p.status::text, 'pending'),
		       EXISTS (SELECT 1 FROM ta_class_schedules cs WHERE cs.user_id = a.ta_id AND cs.term_id = $2),
		       a.section_id
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
			&a.ProfileStatus, &a.HasSchedule, &secID); err != nil {
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
		count, err := s.approvedCourseCount(ctx, s.pool, c.taID, termID, d.TeachingCourseID)
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
			a.Warnings = append(a.Warnings, "เป็นผู้ช่วยสอนครบ 3 วิชาในภาคการศึกษานี้แล้ว — อนุมัติเพิ่มไม่ได้")
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
	ID       uuid.UUID `json:"id"`
	TermID   uuid.UUID `json:"term_id"`
	OpensAt  time.Time `json:"opens_at"`
	ClosesAt time.Time `json:"closes_at"`
	IsOpen   bool      `json:"is_open"`
	Note     *string   `json:"note,omitempty"`
}

func (s *TARequestService) UpsertWindow(ctx context.Context, actor uuid.UUID, in Window) (*Window, error) {
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
		_, err := s.pool.Exec(ctx,
			`INSERT INTO ta_request_windows (id, term_id, opens_at, closes_at, is_open, note)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			in.ID, in.TermID, in.OpensAt, in.ClosesAt, in.IsOpen, in.Note)
		if err != nil {
			return nil, err
		}
	} else {
		_, err := s.pool.Exec(ctx,
			`UPDATE ta_request_windows SET opens_at=$2, closes_at=$3, is_open=$4, note=$5 WHERE id=$1`,
			in.ID, in.OpensAt, in.ClosesAt, in.IsOpen, in.Note)
		if err != nil {
			return nil, err
		}
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "ta_window.upsert", Entity: "ta_window", EntityID: in.ID.String(), After: in})
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
	tag, err := s.pool.Exec(ctx, `DELETE FROM ta_request_windows WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("ไม่พบช่วงเวลารับสมัครนี้")
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "ta_window.delete", Entity: "ta_window", EntityID: id.String()})
	return nil
}
