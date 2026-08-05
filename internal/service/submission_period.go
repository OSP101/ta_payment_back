package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	StartsOn         string    `json:"starts_on"`  // "2569-06-01" — window opens
	DueDate          string    `json:"due_date"`   // "2569-07-31" — window closes
	Label            string    `json:"label"`      // "มิถุนายน 2569"
	RemindDaysBefore int       `json:"remind_days_before"`
	IsClosed         bool      `json:"is_closed"`
}

// SubmissionPeriodStatus is one (TA, teaching_course, period) row from the
// TA-facing reminders page.
type SubmissionPeriodStatus struct {
	PeriodID         uuid.UUID `json:"period_id"`
	Label            string    `json:"label"`
	YearMonth        string    `json:"year_month"` // "2569-06" — Buddhist academic year + submission month
	StartsOn         string    `json:"starts_on"`
	DueDate          string    `json:"due_date"`
	IsClosed         bool      `json:"is_closed"`
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	CourseCode       string    `json:"course_code"`
	CourseNameTH     string    `json:"course_name_th"`
	Status           string    `json:"status"`
	// Worklog readiness for this (TA, course, month). The month is "ready for
	// staff" (derived, not stored) once WorklogTotal>0 and WorklogUnapproved==0
	// — i.e. the lecturer has approved every daily row. WorklogUnapproved counts
	// draft/submitted/rejected rows (anything not yet approved).
	WorklogTotal      int `json:"worklog_total"`
	WorklogUnapproved int `json:"worklog_unapproved"`
	// WHO the unapproved rows are actually sitting with. WorklogUnapproved alone
	// could not say, and the TA home page read it as "the lecturer has not
	// approved these" — so a TA who had never pressed ส่งอนุมัติ was told their
	// lecturer was holding up work the lecturer had never seen.
	//   WorklogWaitingTA       = draft + rejected, still the TA's move
	//   WorklogWaitingLecturer = submitted, genuinely in the approval queue
	WorklogWaitingTA       int `json:"worklog_waiting_ta"`
	WorklogWaitingLecturer int `json:"worklog_waiting_lecturer"`
	// WorklogRejected is the part of WorklogWaitingTA the lecturer sent BACK.
	// Folded into "waiting on the TA" it was invisible: the screen said
	// "ยังไม่ได้ส่งอนุมัติ", which is true of a bounced row and tells the TA
	// nothing about why it came back or that it ever went.
	WorklogRejected    int     `json:"worklog_rejected"`
	WorklogApprovedHrs float64 `json:"worklog_approved_hrs"`
}

// SubmissionTimeline is the (period, TA, course) tracker row used by the
// vertical stepper UI. There are no digital signatures: the lecturer's daily
// worklog approval is the review, then staff export the file (lock) and mark
// it sent to finance. Each populated *_name/*_at pair marks a step done; the
// current step is whichever is next in the sequence exported → finance_sent.
type SubmissionTimeline struct {
	PeriodID         uuid.UUID `json:"period_id"`
	PeriodLabel      string    `json:"period_label"`
	YearMonth        string    `json:"year_month"`
	DueDate          string    `json:"due_date"`
	IsClosed         bool      `json:"is_closed"`
	TAID             uuid.UUID `json:"ta_id"`
	TAName           string    `json:"ta_name"`
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	CourseCode       string    `json:"course_code"`
	CourseNameTH     string    `json:"course_name_th"`
	Status           string    `json:"status"`
	// Worklog readiness so the stepper can show the derived "รอเจ้าหน้าที่"
	// state before any staff action has created a row.
	WorklogTotal      int     `json:"worklog_total"`
	WorklogUnapproved int     `json:"worklog_unapproved"`
	ExportedAt        *string `json:"exported_at,omitempty"`
	ExportedBy        *string `json:"exported_by,omitempty"`
	ExportedName      *string `json:"exported_name,omitempty"`
	FinanceSentAt     *string `json:"finance_sent_at,omitempty"`
	FinanceSentBy     *string `json:"finance_sent_by,omitempty"`
	FinanceSentName   *string `json:"finance_sent_name,omitempty"`
	FinanceNote       *string `json:"finance_note,omitempty"`
	// sent_back_* snapshot the latest "ตีกลับ" (send-back) event, if any, so
	// the stepper can render who bounced the row, when, and why.
	SentBackAt     *string `json:"sent_back_at,omitempty"`
	SentBackBy     *string `json:"sent_back_by,omitempty"`
	SentBackName   *string `json:"sent_back_name,omitempty"`
	SentBackReason *string `json:"sent_back_reason,omitempty"`
}

// statusRank fixes the forward order of the monthly staff lifecycle. Send-back
// transitions must strictly decrease the rank; the guarded upserts refuse to
// move it backwards out of order. 'skipped' sits outside the linear flow.
var statusRank = map[string]int{
	"pending": 0,
	// Staff sign-off sits between the lecturer's approval and the export
	// (24/07/2026 meeting). Inserting it here rather than appending keeps
	// send-back's "must strictly decrease" rule meaningful: bouncing an
	// exported month lands on staff_reviewed or pending, never past them.
	StatusStaffReviewed: 1,
	"exported":          2,
	"finance_sent":      3,
}

