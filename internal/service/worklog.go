package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

type WorkLogService struct {
	pool *pgxpool.Pool
	aud  *audit.Auditor
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

func (s *WorkLogService) List(ctx context.Context, assignmentID uuid.UUID) ([]WorkLog, error) {
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
	if w.Hours > 7 {
		return uuid.Nil, errors.New("daily work log entry cannot exceed 7 hours")
	}
	if w.ID == uuid.Nil {
		w.ID = uuid.New()
		_, err := s.pool.Exec(ctx,
			`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, room, note, status)
			 VALUES ($1,$2,$3::date,$4::time,$5::time,$6,$7,$8,$9,'draft')`,
			w.ID, w.AssignmentID, w.WorkDate, w.StartTime, w.EndTime, w.Hours, w.Activity, w.Room, w.Note)
		return w.ID, err
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE work_logs SET work_date=$1::date, start_time=$2::time, end_time=$3::time, hours=$4, activity=$5, room=$6, note=$7
		 WHERE id=$8 AND status='draft'`,
		w.WorkDate, w.StartTime, w.EndTime, w.Hours, w.Activity, w.Room, w.Note, w.ID)
	if err == nil {
		s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "worklog.update", Entity: "work_log", EntityID: w.ID.String(), After: w})
	}
	return w.ID, err
}

func (s *WorkLogService) Submit(ctx context.Context, actor, assignmentID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE work_logs SET status='submitted', submitted_at=NOW() WHERE assignment_id=$1 AND status='draft'`, assignmentID)
	if err == nil {
		s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "worklog.submit", Entity: "assignment", EntityID: assignmentID.String()})
	}
	return err
}

func (s *WorkLogService) Approve(ctx context.Context, actor, assignmentID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE work_logs SET status='approved', approved_at=NOW(), approved_by=$1
		 WHERE assignment_id=$2 AND status='submitted'`, actor, assignmentID)
	if err == nil {
		s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "worklog.approve", Entity: "assignment", EntityID: assignmentID.String()})
	}
	return err
}

func (s *WorkLogService) Reject(ctx context.Context, actor, assignmentID uuid.UUID, reason string) error {
	if reason == "" {
		return errors.New("reject reason required")
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE work_logs SET status='rejected', reject_reason=$1 WHERE assignment_id=$2 AND status='submitted'`,
		reason, assignmentID)
	if err == nil {
		s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "worklog.reject", Entity: "assignment", EntityID: assignmentID.String(), Note: reason})
	}
	return err
}
