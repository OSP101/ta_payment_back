package service

import (
	"context"
	"errors"
	"fmt"
	"time"

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
		SectionID uuid.UUID     `json:"section_id"`
		TAID      uuid.UUID     `json:"ta_id"`
		Level     string        `json:"level"`
		Workload  WorkloadInput `json:"workload"`
	} `json:"assignments"`
}

func (s *TARequestService) Create(ctx context.Context, lecturerID uuid.UUID, in CreateTARequestInput) (uuid.UUID, error) {
	if in.TeachingCourseID == uuid.Nil {
		return uuid.Nil, ErrInvalidInput
	}
	if in.ReimburseScope != "lecture" && in.ReimburseScope != "lab" && in.ReimburseScope != "both" {
		return uuid.Nil, errors.New("ประเภทการเบิกไม่ถูกต้อง")
	}
	if len(in.Assignments) == 0 {
		return uuid.Nil, errors.New("ต้องระบุรายชื่อ TA อย่างน้อย 1 คน")
	}

	// The lecturer must actually teach this course.
	var teaches bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM teaching_lecturers WHERE teaching_course_id = $1 AND lecturer_id = $2)
	`, in.TeachingCourseID, lecturerID).Scan(&teaches); err != nil {
		return uuid.Nil, err
	}
	if !teaches {
		return uuid.Nil, errors.New("คุณไม่ได้เป็นผู้สอนของรายวิชานี้")
	}

	var termID uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT term_id FROM teaching_courses WHERE id = $1`, in.TeachingCourseID).Scan(&termID); err != nil {
		return uuid.Nil, errors.New("ไม่พบรายวิชาที่เลือก")
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
		return uuid.Nil, errors.New("ยังไม่เปิดรับคำขอ TA สำหรับภาคการศึกษานี้")
	}
	if err != nil {
		return uuid.Nil, err
	}

	// A lecturer may file multiple TA requests for the same course (e.g. to add
	// more TAs after an earlier batch was approved). However, a given TA may
	// appear only once across the course's live requests — any submitted or
	// approved slot already ties them to this course and lets them start work
	// or draw pay. Rejected/cancelled slots are ignored so the lecturer can
	// re-file after fixing the problem.
	for _, a := range in.Assignments {
		var exists bool
		if err := s.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM ta_request_assignments ra
				JOIN ta_requests r ON r.id = ra.request_id
				WHERE r.teaching_course_id = $1
				  AND r.status IN ('submitted','approved')
				  AND ra.ta_id = $2
			)`, in.TeachingCourseID, a.TAID).Scan(&exists); err != nil {
			return uuid.Nil, err
		}
		if exists {
			name := s.taName(ctx, a.TAID)
			return uuid.Nil, fmt.Errorf("%s อยู่ในคำขอ TA ของวิชานี้อยู่แล้ว (รอพิจารณาหรืออนุมัติแล้ว) ไม่สามารถเพิ่มซ้ำได้", name)
		}
	}

	// Guard against the same TA being listed twice within one request. The UI
	// prevents this today, but the API is public and mistakes upstream would
	// otherwise leak into workload/audit rows.
	seen := map[uuid.UUID]struct{}{}
	for _, a := range in.Assignments {
		if _, dup := seen[a.TAID]; dup {
			return uuid.Nil, fmt.Errorf("มี %s ปรากฏซ้ำในคำขอเดียวกัน", s.taName(ctx, a.TAID))
		}
		seen[a.TAID] = struct{}{}
	}

	// All referenced sections must belong to this course.
	sectionIDs := map[uuid.UUID]struct{}{}
	for _, c := range in.Counts {
		sectionIDs[c.SectionID] = struct{}{}
		if c.UndergradCount < 0 || c.GraduateCount < 0 {
			return uuid.Nil, errors.New("จำนวน TA ต้องไม่ติดลบ")
		}
	}
	for _, a := range in.Assignments {
		sectionIDs[a.SectionID] = struct{}{}
	}
	for secID := range sectionIDs {
		var ok bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM sections WHERE id = $1 AND teaching_course_id = $2)`,
			secID, in.TeachingCourseID).Scan(&ok); err != nil {
			return uuid.Nil, err
		}
		if !ok {
			return uuid.Nil, errors.New("พบ section ที่ไม่ได้อยู่ในรายวิชานี้")
		}
	}

	// Per-TA validation.
	taSections := map[uuid.UUID][]uuid.UUID{}
	levels := make([]string, len(in.Assignments))
	for i, a := range in.Assignments {
		name, level, err := s.validateTA(ctx, a.TAID, a.Level)
		if err != nil {
			return uuid.Nil, err
		}
		levels[i] = level

		// Requirement 1: documents approved + class schedule recorded for this term.
		if err := s.checkTAEligibility(ctx, a.TAID, termID, name); err != nil {
			return uuid.Nil, err
		}
		// Requirement 2a: must not clash with the TA's own class schedule.
		if err := s.checkOwnClassConflict(ctx, a.TAID, a.SectionID, name); err != nil {
			return uuid.Nil, err
		}
		// Requirement 2b: must not clash with sections of other courses the TA
		// is already requested/assigned to this term.
		if err := s.checkCrossRequestConflict(ctx, a.TAID, a.SectionID, termID, in.TeachingCourseID, uuid.Nil, []string{"submitted", "approved"}, name); err != nil {
			return uuid.Nil, err
		}

		// Each workload component must be a sane non-negative number. Without
		// this, negative values can cancel out to satisfy the total-hours rule
		// (e.g. 20 + (−9) = 11) and values > 99.99 overflow NUMERIC(4,2) → 500.
		if err := validateWorkloadFields(a.Workload, name); err != nil {
			return uuid.Nil, err
		}
		if level == "master" || level == "phd" {
			tot := a.Workload.HelpTeachHrs + a.Workload.PrepHrs + a.Workload.GradeHrs + a.Workload.OtherHrs
			if tot < 10 || tot > 12 {
				return uuid.Nil, fmt.Errorf("ภาระงานของ %s (บัณฑิตศึกษา) ต้องรวม 10–12 ชม./สัปดาห์", name)
			}
		} else {
			tot := a.Workload.CheckWorkHrs + a.Workload.AttendanceHrs + a.Workload.UGOtherHrs + a.Workload.LabHrs
			if tot <= 0 {
				return uuid.Nil, fmt.Errorf("ยังไม่ได้ระบุภาระงานของ %s", name)
			}
		}

		taSections[a.TAID] = append(taSections[a.TAID], a.SectionID)
	}

	// Requirement 2c: sections within this same request must not overlap for
	// the same TA.
	for taID, secs := range taSections {
		if len(secs) < 2 {
			continue
		}
		var clash int
		if err := s.pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM section_schedules a
			JOIN section_schedules b ON b.section_id = ANY($1) AND b.section_id <> a.section_id
			WHERE a.section_id = ANY($1)
			  AND a.day_of_week = b.day_of_week
			  AND a.start_time < b.end_time AND b.start_time < a.end_time
		`, secs).Scan(&clash); err != nil {
			return uuid.Nil, err
		}
		if clash > 0 {
			name := s.taName(ctx, taID)
			return uuid.Nil, fmt.Errorf("%s ถูกมอบหมายให้ section ที่เวลาสอนทับซ้อนกันเองในคำขอนี้", name)
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)
	rid := uuid.New()
	_, err = tx.Exec(ctx,
		`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at, window_id)
		 VALUES ($1,$2,$3,$4::reimburse_scope,'submitted',NOW(),$5)`,
		rid, in.TeachingCourseID, lecturerID, in.ReimburseScope, windowID)
	if err != nil {
		return uuid.Nil, err
	}
	for _, c := range in.Counts {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ta_request_counts (request_id, section_id, undergrad_count, graduate_count)
			 VALUES ($1,$2,$3,$4)`, rid, c.SectionID, c.UndergradCount, c.GraduateCount); err != nil {
			return uuid.Nil, err
		}
	}
	for i, a := range in.Assignments {
		aid := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
			 VALUES ($1,$2,$3,$4,$5::study_level)`, aid, rid, a.SectionID, a.TAID, levels[i]); err != nil {
			return uuid.Nil, err
		}
		wl := a.Workload
		if _, err := tx.Exec(ctx, `
			INSERT INTO ta_workload_forms (id, assignment_id, help_teach_hrs, help_teach_desc, prep_hrs, prep_desc,
				grade_hrs, grade_desc, other_hrs, other_desc, check_work_hrs, attendance_hrs, ug_other_hrs, ug_other_desc, lab_hrs)
			VALUES (gen_random_uuid(), $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			aid, wl.HelpTeachHrs, wl.HelpTeachDesc, wl.PrepHrs, wl.PrepDesc, wl.GradeHrs, wl.GradeDesc,
			wl.OtherHrs, wl.OtherDesc, wl.CheckWorkHrs, wl.AttendanceHrs, wl.UGOtherHrs, wl.UGOtherDesc, wl.LabHrs); err != nil {
			return uuid.Nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &lecturerID, Action: "ta_request.submit", Entity: "ta_request", EntityID: rid.String(), After: in})
	return rid, nil
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