// List returns all periods for a term (or every term when termID is Nil).
func (s *SubmissionPeriodService) List(ctx context.Context, termID uuid.UUID) ([]SubmissionPeriod, error) {
	q := `SELECT id, term_id, year_month,
	             TO_CHAR(starts_on,'YYYY-MM-DD'),
	             TO_CHAR(due_date,'YYYY-MM-DD'),
	             label, remind_days_before, is_closed
	      FROM submission_periods`
	args := []any{}
	if termID != uuid.Nil {
		q += " WHERE term_id = $1"
		args = append(args, termID)
	}
	q += " ORDER BY starts_on, due_date"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubmissionPeriod{}
	for rows.Next() {
		var p SubmissionPeriod
		if err := rows.Scan(&p.ID, &p.TermID, &p.YearMonth, &p.StartsOn, &p.DueDate,
			&p.Label, &p.RemindDaysBefore, &p.IsClosed); err != nil {
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
	if in.YearMonth == "" || in.DueDate == "" || in.Label == "" || in.StartsOn == "" {
		return nil, Invalid("กรุณาระบุเดือน, วันเปิดรอบ, กำหนดส่ง และป้ายกำกับ")
	}
	if in.StartsOn >= in.DueDate {
		return nil, Invalid("วันเปิดรอบต้องมาก่อนวันครบกำหนด")
	}
	// year_month MUST be "<academic_year>-<MM>" (Buddhist academic year + 2-digit
	// month). The finance-lock join in period_lock.go matches
	// `academic_year || '-' || to_char(work_date,'MM')`, so a typo like "2569-6"
	// or a Gregorian "2026-06" would silently never match and the month could
	// never lock/close. Reject it up front instead of failing invisibly later.
	var acadYear int
	if err := s.pool.QueryRow(ctx,
		`SELECT academic_year FROM academic_terms WHERE id=$1`, in.TermID).Scan(&acadYear); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, Invalid("ไม่พบภาคเรียนที่ระบุ")
		}
		return nil, err
	}
	if !validYearMonth(in.YearMonth, acadYear) {
		return nil, Invalid(fmt.Sprintf("รูปแบบเดือนไม่ถูกต้อง — ต้องเป็น %d-MM (เช่น %d-06)", acadYear, acadYear))
	}
	if in.RemindDaysBefore <= 0 {
		in.RemindDaysBefore = 3
	}
	isNew := in.ID == uuid.Nil
	if isNew {
		in.ID = uuid.New()
	}
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "submission_period.upsert",
			Entity: "submission_period", EntityID: in.ID.String(), After: in},
		func(tx pgx.Tx) error {
			if isNew {
				_, err := tx.Exec(ctx, `
					INSERT INTO submission_periods (id, term_id, year_month, starts_on, due_date, label, remind_days_before, is_closed)
					VALUES ($1,$2,$3,$4::date,$5::date,$6,$7,$8)
					ON CONFLICT (term_id, year_month) DO UPDATE
					SET starts_on=EXCLUDED.starts_on, due_date=EXCLUDED.due_date, label=EXCLUDED.label,
					    remind_days_before=EXCLUDED.remind_days_before, is_closed=EXCLUDED.is_closed`,
					in.ID, in.TermID, in.YearMonth, in.StartsOn, in.DueDate, in.Label, in.RemindDaysBefore, in.IsClosed)
				return err
			}
			_, err := tx.Exec(ctx, `
				UPDATE submission_periods
				SET year_month=$2, starts_on=$3::date, due_date=$4::date, label=$5,
				    remind_days_before=$6, is_closed=$7
				WHERE id=$1`,
				in.ID, in.YearMonth, in.StartsOn, in.DueDate, in.Label, in.RemindDaysBefore, in.IsClosed)
			return err
		}); err != nil {
		return nil, err
	}
	return &in, nil
}

// Delete removes a period (cascades status rows). Only staff/admin.
// Refuses when any month of the period has already been exported or sent to
// finance — deleting it would cascade away the lock rows, silently reopening
// frozen worklogs and destroying the audit snapshot the payout file relies on.
// Such a period must be sent back / admin-unlocked before it can be removed.
func (s *SubmissionPeriodService) Delete(ctx context.Context, actor, id uuid.UUID) error {
	var lockedCount int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM submission_period_status
		WHERE submission_period_id=$1 AND status IN ('exported','finance_sent')`, id).Scan(&lockedCount); err != nil {
		return err
	}
	if lockedCount > 0 {
		return Conflict("ลบงวดนี้ไม่ได้ — มีเดือนที่ส่งออกไฟล์หรือส่งการเงินไปแล้ว กรุณาตีกลับหรือให้ผู้ดูแลระบบปลดล็อกก่อน")
	}
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "submission_period.delete",
			Entity: "submission_period", EntityID: id.String()},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM submission_periods WHERE id=$1`, id)
			return err
		})
}

