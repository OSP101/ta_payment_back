package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

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
	ID     uuid.UUID `json:"id"`
	TermID uuid.UUID `json:"term_id" validate:"required"`
	// YearMonth is CHAR(7) in the DB ("2569-06") — len matches that column
	// exactly; the "<academic_year>-MM" shape itself is still checked by
	// Upsert against the term's academic_year, which a struct tag can't know.
	YearMonth        string `json:"year_month" validate:"required,len=7"`
	StartsOn         string `json:"starts_on" validate:"required"`     // "2569-06-01" — window opens
	DueDate          string `json:"due_date" validate:"required"`      // "2569-07-31" — window closes
	Label            string `json:"label" validate:"required,max=200"` // "มิถุนายน 2569"
	RemindDaysBefore int    `json:"remind_days_before"`                // <=0 defaults to 3 in Upsert — not required
	IsClosed         bool   `json:"is_closed"`
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
	return out, rows.Err()
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
		return nil, Invalid(fmt.Sprintf("รูปแบบเดือนไม่ถูกต้อง ต้องเป็น %d-MM (เช่น %d-06)", acadYear, acadYear))
	}
	if in.RemindDaysBefore <= 0 {
		in.RemindDaysBefore = 3
	}
	isNew := in.ID == uuid.Nil
	if isNew {
		in.ID = uuid.New()
	} else {
		// The status rows (staff sign-off, export and finance locks) hang off
		// the period id, so rewriting year_month would move them onto another
		// month: the signed-off month would lose its period (and with it every
		// lock — no period reads as unrestricted) while the new month froze.
		var curYM string
		var signed int
		if err := s.pool.QueryRow(ctx, `
			SELECT sp.year_month,
			       (SELECT COUNT(*) FROM submission_period_status st
			         WHERE st.submission_period_id = sp.id
			           AND st.status IN ('staff_reviewed','exported','finance_sent'))
			  FROM submission_periods sp WHERE sp.id = $1`, in.ID).Scan(&curYM, &signed); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, Invalid("ไม่พบรอบลงเวลาที่ระบุ")
			}
			return nil, err
		}
		if curYM != in.YearMonth && signed > 0 {
			return nil, Conflict("เปลี่ยนเดือนของรอบนี้ไม่ได้ มีผู้ช่วยสอนที่เจ้าหน้าที่ตรวจแล้วหรือส่งออกเอกสารแล้ว กรุณาสร้างรอบของเดือนใหม่แทน")
		}
	}
	// Deadlines decide whether a TA can still submit at all, so "the due date
	// used to be the 5th" is the answer to "why was my month forfeited". The
	// request struct that was recorded as After could not say it.
	if err := writeAuditedRow(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "submission_period.upsert",
			Entity: "submission_period", EntityID: in.ID.String()},
		"submission_periods", in.ID,
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
// Refuses when any TA's month in the period has been signed off by staff,
// exported or sent to finance — deleting it would cascade away those rows:
// a sign-off would vanish without trace (the month silently back to "not
// reviewed"), and a lock would silently reopen frozen worklogs and destroy the
// snapshot the payout file relies on. Such a month must be sent back (or
// admin-unlocked) first.
func (s *SubmissionPeriodService) Delete(ctx context.Context, actor, id uuid.UUID) error {
	var reviewed, lockedCount int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE status = 'staff_reviewed'),
		       COUNT(*) FILTER (WHERE status IN ('exported','finance_sent'))
		FROM submission_period_status
		WHERE submission_period_id=$1`, id).Scan(&reviewed, &lockedCount); err != nil {
		return err
	}
	if lockedCount > 0 {
		return Conflict("ลบงวดนี้ไม่ได้ มีเดือนที่ส่งออกไฟล์หรือส่งการเงินไปแล้ว กรุณาตีกลับหรือให้ผู้ดูแลระบบปลดล็อกก่อน")
	}
	if reviewed > 0 {
		return Conflict(fmt.Sprintf("ลบงวดนี้ไม่ได้ มีผู้ช่วยสอน %d คนที่เจ้าหน้าที่ตรวจสอบเบิกจ่ายเดือนนี้แล้ว "+
			"การลบจะทำให้ผลการตรวจหายไป กรุณาตีกลับเดือนดังกล่าวก่อน", reviewed))
	}
	return writeAuditedRow(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "submission_period.delete",
			Entity: "submission_period", EntityID: id.String()},
		"submission_periods", id,
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

// BulkCreateForTerm generates one submission period per calendar month the
// term actually covers (academic_terms.starts_on..ends_on), each opening on
// the 1st of ITS OWN month. Staff can then edit them. Idempotent — ON CONFLICT
// preserves existing rows.
//
// It used to stamp a fixed template — มิ.ย.–ต.ค. for ภาคต้น, พ.ย.–มี.ค. for
// everything else — whatever the term's real dates were, and opened สิงหาคม on
// 1 ก.ค. because the ประกาศ's shared 31 ก.ค. due date fell before the month
// began. The due dates keep the ประกาศ's convention where it applies (ภาคต้น:
// มิ.ย. and ก.ค. due 31 ก.ค.); every other month — and สิงหาคม, whose shared
// date would close it before it opens — is due on the 5th of the month after.
func (s *SubmissionPeriodService) BulkCreateForTerm(ctx context.Context, actor, termID uuid.UUID) ([]SubmissionPeriod, error) {
	var year, semester int
	var startsOn, endsOn *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT academic_year, semester, starts_on, ends_on FROM academic_terms WHERE id=$1`, termID).
		Scan(&year, &semester, &startsOn, &endsOn); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, Invalid("ไม่พบภาคเรียนที่ระบุ")
		}
		return nil, err
	}
	if startsOn == nil || endsOn == nil || endsOn.Before(*startsOn) {
		return nil, Invalid("ภาคเรียนนี้ยังไม่ได้กำหนดวันเปิดและวันปิดภาค จึงสร้างรอบลงเวลาอัตโนมัติไม่ได้ กรุณากำหนดวันของภาคเรียนก่อน")
	}
	// The ประกาศ's own dates (MM-DD), ภาคต้น only. Applied only when they fall
	// on or after the month's 1st — otherwise the window would close before it
	// opens.
	sharedDue := map[int]string{}
	if semester == 1 {
		sharedDue = map[int]string{6: "07-31", 7: "07-31", 8: "07-31"}
	}
	type tpl struct {
		start time.Time
		due   string
	}
	var templates []tpl
	first := time.Date(startsOn.Year(), startsOn.Month(), 1, 0, 0, 0, 0, time.UTC)
	for m := first; !m.After(*endsOn); m = m.AddDate(0, 1, 0) {
		start := m.Format("2006-01-02")
		due := m.AddDate(0, 1, 4).Format("2006-01-02") // 5th of the next month
		if mmdd, ok := sharedDue[int(m.Month())]; ok {
			if d := fmt.Sprintf("%d-%s", m.Year(), mmdd); d > start {
				due = d
			}
		}
		templates = append(templates, tpl{start: m, due: due})
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
		// year_month is the academic key: the term's Buddhist academic year and
		// the calendar month, e.g. มกราคม of academic year 2568 is "2568-01".
		ym := fmt.Sprintf("%d-%02d", year, int(t.start.Month()))
		// Labels carry the calendar Buddhist year a reader expects: มกราคม of
		// academic year 2568 is มกราคม 2569.
		label := fmt.Sprintf("%s %d", thaiMonthNames[int(t.start.Month())], t.start.Year()+543)
		p := SubmissionPeriod{
			ID: uuid.New(), TermID: termID, YearMonth: ym,
			StartsOn: t.start.Format("2006-01-02"), DueDate: t.due, Label: label,
			RemindDaysBefore: 3, IsClosed: false,
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
//
// enrollmentID, when non-nil, scopes this to one ta_enrollments period —
// same "which period am I viewing" filter as DashboardService.TaOverview
// (see that function's doc comment for the enrollment_id IS NULL fallback
// reasoning). The condition has to be repeated on the outer `a` join AND on
// each of the five `a2` subqueries below, since they each independently
// re-derive hour counts for the month rather than reusing the outer row.
func (s *SubmissionPeriodService) PendingByTA(ctx context.Context, taID uuid.UUID, enrollmentID *uuid.UUID) ([]SubmissionPeriodStatus, error) {
	outerFilter, subFilter := "", ""
	args := []any{taID}
	if enrollmentID != nil {
		outerFilter = " AND (a.enrollment_id = $2 OR a.enrollment_id IS NULL)"
		subFilter = " AND (a2.enrollment_id = $2 OR a2.enrollment_id IS NULL)"
		args = append(args, *enrollmentID)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT sp.id, sp.label, sp.year_month,
		       TO_CHAR(sp.starts_on,'YYYY-MM-DD'),
		       TO_CHAR(sp.due_date,'YYYY-MM-DD'),
		       sp.is_closed,
		       tc.id, tc.code, tc.name_th,
		       COALESCE(st.status, 'pending'),
		       -- Worklog readiness for the month. MM is taken from the period's
		       -- year_month and matched against work_date. Grad-special
		       -- (master/phd, track=special) is excluded throughout: those TAs
		       -- no longer log work_logs at all, so a leftover 'submitted' row
		       -- on a dead grad-special assignment must not show up as pending
		       -- work on a TA's OWN reminders screen either — the same rule
		       -- ListPending/ListReviewQueue already apply on the staff side.
		       (SELECT COUNT(*) FROM work_logs wl2
		          JOIN ta_request_assignments a2 ON a2.id = wl2.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = $1 AND s2.teaching_course_id = tc.id
		           AND to_char(wl2.work_date,'MM') = RIGHT(sp.year_month, 2)
		           AND (a2.level::text NOT IN ('master','phd') OR s2.track <> 'special')`+subFilter+`),
		       (SELECT COUNT(*) FROM work_logs wl2
		          JOIN ta_request_assignments a2 ON a2.id = wl2.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = $1 AND s2.teaching_course_id = tc.id
		           AND to_char(wl2.work_date,'MM') = RIGHT(sp.year_month, 2)
		           AND (a2.level::text NOT IN ('master','phd') OR s2.track <> 'special')`+subFilter+`
		           AND wl2.status IN ('draft','submitted','rejected')),
		       (SELECT COUNT(*) FROM work_logs wl2
		          JOIN ta_request_assignments a2 ON a2.id = wl2.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = $1 AND s2.teaching_course_id = tc.id
		           AND to_char(wl2.work_date,'MM') = RIGHT(sp.year_month, 2)
		           AND (a2.level::text NOT IN ('master','phd') OR s2.track <> 'special')`+subFilter+`
		           AND wl2.status IN ('draft','rejected')),
		       (SELECT COUNT(*) FROM work_logs wl2
		          JOIN ta_request_assignments a2 ON a2.id = wl2.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = $1 AND s2.teaching_course_id = tc.id
		           AND to_char(wl2.work_date,'MM') = RIGHT(sp.year_month, 2)
		           AND (a2.level::text NOT IN ('master','phd') OR s2.track <> 'special')`+subFilter+`
		           AND wl2.status = 'submitted'),
		       (SELECT COUNT(*) FROM work_logs wl2
		          JOIN ta_request_assignments a2 ON a2.id = wl2.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = $1 AND s2.teaching_course_id = tc.id
		           AND to_char(wl2.work_date,'MM') = RIGHT(sp.year_month, 2)
		           AND (a2.level::text NOT IN ('master','phd') OR s2.track <> 'special')`+subFilter+`
		           AND wl2.status = 'rejected'),
		       COALESCE((SELECT SUM(wl2.hours) FROM work_logs wl2
		          JOIN ta_request_assignments a2 ON a2.id = wl2.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = $1 AND s2.teaching_course_id = tc.id
		           AND to_char(wl2.work_date,'MM') = RIGHT(sp.year_month, 2)
		           AND (a2.level::text NOT IN ('master','phd') OR s2.track <> 'special')`+subFilter+`
		           AND wl2.status = 'approved'), 0)
		FROM submission_periods sp
		JOIN teaching_courses tc ON tc.term_id = sp.term_id
		JOIN sections sec ON sec.teaching_course_id = tc.id
		JOIN ta_request_assignments a ON a.section_id = sec.id
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		LEFT JOIN submission_period_status st
		    ON st.submission_period_id = sp.id
		   AND st.ta_id = a.ta_id
		   AND st.teaching_course_id = tc.id
		WHERE a.ta_id = $1
		  -- A TA whose ONLY assignment on this course is grad-special must not
		  -- see the course at all here: there is nothing left for them to send
		  -- or wait on.
		  AND (a.level::text NOT IN ('master','phd') OR sec.track <> 'special')`+outerFilter+`
		GROUP BY sp.id, sp.label, sp.year_month, sp.starts_on, sp.due_date, sp.is_closed,
		         tc.id, tc.code, tc.name_th, st.status
		ORDER BY `+periodOrderSQL("sp.year_month")+`, tc.code`, args...)
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
	return out, rows.Err()
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
// where the month is the period's full year_month (Buddhist academic year +
// MM) matched against work_date via the course's term. Used to gate the
// monthly sign step and to enrich the TA reminders list. unapproved counts
// draft/submitted/rejected rows.
//
// QUAL-03: used to extract only the MM digits from yearMonth and compare
// that against to_char(wl.work_date,'MM') alone, throwing the year away —
// safe only by the accident that this call is already scoped to one
// teaching_course_id (one term, under 12 months, so no month repeats). A
// course spanning a calendar-year boundary or a reused teaching_courses row
// would have silently counted the wrong year's work_logs.
func (s *SubmissionPeriodService) monthWorklogReadiness(ctx context.Context, taID, tcID uuid.UUID, yearMonth string) (total, unapproved int, err error) {
	return monthWorklogReadinessQ(ctx, s.pool, taID, tcID, yearMonth)
}

// monthWorklogReadinessQ is monthWorklogReadiness on a caller's connection —
// the staff sign-off counts inside its own locked transaction, so the count
// and the write cannot be separated by a TA's commit.
func monthWorklogReadinessQ(ctx context.Context, q ledgerQuerier, taID, tcID uuid.UUID, yearMonth string) (total, unapproved int, err error) {
	// Rows the TA can no longer send — draft/rejected in a closed period — are
	// forfeited ("ไม่ประสงค์ลงเวลา", staff decision 03/08/2026). They are
	// neither work to approve nor work that exists, so they count in neither
	// figure: counting them as "not yet approved" meant a month holding one
	// forgotten draft could never be signed off or exported, however complete
	// the rest was (found in the UAT follow-up review).
	err = q.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE wl.status = 'approved'),
		       COUNT(*) FILTER (WHERE wl.status = 'submitted'
		                           OR (wl.status IN ('draft','rejected') AND NOT `+unsubmittableMonthSQL("wl")+`))
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		JOIN sections sec ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		JOIN academic_terms trm ON trm.id = tc.term_id
		WHERE a.ta_id = $1 AND sec.teaching_course_id = $2
		  -- เทียบทั้ง year_month (ปีการศึกษา + MM) ตรง ๆ กับพารามิเตอร์ ไม่ใช่
		  -- แยกเอาแค่ MM มาเทียบแบบเดิม — ดู workLogInPeriodSQL's doc comment
		  AND trm.academic_year::text || '-' || to_char(wl.work_date,'MM') = $3
		  -- Grad-special no longer logs work_logs at all — pay is computed
		  -- automatically from the regular track's class schedule. A TA who
		  -- also holds a real (grad-regular or undergrad) assignment on this
		  -- course must not have their readiness blocked by leftover
		  -- 'submitted' rows on a dead grad-special assignment: nobody can
		  -- ever move those rows forward, so counting them here would refuse
		  -- MarkStaffReviewed on the TA's real hours forever.
		  AND (a.level::text NOT IN ('master','phd') OR sec.track <> 'special')`,
		taID, tcID, yearMonth).Scan(&total, &unapproved)
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
//
// months (Gregorian "YYYY-MM", empty = every month) confines the lock to the
// fiscal slice actually exported — see the SQL comment below.
func (s *SubmissionPeriodService) MarkCourseExported(ctx context.Context, actor, tcID uuid.UUID, months []string) (int, error) {
	return s.MarkCourseExportedAsBuilt(ctx, actor, tcID, months, "")
}

// courseWorklogFingerprintSQL digests every work_log row of one course: any
// insert, edit, status change or delete changes it.
//
// Scoped to the months being exported ($2, Gregorian "YYYY-MM", empty = all)
// and to non-draft rows: the file never prints drafts, and an October draft
// saved while the มิ.ย.–ก.ย. slice builds must not fail that export.
const courseWorklogFingerprintSQL = `
	SELECT COALESCE(md5(string_agg(
	         wl.id::text || wl.status::text || wl.work_date::text || wl.start_time::text
	         || wl.end_time::text || wl.hours::text || wl.activity::text,
	         ',' ORDER BY wl.id)), '')
	FROM work_logs wl
	JOIN ta_request_assignments a ON a.id = wl.assignment_id
	JOIN sections sec ON sec.id = a.section_id
	WHERE sec.teaching_course_id = $1
	  AND wl.status <> 'draft'
	  AND (COALESCE(cardinality($2::text[]), 0) = 0
	       OR to_char(wl.work_date, 'YYYY-MM') = ANY($2::text[]))`

// CourseWorklogFingerprint is taken before a ZIP is built and handed to
// MarkCourseExportedAsBuilt, which refuses to lock if the rows moved since.
func (s *SubmissionPeriodService) CourseWorklogFingerprint(ctx context.Context, tcID uuid.UUID, months []string) (string, error) {
	var fp string
	err := s.pool.QueryRow(ctx, courseWorklogFingerprintSQL, tcID, months).Scan(&fp)
	return fp, err
}

// MarkCourseExportedAsBuilt locks like MarkCourseExported, but first proves the
// course's work_logs are still what the file was built from. The ZIP is built
// before the lock (it has to exist before it can be refused), and worklog
// writes share no lock with export, so a StaffUpsert landing in between left
// finance holding a file the database no longer matched. The table lock below
// waits for in-flight writes and holds new ones until the lock commits; after
// that the row trigger (migration 0124) refuses writes into locked months.
// An empty builtFP skips the check.
func (s *SubmissionPeriodService) MarkCourseExportedAsBuilt(ctx context.Context, actor, tcID uuid.UUID, months []string, builtFP string) (int, error) {
	return s.MarkCourseExportedWithFigures(ctx, actor, tcID, months, builtFP, nil)
}

// MarkCourseExportedWithFigures is MarkCourseExportedAsBuilt that also records,
// in the same transaction, what the pack paid for every cell it locks
// (export_month_ledger, migration 0140). From then on every reader prices those
// months from the record rather than live, so the document and the system
// cannot drift apart. nil figs locks without a record (the demo seeder).
func (s *SubmissionPeriodService) MarkCourseExportedWithFigures(ctx context.Context, actor, tcID uuid.UUID, months []string, builtFP string, figs MonthFigures) (int, error) {
	name := s.userDisplayName(ctx, actor)
	// This is the freeze point for a course's payout numbers, so the lock rows
	// and the record of the lock go in together.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	if builtFP != "" {
		if _, err := tx.Exec(ctx, `LOCK TABLE work_logs IN SHARE MODE`); err != nil {
			return 0, err
		}
		var nowFP string
		if err := tx.QueryRow(ctx, courseWorklogFingerprintSQL, tcID, months).Scan(&nowFP); err != nil {
			return 0, err
		}
		if nowFP != builtFP {
			return 0, Conflict("บันทึกเวลาของรายวิชานี้เปลี่ยนระหว่างสร้างไฟล์ จึงยังไม่ล็อกและไม่ส่งไฟล์ กรุณากดดาวน์โหลดอีกครั้ง")
		}
	}

	rows, err := tx.Query(ctx, `
		INSERT INTO submission_period_status
		    (id, submission_period_id, ta_id, teaching_course_id, status,
		     exported_at, exported_by, exported_name)
		SELECT gen_random_uuid(), sp.id, a.ta_id, tc.id, 'exported', now(), $2, $3
		FROM teaching_courses tc
		JOIN academic_terms trm ON trm.id = tc.term_id
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
		  -- Never lock a pair absent from a printed order. MarkStaffReviewed now
		  -- refuses such pairs, but rows reviewed before that check existed can
		  -- still be sitting at staff_reviewed.
		  AND `+AppointedSQL("tc.id", "a.ta_id")+`
		  -- The approved work this period is locked FOR must fall inside the
		  -- fiscal slice being exported. Without this, issuing มิ.ย.–ก.ย. in
		  -- September would also freeze ตุลาคม, which is still being taught and
		  -- belongs to the next budget year's document.
		  AND EXISTS (
		        SELECT 1 FROM work_logs wl
		        JOIN ta_request_assignments a2 ON a2.id = wl.assignment_id
		        JOIN sections s2 ON s2.id = a2.section_id
		        WHERE a2.ta_id = a.ta_id AND s2.teaching_course_id = tc.id
		          AND `+workLogInPeriodSQL("wl", "trm", "sp")+`
		          AND wl.status = 'approved'
		          AND (COALESCE(cardinality($4::text[]), 0) = 0
		               OR to_char(wl.work_date,'YYYY-MM') = ANY($4::text[])))
		  AND NOT EXISTS (
		        SELECT 1 FROM work_logs wl
		        JOIN ta_request_assignments a2 ON a2.id = wl.assignment_id
		        JOIN sections s2 ON s2.id = a2.section_id
		        WHERE a2.ta_id = a.ta_id AND s2.teaching_course_id = tc.id
		          AND `+workLogInPeriodSQL("wl", "trm", "sp")+`
		          -- Forfeited rows (unsent when the period closed) do not hold
		          -- the month open — same rule as monthWorklogReadiness.
		          AND (wl.status = 'submitted'
		               OR (wl.status IN ('draft','rejected') AND NOT `+unsubmittableMonthSQL("wl")+`))
		          -- Same grad-special exclusion as monthWorklogReadiness: a
		          -- leftover 'submitted' row on a dead grad-special assignment
		          -- must not block locking the TA's real (regular-track or
		          -- undergrad) hours once staff have signed those off.
		          AND (a2.level::text NOT IN ('master','phd') OR s2.track <> 'special'))
		GROUP BY sp.id, a.ta_id, tc.id
		ON CONFLICT (submission_period_id, ta_id, teaching_course_id) DO UPDATE
		SET status        = 'exported',
		    exported_at   = now(),
		    exported_by   = EXCLUDED.exported_by,
		    exported_name = EXCLUDED.exported_name
		WHERE submission_period_status.status NOT IN ('exported','finance_sent')
		RETURNING ta_id, submission_period_id`, tcID, actor, name, months)
	if err != nil {
		return 0, err
	}
	type cell = lockedCell
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
	for i := range cells {
		var periodYM string
		if err := tx.QueryRow(ctx, `SELECT year_month FROM submission_periods WHERE id = $1`,
			cells[i].periodID).Scan(&periodYM); err != nil {
			return 0, err
		}
		if cells[i].gregYM, err = gregorianYearMonth(periodYM); err != nil {
			return 0, err
		}
	}
	if err := writeMonthLedger(ctx, tx, actor, tcID, cells, figs); err != nil {
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
			// One mail per TA naming every month that was locked — the old text
			// said "เดือนดังกล่าว" without ever saying which month, once per cell.
			// Months in calendar order (UAT DEF-006): RETURNING hands the cells
			// back in no particular order, which read as ก.ย., ส.ค., ก.ค., มิ.ย.
			type month struct {
				start time.Time
				label string
			}
			byTA := map[uuid.UUID][]month{}
			var order []uuid.UUID
			for _, c := range cells {
				var m month
				_ = s.pool.QueryRow(ctx, `SELECT starts_on, label FROM submission_periods WHERE id = $1`,
					c.periodID).Scan(&m.start, &m.label)
				if _, seen := byTA[c.taID]; !seen {
					order = append(order, c.taID)
				}
				byTA[c.taID] = append(byTA[c.taID], m)
			}
			var code string
			_ = s.pool.QueryRow(ctx, `SELECT code FROM teaching_courses WHERE id = $1`, tcID).Scan(&code)
			for _, taID := range order {
				ms := byTA[taID]
				sort.Slice(ms, func(i, j int) bool { return ms[i].start.Before(ms[j].start) })
				labels := make([]string, len(ms))
				for i, m := range ms {
					labels[i] = m.label
				}
				// The course code is in the title because unread in-app notices
				// are folded together by (title, link), and the link is the same
				// page for every course: a TA whose second course was exported
				// lost the unread notice about the first.
				s.notify.Send(ctx, taID,
					"เจ้าหน้าที่จัดทำเอกสารเบิกจ่ายแล้ว "+code,
					"เจ้าหน้าที่ได้ตรวจสอบและจัดทำเอกสารเบิกจ่ายค่าตอบแทนรายวิชา "+courseLabelOf(ctx, s.pool, tcID)+
						" ของท่าน ประจำเดือน"+strings.Join(labels, ", ")+
						" เรียบร้อยแล้ว บันทึกเวลาปฏิบัติงานของเดือนดังกล่าวจึงถูกล็อกและไม่สามารถแก้ไขได้",
					"/ta/reminders")
			}
		}
	}
	return len(cells), nil
}

// periodNoticeKeys returns the course code and period label a per-month notice
// names. Both go into the TITLE, not only the body: unread in-app notices are
// folded by (title, link), and these links are shared across courses (the TA's
// /ta/reminders) or across TAs and months (the lecturer's reports page), so a
// bare title let a later send-back overwrite an unread one about another month.
func periodNoticeKeys(ctx context.Context, pool *pgxpool.Pool, tcID, periodID uuid.UUID) (code, month string) {
	_ = pool.QueryRow(ctx, `SELECT code FROM teaching_courses WHERE id = $1`, tcID).Scan(&code)
	_ = pool.QueryRow(ctx, `SELECT label FROM submission_periods WHERE id = $1`, periodID).Scan(&month)
	return code, month
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
	if err := assertAppointed(ctx, s.pool, tcID, taID); err != nil {
		return err
	}
	// Payout readiness: the reimbursement documents carry the TA's national ID
	// and bank account — refuse the handoff while the profile is unapproved or
	// incomplete instead of shipping paperwork with blank fields.
	if err := s.assertPayoutReady(ctx, taID); err != nil {
		return err
	}
	name := s.userDisplayName(ctx, actor)
	// The before-image is read inside the transaction, under the cell's lock
	// (see writeAuditedLocked / lockPeriodCell), so it is the state this write
	// actually replaced. It used to be read on the pool first, on the theory that
	// only one officer moves a cell at a time — which the audit trail cannot
	// assume, since it is the record used when that assumption is in question.
	sentEntry := audit.Entry{ActorID: &actor, Action: "submission_period.finance_sent",
		Entity: "submission_period_status", EntityID: periodID.String() + "/" + taID.String(),
		After: map[string]any{"status": "finance_sent"}}
	if err := writeAuditedLocked(ctx, s.pool, s.aud, sentEntry,
		func(tx pgx.Tx, e *audit.Entry) error {
			if err := lockPeriodCell(ctx, tx, periodID, taID, tcID); err != nil {
				return err
			}
			prevStatus, err := periodStatus(ctx, tx, periodID, taID, tcID)
			if err != nil {
				return err
			}
			e.Before = prevStatus
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
				return Invalid("ยังไม่พร้อมส่งการเงิน ต้องส่งออกไฟล์เบิกจ่ายของเดือนนี้ก่อน")
			}
			return nil
		}); err != nil {
		return err
	}
	if s.notify != nil {
		// Course and month named, and the code in the title: unread in-app
		// notices fold by (title, link) and the link is one page for all
		// courses, so a fixed title let the next handoff overwrite this one.
		var code, month string
		_ = s.pool.QueryRow(ctx, `SELECT code FROM teaching_courses WHERE id = $1`, tcID).Scan(&code)
		_ = s.pool.QueryRow(ctx, `SELECT label FROM submission_periods WHERE id = $1`, periodID).Scan(&month)
		s.notify.Send(ctx, taID,
			"ส่งเรื่องเบิกจ่ายไปยังงานการเงินแล้ว "+code,
			"บันทึกเวลาปฏิบัติงานรายวิชา "+courseLabelOf(ctx, s.pool, tcID)+" ประจำเดือน"+month+
				" ของท่านได้ถูกส่งไปยังงานการเงินเพื่อดำเนินการเบิกจ่ายเรียบร้อยแล้ว",
			"/ta/reminders")
	}
	return nil
}