// checkTAEligibility enforces requirement 1: documents approved by staff and a
// class schedule recorded for the term (a WBA/year-4 row counts).
func (s *TARequestService) checkTAEligibility(ctx context.Context, taID, termID uuid.UUID, name string) error {
	var profileStatus *string
	if err := s.pool.QueryRow(ctx,
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
	if err := s.pool.QueryRow(ctx, `
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
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM ta_class_schedules WHERE user_id = $1 AND term_id = $2)`,
		taID, termID).Scan(&hasSchedule); err != nil {
		return err
	}
	if !hasSchedule {
		return fmt.Errorf("%s ยังไม่ได้บันทึกตารางเรียนของภาคการศึกษานี้", name)
	}
	return nil
}

// checkOwnClassConflict enforces requirement 2a: the section's teaching slots
// must not overlap the TA's own class schedule. WBA rows (00:00–00:00) never
// overlap, so year-4 students pass automatically.
func (s *TARequestService) checkOwnClassConflict(ctx context.Context, taID, sectionID uuid.UUID, name string) error {
	var conflicts int
	err := s.pool.QueryRow(ctx, `
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

// checkCrossRequestConflict enforces requirement 2b: the section must not
// overlap sections of other courses the TA already holds in the same term.
// excludeRequestID skips the request being approved itself.
func (s *TARequestService) checkCrossRequestConflict(ctx context.Context, taID, sectionID, termID, courseID, excludeRequestID uuid.UUID, statuses []string, name string) error {
	var conflicts int
	err := s.pool.QueryRow(ctx, `
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
func (s *TARequestService) approvedCourseCount(ctx context.Context, taID, termID, courseID uuid.UUID) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT r.teaching_course_id) FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		WHERE a.ta_id = $1 AND tc.term_id = $2 AND r.status = 'approved'
		  AND r.teaching_course_id <> $3`,
		taID, termID, courseID).Scan(&count)
	return count, err
}

// Approve is the final gate (requirement 3): the 3-course cap and all TA
// eligibility rules are re-checked here, because a TA only "counts" once
// approved.
func (s *TARequestService) Approve(ctx context.Context, actor, reqID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var status string
	var courseID, lecturerID uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT status::text, teaching_course_id, lecturer_id FROM ta_requests WHERE id = $1 FOR UPDATE`,
		reqID).Scan(&status, &courseID, &lecturerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("ไม่พบคำขอนี้")
		}
		return err
	}
	if status != "submitted" {
		return errors.New("คำขอนี้ถูกดำเนินการไปแล้ว")
	}

	var termID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT term_id FROM teaching_courses WHERE id = $1`, courseID).Scan(&termID); err != nil {
		return err
	}

	rows, err := tx.Query(ctx, `
		SELECT DISTINCT a.ta_id, u.first_name || ' ' || u.last_name
		FROM ta_request_assignments a JOIN users u ON u.id = a.ta_id
		WHERE a.request_id = $1`, reqID)
	if err != nil {
		return err
	}
	type taRow struct {
		id   uuid.UUID
		name string
	}
	var tas []taRow
	for rows.Next() {
		var t taRow
		if err := rows.Scan(&t.id, &t.name); err != nil {
			rows.Close()
			return err
		}
		tas = append(tas, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, t := range tas {
		// Serialize concurrent approvals touching the same TA so the cap
		// cannot be raced past by two staff members.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, t.id); err != nil {
			return err
		}
		if err := s.checkTAEligibility(ctx, t.id, termID, t.name); err != nil {
			return err
		}
		// Same-course duplicate guard. Two pending requests for the same course
		// could both name this TA; whichever wins first must block the second.
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
			return err
		}
		if dupSameCourse {
			return fmt.Errorf("%s ได้รับอนุมัติเป็น TA ของวิชานี้ในคำขออื่นแล้ว", t.name)
		}
		count, err := s.approvedCourseCount(ctx, t.id, termID, courseID)
		if err != nil {
			return err
		}
		if count >= 3 {
			return fmt.Errorf("%s เป็นผู้ช่วยสอนครบ 3 วิชาในภาคการศึกษานี้แล้ว ไม่สามารถอนุมัติเพิ่มได้", t.name)
		}
	}

	// Re-check teaching-time overlap against courses approved after this
	// request was submitted.
	arows, err := tx.Query(ctx, `
		SELECT a.ta_id, a.section_id, u.first_name || ' ' || u.last_name
		FROM ta_request_assignments a JOIN users u ON u.id = a.ta_id
		WHERE a.request_id = $1`, reqID)
	if err != nil {
		return err
	}
	type asgRow struct {
		taID, secID uuid.UUID
		name        string
	}
	var asgs []asgRow
	for arows.Next() {
		var a asgRow
		if err := arows.Scan(&a.taID, &a.secID, &a.name); err != nil {
			arows.Close()
			return err
		}
		asgs = append(asgs, a)
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return err
	}
	for _, a := range asgs {
		if err := s.checkOwnClassConflict(ctx, a.taID, a.secID, a.name); err != nil {
			return err
		}
		if err := s.checkCrossRequestConflict(ctx, a.taID, a.secID, termID, courseID, reqID, []string{"approved"}, a.name); err != nil {
			return err
		}
	}

	tag, err := tx.Exec(ctx,
		`UPDATE ta_requests SET status='approved', decided_at=NOW(), decided_by=$1, updated_at=NOW()
		 WHERE id=$2 AND status='submitted'`, actor, reqID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("คำขอนี้ถูกดำเนินการไปแล้ว")
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "ta_request.approve", Entity: "ta_request", EntityID: reqID.String()})
	if s.notify != nil {
		code, nameTH := s.courseLabel(ctx, courseID)
		s.notify.Send(ctx, lecturerID, "คำขอ TA ได้รับการอนุมัติ",
			fmt.Sprintf("คำขอผู้ช่วยสอนวิชา %s %s ได้รับการอนุมัติแล้ว", code, nameTH), "/lecturer")
		for _, t := range tas {
			s.notify.Send(ctx, t.id, "คุณได้รับมอบหมายเป็นผู้ช่วยสอน",
				fmt.Sprintf("คุณได้รับอนุมัติเป็นผู้ช่วยสอนวิชา %s %s", code, nameTH), "/ta")
		}
	}
	return nil
}

func (s *TARequestService) Reject(ctx context.Context, actor, reqID uuid.UUID, reason string) error {
	if reason == "" {
		return errors.New("ต้องระบุเหตุผลการปฏิเสธ")
	}
	var lecturerID, courseID uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT lecturer_id, teaching_course_id FROM ta_requests WHERE id = $1`, reqID).Scan(&lecturerID, &courseID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("ไม่พบคำขอนี้")
		}
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE ta_requests SET status='rejected', decided_at=NOW(), decided_by=$1, reject_reason=$2, updated_at=NOW()
		 WHERE id=$3 AND status='submitted'`,
		actor, reason, reqID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("คำขอนี้ถูกดำเนินการไปแล้ว")
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "ta_request.reject", Entity: "ta_request", EntityID: reqID.String(), Note: reason})
	if s.notify != nil {
		code, nameTH := s.courseLabel(ctx, courseID)
		s.notify.Send(ctx, lecturerID, "คำขอ TA ถูกปฏิเสธ",
			fmt.Sprintf("คำขอผู้ช่วยสอนวิชา %s %s ถูกปฏิเสธ: %s", code, nameTH, reason), "/lecturer")
	}
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
	ID               uuid.UUID  `json:"id"`
	Code             string     `json:"course_code"`
	NameTH           string     `json:"course_name"`
	Status           string     `json:"status"`
	SubmittedAt      *time.Time `json:"submitted_at,omitempty"`
	DecidedAt        *time.Time `json:"decided_at,omitempty"`
	RejectReason     *string    `json:"reject_reason,omitempty"`
	TeachingCourseID uuid.UUID  `json:"teaching_course_id"`
	LecturerName     string     `json:"lecturer_name"`
	TACount          int        `json:"ta_count"`
	TermID           uuid.UUID  `json:"term_id"`
	AcademicYear     int        `json:"academic_year"`
	Semester         int        `json:"semester"`
}

const requestSummarySelect = `
	SELECT r.id, fc.code, fc.name_th, r.status::text, r.submitted_at, r.decided_at, r.reject_reason, r.teaching_course_id,
	       u.first_name || ' ' || u.last_name,
	       (SELECT COUNT(DISTINCT a.ta_id) FROM ta_request_assignments a WHERE a.request_id = r.id),
	       tc.term_id, at.academic_year, at.semester
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
		if err := rows.Scan(&t.ID, &t.Code, &t.NameTH, &t.Status, &t.SubmittedAt, &t.DecidedAt, &t.RejectReason, &t.TeachingCourseID, &t.LecturerName, &t.TACount, &t.TermID, &t.AcademicYear, &t.Semester); err != nil {
			return nil, err
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
	err := s.pool.QueryRow(ctx, `
		SELECT r.id, fc.code, fc.name_th, r.status::text, r.submitted_at, r.decided_at, r.reject_reason, r.teaching_course_id,
		       u.first_name || ' ' || u.last_name,
		       (SELECT COUNT(DISTINCT a.ta_id) FROM ta_request_assignments a WHERE a.request_id = r.id),
		       r.reimburse_scope::text
		FROM ta_requests r
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		JOIN faculty_courses fc ON fc.id = tc.faculty_course_id
		JOIN users u ON u.id = r.lecturer_id
		WHERE r.id = $1`, reqID).Scan(
		&d.ID, &d.Code, &d.NameTH, &d.Status, &d.SubmittedAt, &d.DecidedAt, &d.RejectReason,
		&d.TeachingCourseID, &d.LecturerName, &d.TACount, &d.ReimburseScope)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errors.New("ไม่พบคำขอนี้")
		}
		return nil, err
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
		count, err := s.approvedCourseCount(ctx, c.taID, termID, d.TeachingCourseID)
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
		if err := s.checkOwnClassConflict(ctx, c.taID, c.secID, a.TAName); err != nil {
			a.Warnings = append(a.Warnings, "เวลาสอนทับซ้อนกับตารางเรียนของ TA")
		}
		if err := s.checkCrossRequestConflict(ctx, c.taID, c.secID, termID, d.TeachingCourseID, reqID, []string{"approved"}, a.TAName); err != nil {
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