// validYearMonth reports whether ym is exactly "<acadYear>-MM" with MM in 01..12.
func validYearMonth(ym string, acadYear int) bool {
	parts := strings.SplitN(ym, "-", 2)
	if len(parts) != 2 || parts[0] != strconv.Itoa(acadYear) || len(parts[1]) != 2 {
		return false
	}
	mm, err := strconv.Atoi(parts[1])
	return err == nil && mm >= 1 && mm <= 12
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
	// One transaction for the whole batch: a half-created set of periods with no
	// record of the run is worse than none, and the audit row reports a COUNT
	// that only means something if every insert it counts actually landed.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
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
		// starts_on = 1st of that submission month in Gregorian.
		startYear := gregYear
		if semester == 2 && (t.month == 1 || t.month == 2 || t.month == 3) {
			startYear++
		}
		starts := fmt.Sprintf("%d-%02d-01", startYear, t.month)
		due := fmt.Sprintf("%d-%s", dueYear, t.due)
		// Months whose shared due date lands BEFORE the month itself (ประกาศ:
		// มิ.ย.–ส.ค. all close 31 ก.ค., so August's window would be 1 ส.ค. →
		// 31 ก.ค. — never open, and MarkTASigned could never pass). Pull
		// starts_on back to the 1st of the due month so the window is valid;
		// staff can fine-tune via the period editor.
		if starts >= due {
			starts = due[:8] + "01"
		}
		label := fmt.Sprintf("%s %d", t.label, year)
		p := SubmissionPeriod{
			ID: uuid.New(), TermID: termID, YearMonth: ym,
			StartsOn: starts, DueDate: due, Label: label, RemindDaysBefore: 3, IsClosed: false,
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO submission_periods (id, term_id, year_month, starts_on, due_date, label, remind_days_before, is_closed)
			VALUES ($1,$2,$3,$4::date,$5::date,$6,$7,$8)
			ON CONFLICT (term_id, year_month) DO NOTHING`,
			p.ID, p.TermID, p.YearMonth, p.StartsOn, p.DueDate, p.Label, p.RemindDaysBefore, p.IsClosed)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "submission_period.bulk_create",
		Entity: "term", EntityID: termID.String(), After: map[string]int{"count": len(out)}}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// PendingByTA lists every (period × course) row a TA still owes. Rows are
// synthesised via LEFT JOIN so we don't need to pre-populate
// submission_period_status when a period is created — status rows are lazily
// created only when the TA acts on them.
func (s *SubmissionPeriodService) PendingByTA(ctx context.Context, taID uuid.UUID) ([]SubmissionPeriodStatus, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sp.id, sp.label, sp.year_month,
		       TO_CHAR(sp.starts_on,'YYYY-MM-DD'),
		       TO_CHAR(sp.due_date,'YYYY-MM-DD'),
		       sp.is_closed,
		       tc.id, tc.code, tc.name_th,
		       COALESCE(st.status, 'pending'),
		       -- Worklog readiness for the month. MM is taken from the period's
		       -- year_month and matched against work_date.
		       (SELECT COUNT(*) FROM work_logs wl2
		          JOIN ta_request_assignments a2 ON a2.id = wl2.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = $1 AND s2.teaching_course_id = tc.id
		           AND to_char(wl2.work_date,'MM') = RIGHT(sp.year_month, 2)),
		       (SELECT COUNT(*) FROM work_logs wl2
		          JOIN ta_request_assignments a2 ON a2.id = wl2.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = $1 AND s2.teaching_course_id = tc.id
		           AND to_char(wl2.work_date,'MM') = RIGHT(sp.year_month, 2)
		           AND wl2.status IN ('draft','submitted','rejected')),
		       (SELECT COUNT(*) FROM work_logs wl2
		          JOIN ta_request_assignments a2 ON a2.id = wl2.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = $1 AND s2.teaching_course_id = tc.id
		           AND to_char(wl2.work_date,'MM') = RIGHT(sp.year_month, 2)
		           AND wl2.status IN ('draft','rejected')),
		       (SELECT COUNT(*) FROM work_logs wl2
		          JOIN ta_request_assignments a2 ON a2.id = wl2.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = $1 AND s2.teaching_course_id = tc.id
		           AND to_char(wl2.work_date,'MM') = RIGHT(sp.year_month, 2)
		           AND wl2.status = 'submitted'),
		       (SELECT COUNT(*) FROM work_logs wl2
		          JOIN ta_request_assignments a2 ON a2.id = wl2.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = $1 AND s2.teaching_course_id = tc.id
		           AND to_char(wl2.work_date,'MM') = RIGHT(sp.year_month, 2)
		           AND wl2.status = 'rejected'),
		       COALESCE((SELECT SUM(wl2.hours) FROM work_logs wl2
		          JOIN ta_request_assignments a2 ON a2.id = wl2.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = $1 AND s2.teaching_course_id = tc.id
		           AND to_char(wl2.work_date,'MM') = RIGHT(sp.year_month, 2)
		           AND wl2.status = 'approved'), 0)
		FROM submission_periods sp
		JOIN teaching_courses tc ON tc.term_id = sp.term_id		JOIN ta_request_assignments a ON a.section_id IN
		    (SELECT id FROM sections WHERE teaching_course_id = tc.id)
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		LEFT JOIN submission_period_status st
		    ON st.submission_period_id = sp.id
		   AND st.ta_id = a.ta_id
		   AND st.teaching_course_id = tc.id
		WHERE a.ta_id = $1
		GROUP BY sp.id, sp.label, sp.year_month, sp.starts_on, sp.due_date, sp.is_closed,
		         tc.id, tc.code, tc.name_th, st.status
		ORDER BY sp.due_date, tc.code`, taID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubmissionPeriodStatus{}
	for rows.Next() {
		var s SubmissionPeriodStatus
		if err := rows.Scan(&s.PeriodID, &s.Label, &s.YearMonth, &s.StartsOn, &s.DueDate, &s.IsClosed,
			&s.TeachingCourseID, &s.CourseCode, &s.CourseNameTH,
			&s.Status,
			&s.WorklogTotal, &s.WorklogUnapproved,
			&s.WorklogWaitingTA, &s.WorklogWaitingLecturer, &s.WorklogRejected,
			&s.WorklogApprovedHrs); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// userDisplayName fetches "first last" (falling back to email) so we can
// snapshot the signer's name on the timeline row — read-through so a rename
// later doesn't rewrite history.
func (s *SubmissionPeriodService) userDisplayName(ctx context.Context, uid uuid.UUID) string {
	var first, last, email string
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(first_name,''), COALESCE(last_name,''), COALESCE(email,'')
		 FROM users WHERE id=$1`, uid).Scan(&first, &last, &email); err != nil {
		return ""
	}
	name := (first + " " + last)
	if name == " " || name == "" {
		return email
	}
	return name
}

// assertSignTarget validates the (period, TA, course) triple every signature
// transition operates on: the period must belong to the course's term, and the
// TA must hold an approved assignment in the course. Without this, route-level
// RBAC alone would let any caller mint status rows for arbitrary pairs.
func (s *SubmissionPeriodService) assertSignTarget(ctx context.Context, periodID, taID, tcID uuid.UUID) error {
	var periodMatches bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM submission_periods sp
			JOIN teaching_courses tc ON tc.term_id = sp.term_id
			WHERE sp.id = $1 AND tc.id = $2)`, periodID, tcID).Scan(&periodMatches); err != nil {
		return err
	}
	if !periodMatches {
		return Invalid("งวดส่งบันทึกเวลาไม่อยู่ในภาคเรียนเดียวกับรายวิชานี้")
	}
	ok, err := taHasApprovedAssignment(ctx, s.pool, taID, tcID)
	if err != nil {
		return err
	}
	if !ok {
		return Invalid("TA คนนี้ไม่มีการแต่งตั้งที่อนุมัติแล้วในรายวิชานี้")
	}
	return nil
}

// assertLecturerOrPrivileged gates lecturer-facing transitions: admin/staff
// pass outright, otherwise the actor must actually teach the course — route
// RBAC only proves "is a lecturer", not "is THIS course's lecturer".
func (s *SubmissionPeriodService) assertLecturerOrPrivileged(ctx context.Context, actor, tcID uuid.UUID) error {
	priv, err := isPrivileged(ctx, s.pool, actor)
	if err != nil {
		return err
	}
	if priv {
		return nil
	}
	owns, err := lecturerOwnsCourse(ctx, s.pool, actor, tcID)
	if err != nil {
		return err
	}
	if !owns {
		return ErrForbidden
	}
	return nil
}

// assertPrivileged gates staff-only transitions at the service layer as
// defense-in-depth alongside the route's RequireRole.
func (s *SubmissionPeriodService) assertPrivileged(ctx context.Context, actor uuid.UUID) error {
	priv, err := isPrivileged(ctx, s.pool, actor)
	if err != nil {
		return err
	}
	if !priv {
		return ErrForbidden
	}
	return nil
}

// monthWorklogReadiness returns the worklog counts for a (TA, course, month),
// where the month is the MM of the period's year_month matched against
// work_date. Used to gate the monthly sign step and to enrich the TA reminders
// list. unapproved counts draft/submitted/rejected rows.
func (s *SubmissionPeriodService) monthWorklogReadiness(ctx context.Context, taID, tcID uuid.UUID, yearMonth string) (total, unapproved int, err error) {
	mm := yearMonth
	if len(mm) >= 2 {
		mm = mm[len(mm)-2:]
	}
	err = s.pool.QueryRow(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE wl.status IN ('draft','submitted','rejected'))
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		JOIN sections sec ON sec.id = a.section_id
		WHERE a.ta_id = $1 AND sec.teaching_course_id = $2
		  AND to_char(wl.work_date,'MM') = $3`, taID, tcID, mm).Scan(&total, &unapproved)
	return
}

// MarkCourseExported is called by the staff ZIP export. There are no digital
// signatures in this system — exporting the file for a course is what locks a
// month. For every (TA × month) in the course whose daily worklog is fully
// approved (>=1 row, none draft/submitted/rejected) and not already
// exported/finance_sent, it flips the status to 'exported' and snapshots who
// exported it. Months still being worked on are left untouched (editable), so
// re-exporting later locks whatever has since become ready. Returns the number
// of (TA × month) cells newly locked.
func (s *SubmissionPeriodService) MarkCourseExported(ctx context.Context, actor, tcID uuid.UUID) (int, error) {
	name := s.userDisplayName(ctx, actor)
	// This is the freeze point for a course's payout numbers, so the lock rows
	// and the record of the lock go in together.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		INSERT INTO submission_period_status
		    (id, submission_period_id, ta_id, teaching_course_id, status,
		     exported_at, exported_by, exported_name)
		SELECT gen_random_uuid(), sp.id, a.ta_id, tc.id, 'exported', now(), $2, $3
		FROM teaching_courses tc
		JOIN submission_periods sp ON sp.term_id = tc.term_id
		JOIN ta_request_assignments a
		    ON a.section_id IN (SELECT id FROM sections WHERE teaching_course_id = tc.id)
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		LEFT JOIN submission_period_status st
		    ON st.submission_period_id = sp.id
		   AND st.ta_id = a.ta_id
		   AND st.teaching_course_id = tc.id
		WHERE tc.id = $1
		  -- Only months staff have signed off may be exported. Previously any
		  -- lecturer-approved month qualified, which is the gap the meeting
		  -- closed by making "ตรวจสอบเบิกจ่ายค่าตอบแทน" its own step.
		  AND COALESCE(st.status,'pending') = 'staff_reviewed'
		  AND EXISTS (
		        SELECT 1 FROM work_logs wl
		        JOIN ta_request_assignments a2 ON a2.id = wl.assignment_id
		        JOIN sections s2 ON s2.id = a2.section_id
		        WHERE a2.ta_id = a.ta_id AND s2.teaching_course_id = tc.id
		          AND to_char(wl.work_date,'MM') = RIGHT(sp.year_month, 2)
		          AND wl.status = 'approved')
		  AND NOT EXISTS (
		        SELECT 1 FROM work_logs wl
		        JOIN ta_request_assignments a2 ON a2.id = wl.assignment_id
		        JOIN sections s2 ON s2.id = a2.section_id
		        WHERE a2.ta_id = a.ta_id AND s2.teaching_course_id = tc.id
		          AND to_char(wl.work_date,'MM') = RIGHT(sp.year_month, 2)
		          AND wl.status IN ('draft','submitted','rejected'))
		GROUP BY sp.id, a.ta_id, tc.id
		ON CONFLICT (submission_period_id, ta_id, teaching_course_id) DO UPDATE
		SET status        = 'exported',
		    exported_at   = now(),
		    exported_by   = EXCLUDED.exported_by,
		    exported_name = EXCLUDED.exported_name
		WHERE submission_period_status.status NOT IN ('exported','finance_sent')
		RETURNING ta_id, submission_period_id`, tcID, actor, name)
	if err != nil {
		return 0, err
	}
	type cell struct{ taID, periodID uuid.UUID }
	var cells []cell
	for rows.Next() {
		var c cell
		if err := rows.Scan(&c.taID, &c.periodID); err != nil {
			rows.Close()
			return 0, err
		}
		cells = append(cells, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(cells) > 0 {
		if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "submission_period.exported",
			Entity: "teaching_course", EntityID: tcID.String(),
			After: map[string]int{"locked_cells": len(cells)}}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	if len(cells) > 0 {
		// After the commit: notifications reach TAs and cannot be recalled.
		if s.notify != nil {
			for _, c := range cells {
				s.notify.Send(ctx, c.taID,
					"เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว",
					"เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก",
					"/ta/reminders")
			}
		}
	}
	return len(cells), nil
}

// MarkFinanceSent is the final step — staff records that the exported paperwork
// has been physically handed off to the finance office. Requires the row to
// already be 'exported' (i.e. the file was generated and the month locked).
func (s *SubmissionPeriodService) MarkFinanceSent(ctx context.Context, actor, periodID, taID, tcID uuid.UUID, note string) error {
	if err := s.assertPrivileged(ctx, actor); err != nil {
		return err
	}
	if err := s.assertSignTarget(ctx, periodID, taID, tcID); err != nil {
		return err
	}
	// Payout readiness: the reimbursement documents carry the TA's national ID
	// and bank account — refuse the handoff while the profile is unapproved or
	// incomplete instead of shipping paperwork with blank fields.
	if err := s.assertPayoutReady(ctx, taID); err != nil {
		return err
	}
	name := s.userDisplayName(ctx, actor)
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "submission_period.finance_sent",
			Entity: "submission_period_status", EntityID: periodID.String() + "/" + taID.String()},
		func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
				UPDATE submission_period_status
				SET status            = 'finance_sent',
				    finance_sent_at   = now(),
				    finance_sent_by   = $4,
				    finance_sent_name = $5,
				    finance_note      = NULLIF($6,'')
				WHERE submission_period_id = $1 AND ta_id = $2 AND teaching_course_id = $3
				  AND status IN ('exported','finance_sent')`,
				periodID, taID, tcID, actor, name, note)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return Invalid("ยังไม่พร้อมส่งการเงิน — ต้องส่งออกไฟล์เบิกจ่ายของเดือนนี้ก่อน")
			}
			return nil
		}); err != nil {
		return err
	}
	if s.notify != nil {
		s.notify.Send(ctx, taID,
			"ส่งเบิกจ่ายไปยังการเงินแล้ว",
			"บันทึกเวลาประจำเดือนของคุณถูกส่งไปยังการเงินเรียบร้อยแล้ว",
			"/ta/reminders")
	}
	return nil
}

// assertPayoutReady rejects the finance handoff when the TA's profile is
// missing, not yet approved by staff, or lacks the national ID / bank fields
// the reimbursement documents need.
func (s *SubmissionPeriodService) assertPayoutReady(ctx context.Context, taID uuid.UUID) error {
	var status, nid, acct, bank string
	err := s.pool.QueryRow(ctx, `
		SELECT status::text, COALESCE(national_id,''), COALESCE(account_no,''), COALESCE(bank_name,'')
		FROM ta_profiles WHERE user_id = $1`, taID).Scan(&status, &nid, &acct, &bank)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invalid("ส่งการเงินไม่ได้ — TA ยังไม่ได้กรอกข้อมูลโปรไฟล์/บัญชีธนาคาร")
	}
	if err != nil {
		return err
	}
	if status != "approved" {
		return Invalid("ส่งการเงินไม่ได้ — เอกสารโปรไฟล์ของ TA ยังไม่ผ่านการอนุมัติจากเจ้าหน้าที่")
	}
	if nid == "" || acct == "" || bank == "" {
		return Invalid("ส่งการเงินไม่ได้ — ข้อมูลเลขบัตรประชาชนหรือบัญชีธนาคารของ TA ไม่ครบถ้วน")
	}
	return nil
}

// MarkSentBack is the "ตีกลับ" transition: staff/admin push an exported month
// back to pending with a mandatory reason, which unlocks the daily worklog so
// the TA/lecturer can correct it. The exported snapshot is cleared. Lecturers
// have no monthly action in this flow (their review is the daily approve/
// reject), so send-back is staff/admin only. finance_sent rows can only be
// reopened via the admin-only RevertFinanceSent.
func (s *SubmissionPeriodService) MarkSentBack(ctx context.Context, actor, periodID, taID, tcID uuid.UUID, toStatus, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Invalid("ต้องระบุเหตุผลการตีกลับ")
	}
	// The only backward target in the new flow is pending (unlock for edits).
	if toStatus == "" {
		toStatus = "pending"
	}
	toRank, known := statusRank[toStatus]
	if !known || toStatus == "finance_sent" {
		return Invalid("สถานะปลายทางไม่ถูกต้อง")
	}
	if err := s.assertPrivileged(ctx, actor); err != nil {
		return err
	}
	if err := s.assertSignTarget(ctx, periodID, taID, tcID); err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Lock the row so a concurrent export/finance-send can't race the revert.
	var cur string
	err = tx.QueryRow(ctx, `
		SELECT status FROM submission_period_status
		WHERE submission_period_id=$1 AND ta_id=$2 AND teaching_course_id=$3
		FOR UPDATE`, periodID, taID, tcID).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invalid("ยังไม่มีสถานะให้ตีกลับ (เดือนนี้ยังไม่ถูกส่งออก)")
	}
	if err != nil {
		return err
	}
	curRank, known := statusRank[cur]
	if !known {
		return Invalid("สถานะปัจจุบันไม่รองรับการตีกลับ")
	}
	if cur == "finance_sent" {
		return Conflict("รายการนี้ส่งการเงินแล้ว — ต้องให้ผู้ดูแลระบบปลดล็อกก่อน")
	}
	if toRank >= curRank {
		return Invalid("สถานะปลายทางต้องอยู่ก่อนสถานะปัจจุบัน")
	}

	name := s.userDisplayName(ctx, actor)
	if _, err := tx.Exec(ctx, `
		UPDATE submission_period_status SET
		  status        = $4,
		  exported_at   = NULL,
		  exported_by   = NULL,
		  exported_name = NULL,
		  sent_back_at     = now(),
		  sent_back_by     = $5,
		  sent_back_name   = $6,
		  sent_back_reason = $7
		WHERE submission_period_id=$1 AND ta_id=$2 AND teaching_course_id=$3`,
		periodID, taID, tcID, toStatus, actor, name, reason); err != nil {
		return err
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "submission_period.sent_back",
		Entity: "submission_period_status", EntityID: periodID.String() + "/" + taID.String(),
		Note: reason, After: map[string]string{"from": cur, "to": toStatus}}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if s.notify != nil {
		body := "บันทึกเวลาประจำเดือนของคุณถูกตีกลับให้แก้ไข: " + reason
		s.notify.Send(ctx, taID, "บันทึกเวลาประจำเดือนถูกตีกลับ", body, "/ta/reminders")
		// The course lecturers may need to re-open a worklog for correction.
		if lects, err := courseLecturerIDs(ctx, s.pool, tcID); err == nil {
			for _, lid := range lects {
				if lid != actor {
					s.notify.Send(ctx, lid, "บันทึกเวลาประจำเดือนถูกตีกลับ",
						"เจ้าหน้าที่ตีกลับบันทึกเวลาประจำเดือนของ TA ในรายวิชาของคุณ: "+reason,
						"/lecturer/courses/"+tcID.String()+"/reports")
				}
			}
		}
	}
	return nil
}

// RevertFinanceSent is the admin-only escape hatch that reopens a
// finance_sent month (status → exported) so corrections can flow again after
// an admin also sends it back to pending. Requires a reason; audited and
// notified so the paper trail explains why the re-issued documents differ.
func (s *SubmissionPeriodService) RevertFinanceSent(ctx context.Context, actor, periodID, taID, tcID uuid.UUID, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Invalid("ต้องระบุเหตุผลการปลดล็อก")
	}
	isAdmin, err := hasRole(ctx, s.pool, actor, "admin")
	if err != nil {
		return err
	}
	if !isAdmin {
		return ErrForbidden
	}
	if err := s.assertSignTarget(ctx, periodID, taID, tcID); err != nil {
		return err
	}
	name := s.userDisplayName(ctx, actor)
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "submission_period.finance_revert",
			Entity: "submission_period_status", EntityID: periodID.String() + "/" + taID.String(),
			Note: reason},
		func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
				UPDATE submission_period_status SET
				  status            = 'exported',
				  finance_sent_at   = NULL,
				  finance_sent_by   = NULL,
				  finance_sent_name = NULL,
				  finance_note      = NULL,
				  sent_back_at      = now(),
				  sent_back_by      = $4,
				  sent_back_name    = $5,
				  sent_back_reason  = $6
				WHERE submission_period_id=$1 AND ta_id=$2 AND teaching_course_id=$3
				  AND status = 'finance_sent'`,
				periodID, taID, tcID, actor, name, reason)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return Invalid("รายการนี้ยังไม่อยู่ในสถานะส่งการเงิน")
			}
			return nil
		}); err != nil {
		return err
	}
	if s.notify != nil {
		s.notify.Send(ctx, taID, "ปลดล็อกสถานะส่งการเงิน",
			"ผู้ดูแลระบบปลดล็อกบันทึกเวลาประจำเดือนของคุณเพื่อแก้ไข: "+reason,
			"/ta/reminders")
		if lects, err := courseLecturerIDs(ctx, s.pool, tcID); err == nil {
			for _, lid := range lects {
				s.notify.Send(ctx, lid, "ปลดล็อกสถานะส่งการเงิน",
					"ผู้ดูแลระบบปลดล็อกบันทึกเวลาประจำเดือนของ TA ในรายวิชาของคุณ: "+reason,
					"/lecturer/courses/"+tcID.String()+"/reports")
			}
		}
	}
	return nil
}

