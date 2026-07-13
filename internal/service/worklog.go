package service

import (
	"context"
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
		return nil, Invalid("ยังไม่สามารถบันทึกภาระงานได้ เนื่องจากคำขอ TA ยังไม่ได้รับการอนุมัติ")
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

// examWindow is a closed date interval [Start, End]. Zero-value → not set,
// which the validator treats as an open (non-blocking) window.
type examWindow struct{ Start, End time.Time }

func (w examWindow) contains(d time.Time) bool {
	if w.Start.IsZero() || w.End.IsZero() {
		return false
	}
	return !d.Before(w.Start) && !d.After(w.End)
}

// courseExamWindows returns the midterm and final exam ranges published on
// the teaching course's term. A zero-value examWindow means the range is not
// set on that term (older records) — the validator will not block on it.
func (s *WorkLogService) courseExamWindows(ctx context.Context, tcID uuid.UUID) (midterm, final examWindow, err error) {
	var ms, me, fs, fe *time.Time
	err = s.pool.QueryRow(ctx, `
		SELECT at.midterm_starts_on, at.midterm_ends_on,
		       at.final_starts_on,   at.final_ends_on
		FROM teaching_courses tc
		JOIN academic_terms  at ON at.id = tc.term_id
		WHERE tc.id = $1`, tcID).Scan(&ms, &me, &fs, &fe)
	if err != nil {
		return
	}
	if ms != nil && me != nil {
		midterm = examWindow{Start: *ms, End: *me}
	}
	if fs != nil && fe != nil {
		final = examWindow{Start: *fs, End: *fe}
	}
	return
}

// validateWorkLogEntry enforces sane hours/time/date on a single manual entry.
// The per-day baht cap and per-track hour cap are enforced separately in Upsert
// (they need DB context — pay_rates + assignment level/track).
func validateWorkLogEntry(w WorkLog, termStart, termEnd time.Time, midterm, final examWindow) error {
	sm, ok1 := parseHM(w.StartTime)
	em, ok2 := parseHM(w.EndTime)
	if !ok1 || !ok2 {
		return Invalid("รูปแบบเวลาไม่ถูกต้อง (ต้องเป็น HH:MM)")
	}
	if sm >= em {
		return Invalid("เวลาสิ้นสุดต้องมากกว่าเวลาเริ่ม")
	}
	if w.Hours <= 0 {
		return Invalid("จำนวนชั่วโมงต้องมากกว่า 0")
	}
	span := float64(em-sm) / 60.0
	if math.Abs(span-w.Hours) > 0.01 {
		return Invalid(fmt.Sprintf("จำนวนชั่วโมง (%.2f) ไม่ตรงกับช่วงเวลา %s–%s (%.2f ชม.)", w.Hours, w.StartTime, w.EndTime, span))
	}
	d, err := time.Parse("2006-01-02", w.WorkDate)
	if err != nil {
		return Invalid("รูปแบบวันที่ไม่ถูกต้อง")
	}
	if d.Before(termStart) || d.After(termEnd) {
		return Invalid("วันที่ทำงานต้องอยู่ในช่วงภาคการศึกษา")
	}
	// Exam-window blackout — faculty publishes the range on the term. TA
	// worklog stops accruing pay during exams even if the section still has
	// scheduled meetings that week.
	if midterm.contains(d) {
		return Invalid("วันที่ทำงานตรงกับช่วงสอบกลางภาค — ลงเวลาไม่ได้")
	}
	if final.contains(d) {
		return Invalid("วันที่ทำงานตรงกับช่วงสอบปลายภาค — ลงเวลาไม่ได้")
	}
	// Q&A rule 2: "อื่นๆ" entries must be tagged with the parent session type
	// so the per-session credit-hour cap can be enforced in Upsert.
	if w.Activity == "other" {
		if w.ParentKind == nil || (*w.ParentKind != "lecture" && *w.ParentKind != "lab") {
			return Invalid("กรุณาระบุประเภทกิจกรรมหลัก (บรรยาย/ปฏิบัติการ) สำหรับกิจกรรมอื่นๆ")
		}
	}
	return nil
}

type WorkLogService struct {
	pool   *pgxpool.Pool
	aud    *audit.Auditor
	budget *BudgetService
	notify *NotifyService
}

type WorkLog struct {
	ID           uuid.UUID `json:"id"`
	AssignmentID uuid.UUID `json:"assignment_id"`
	WorkDate     string    `json:"work_date"`
	StartTime    string    `json:"start_time"`
	EndTime      string    `json:"end_time"`
	Hours        float64   `json:"hours"`
	Activity     string    `json:"activity"`
	// ParentKind ties an activity='other' row to the session type it belongs to
	// (lecture|lab) so the per-session credit-hour cap can be enforced.
	// NULL for lecture/lab/review/makeup rows.
	ParentKind   *string   `json:"parent_kind,omitempty"`
	Room         *string   `json:"room,omitempty"`
	Note         *string   `json:"note,omitempty"`
	Status       string    `json:"status"`
}

// Generate auto-creates a draft set of work logs from a section's schedule between two dates.
// Rules:
//   - Skip weekends (Sat/Sun) unless a makeup row moves it
//   - Skip exam dates
//   - Apply makeup: if original_date falls in a skipped day, move to makeup_date
//   - Cap daily hours per pay_rates (per-track: ป.ตรี regular=7, others=6)
//   - Review entries (kind='review' in lecture_review_dates) are inserted on
//     their ORIGINAL date — no makeup shift (Q&A rule 7).
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
		return nil, Invalid("ไม่สามารถสร้างใหม่ได้ เนื่องจากมีรายการที่ส่งอนุมัติหรืออนุมัติแล้ว")
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
	// Resolve the per-track daily hour cap once (used by both loops below).
	dailyHourCap := s.dailyHourCapFor(ctx, assignmentID)
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

	out := []WorkLog{}
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
			// enforce per-track daily hour cap (Q&A rule 6d)
			if dailyHrs[useDate.Format("2006-01-02")]+sc.hours > dailyHourCap {
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

	// Q&A rule 7: review entries reference the ORIGINAL date and are NOT shifted
	// by the makeup mapping. They live in a separate table (lecture_review_dates).
	reviewRows, err := tx.Query(ctx, `
		SELECT TO_CHAR(review_date,'YYYY-MM-DD'), start_time::text, end_time::text,
		       EXTRACT(EPOCH FROM (end_time - start_time))/3600
		FROM lecture_review_dates WHERE section_id=$1`, sectionID)
	if err != nil {
		return nil, err
	}
	for reviewRows.Next() {
		var dstr, start, end string
		var hrs float64
		if err := reviewRows.Scan(&dstr, &start, &end, &hrs); err != nil {
			reviewRows.Close()
			return nil, err
		}
		if examDates[dstr] {
			continue
		}
		if dailyHrs[dstr]+hrs > dailyHourCap {
			continue
		}
		id := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
			 VALUES ($1,$2,$3::date,$4::time,$5::time,$6,'review','draft')`,
			id, assignmentID, dstr, start, end, hrs); err != nil {
			reviewRows.Close()
			return nil, err
		}
		out = append(out, WorkLog{ID: id, AssignmentID: assignmentID,
			WorkDate: dstr, StartTime: start, EndTime: end,
			Hours: hrs, Activity: "review", Status: "draft"})
		dailyHrs[dstr] += hrs
	}
	reviewRows.Close()

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "worklog.generate", Entity: "assignment", EntityID: assignmentID.String(), After: map[string]int{"count": len(out)}})
	return out, nil
}

// dailyHourCapFor resolves the daily hour cap that applies to an assignment
// based on (level, track). Falls back to 7 if pay_rates is missing.
//   ป.ตรี ปกติ  → ug_regular_daily_hour_cap    (default 7)
//   ป.ตรี พิเศษ → ug_special_daily_hour_cap    (default 6)
//   บัณฑิต ปกติ → grad_regular_daily_hour_cap  (default 6)
//   บัณฑิต พิเศษ → 24 (flat monthly, hours not billed)
func (s *WorkLogService) dailyHourCapFor(ctx context.Context, assignmentID uuid.UUID) float64 {
	var cap float64
	err := s.pool.QueryRow(ctx, `
		WITH latest AS (SELECT * FROM pay_rates ORDER BY effective_from DESC LIMIT 1)
		SELECT CASE
		    WHEN a.level = 'undergrad' AND sec.track = 'regular' THEN pr.ug_regular_daily_hour_cap
		    WHEN a.level = 'undergrad' AND sec.track = 'special' THEN pr.ug_special_daily_hour_cap
		    WHEN a.level IN ('master','phd') AND sec.track = 'regular' THEN pr.grad_regular_daily_hour_cap
		    ELSE 24
		END
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		CROSS JOIN latest pr
		WHERE a.id = $1`, assignmentID).Scan(&cap)
	if err != nil || cap <= 0 {
		return 7
	}
	return cap
}

// enforceDailyBahtCap ensures the TA's total hourly-billed earnings on
// work_date do NOT exceed pay_rates.daily_pay_cap_baht (Q&A rule 6a: 300฿/day).
// Skips grad-special (flat monthly, not billed hourly). Excludes the row
// currently being upserted so re-saves aren't penalised.
func (s *WorkLogService) enforceDailyBahtCap(ctx context.Context, taID uuid.UUID, workDate string,
	excludeRowID uuid.UUID, additionalBaht float64) error {
	var capBaht float64
	if err := s.pool.QueryRow(ctx,
		`SELECT daily_pay_cap_baht FROM pay_rates ORDER BY effective_from DESC LIMIT 1`).Scan(&capBaht); err != nil {
		return nil // no pay_rates → skip cap
	}
	if capBaht <= 0 {
		return nil
	}
	var existing float64
	if err := s.pool.QueryRow(ctx, `
		WITH latest AS (SELECT * FROM pay_rates ORDER BY effective_from DESC LIMIT 1)
		SELECT COALESCE(SUM(wl.hours *
		    CASE
		        WHEN a.level='undergrad' AND sec.track='regular' THEN pr.undergrad_regular
		        WHEN a.level='undergrad' AND sec.track='special' THEN pr.undergrad_special
		        WHEN a.level IN ('master','phd') AND sec.track='regular' THEN pr.graduate_regular_hourly
		        ELSE 0  -- grad special = flat monthly, does not count toward daily cap
		    END), 0)
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		JOIN sections sec ON sec.id = a.section_id
		CROSS JOIN latest pr
		WHERE a.ta_id=$1 AND wl.work_date=$2::date
		  AND wl.status <> 'rejected' AND wl.id <> $3`,
		taID, workDate, excludeRowID).Scan(&existing); err != nil {
		return err
	}
	if existing+additionalBaht > capBaht+0.01 {
		return Invalid(fmt.Sprintf(
			"รวมค่าตอบแทนของวันนี้เกิน %.0f บาท (ปัจจุบัน %.2f บาท ต้องการเพิ่ม %.2f บาท)",
			capBaht, existing, additionalBaht))
	}
	return nil
}

// assignmentRate returns the hourly rate for this assignment, or 0 if the row
// is a flat-monthly grad-special (which doesn't participate in the daily baht cap).
func (s *WorkLogService) assignmentRate(ctx context.Context, assignmentID uuid.UUID) float64 {
	var rate float64
	_ = s.pool.QueryRow(ctx, `
		WITH latest AS (SELECT * FROM pay_rates ORDER BY effective_from DESC LIMIT 1)
		SELECT CASE
		    WHEN a.level='undergrad' AND sec.track='regular' THEN pr.undergrad_regular
		    WHEN a.level='undergrad' AND sec.track='special' THEN pr.undergrad_special
		    WHEN a.level IN ('master','phd') AND sec.track='regular' THEN pr.graduate_regular_hourly
		    ELSE 0
		END
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		CROSS JOIN latest pr
		WHERE a.id = $1`, assignmentID).Scan(&rate)
	return rate
}

// otherActivityCapHours returns the per-session credit-hour cap for an "อื่นๆ"
// (activity='other') work_log, based on its parent_kind. Q&A rule 2.
func (s *WorkLogService) otherActivityCapHours(ctx context.Context, assignmentID uuid.UUID, parentKind string) (float64, error) {
	var capHrs float64
	err := s.pool.QueryRow(ctx, `
		SELECT CASE WHEN $2='lecture' THEN fc.lecture_hrs ELSE fc.lab_hrs END
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		JOIN faculty_courses fc ON fc.id = tc.faculty_course_id
		WHERE a.id = $1`, assignmentID, parentKind).Scan(&capHrs)
	return capHrs, err
}

func (s *WorkLogService) List(ctx context.Context, actor, assignmentID uuid.UUID, privileged bool) ([]WorkLog, error) {
	if err := s.assertCanView(ctx, actor, assignmentID, privileged); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, assignment_id, TO_CHAR(work_date,'YYYY-MM-DD'), start_time::text, end_time::text, hours, activity, parent_kind, room, note, status::text
		 FROM work_logs WHERE assignment_id=$1 ORDER BY work_date, start_time`, assignmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkLog{}
	for rows.Next() {
		var w WorkLog
		if err := rows.Scan(&w.ID, &w.AssignmentID, &w.WorkDate, &w.StartTime, &w.EndTime, &w.Hours, &w.Activity, &w.ParentKind, &w.Room, &w.Note, &w.Status); err != nil {
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
	midterm, final, err := s.courseExamWindows(ctx, ac.TeachingCourseID)
	if err != nil {
		return uuid.Nil, err
	}
	if err := validateWorkLogEntry(w, termStart, termEnd, midterm, final); err != nil {
		return uuid.Nil, err
	}
	// Per-track daily hour cap (Q&A rule 6d). Aggregate across all rows for the
	// same assignment on this date so the total, not just this single entry, obeys the cap.
	dailyHourCap := s.dailyHourCapFor(ctx, w.AssignmentID)
	var dayTotal float64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(hours),0) FROM work_logs
		 WHERE assignment_id=$1 AND work_date=$2::date AND id <> $3`,
		w.AssignmentID, w.WorkDate, w.ID).Scan(&dayTotal); err != nil {
		return uuid.Nil, err
	}
	if dayTotal+w.Hours > dailyHourCap+0.01 {
		return uuid.Nil, Invalid(fmt.Sprintf(
			"รวมชั่วโมงของวันนี้เกิน %.1f ชม. (มีอยู่แล้ว %.2f ชม.)", dailyHourCap, dayTotal))
	}

	// Q&A rule 2 — "อื่นๆ" per session ≤ credit hours of parent kind.
	if w.Activity == "other" && w.ParentKind != nil {
		capHrs, err := s.otherActivityCapHours(ctx, w.AssignmentID, *w.ParentKind)
		if err == nil && capHrs > 0 && w.Hours > capHrs+0.01 {
			kindTH := "บรรยาย"
			if *w.ParentKind == "lab" {
				kindTH = "ปฏิบัติการ"
			}
			return uuid.Nil, Invalid(fmt.Sprintf(
				"กิจกรรมอื่นๆ (คู่กับ%s) ต้องไม่เกิน %.1f ชั่วโมง/ครั้ง", kindTH, capHrs))
		}
	}

	// Q&A rule 6a — 300฿/day cap across every hourly-billed assignment the same
	// TA holds. Grad-special (flat monthly) contributes 0 and is exempt.
	if rate := s.assignmentRate(ctx, w.AssignmentID); rate > 0 {
		if err := s.enforceDailyBahtCap(ctx, ac.TAID, w.WorkDate, w.ID, w.Hours*rate); err != nil {
			return uuid.Nil, err
		}
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
			return uuid.Nil, Invalid("ไม่สามารถเพิ่มรายการใหม่ได้ เนื่องจากมีรายการที่ส่งอนุมัติหรืออนุมัติแล้ว")
		}
		w.ID = uuid.New()
		_, err := s.pool.Exec(ctx,
			`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, parent_kind, room, note, status)
			 VALUES ($1,$2,$3::date,$4::time,$5::time,$6,$7,$8,$9,$10,'draft')`,
			w.ID, w.AssignmentID, w.WorkDate, w.StartTime, w.EndTime, w.Hours, w.Activity, w.ParentKind, w.Room, w.Note)
		if err == nil {
			s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "worklog.create", Entity: "work_log", EntityID: w.ID.String(), After: w})
		}
		return w.ID, err
	}
	// Editable states: draft (in progress) and rejected (TA fixing after a
	// bounce — the update resets it to draft so it can be resubmitted).
	tag, err := s.pool.Exec(ctx,
		`UPDATE work_logs SET work_date=$1::date, start_time=$2::time, end_time=$3::time, hours=$4, activity=$5, parent_kind=$6, room=$7, note=$8, status='draft'
		 WHERE id=$9 AND assignment_id=$10 AND status IN ('draft','rejected')`,
		w.WorkDate, w.StartTime, w.EndTime, w.Hours, w.Activity, w.ParentKind, w.Room, w.Note, w.ID, w.AssignmentID)
	if err != nil {
		return uuid.Nil, err
	}
	if tag.RowsAffected() == 0 {
		return uuid.Nil, Invalid("ไม่พบรายการที่แก้ไขได้ (อาจถูกส่งอนุมัติหรืออนุมัติแล้ว)")
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
		return Invalid("ไม่มีรายการที่ส่งอนุมัติได้")
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
	// must not push the course past its derived cap.
	// Both undergrad (hourly, per-track) AND graduate-regular (hourly per ประกาศ)
	// are billed by hours. Grad-special is flat monthly, so its hours contribute 0.
	var addBaht float64
	if err := tx.QueryRow(ctx, `
		WITH latest AS (SELECT * FROM pay_rates ORDER BY effective_from DESC LIMIT 1)
		SELECT COALESCE(SUM(wl.hours *
			CASE
			    WHEN a.level='undergrad' AND sec.track='regular' THEN pr.undergrad_regular
			    WHEN a.level='undergrad' AND sec.track='special' THEN pr.undergrad_special
			    WHEN a.level IN ('master','phd') AND sec.track='regular' THEN pr.graduate_regular_hourly
			    ELSE 0
			END), 0)
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
			return Conflict(fmt.Sprintf("อนุมัติไม่ได้: จะทำให้เกินงบประมาณของรายวิชา (คงเหลือ %.2f บาท ต้องการ %.2f บาท)", snap.RemainingBaht, addBaht))
		}
	}

	tag, err := tx.Exec(ctx,
		`UPDATE work_logs SET status='approved', approved_at=NOW(), approved_by=$1
		 WHERE assignment_id=$2 AND status='submitted'`, actor, assignmentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return Invalid("ไม่มีรายการที่รออนุมัติ (อาจถูกดำเนินการไปแล้ว)")
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "worklog.approve", Entity: "assignment", EntityID: assignmentID.String()})
	if s.notify != nil {
		s.notify.Send(ctx, ac.TAID,
			"อนุมัติบันทึกเวลา",
			"บันทึกเวลาปฏิบัติงานของคุณได้รับการอนุมัติแล้ว",
			"/ta/worklog")
	}
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

