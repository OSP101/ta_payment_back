package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

// parseHM parses a "HH:MM" or "HH:MM:SS" clock string into minutes-from-midnight.
func parseHM(s string) (int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// assertTAOwnsAssignment ensures the actor is the TA who owns the assignment and
// that the parent request is approved (work may only be logged against approved
// TA appointments). Returns the resolved context for callers that need dates.
func (s *WorkLogService) assertTAOwnsAssignment(ctx context.Context, actor, assignmentID uuid.UUID) (*assignmentContext, error) {
	ac, err := loadAssignmentContext(ctx, s.pool, assignmentID)
	if err != nil {
		return nil, err
	}
	if ac.TAID != actor {
		return nil, ErrForbidden
	}
	if ac.RequestStatus != "approved" {
		return nil, errors.New("ยังไม่สามารถบันทึกภาระงานได้ เนื่องจากคำขอ TA ยังไม่ได้รับการอนุมัติ")
	}
	return ac, nil
}

// assertCanReview ensures the actor may approve/reject work logs for the
// assignment: either a privileged (staff/admin) user, or a lecturer who teaches
// the parent course.
func (s *WorkLogService) assertCanReview(ctx context.Context, actor, assignmentID uuid.UUID, privileged bool) (*assignmentContext, error) {
	ac, err := loadAssignmentContext(ctx, s.pool, assignmentID)
	if err != nil {
		return nil, err
	}
	if privileged {
		return ac, nil
	}
	owns, err := lecturerOwnsCourse(ctx, s.pool, actor, ac.TeachingCourseID)
	if err != nil {
		return nil, err
	}
	if !owns {
		return nil, ErrForbidden
	}
	return ac, nil
}

// assertCanView ensures the actor may read the assignment's work logs: the
// owning TA, a lecturer who teaches the course, or a privileged user.
func (s *WorkLogService) assertCanView(ctx context.Context, actor, assignmentID uuid.UUID, privileged bool) error {
	ac, err := loadAssignmentContext(ctx, s.pool, assignmentID)
	if err != nil {
		return err
	}
	if privileged || ac.TAID == actor {
		return nil
	}
	owns, err := lecturerOwnsCourse(ctx, s.pool, actor, ac.TeachingCourseID)
	if err != nil {
		return err
	}
	if !owns {
		return ErrForbidden
	}
	return nil
}

// courseDateRange returns the teaching course's start/end dates for validating
// that a work_date falls within the term.
func (s *WorkLogService) courseDateRange(ctx context.Context, tcID uuid.UUID) (start, end time.Time, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT COALESCE(starts_on, '0001-01-01'::date), COALESCE(ends_on, '9999-12-31'::date)
		 FROM teaching_courses WHERE id = $1`, tcID).Scan(&start, &end)
	return
}

// validateWorkLogEntry enforces sane hours/time/date on a single manual entry.
func validateWorkLogEntry(w WorkLog, termStart, termEnd time.Time) error {
	sm, ok1 := parseHM(w.StartTime)
	em, ok2 := parseHM(w.EndTime)
	if !ok1 || !ok2 {
		return errors.New("รูปแบบเวลาไม่ถูกต้อง (ต้องเป็น HH:MM)")
	}
	if sm >= em {
		return errors.New("เวลาสิ้นสุดต้องมากกว่าเวลาเริ่ม")
	}
	if w.Hours <= 0 {
		return errors.New("จำนวนชั่วโมงต้องมากกว่า 0")
	}
	if w.Hours > 7 {
		return errors.New("บันทึกภาระงานต่อวันต้องไม่เกิน 7 ชั่วโมง")
	}
	span := float64(em-sm) / 60.0
	if math.Abs(span-w.Hours) > 0.01 {
		return fmt.Errorf("จำนวนชั่วโมง (%.2f) ไม่ตรงกับช่วงเวลา %s–%s (%.2f ชม.)", w.Hours, w.StartTime, w.EndTime, span)
	}
	d, err := time.Parse("2006-01-02", w.WorkDate)
	if err != nil {
		return errors.New("รูปแบบวันที่ไม่ถูกต้อง")
	}
	if d.Before(termStart) || d.After(termEnd) {
		return errors.New("วันที่ทำงานต้องอยู่ในช่วงภาคการศึกษา")
	}
	return nil
}

type WorkLogService struct {
	pool   *pgxpool.Pool
	aud    *audit.Auditor
	budget *BudgetService
}

type WorkLog struct {
	ID           uuid.UUID `json:"id"`
	AssignmentID uuid.UUID `json:"assignment_id"`
	WorkDate     string    `json:"work_date"`
	StartTime    string    `json:"start_time"`
	EndTime      string    `json:"end_time"`
	Hours        float64   `json:"hours"`
	Activity     string    `json:"activity"`
	Room         *string   `json:"room,omitempty"`
	Note         *string   `json:"note,omitempty"`
	Status       string    `json:"status"`
}

// Generate auto-creates a draft set of work logs from a section's schedule between two dates.
// Rules:
//   - Skip weekends (Sat/Sun) unless a makeup row moves it
//   - Skip exam dates
//   - Apply makeup: if original_date falls in a skipped day, move to makeup_date
//   - Cap daily hours at 7 per TA
func (s *WorkLogService) Generate(ctx context.Context, actor, assignmentID uuid.UUID) ([]WorkLog, error) {
	if _, err := s.assertTAOwnsAssignment(ctx, actor, assignmentID); err != nil {
		return nil, err
	}
	// Refuse to regenerate once anything has been submitted/approved, otherwise
	// the wipe-and-recreate below would silently drop reviewed rows.
	var locked int
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM work_logs WHERE assignment_id=$1 AND status IN ('submitted','approved')`,
		assignmentID).Scan(&locked); err != nil {
		return nil, err
	}
	if locked > 0 {
		return nil, errors.New("ไม่สามารถสร้างใหม่ได้ เนื่องจากมีรายการที่ส่งอนุมัติหรืออนุมัติแล้ว")
	}
	var sectionID uuid.UUID
	var startsOn, endsOn time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT a.section_id, COALESCE(tc.starts_on, CURRENT_DATE), COALESCE(tc.ends_on, CURRENT_DATE)
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		WHERE a.id = $1`, assignmentID).Scan(&sectionID, &startsOn, &endsOn)
	if err != nil {
		return nil, err
	}
	// Collect exam dates & makeup mapping
	examDates := map[string]bool{}
	if rows, err := s.pool.Query(ctx, `SELECT exam_date FROM exam_schedules WHERE section_id=$1`, sectionID); err == nil {
		for rows.Next() {
			var d time.Time
			if err := rows.Scan(&d); err == nil {
				examDates[d.Format("2006-01-02")] = true
			}
		}
		rows.Close()
	}
	makeup := map[string]struct {
		date  time.Time
		start *time.Time
		end   *time.Time
	}{}
	if rows, err := s.pool.Query(ctx, `SELECT original_date, makeup_date FROM makeup_schedules WHERE section_id=$1`, sectionID); err == nil {
		for rows.Next() {
			var orig, mk time.Time
			if err := rows.Scan(&orig, &mk); err == nil {
				makeup[orig.Format("2006-01-02")] = struct {
					date  time.Time
					start *time.Time
					end   *time.Time
				}{date: mk}
			}
		}
		rows.Close()
	}

	// Load schedules
	type sch struct {
		kind      string
		day       int
		start     string
		end       string
		hours     float64
	}
	var schs []sch
	rows, err := s.pool.Query(ctx,
		`SELECT kind, day_of_week, start_time::text, end_time::text,
		 EXTRACT(EPOCH FROM (end_time - start_time))/3600 FROM section_schedules WHERE section_id=$1`, sectionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s sch
		if err := rows.Scan(&s.kind, &s.day, &s.start, &s.end, &s.hours); err == nil {
			schs = append(schs, s)
		}
	}
	rows.Close()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	// wipe existing draft logs for this assignment
	if _, err := tx.Exec(ctx,
		`DELETE FROM work_logs WHERE assignment_id=$1 AND status='draft'`, assignmentID); err != nil {
		return nil, err
	}

	var out []WorkLog
	dailyHrs := map[string]float64{}
	for d := startsOn; !d.After(endsOn); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		useDate := d
		if m, ok := makeup[key]; ok {
			useDate = m.date
		}
		if examDates[useDate.Format("2006-01-02")] {
			continue
		}
		weekday := int(useDate.Weekday())
		if weekday == 0 || weekday == 6 {
			// skip weekend (unless make-up moved us out)
			continue
		}
		for _, sc := range schs {
			if sc.day != weekday {
				continue
			}
			// enforce 7h/day cap
			if dailyHrs[useDate.Format("2006-01-02")]+sc.hours > 7 {
				continue
			}
			id := uuid.New()
			if _, err := tx.Exec(ctx,
				`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
				 VALUES ($1,$2,$3::date,$4::time,$5::time,$6,$7,'draft')`,
				id, assignmentID, useDate.Format("2006-01-02"), sc.start, sc.end, sc.hours, sc.kind); err != nil {
				return nil, err
			}
			out = append(out, WorkLog{ID: id, AssignmentID: assignmentID,
				WorkDate: useDate.Format("2006-01-02"), StartTime: sc.start, EndTime: sc.end,
				Hours: sc.hours, Activity: sc.kind, Status: "draft"})
			dailyHrs[useDate.Format("2006-01-02")] += sc.hours
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "worklog.generate", Entity: "assignment", EntityID: assignmentID.String(), After: map[string]int{"count": len(out)}})
	return out, nil
}

func (s *WorkLogService) List(ctx context.Context, actor, assignmentID uuid.UUID, privileged bool) ([]WorkLog, error) {
	if err := s.assertCanView(ctx, actor, assignmentID, privileged); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, assignment_id, TO_CHAR(work_date,'YYYY-MM-DD'), start_time::text, end_time::text, hours, activity, room, note, status::text
		 FROM work_logs WHERE assignment_id=$1 ORDER BY work_date, start_time`, assignmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkLog
	for rows.Next() {
		var w WorkLog
		if err := rows.Scan(&w.ID, &w.AssignmentID, &w.WorkDate, &w.StartTime, &w.EndTime, &w.Hours, &w.Activity, &w.Room, &w.Note, &w.Status); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

func (s *WorkLogService) Upsert(ctx context.Context, actor uuid.UUID, w WorkLog) (uuid.UUID, error) {
	ac, err := s.assertTAOwnsAssignment(ctx, actor, w.AssignmentID)
	if err != nil {
		return uuid.Nil, err
	}
	termStart, termEnd, err := s.courseDateRange(ctx, ac.TeachingCourseID)
	if err != nil {
		return uuid.Nil, err
	}
	if err := validateWorkLogEntry(w, termStart, termEnd); err != nil {
		return uuid.Nil, err
	}
	// Enforce the 7h/day cap across all of the assignment's rows for that date,
	// not just the single entry.
	var dayTotal float64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(hours),0) FROM work_logs
		 WHERE assignment_id=$1 AND work_date=$2::date AND id <> $3`,
		w.AssignmentID, w.WorkDate, w.ID).Scan(&dayTotal); err != nil {
		return uuid.Nil, err
	}
	if dayTotal+w.Hours > 7 {
		return uuid.Nil, fmt.Errorf("รวมชั่วโมงของวันนี้เกิน 7 ชม. (มีอยู่แล้ว %.2f ชม.)", dayTotal)
	}

	if w.ID == uuid.Nil {
		// Block adding new rows once the assignment has entered review, so hours
		// cannot be inflated after approval.
		var reviewed int
		if err := s.pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM work_logs WHERE assignment_id=$1 AND status IN ('submitted','approved')`,
			w.AssignmentID).Scan(&reviewed); err != nil {
			return uuid.Nil, err
		}
		if reviewed > 0 {
			return uuid.Nil, errors.New("ไม่สามารถเพิ่มรายการใหม่ได้ เนื่องจากมีรายการที่ส่งอนุมัติหรืออนุมัติแล้ว")
		}
		w.ID = uuid.New()
		_, err := s.pool.Exec(ctx,
			`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, room, note, status)
			 VALUES ($1,$2,$3::date,$4::time,$5::time,$6,$7,$8,$9,'draft')`,
			w.ID, w.AssignmentID, w.WorkDate, w.StartTime, w.EndTime, w.Hours, w.Activity, w.Room, w.Note)
		if err == nil {
			s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "worklog.create", Entity: "work_log", EntityID: w.ID.String(), After: w})
		}
		return w.ID, err
	}
	// Editable states: draft (in progress) and rejected (TA fixing after a
	// bounce — the update resets it to draft so it can be resubmitted).
	tag, err := s.pool.Exec(ctx,
		`UPDATE work_logs SET work_date=$1::date, start_time=$2::time, end_time=$3::time, hours=$4, activity=$5, room=$6, note=$7, status='draft'
		 WHERE id=$8 AND assignment_id=$9 AND status IN ('draft','rejected')`,
		w.WorkDate, w.StartTime, w.EndTime, w.Hours, w.Activity, w.Room, w.Note, w.ID, w.AssignmentID)
	if err != nil {
		return uuid.Nil, err
	}
	if tag.RowsAffected() == 0 {
		return uuid.Nil, errors.New("ไม่พบรายการที่แก้ไขได้ (อาจถูกส่งอนุมัติหรืออนุมัติแล้ว)")
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "worklog.update", Entity: "work_log", EntityID: w.ID.String(), After: w})
	return w.ID, nil
}