// GetTimeline returns the full approval timeline for one (period, TA, course).
// Returns nil, nil if no row exists yet (i.e. still fully pending).
// Readable by the TA themself, the course's lecturers, and staff/admin —
// timeline rows carry names and approval history, so they are not world-readable.
func (s *SubmissionPeriodService) GetTimeline(ctx context.Context, actor, periodID, taID, tcID uuid.UUID) (*SubmissionTimeline, error) {
	if actor != taID {
		if err := s.assertLecturerOrPrivileged(ctx, actor, tcID); err != nil {
			return nil, err
		}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT sp.id, sp.label, sp.year_month, TO_CHAR(sp.due_date,'YYYY-MM-DD'), sp.is_closed,
		       u.id, u.first_name || ' ' || u.last_name,
		       tc.id, tc.code, tc.name_th,
		       COALESCE(st.status, 'pending'),
		       (SELECT COUNT(*) FROM work_logs wl
		          JOIN ta_request_assignments a2 ON a2.id = wl.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = $2 AND s2.teaching_course_id = tc.id
		           AND to_char(wl.work_date,'MM') = RIGHT(sp.year_month, 2)),
		       (SELECT COUNT(*) FROM work_logs wl
		          JOIN ta_request_assignments a2 ON a2.id = wl.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = $2 AND s2.teaching_course_id = tc.id
		           AND to_char(wl.work_date,'MM') = RIGHT(sp.year_month, 2)
		           AND wl.status IN ('draft','submitted','rejected')),
		       TO_CHAR(st.exported_at,        'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'),
		       st.exported_by::text,
		       st.exported_name,
		       TO_CHAR(st.finance_sent_at,    'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'),
		       st.finance_sent_by::text,
		       st.finance_sent_name,
		       st.finance_note,
		       TO_CHAR(st.sent_back_at,       'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'),
		       st.sent_back_by::text,
		       st.sent_back_name,
		       st.sent_back_reason
		FROM submission_periods sp
		JOIN users u ON u.id = $2
		JOIN teaching_courses tc ON tc.id = $3		LEFT JOIN submission_period_status st
		    ON st.submission_period_id = sp.id
		   AND st.ta_id = $2
		   AND st.teaching_course_id = $3
		WHERE sp.id = $1`, periodID, taID, tcID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	var t SubmissionTimeline
	if err := rows.Scan(&t.PeriodID, &t.PeriodLabel, &t.YearMonth, &t.DueDate, &t.IsClosed,
		&t.TAID, &t.TAName,
		&t.TeachingCourseID, &t.CourseCode, &t.CourseNameTH,
		&t.Status,
		&t.WorklogTotal, &t.WorklogUnapproved,
		&t.ExportedAt, &t.ExportedBy, &t.ExportedName,
		&t.FinanceSentAt, &t.FinanceSentBy, &t.FinanceSentName, &t.FinanceNote,
		&t.SentBackAt, &t.SentBackBy, &t.SentBackName, &t.SentBackReason,
	); err != nil {
		return nil, err
	}
	return &t, nil
}

// ListByCourse returns one timeline row per (TA × period) for a given
// teaching course, used by the lecturer and staff dashboards. Only assignments
// whose ta_request is approved are included. Restricted to the course's
// lecturers and staff/admin — it exposes every TA's status in the course.
func (s *SubmissionPeriodService) ListByCourse(ctx context.Context, actor, tcID uuid.UUID, periodID uuid.UUID) ([]SubmissionTimeline, error) {
	if err := s.assertLecturerOrPrivileged(ctx, actor, tcID); err != nil {
		return nil, err
	}
	args := []any{tcID}
	filter := ""
	if periodID != uuid.Nil {
		filter = " AND sp.id = $2"
		args = append(args, periodID)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT sp.id, sp.label, sp.year_month, TO_CHAR(sp.due_date,'YYYY-MM-DD'), sp.is_closed,
		       u.id, u.first_name || ' ' || u.last_name,
		       tc.id, tc.code, tc.name_th,
		       COALESCE(st.status, 'pending'),
		       (SELECT COUNT(*) FROM work_logs wl
		          JOIN ta_request_assignments a2 ON a2.id = wl.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = u.id AND s2.teaching_course_id = tc.id
		           AND to_char(wl.work_date,'MM') = RIGHT(sp.year_month, 2)),
		       (SELECT COUNT(*) FROM work_logs wl
		          JOIN ta_request_assignments a2 ON a2.id = wl.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = u.id AND s2.teaching_course_id = tc.id
		           AND to_char(wl.work_date,'MM') = RIGHT(sp.year_month, 2)
		           AND wl.status IN ('draft','submitted','rejected')),
		       TO_CHAR(st.exported_at,        'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'),
		       st.exported_by::text,
		       st.exported_name,
		       TO_CHAR(st.finance_sent_at,    'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'),
		       st.finance_sent_by::text,
		       st.finance_sent_name,
		       st.finance_note,
		       TO_CHAR(st.sent_back_at,       'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'),
		       st.sent_back_by::text,
		       st.sent_back_name,
		       st.sent_back_reason
		FROM teaching_courses tc		JOIN submission_periods sp ON sp.term_id = tc.term_id
		JOIN ta_request_assignments a
		    ON a.section_id IN (SELECT id FROM sections WHERE teaching_course_id = tc.id)
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		JOIN users u ON u.id = a.ta_id
		LEFT JOIN submission_period_status st
		    ON st.submission_period_id = sp.id
		   AND st.ta_id = a.ta_id
		   AND st.teaching_course_id = tc.id
		WHERE tc.id = $1`+filter+`
		GROUP BY sp.id, sp.label, sp.year_month, sp.due_date, sp.is_closed,
		         u.id, u.first_name, u.last_name,
		         tc.id, tc.code, tc.name_th,
		         st.status, st.exported_at, st.exported_by, st.exported_name,
		         st.finance_sent_at, st.finance_sent_by, st.finance_sent_name, st.finance_note,
		         st.sent_back_at, st.sent_back_by, st.sent_back_name, st.sent_back_reason
		ORDER BY sp.due_date, u.first_name, u.last_name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubmissionTimeline{}
	for rows.Next() {
		var t SubmissionTimeline
		if err := rows.Scan(&t.PeriodID, &t.PeriodLabel, &t.YearMonth, &t.DueDate, &t.IsClosed,
			&t.TAID, &t.TAName,
			&t.TeachingCourseID, &t.CourseCode, &t.CourseNameTH,
			&t.Status,
			&t.WorklogTotal, &t.WorklogUnapproved,
			&t.ExportedAt, &t.ExportedBy, &t.ExportedName,
			&t.FinanceSentAt, &t.FinanceSentBy, &t.FinanceSentName, &t.FinanceNote,
			&t.SentBackAt, &t.SentBackBy, &t.SentBackName, &t.SentBackReason,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
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
			                a.ta_id, tc.id AS tc_id, tc.code, tc.name_th
			FROM submission_periods sp
			JOIN teaching_courses tc ON tc.term_id = sp.term_id
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
			  AND (st.status IS NULL OR st.status = 'pending')
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
		body := fmt.Sprintf("โปรดบันทึกเวลาปฏิบัติงาน %s วิชา %s (%s) ให้ครบและส่งให้อาจารย์อนุมัติ — กำหนดส่งภายในวันที่ %s",
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
