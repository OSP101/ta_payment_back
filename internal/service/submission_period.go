package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

// SubmissionPeriodService owns the monthly submission_periods table introduced
// by migration 0019. A period represents "the deadline for a TA to submit
// their signed worklog for month X of term Y" — created by staff, tracked
// per (TA × teaching_course), and reminded by the scheduler daemon.
type SubmissionPeriodService struct {
	pool   *pgxpool.Pool
	aud    *audit.Auditor
	notify *NotifyService
}

type SubmissionPeriod struct {
	ID               uuid.UUID `json:"id"`
	TermID           uuid.UUID `json:"term_id"`
	YearMonth        string    `json:"year_month"` // "2569-06"
	DueDate          string    `json:"due_date"`   // "2569-07-31"
	Label            string    `json:"label"`      // "มิถุนายน 2569"
	RemindDaysBefore int       `json:"remind_days_before"`
	IsClosed         bool      `json:"is_closed"`
}

// SubmissionPeriodStatus is one (TA, teaching_course, period) row from the
// TA-facing reminders page.
type SubmissionPeriodStatus struct {
	PeriodID         uuid.UUID `json:"period_id"`
	Label            string    `json:"label"`
	DueDate          string    `json:"due_date"`
	IsClosed         bool      `json:"is_closed"`
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	CourseCode       string    `json:"course_code"`
	CourseNameTH     string    `json:"course_name_th"`
	Status           string    `json:"status"`
	TASignedAt       *string   `json:"ta_signed_at,omitempty"`
	LecturerSignedAt *string   `json:"lecturer_signed_at,omitempty"`
	SubmittedAt      *string   `json:"submitted_at,omitempty"`
}

// List returns all periods for a term (or every term when termID is Nil).
func (s *SubmissionPeriodService) List(ctx context.Context, termID uuid.UUID) ([]SubmissionPeriod, error) {
	q := `SELECT id, term_id, year_month, TO_CHAR(due_date,'YYYY-MM-DD'), label, remind_days_before, is_closed
	      FROM submission_periods`
	args := []any{}
	if termID != uuid.Nil {
		q += " WHERE term_id = $1"
		args = append(args, termID)
	}
	q += " ORDER BY due_date"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubmissionPeriod{}
	for rows.Next() {
		var p SubmissionPeriod
		if err := rows.Scan(&p.ID, &p.TermID, &p.YearMonth, &p.DueDate, &p.Label,
			&p.RemindDaysBefore, &p.IsClosed); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Upsert creates or updates a single submission period. Only staff/admin.
func (s *SubmissionPeriodService) Upsert(ctx context.Context, actor uuid.UUID, in SubmissionPeriod) (*SubmissionPeriod, error) {
	if in.TermID == uuid.Nil {
		return nil, Invalid("กรุณาระบุภาคเรียน")
	}
	if in.YearMonth == "" || in.DueDate == "" || in.Label == "" {
		return nil, Invalid("กรุณาระบุเดือน, กำหนดส่ง และป้ายกำกับ")
	}
	if in.RemindDaysBefore <= 0 {
		in.RemindDaysBefore = 3
	}
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
		_, err := s.pool.Exec(ctx, `
			INSERT INTO submission_periods (id, term_id, year_month, due_date, label, remind_days_before, is_closed)
			VALUES ($1,$2,$3,$4::date,$5,$6,$7)
			ON CONFLICT (term_id, year_month) DO UPDATE
			SET due_date=EXCLUDED.due_date, label=EXCLUDED.label,
			    remind_days_before=EXCLUDED.remind_days_before, is_closed=EXCLUDED.is_closed`,
			in.ID, in.TermID, in.YearMonth, in.DueDate, in.Label, in.RemindDaysBefore, in.IsClosed)
		if err != nil {
			return nil, err
		}
	} else {
		_, err := s.pool.Exec(ctx, `
			UPDATE submission_periods
			SET year_month=$2, due_date=$3::date, label=$4,
			    remind_days_before=$5, is_closed=$6
			WHERE id=$1`,
			in.ID, in.YearMonth, in.DueDate, in.Label, in.RemindDaysBefore, in.IsClosed)
		if err != nil {
			return nil, err
		}
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "submission_period.upsert",
		Entity: "submission_period", EntityID: in.ID.String(), After: in})
	return &in, nil
}