// assertPayoutReady rejects the finance handoff when the TA's profile is
// missing, not yet approved by staff, or lacks an approved creditor-form
// document. The national ID / bank columns this used to check no longer
// exist (PDPA, migration 0047) — that data now lives only in the
// creditor-form PDF, so an approved copy of that document is the readiness
// signal. Mirrors ExportService.validatePayoutReadiness in export.go, scoped
// to a single TA.
func (s *SubmissionPeriodService) assertPayoutReady(ctx context.Context, taID uuid.UUID) error {
	issue, err := payoutIssue(ctx, s.pool, taID)
	if err != nil {
		return err
	}
	if issue != "" {
		return Invalid("ส่งการเงินไม่ได้ " + issue)
	}
	return nil
}

// payoutIssue is why a TA's profile would produce a defective claim document,
// or "" when it would not: no profile, a profile staff have not approved, or no
// approved creditor form. One definition shared by the finance handoff and the
// staff sign-off (MarkStaffReviewed), so a month is never signed off for a TA
// whose documents the export will then refuse.
func payoutIssue(ctx context.Context, q ledgerQuerier, taID uuid.UUID) (string, error) {
	var hasProfile bool
	var status string
	var missingForm bool
	err := q.QueryRow(ctx, `
		SELECT p.user_id IS NOT NULL, COALESCE(p.status::text, ''),
		       NOT EXISTS (
		           SELECT 1 FROM ta_documents d
		           WHERE d.user_id = $1 AND d.kind = 'creditor_form'
		             AND d.superseded_at IS NULL AND d.status = 'approved')
		FROM (SELECT 1) x
		LEFT JOIN ta_profiles p ON p.user_id = $1`, taID).Scan(&hasProfile, &status, &missingForm)
	if err != nil {
		return "", err
	}
	switch {
	case !hasProfile:
		return "TA ยังไม่ได้กรอกข้อมูลโปรไฟล์/บัญชีธนาคาร", nil
	case status != "approved":
		return "เอกสารโปรไฟล์ของ TA ยังไม่ผ่านการอนุมัติจากเจ้าหน้าที่", nil
	case missingForm:
		return "ยังไม่มีแบบฟอร์มเจ้าหนี้ที่อนุมัติแล้ว", nil
	}
	return "", nil
}