func (s *WorkLogService) Submit(ctx context.Context, actor, assignmentID uuid.UUID) error {
	if _, err := s.assertTAOwnsAssignment(ctx, actor, assignmentID); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE work_logs SET status='submitted', submitted_at=NOW() WHERE assignment_id=$1 AND status='draft'`, assignmentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("ไม่มีรายการที่ส่งอนุมัติได้")
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "worklog.submit", Entity: "assignment", EntityID: assignmentID.String()})
	return nil
}

func (s *WorkLogService) Approve(ctx context.Context, actor, assignmentID uuid.UUID, privileged bool) error {
	ac, err := s.assertCanReview(ctx, actor, assignmentID, privileged)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Serialize concurrent approvals on the same course so two reviewers cannot
	// both push the course over budget.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 42))`, ac.TeachingCourseID); err != nil {
		return err
	}

	// Budget guard (C3): the additional pay these newly-approved hours represent
	// must not push the course past its derived cap. Undergrad hours are paid by
	// the hourly rate for the section's track.
	var addBaht float64
	if err := tx.QueryRow(ctx, `
		WITH latest AS (SELECT * FROM pay_rates ORDER BY effective_from DESC LIMIT 1)
		SELECT COALESCE(SUM(wl.hours *
			CASE WHEN sec.track = 'regular' THEN pr.undergrad_regular ELSE pr.undergrad_special END), 0)
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		JOIN sections sec ON sec.id = a.section_id
		CROSS JOIN latest pr
		WHERE wl.assignment_id = $1 AND wl.status = 'submitted'`, assignmentID).Scan(&addBaht); err != nil {
		return err
	}
	if addBaht > 0 {
		snap, err := s.budget.Compute(ctx, ac.TeachingCourseID)
		if err != nil {
			return err
		}
		if snap.PerCourseMaxBaht > 0 && snap.UsedBaht+addBaht > snap.PerCourseMaxBaht+0.01 {
			return fmt.Errorf("อนุมัติไม่ได้: จะทำให้เกินงบประมาณของรายวิชา (คงเหลือ %.2f บาท ต้องการ %.2f บาท)", snap.RemainingBaht, addBaht)
		}
	}

	tag, err := tx.Exec(ctx,
		`UPDATE work_logs SET status='approved', approved_at=NOW(), approved_by=$1
		 WHERE assignment_id=$2 AND status='submitted'`, actor, assignmentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("ไม่มีรายการที่รออนุมัติ (อาจถูกดำเนินการไปแล้ว)")
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "worklog.approve", Entity: "assignment", EntityID: assignmentID.String()})
	return nil
}

