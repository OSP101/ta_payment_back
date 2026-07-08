package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

type TARequestService struct {
	pool   *pgxpool.Pool
	aud    *audit.Auditor
	budget *BudgetService
}

type CreateTARequestInput struct {
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	ReimburseScope   string    `json:"reimburse_scope"`
	Counts           []struct {
		SectionID       uuid.UUID `json:"section_id"`
		UndergradCount  int       `json:"undergrad_count"`
		GraduateCount   int       `json:"graduate_count"`
	} `json:"counts"`
	Assignments []struct {
		SectionID uuid.UUID `json:"section_id"`
		TAID      uuid.UUID `json:"ta_id"`
		Level     string    `json:"level"`
		Workload  struct {
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
		} `json:"workload"`
	} `json:"assignments"`
}

func (s *TARequestService) Create(ctx context.Context, lecturerID uuid.UUID, in CreateTARequestInput) (uuid.UUID, error) {
	if in.TeachingCourseID == uuid.Nil {
		return uuid.Nil, ErrInvalidInput
	}
	// enforce active window
	var openWindows int
	_ = s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM ta_request_windows w
		JOIN academic_terms t ON t.id = w.term_id
		JOIN teaching_courses tc ON tc.term_id = t.id
		WHERE tc.id = $1 AND w.is_open AND NOW() BETWEEN w.opens_at AND w.closes_at
	`, in.TeachingCourseID).Scan(&openWindows)
	if openWindows == 0 {
		return uuid.Nil, errors.New("no open TA request window for this term")
	}

	// validate: no TA should have >3 accepted requests in this term
	for _, a := range in.Assignments {
		if err := s.checkTAAcceptedLimit(ctx, a.TAID, in.TeachingCourseID); err != nil {
			return uuid.Nil, err
		}
		if err := s.checkScheduleConflict(ctx, a.TAID, a.SectionID); err != nil {
			return uuid.Nil, err
		}
		if a.Level == "master" || a.Level == "phd" {
			tot := a.Workload.HelpTeachHrs + a.Workload.PrepHrs + a.Workload.GradeHrs + a.Workload.OtherHrs
			if tot < 10 || tot > 12 {
				return uuid.Nil, errors.New("graduate workload must sum to 10–12 hours/week")
			}
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)
	rid := uuid.New()
	_, err = tx.Exec(ctx,
		`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status)
		 VALUES ($1,$2,$3,$4::reimburse_scope,'submitted')`,
		rid, in.TeachingCourseID, lecturerID, in.ReimburseScope)
	if err != nil {
		return uuid.Nil, err
	}
	_, err = tx.Exec(ctx, `UPDATE ta_requests SET submitted_at = NOW() WHERE id = $1`, rid)
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
	for _, a := range in.Assignments {
		aid := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
			 VALUES ($1,$2,$3,$4,$5::study_level)`, aid, rid, a.SectionID, a.TAID, a.Level); err != nil {
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

func (s *TARequestService) checkTAAcceptedLimit(ctx context.Context, taID, tcID uuid.UUID) error {
	var termID uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT term_id FROM teaching_courses WHERE id = $1`, tcID).Scan(&termID); err != nil {
		return err
	}
	var count int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT r.teaching_course_id) FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		WHERE a.ta_id = $1 AND tc.term_id = $2 AND r.status IN ('submitted','approved')`,
		taID, termID).Scan(&count)
	if err != nil {
		return err
	}
	if count >= 3 {
		return errors.New("TA already assigned to 3 courses in this term")
	}
	return nil
}

func (s *TARequestService) checkScheduleConflict(ctx context.Context, taID, sectionID uuid.UUID) error {
	var conflicts int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM section_schedules ss
		JOIN sections sec ON sec.id = ss.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		JOIN ta_class_schedules cs ON cs.user_id = $1 AND cs.term_id = tc.term_id
		WHERE ss.section_id = $2
		  AND ss.day_of_week = cs.day_of_week
		  AND ss.start_time < cs.end_time
		  AND ss.end_time > cs.start_time`,
		taID, sectionID).Scan(&conflicts)
	if err != nil {
		return err
	}
	if conflicts > 0 {
		return errors.New("TA's class schedule conflicts with this section")
	}
	return nil
}

// Approve / reject
func (s *TARequestService) Approve(ctx context.Context, actor, reqID uuid.UUID) error {
	// Ensure all assigned TAs have approved profiles
	var missing int
	_ = s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM ta_request_assignments a
		LEFT JOIN ta_profiles p ON p.user_id = a.ta_id
		WHERE a.request_id = $1 AND (p.status IS DISTINCT FROM 'approved')`, reqID).Scan(&missing)
	if missing > 0 {
		return errors.New("cannot approve: some TA profiles are not yet approved")
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE ta_requests SET status='approved', decided_at=NOW(), decided_by=$1 WHERE id=$2`, actor, reqID)
	if err == nil {
		s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "ta_request.approve", Entity: "ta_request", EntityID: reqID.String()})
	}
	return err
}

func (s *TARequestService) Reject(ctx context.Context, actor, reqID uuid.UUID, reason string) error {
	if reason == "" {
		return errors.New("reject reason required")
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE ta_requests SET status='rejected', decided_at=NOW(), decided_by=$1, reject_reason=$2 WHERE id=$3`,
		actor, reason, reqID)
	if err == nil {
		s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "ta_request.reject", Entity: "ta_request", EntityID: reqID.String(), Note: reason})
	}
	return err
}

type TARequestSummary struct {
	ID             uuid.UUID `json:"id"`
	Code           string    `json:"course_code"`
	NameTH         string    `json:"course_name"`
	Status         string    `json:"status"`
	SubmittedAt    *time.Time `json:"submitted_at,omitempty"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
	RejectReason   *string   `json:"reject_reason,omitempty"`
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
}

func (s *TARequestService) ListForLecturer(ctx context.Context, lecturerID uuid.UUID) ([]TARequestSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, fc.code, fc.name_th, r.status::text, r.submitted_at, r.decided_at, r.reject_reason, r.teaching_course_id
		FROM ta_requests r JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		JOIN faculty_courses fc ON fc.id = tc.faculty_course_id
		WHERE r.lecturer_id = $1 ORDER BY COALESCE(r.submitted_at, r.created_at) DESC`, lecturerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TARequestSummary
	for rows.Next() {
		var t TARequestSummary
		if err := rows.Scan(&t.ID, &t.Code, &t.NameTH, &t.Status, &t.SubmittedAt, &t.DecidedAt, &t.RejectReason, &t.TeachingCourseID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (s *TARequestService) ListPending(ctx context.Context) ([]TARequestSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, fc.code, fc.name_th, r.status::text, r.submitted_at, r.decided_at, r.reject_reason, r.teaching_course_id
		FROM ta_requests r JOIN teaching_courses tc ON tc.id=r.teaching_course_id
		JOIN faculty_courses fc ON fc.id=tc.faculty_course_id
		WHERE r.status = 'submitted' ORDER BY r.submitted_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TARequestSummary
	for rows.Next() {
		var t TARequestSummary
		if err := rows.Scan(&t.ID, &t.Code, &t.NameTH, &t.Status, &t.SubmittedAt, &t.DecidedAt, &t.RejectReason, &t.TeachingCourseID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
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
	var out []Window
	for rows.Next() {
		var w Window
		if err := rows.Scan(&w.ID, &w.TermID, &w.OpensAt, &w.ClosesAt, &w.IsOpen, &w.Note); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}