// MarkSentBack is the "ตีกลับ" transition: staff/admin return one TA's month
// to them for correction, with a mandatory reason.
//
// It does ONE thing, whatever stage the month is at (pending, staff_reviewed
// or exported — anything before finance_sent): the month is reopened so the
// TA can actually edit it. Its approved and still-submitted rows become
// rejected with the officer's reason, the month's status goes back to
// pending (clearing the sign-off and the export lock), and the lecturer must
// approve the corrected rows again before staff can review and export.
//
// It used to do two different things. On an unsigned month it rejected the
// rows; on a reviewed or exported month it only moved the status back and left
// the rows approved — so the TA and the lecturer were told "ส่งกลับให้แก้ไข"
// while neither could edit anything (the TA edits only draft/rejected rows,
// the lecturer approves only submitted ones), until a second press finally
// rejected the rows.
//
// An exported month that is sent back and exported again produces a
// corrected document ("ฉบับแก้ไข", export_batches.version); the old batch
// stays in the history.
//
// Refused once the period has closed: a rejected row in a closed month is
// forfeited (ไม่ประสงค์ลงเวลา) and can never be resent, so the send-back would
// silently cancel the TA's pay. Moving the period's due date is the deliberate,
// visible act that reopens a closed month (decision 03/08/2026).
//
// toStatus is kept for the API's shape; only "pending" (or empty) is valid.
func (s *SubmissionPeriodService) MarkSentBack(ctx context.Context, actor, periodID, taID, tcID uuid.UUID, toStatus, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Invalid("ต้องระบุเหตุผลการตีกลับ")
	}
	if toStatus != "" && toStatus != "pending" {
		return Invalid("สถานะปลายทางไม่ถูกต้อง การตีกลับจะคืนเดือนนี้ให้ผู้ช่วยสอนแก้ไขเสมอ")
	}
	if err := s.assertPrivileged(ctx, actor); err != nil {
		return err
	}
	if err := s.assertSignTarget(ctx, periodID, taID, tcID); err != nil {
		return err
	}
	name := s.userDisplayName(ctx, actor)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	res, err := reopenCellForCorrection(ctx, tx, actor, name, periodID, taID, tcID, reason)
	if err != nil {
		return err
	}
	if res.prev == "pending" && len(res.moved) == 0 {
		return Invalid("เดือนนี้ไม่มีรายการที่อนุมัติแล้วหรือรออนุมัติให้ตีกลับ")
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "submission_period.sent_back",
		Entity: "submission_period_status", EntityID: periodID.String() + "/" + taID.String(),
		Note:   reason,
		Before: map[string]any{"status": res.prev, "rows": res.moved},
		After:  map[string]any{"status": "pending", "rows_rejected": len(res.moved), "hours": res.hours}}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.notifyReopened(ctx, actor, periodID, taID, tcID, res.prev,
		"เจ้าหน้าที่", reason)
	return nil
}