// Delete removes a period (cascades status rows). Only staff/admin.
func (s *SubmissionPeriodService) Delete(ctx context.Context, actor, id uuid.UUID) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM submission_periods WHERE id=$1`, id); err != nil {
		return err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "submission_period.delete",
		Entity: "submission_period", EntityID: id.String()})
	return nil
}

// BulkCreateForTerm auto-generates 5 periods for a term (มิ.ย. → ต.ค.) with
// due-dates matching the KKU 2569 rulebook (first three months share a due
// date, months 4/5 have month+5-day due dates). Staff can then edit them.
// Idempotent — ON CONFLICT preserves existing rows.
func (s *SubmissionPeriodService) BulkCreateForTerm(ctx context.Context, actor, termID uuid.UUID) ([]SubmissionPeriod, error) {
	// Fetch term for its academic_year (Buddhist) and semester.
	var year, semester int
	if err := s.pool.QueryRow(ctx,
		`SELECT academic_year, semester FROM academic_terms WHERE id=$1`, termID).
		Scan(&year, &semester); err != nil {
		return nil, err
	}

	// First semester runs มิ.ย.–ต.ค. Second runs พ.ย.–มี.ค. We only ship the
	// first-semester template here (Q&A rule 2569); staff can add manually.
	type tpl struct {
		month int
		due   string // MM-DD in Buddhist year (year set below)
		label string
	}
	var templates []tpl
	if semester == 1 {
		templates = []tpl{
			{6, "07-31", "มิถุนายน"},
			{7, "07-31", "กรกฎาคม"},
			{8, "07-31", "สิงหาคม"}, // matches ประกาศ: 3 months share 31 ก.ค. due
			{9, "10-05", "กันยายน"},
			{10, "11-05", "ตุลาคม"},
		}
	} else {
		// Second-semester template (skeleton — staff will adjust dates).
		templates = []tpl{
			{11, "12-05", "พฤศจิกายน"},
			{12, "01-05", "ธันวาคม"},
			{1, "02-05", "มกราคม"},
			{2, "03-05", "กุมภาพันธ์"},
			{3, "04-05", "มีนาคม"},
		}
	}

	out := []SubmissionPeriod{}
	for _, t := range templates {
		// year_month is the SUBMISSION month (Buddhist); due_date uses the same
		// Buddhist year but is a Gregorian DATE — Postgres will interpret e.g.
		// '2569-07-31' as year 2569 A.D. (which pgxpool converts fine so long
		// as we don't rely on absolute time arithmetic). To keep behaviour
		// predictable across environments we convert Buddhist → Gregorian.
		gregYear := year - 543
		// Handle Dec→Jan wrap for the second semester template.
		dueYear := gregYear
		if semester == 2 && (t.month == 1 || t.month == 2 || t.month == 3) {
			dueYear++
		}
		ym := fmt.Sprintf("%d-%02d", year, t.month)
		due := fmt.Sprintf("%d-%s", dueYear, t.due)
		label := fmt.Sprintf("%s %d", t.label, year)
		p := SubmissionPeriod{
			ID: uuid.New(), TermID: termID, YearMonth: ym,
			DueDate: due, Label: label, RemindDaysBefore: 3, IsClosed: false,
		}
		_, err := s.pool.Exec(ctx, `
			INSERT INTO submission_periods (id, term_id, year_month, due_date, label, remind_days_before, is_closed)
			VALUES ($1,$2,$3,$4::date,$5,$6,$7)
			ON CONFLICT (term_id, year_month) DO NOTHING`,
			p.ID, p.TermID, p.YearMonth, p.DueDate, p.Label, p.RemindDaysBefore, p.IsClosed)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "submission_period.bulk_create",
		Entity: "term", EntityID: termID.String(), After: map[string]int{"count": len(out)}})
	return out, nil
}

// PendingByTA lists every (period × course) row a TA still owes. Rows are
// synthesised via LEFT JOIN so we don't need to pre-populate
// submission_period_status when a period is created — status rows are lazily
// created only when the TA acts on them.
func (s *SubmissionPeriodService) PendingByTA(ctx context.Context, taID uuid.UUID) ([]SubmissionPeriodStatus, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sp.id, sp.label, TO_CHAR(sp.due_date,'YYYY-MM-DD'), sp.is_closed,
		       tc.id, fc.code, fc.name_th,
		       COALESCE(st.status, 'pending'),
		       TO_CHAR(st.ta_signed_at,       'YYYY-MM-DD"T"HH24:MI:SSTZ'),
		       TO_CHAR(st.lecturer_signed_at, 'YYYY-MM-DD"T"HH24:MI:SSTZ'),
		       TO_CHAR(st.submitted_at,       'YYYY-MM-DD"T"HH24:MI:SSTZ')
		FROM submission_periods sp
		JOIN teaching_courses tc ON tc.term_id = sp.term_id
		JOIN faculty_courses fc ON fc.id = tc.faculty_course_id
		JOIN ta_request_assignments a ON a.section_id IN
		    (SELECT id FROM sections WHERE teaching_course_id = tc.id)
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		LEFT JOIN submission_period_status st
		    ON st.submission_period_id = sp.id
		   AND st.ta_id = a.ta_id
		   AND st.teaching_course_id = tc.id
		WHERE a.ta_id = $1
		GROUP BY sp.id, sp.label, sp.due_date, sp.is_closed,
		         tc.id, fc.code, fc.name_th,
		         st.status, st.ta_signed_at, st.lecturer_signed_at, st.submitted_at
		ORDER BY sp.due_date, fc.code`, taID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubmissionPeriodStatus{}
	for rows.Next() {
		var s SubmissionPeriodStatus
		if err := rows.Scan(&s.PeriodID, &s.Label, &s.DueDate, &s.IsClosed,
			&s.TeachingCourseID, &s.CourseCode, &s.CourseNameTH,
			&s.Status, &s.TASignedAt, &s.LecturerSignedAt, &s.SubmittedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// MarkTASigned upserts the status row and flips it to ta_signed. Called from
// the TA UI when they click "ยืนยันบันทึกเวลาประจำเดือน".
func (s *SubmissionPeriodService) MarkTASigned(ctx context.Context, actor, periodID, tcID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO submission_period_status
		    (id, submission_period_id, ta_id, teaching_course_id, status, ta_signed_at)
		VALUES ($1, $2, $3, $4, 'ta_signed', now())
		ON CONFLICT (submission_period_id, ta_id, teaching_course_id) DO UPDATE
		SET status = CASE
		    WHEN submission_period_status.status IN ('lecturer_signed','submitted')
		    THEN submission_period_status.status
		    ELSE 'ta_signed'
		END,
		    ta_signed_at = now()`,
		uuid.New(), periodID, actor, tcID)
	if err != nil {
		return err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "submission_period.ta_signed",
		Entity: "submission_period_status", EntityID: periodID.String() + "/" + tcID.String()})
	return nil
}

// MarkLecturerSigned lets a lecturer confirm on behalf of a TA — advances the
// state row to lecturer_signed.
func (s *SubmissionPeriodService) MarkLecturerSigned(ctx context.Context, actor, periodID, taID, tcID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO submission_period_status
		    (id, submission_period_id, ta_id, teaching_course_id, status, lecturer_signed_at)
		VALUES ($1, $2, $3, $4, 'lecturer_signed', now())
		ON CONFLICT (submission_period_id, ta_id, teaching_course_id) DO UPDATE
		SET status = 'lecturer_signed', lecturer_signed_at = now()`,
		uuid.New(), periodID, taID, tcID)
	if err != nil {
		return err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "submission_period.lecturer_signed",
		Entity: "submission_period_status", EntityID: periodID.String() + "/" + taID.String()})
	return nil
}