// PendingReport is one row on the lecturer's "อนุมัติรายงาน TA" list — an
// assignment that currently has at least one submitted work-log row awaiting
// review.
type PendingReport struct {
	ID               uuid.UUID `json:"id"`
	TAName           string    `json:"ta_name"`
	CourseCode       string    `json:"course_code"`
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	TotalHours       float64   `json:"total_hours"`
	PeriodLabel      string    `json:"period_label,omitempty"`
}

// ListPending returns assignments with submitted (awaiting-review) work-logs.
// Lecturers see only assignments in courses they teach; privileged users
// (admin/staff) see all.
func (s *WorkLogService) ListPending(ctx context.Context, actor uuid.UUID, privileged bool) ([]PendingReport, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id,
		       u.first_name || ' ' || u.last_name,
		       fc.code,
		       tc.id,
		       COALESCE(SUM(wl.hours), 0),
		       MIN(wl.work_date),
		       MAX(wl.work_date),
		       MIN(wl.submitted_at)
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		JOIN faculty_courses fc ON fc.id = tc.faculty_course_id
		JOIN users u ON u.id = a.ta_id
		JOIN work_logs wl ON wl.assignment_id = a.id AND wl.status = 'submitted'
		WHERE $1 = TRUE OR EXISTS (
		    SELECT 1 FROM teaching_lecturers tl
		    WHERE tl.teaching_course_id = tc.id AND tl.lecturer_id = $2
		)
		GROUP BY a.id, u.first_name, u.last_name, fc.code, tc.id
		ORDER BY MIN(wl.submitted_at) ASC NULLS LAST, fc.code`,
		privileged, actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]PendingReport, 0)
	for rows.Next() {
		var (
			p           PendingReport
			minD, maxD  time.Time
			submittedAt *time.Time
		)
		if err := rows.Scan(&p.ID, &p.TAName, &p.CourseCode, &p.TeachingCourseID,
			&p.TotalHours, &minD, &maxD, &submittedAt); err != nil {
			return nil, err
		}
		if minD.Equal(maxD) {
			p.PeriodLabel = minD.Format("2006-01-02")
		} else {
			p.PeriodLabel = minD.Format("2006-01-02") + " – " + maxD.Format("2006-01-02")
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *WorkLogService) Reject(ctx context.Context, actor, assignmentID uuid.UUID, reason string, privileged bool) error {
	if reason == "" {
		return errors.New("ต้องระบุเหตุผลการปฏิเสธ")
	}
	if _, err := s.assertCanReview(ctx, actor, assignmentID, privileged); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE work_logs SET status='rejected', reject_reason=$1 WHERE assignment_id=$2 AND status='submitted'`,
		reason, assignmentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("ไม่มีรายการที่รออนุมัติ (อาจถูกดำเนินการไปแล้ว)")
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "worklog.reject", Entity: "assignment", EntityID: assignmentID.String(), Note: reason})
	return nil
}