// reopenResult is what reopenCellForCorrection moved.
type reopenResult struct {
	prev  string           // status before: pending | staff_reviewed | exported
	moved []map[string]any // the rows sent back to the TA
	hours float64
}

// reopenCellForCorrection is the one definition of "give this TA's month back
// to them": shared by the staff send-back and the admin course unlock, so the
// two can never again leave a month in different half-open states.
//
// Serialised against the lecturer's approval (the same per-course lock
// ApproveMany takes) and against the staff sign-off and the worklog trigger
// (the per-cell lock, lockPeriodCell / migration 0141). The rows are then
// rejected in one statement that covers submitted rows too: an approval racing
// this either commits first and its rows are rejected here, or finds nothing
// left to approve — the TA never gets "ส่งกลับ" and "อนุมัติ" for the same rows.
//
// The status is reopened BEFORE the rows move: an exported month's rows are
// write-protected by the lock trigger (0124) until it is no longer exported.
func reopenCellForCorrection(ctx context.Context, tx pgx.Tx, actor uuid.UUID, actorName string,
	periodID, taID, tcID uuid.UUID, reason string) (*reopenResult, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 42))`, tcID); err != nil {
		return nil, err
	}
	if err := lockPeriodCell(ctx, tx, periodID, taID, tcID); err != nil {
		return nil, err
	}
	res := &reopenResult{prev: "pending", moved: []map[string]any{}}
	err := tx.QueryRow(ctx, `
		SELECT status FROM submission_period_status
		WHERE submission_period_id=$1 AND ta_id=$2 AND teaching_course_id=$3
		FOR UPDATE`, periodID, taID, tcID).Scan(&res.prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	switch res.prev {
	case "pending", StatusStaffReviewed, "exported":
	case "finance_sent":
		return nil, Conflict("รายการนี้ส่งการเงินแล้ว ต้องให้ผู้ดูแลระบบยกเลิกสถานะส่งการเงินก่อน")
	default:
		return nil, Invalid("สถานะปัจจุบันไม่รองรับการตีกลับ")
	}
	var closed bool
	var label string
	if err := tx.QueryRow(ctx,
		`SELECT `+periodClosedSQL("sp")+`, sp.label FROM submission_periods sp WHERE sp.id = $1`,
		periodID).Scan(&closed, &label); err != nil {
		return nil, err
	}
	if closed {
		return nil, Invalid(fmt.Sprintf(
			"เดือน %s ปิดรับบันทึกเวลาแล้ว ถ้าตีกลับ ผู้ช่วยสอนจะส่งใหม่ไม่ได้และรายการจะถือว่าไม่ประสงค์ลงเวลา "+
				"กรุณาขยายกำหนดส่งของรอบนี้ที่หน้าตั้งค่ารอบลงเวลาก่อน แล้วจึงตีกลับ", label))
	}
	// Every send-back writes its reason, so the timeline shows the CURRENT one
	// rather than whichever earlier event happened to create the row.
	if _, err := tx.Exec(ctx, `
		INSERT INTO submission_period_status
		    (id, submission_period_id, ta_id, teaching_course_id, status,
		     sent_back_at, sent_back_by, sent_back_name, sent_back_reason)
		VALUES (gen_random_uuid(), $1, $2, $3, 'pending', now(), $4, $5, $6)
		ON CONFLICT (submission_period_id, ta_id, teaching_course_id) DO UPDATE
		SET status              = 'pending',
		    staff_reviewed_by   = NULL,
		    staff_reviewed_name = NULL,
		    exported_at         = NULL,
		    exported_by         = NULL,
		    exported_name       = NULL,
		    sent_back_at        = now(),
		    sent_back_by        = $4,
		    sent_back_name      = $5,
		    sent_back_reason    = $6
		WHERE submission_period_status.status <> 'finance_sent'`,
		periodID, taID, tcID, actor, actorName, reason); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		UPDATE work_logs wl SET status = 'rejected', reject_reason = $4,
		       rejected_by_role = 'staff', approved_at = NULL, approved_by = NULL
		FROM ta_request_assignments a
		JOIN sections sec         ON sec.id = a.section_id
		JOIN teaching_courses tc  ON tc.id = sec.teaching_course_id
		JOIN academic_terms trm   ON trm.id = tc.term_id
		JOIN submission_periods sp ON sp.term_id = tc.term_id
		WHERE wl.assignment_id = a.id
		  AND sp.id = $1 AND a.ta_id = $2 AND tc.id = $3
		  AND `+workLogInPeriodSQL("wl", "trm", "sp")+`
		  AND wl.status IN ('approved','submitted')
		  -- Same exclusion as the review queue: grad-special rows are dead.
		  AND (a.level::text NOT IN ('master','phd') OR sec.track <> 'special')
		RETURNING wl.id, TO_CHAR(wl.work_date, 'YYYY-MM-DD'), wl.hours`,
		periodID, taID, tcID, reason)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id uuid.UUID
		var date string
		var h float64
		if err := rows.Scan(&id, &date, &h); err != nil {
			rows.Close()
			return nil, err
		}
		res.moved = append(res.moved, map[string]any{"id": id, "work_date": date, "hours": h})
		res.hours += h
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// notifyReopened tells the TA and the course's lecturers that a month came
// back for correction, and exactly what each of them now has to do. by names
// the sender ("เจ้าหน้าที่" / "ผู้ดูแลระบบ").
func (s *SubmissionPeriodService) notifyReopened(ctx context.Context, actor, periodID, taID, tcID uuid.UUID, prev, by, reason string) {
	if s.notify == nil {
		return
	}
	label := courseLabelOf(ctx, s.pool, tcID)
	code, month := periodNoticeKeys(ctx, s.pool, tcID, periodID)
	reissue := ""
	if prev == "exported" {
		reissue = " เมื่อแก้ไขและผ่านการตรวจอีกครั้ง เจ้าหน้าที่จะออกเอกสารเบิกจ่ายฉบับแก้ไขแทนฉบับเดิม"
	}
	s.notify.SendAction(ctx, taID, "บันทึกเวลาประจำเดือนถูกส่งกลับให้แก้ไข "+code+" "+month,
		by+"ได้ส่งบันทึกเวลาปฏิบัติงานประจำเดือน"+month+" รายวิชา "+label+
			" กลับมาให้ท่านแก้ไขและส่งอนุมัติอีกครั้ง เนื่องจาก "+reason+reissue,
		"/ta/courses/"+tcID.String()+"/worklog")
	if lects, err := courseLecturerIDs(ctx, s.pool, tcID); err == nil {
		for _, lid := range lects {
			if lid == actor {
				continue
			}
			s.notify.Send(ctx, lid, "บันทึกเวลาประจำเดือนถูกส่งกลับให้แก้ไข "+code+" "+month+" "+personName(ctx, s.pool, taID),
				by+"ได้ส่งบันทึกเวลาปฏิบัติงานประจำเดือน"+month+" ของ "+personName(ctx, s.pool, taID)+
					" ผู้ช่วยสอนรายวิชา "+label+" กลับไปให้แก้ไข เมื่อผู้ช่วยสอนส่งใหม่แล้ว ขอให้ท่านอนุมัติอีกครั้ง เนื่องจาก "+reason,
				"/lecturer/courses/"+tcID.String()+"/reports")
		}
	}
}