// MarkSubmitted flips a status row to submitted (used when staff finalises
// the batch after export).
func (s *SubmissionPeriodService) MarkSubmitted(ctx context.Context, actor, periodID, taID, tcID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO submission_period_status
		    (id, submission_period_id, ta_id, teaching_course_id, status, submitted_at)
		VALUES ($1, $2, $3, $4, 'submitted', now())
		ON CONFLICT (submission_period_id, ta_id, teaching_course_id) DO UPDATE
		SET status = 'submitted', submitted_at = now()`,
		uuid.New(), periodID, taID, tcID)
	if err != nil {
		return err
	}
	return nil
}

// SweepReminders is called from the scheduler daemon every hour: for each
// period whose due window is currently open, enumerate every pending
// (TA × course) cell and fire an in-app + email notification. Rate-limited to
// once per 24h per cell via submission_period_status.last_reminded_at.
func (s *SubmissionPeriodService) SweepReminders(ctx context.Context) (int, error) {
	if s.notify == nil {
		return 0, nil
	}
	// One row per pending assignment whose period is currently in-window.
	rows, err := s.pool.Query(ctx, `
		WITH pending AS (
			SELECT DISTINCT sp.id AS period_id, sp.label, sp.due_date,
			                a.ta_id, tc.id AS tc_id, fc.code, fc.name_th
			FROM submission_periods sp
			JOIN teaching_courses tc ON tc.term_id = sp.term_id
			JOIN faculty_courses fc ON fc.id = tc.faculty_course_id
			JOIN ta_request_assignments a
			    ON a.section_id IN (SELECT id FROM sections WHERE teaching_course_id = tc.id)
			JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
			LEFT JOIN submission_period_status st
			    ON st.submission_period_id = sp.id
			   AND st.ta_id = a.ta_id
			   AND st.teaching_course_id = tc.id
			WHERE sp.is_closed = FALSE
			  AND CURRENT_DATE >= sp.due_date - sp.remind_days_before
			  AND CURRENT_DATE <= sp.due_date
			  AND (st.status IS NULL OR st.status IN ('pending','ta_signed'))
			  AND (st.last_reminded_at IS NULL OR st.last_reminded_at < now() - INTERVAL '24 hours')
		)
		SELECT period_id, label, TO_CHAR(due_date,'YYYY-MM-DD'), ta_id, tc_id, code, name_th
		FROM pending`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type item struct {
		periodID, taID, tcID uuid.UUID
		label, due, code, nm string
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.periodID, &it.label, &it.due, &it.taID, &it.tcID, &it.code, &it.nm); err != nil {
			return 0, err
		}
		items = append(items, it)
	}
	for _, it := range items {
		body := fmt.Sprintf("โปรดยืนยันบันทึกเวลาปฏิบัติงาน %s วิชา %s (%s) — กำหนดส่งภายในวันที่ %s",
			it.label, it.code, it.nm, it.due)
		s.notify.Send(ctx, it.taID, "แจ้งเตือน: ใกล้ครบกำหนดส่งบันทึกเวลา TA", body, "/ta/reminders")
		_, _ = s.pool.Exec(ctx, `
			INSERT INTO submission_period_status
			    (id, submission_period_id, ta_id, teaching_course_id, status, last_reminded_at)
			VALUES ($1,$2,$3,$4,'pending', now())
			ON CONFLICT (submission_period_id, ta_id, teaching_course_id) DO UPDATE
			SET last_reminded_at = now()`,
			uuid.New(), it.periodID, it.taID, it.tcID)
	}
	return len(items), nil
}

// AutoCloseExpired flips any period whose due_date is more than 1 day past to
// is_closed=true. Called from the scheduler once a day.
func (s *SubmissionPeriodService) AutoCloseExpired(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE submission_periods SET is_closed = TRUE
		WHERE is_closed = FALSE AND due_date < CURRENT_DATE - INTERVAL '1 day'`)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// Ensure time is imported (used for the future scheduler wiring below).
var _ = time.Now