// WorkLogWithTA augments a WorkLog with TA identity + course code, used by the
// staff worklog editor to show every entry across a course in one table.
type WorkLogWithTA struct {
	WorkLog
	TAID       uuid.UUID `json:"ta_id"`
	TAName     string    `json:"ta_name"`
	CourseCode string    `json:"course_code"`
	SectionNo  string    `json:"section_no"`
	Track      string    `json:"track"`
	Level      string    `json:"level"`
}

// StaffListByCourse returns every work_log tied to a teaching course, joined
// with the assignment's TA + section metadata. Staff-only.
func (s *WorkLogService) StaffListByCourse(ctx context.Context, tcID uuid.UUID) ([]WorkLogWithTA, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT wl.id, wl.assignment_id, TO_CHAR(wl.work_date,'YYYY-MM-DD'),
		       wl.start_time::text, wl.end_time::text, wl.hours, wl.activity,
		       wl.parent_kind, wl.room, wl.note, wl.status::text,
		       a.ta_id, u.first_name || ' ' || u.last_name,
		       fc.code, sec.sec_no, sec.track, a.level
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		JOIN users u ON u.id = a.ta_id
		JOIN sections sec ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		JOIN faculty_courses fc ON fc.id = tc.faculty_course_id
		WHERE tc.id = $1
		ORDER BY u.first_name, wl.work_date, wl.start_time`, tcID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkLogWithTA{}
	for rows.Next() {
		var w WorkLogWithTA
		if err := rows.Scan(&w.ID, &w.AssignmentID, &w.WorkDate,
			&w.StartTime, &w.EndTime, &w.Hours, &w.Activity, &w.ParentKind,
			&w.Room, &w.Note, &w.Status,
			&w.TAID, &w.TAName, &w.CourseCode, &w.SectionNo, &w.Track, &w.Level); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// StaffUpsert lets staff edit or add a work_log entry regardless of the row's
// current status. Enforces the same business rules as TA Upsert (hour cap /
// baht cap / other-cap-per-session / parent_kind). Preserves the current
// status (so an approved row stays approved) — the intent is "fix a typo
// before export" not "undo review". Audits before/after and notifies the TA.
func (s *WorkLogService) StaffUpsert(ctx context.Context, staffID uuid.UUID, w WorkLog) (uuid.UUID, error) {
	ac, err := loadAssignmentContext(ctx, s.pool, w.AssignmentID)
	if err != nil {
		return uuid.Nil, err
	}
	termStart, termEnd, err := s.courseDateRange(ctx, ac.TeachingCourseID)
	if err != nil {
		return uuid.Nil, err
	}
	midterm, final, err := s.courseExamWindows(ctx, ac.TeachingCourseID)
	if err != nil {
		return uuid.Nil, err
	}
	if err := validateWorkLogEntry(w, termStart, termEnd, midterm, final); err != nil {
		return uuid.Nil, err
	}
	dailyHourCap := s.dailyHourCapFor(ctx, w.AssignmentID)
	var dayTotal float64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(hours),0) FROM work_logs
		 WHERE assignment_id=$1 AND work_date=$2::date AND id <> $3`,
		w.AssignmentID, w.WorkDate, w.ID).Scan(&dayTotal); err != nil {
		return uuid.Nil, err
	}
	if dayTotal+w.Hours > dailyHourCap+0.01 {
		return uuid.Nil, Invalid(fmt.Sprintf(
			"รวมชั่วโมงของวันนี้เกิน %.1f ชม. (มีอยู่แล้ว %.2f ชม.)", dailyHourCap, dayTotal))
	}
	if w.Activity == "other" && w.ParentKind != nil {
		capHrs, err := s.otherActivityCapHours(ctx, w.AssignmentID, *w.ParentKind)
		if err == nil && capHrs > 0 && w.Hours > capHrs+0.01 {
			kindTH := "บรรยาย"
			if *w.ParentKind == "lab" {
				kindTH = "ปฏิบัติการ"
			}
			return uuid.Nil, Invalid(fmt.Sprintf(
				"กิจกรรมอื่นๆ (คู่กับ%s) ต้องไม่เกิน %.1f ชั่วโมง/ครั้ง", kindTH, capHrs))
		}
	}
	if rate := s.assignmentRate(ctx, w.AssignmentID); rate > 0 {
		if err := s.enforceDailyBahtCap(ctx, ac.TAID, w.WorkDate, w.ID, w.Hours*rate); err != nil {
			return uuid.Nil, err
		}
	}

	if w.ID == uuid.Nil {
		w.ID = uuid.New()
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, parent_kind, room, note, status)
			 VALUES ($1,$2,$3::date,$4::time,$5::time,$6,$7,$8,$9,$10,'draft')`,
			w.ID, w.AssignmentID, w.WorkDate, w.StartTime, w.EndTime, w.Hours, w.Activity, w.ParentKind, w.Room, w.Note); err != nil {
			return uuid.Nil, err
		}
		s.aud.Log(ctx, audit.Entry{ActorID: &staffID, Action: "worklog.staff_edit", Entity: "work_log", EntityID: w.ID.String(), After: w})
	} else {
		// Preserve status so approved rows stay approved; staff cannot silently unlock review state.
		if _, err := s.pool.Exec(ctx,
			`UPDATE work_logs SET work_date=$1::date, start_time=$2::time, end_time=$3::time, hours=$4, activity=$5, parent_kind=$6, room=$7, note=$8
			 WHERE id=$9`,
			w.WorkDate, w.StartTime, w.EndTime, w.Hours, w.Activity, w.ParentKind, w.Room, w.Note, w.ID); err != nil {
			return uuid.Nil, err
		}
		s.aud.Log(ctx, audit.Entry{ActorID: &staffID, Action: "worklog.staff_edit", Entity: "work_log", EntityID: w.ID.String(), After: w})
	}
	if s.notify != nil {
		s.notify.Send(ctx, ac.TAID,
			"เจ้าหน้าที่แก้ไขบันทึกเวลา",
			fmt.Sprintf("เจ้าหน้าที่ปรับข้อมูลบันทึกเวลาวันที่ %s เวลา %s–%s", w.WorkDate, w.StartTime, w.EndTime),
			"/ta/worklog")
	}
	return w.ID, nil
}

// StaffDelete removes a work_log entry. Only draft/rejected can be removed;
// submitted/approved rows must be handled through Reject or a manual DB fix
// so the audit trail stays intact.
func (s *WorkLogService) StaffDelete(ctx context.Context, staffID, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM work_logs WHERE id=$1 AND status IN ('draft','rejected')`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return Invalid("ลบไม่ได้: รายการอาจถูกส่งอนุมัติหรืออนุมัติแล้ว")
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &staffID, Action: "worklog.staff_delete", Entity: "work_log", EntityID: id.String()})
	return nil
}

func (s *WorkLogService) Reject(ctx context.Context, actor, assignmentID uuid.UUID, reason string, privileged bool) error {
	if reason == "" {
		return Invalid("ต้องระบุเหตุผลการปฏิเสธ")
	}
	ac, err := s.assertCanReview(ctx, actor, assignmentID, privileged)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE work_logs SET status='rejected', reject_reason=$1 WHERE assignment_id=$2 AND status='submitted'`,
		reason, assignmentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return Invalid("ไม่มีรายการที่รออนุมัติ (อาจถูกดำเนินการไปแล้ว)")
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "worklog.reject", Entity: "assignment", EntityID: assignmentID.String(), Note: reason})
	if s.notify != nil {
		s.notify.Send(ctx, ac.TAID,
			"บันทึกเวลาถูกปฏิเสธ",
			"บันทึกเวลาของคุณถูกส่งกลับให้แก้ไข: "+reason,
			"/ta/worklog")
	}
	return nil
}