// ReopenCourseExports is the admin "ปลดล็อก" for a course: every exported
// (TA, month) of the course — not finance_sent — is reopened for correction
// exactly as a send-back would (reopenCellForCorrection), and the course's
// own section-edit lock (teaching_courses.exported_at) is cleared in the same
// transaction. Before, unlock cleared only that course flag: the months stayed
// exported and locked, and the dashboards that read the flag disagreed with
// every screen that read the months.
//
// All or nothing: if any exported month's period has closed the unlock is
// refused, naming the months, because reopening there would forfeit the TA's
// rows (see MarkSentBack).
func ReopenCourseExports(ctx context.Context, tx pgx.Tx, actor uuid.UUID, actorName string, tcID uuid.UUID, reason string) ([]reopenedCell, error) {
	rows, err := tx.Query(ctx, `
		SELECT st.submission_period_id, st.ta_id
		FROM submission_period_status st
		JOIN submission_periods sp ON sp.id = st.submission_period_id
		WHERE st.teaching_course_id = $1 AND st.status = 'exported'
		ORDER BY sp.starts_on, st.ta_id`, tcID)
	if err != nil {
		return nil, err
	}
	var cells []reopenedCell
	for rows.Next() {
		var c reopenedCell
		if err := rows.Scan(&c.PeriodID, &c.TAID); err != nil {
			rows.Close()
			return nil, err
		}
		cells = append(cells, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, c := range cells {
		res, err := reopenCellForCorrection(ctx, tx, actor, actorName, c.PeriodID, c.TAID, tcID, reason)
		if err != nil {
			return nil, err
		}
		cells[i].RowsRejected = len(res.moved)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE teaching_courses SET exported_at = NULL WHERE id = $1`, tcID); err != nil {
		return nil, err
	}
	return cells, nil
}

// reopenedCell is one (TA, month) an admin unlock reopened.
type reopenedCell struct {
	PeriodID     uuid.UUID `json:"period_id"`
	TAID         uuid.UUID `json:"ta_id"`
	RowsRejected int       `json:"rows_rejected"`
}

// NotifyCourseReopened sends the per-cell notices after an admin unlock has
// committed.
func (s *SubmissionPeriodService) NotifyCourseReopened(ctx context.Context, actor, tcID uuid.UUID, cells []reopenedCell, reason string) {
	for _, c := range cells {
		s.notifyReopened(ctx, actor, c.PeriodID, c.TAID, tcID, "exported", "ผู้ดูแลระบบ", reason)
	}
}

// RevertFinanceSent is the admin-only undo of "ส่งการเงินแล้ว": a finance_sent
// month goes back to exported. It does NOT unlock anything — the month is still
// exported and its work_logs stay write-locked; to correct the hours, staff
// then send the month back (MarkSentBack) or an admin unlocks the course.
// Requires a reason; audited and notified.
//
// The notices used to say "ปลดล็อก…เพื่อให้แก้ไข" while leaving the month
// locked, so the TA was told to edit a month nobody could edit. They now say
// what this actually does.
func (s *SubmissionPeriodService) RevertFinanceSent(ctx context.Context, actor, periodID, taID, tcID uuid.UUID, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Invalid("ต้องระบุเหตุผลการยกเลิกสถานะส่งการเงิน")
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
	revertEntry := audit.Entry{ActorID: &actor, Action: "submission_period.finance_revert",
		Entity: "submission_period_status", EntityID: periodID.String() + "/" + taID.String(),
		Note: reason, After: map[string]any{"status": "exported"}}
	if err := writeAuditedLocked(ctx, s.pool, s.aud, revertEntry,
		func(tx pgx.Tx, e *audit.Entry) error {
			if err := lockPeriodCell(ctx, tx, periodID, taID, tcID); err != nil {
				return err
			}
			prevStatus, err := periodStatus(ctx, tx, periodID, taID, tcID)
			if err != nil {
				return err
			}
			e.Before = prevStatus
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
		label := courseLabelOf(ctx, s.pool, tcID)
		code, month := periodNoticeKeys(ctx, s.pool, tcID, periodID)
		s.notify.Send(ctx, taID, "ยกเลิกสถานะส่งการเงิน "+code+" "+month,
			"ผู้ดูแลระบบได้ยกเลิกสถานะส่งการเงินของบันทึกเวลาปฏิบัติงานประจำเดือน"+month+" รายวิชา "+label+
				" ของท่าน กลับเป็นสถานะเจ้าหน้าที่ส่งเอกสารเบิกจ่ายแล้ว เนื่องจาก "+reason+
				" บันทึกเวลาของเดือนนี้ยังคงถูกล็อก หากต้องแก้ไข เจ้าหน้าที่จะส่งกลับให้ท่านอีกครั้ง",
			"/ta/reminders")
		if lects, err := courseLecturerIDs(ctx, s.pool, tcID); err == nil {
			for _, lid := range lects {
				s.notify.Send(ctx, lid, "ยกเลิกสถานะส่งการเงิน "+code+" "+month+" "+personName(ctx, s.pool, taID),
					"ผู้ดูแลระบบได้ยกเลิกสถานะส่งการเงินของบันทึกเวลาปฏิบัติงานประจำเดือน"+month+" ของ "+personName(ctx, s.pool, taID)+
						" ผู้ช่วยสอนรายวิชา "+label+" กลับเป็นสถานะส่งเอกสารเบิกจ่ายแล้ว เนื่องจาก "+reason+
						" บันทึกเวลาของเดือนนี้ยังคงถูกล็อก",
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
		       -- Grad-special (master/phd, track=special) is excluded: those TAs
		       -- log no work_logs at all, so a leftover 'submitted' row on a
		       -- dead grad-special assignment must not show as "unapproved"
		       -- work on a timeline that is otherwise fully done.
		       (SELECT COUNT(*) FROM work_logs wl
		          JOIN ta_request_assignments a2 ON a2.id = wl.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = $2 AND s2.teaching_course_id = tc.id
		           AND `+workLogInPeriodSQL("wl", "trm", "sp")+`
		           AND (a2.level::text NOT IN ('master','phd') OR s2.track <> 'special')),
		       (SELECT COUNT(*) FROM work_logs wl
		          JOIN ta_request_assignments a2 ON a2.id = wl.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = $2 AND s2.teaching_course_id = tc.id
		           AND `+workLogInPeriodSQL("wl", "trm", "sp")+`
		           AND (a2.level::text NOT IN ('master','phd') OR s2.track <> 'special')
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
		JOIN teaching_courses tc ON tc.id = $3
		JOIN academic_terms trm ON trm.id = tc.term_id
		LEFT JOIN submission_period_status st
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
		       -- Same grad-special exclusion as GetTimeline above.
		       (SELECT COUNT(*) FROM work_logs wl
		          JOIN ta_request_assignments a2 ON a2.id = wl.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = u.id AND s2.teaching_course_id = tc.id
		           AND `+workLogInPeriodSQL("wl", "trm", "sp")+`
		           AND (a2.level::text NOT IN ('master','phd') OR s2.track <> 'special')),
		       (SELECT COUNT(*) FROM work_logs wl
		          JOIN ta_request_assignments a2 ON a2.id = wl.assignment_id
		          JOIN sections s2 ON s2.id = a2.section_id
		         WHERE a2.ta_id = u.id AND s2.teaching_course_id = tc.id
		           AND `+workLogInPeriodSQL("wl", "trm", "sp")+`
		           AND (a2.level::text NOT IN ('master','phd') OR s2.track <> 'special')
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
		FROM teaching_courses tc
		JOIN academic_terms trm ON trm.id = tc.term_id
		JOIN submission_periods sp ON sp.term_id = tc.term_id
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
		         tc.id, tc.code, tc.name_th, trm.academic_year,
		         st.status, st.exported_at, st.exported_by, st.exported_name,
		         st.finance_sent_at, st.finance_sent_by, st.finance_sent_name, st.finance_note,
		         st.sent_back_at, st.sent_back_by, st.sent_back_name, st.sent_back_reason
		ORDER BY `+periodOrderSQL("sp.year_month")+`, u.first_name, u.last_name`, args...)
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
	return out, rows.Err()
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
			JOIN sections sec ON sec.teaching_course_id = tc.id
			JOIN ta_request_assignments a ON a.section_id = sec.id
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
			  -- Grad-special no longer logs work_logs at all — reminding them to
			  -- "บันทึกเวลาปฏิบัติงาน...ให้ครบ" is both wrong (they log nothing)
			  -- and would fire every 24h forever since st.status never leaves
			  -- 'pending' for a TA who never submits.
			  AND (a.level::text NOT IN ('master','phd') OR sec.track <> 'special')
			  AND a.state <> 'dropped'
			  -- A TA whose every row of the month is already sent or approved has
			  -- nothing left to do; "please log and submit" daily until the due
			  -- date only teaches them to ignore the reminder. No rows at all, or
			  -- any draft/rejected row, still counts as pending.
			  AND NOT (
			    EXISTS (SELECT 1 FROM work_logs wl
			              JOIN ta_request_assignments a2 ON a2.id = wl.assignment_id
			              JOIN sections s2 ON s2.id = a2.section_id
			              JOIN academic_terms trm ON trm.id = sp.term_id
			             WHERE a2.ta_id = a.ta_id AND s2.teaching_course_id = tc.id
			               AND `+workLogInPeriodSQL("wl", "trm", "sp")+`)
			    AND NOT EXISTS (SELECT 1 FROM work_logs wl
			              JOIN ta_request_assignments a2 ON a2.id = wl.assignment_id
			              JOIN sections s2 ON s2.id = a2.section_id
			              JOIN academic_terms trm ON trm.id = sp.term_id
			             WHERE a2.ta_id = a.ta_id AND s2.teaching_course_id = tc.id
			               AND wl.status IN ('draft','rejected')
			               AND `+workLogInPeriodSQL("wl", "trm", "sp")+`))
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
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, it := range items {
		body := fmt.Sprintf("ขอให้ท่านบันทึกเวลาปฏิบัติงานประจำงวด %s รายวิชา %s %s ให้ครบถ้วน และส่งให้อาจารย์ผู้สอนพิจารณาอนุมัติภายในวันที่ %s",
			it.label, it.code, it.nm, thaiLongDateISO(it.due))
		// Code in the title: unread notices fold by (title, link), so a TA in
		// two courses otherwise kept only the last course's reminder.
		s.notify.SendAction(ctx, it.taID, "ใกล้ครบกำหนดส่งบันทึกเวลาปฏิบัติงาน "+it.code, body, "/ta/reminders")
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
