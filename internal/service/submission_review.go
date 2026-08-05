package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/audit"
)

// submission_review.go adds the step the 24/07/2026 meeting asked for between
// the lecturer's approval and the payout export: เจ้าหน้าที่ตรวจสอบเบิกจ่ายค่าตอบแทน.
//
// Before this, "staff reviewed the numbers" was a habit, not a state. Export
// accepted anything the lecturer had approved, so a mistake caught by staff had
// no way of being recorded, and there was no answer to "who checked this?" once
// the money left. The monthly lifecycle now reads:
//
//	pending → staff_reviewed → exported → finance_sent
//
// The columns this writes (staff_reviewed_by, staff_reviewed_name,
// staff_comment) have existed on submission_period_status since the table was
// created but were never written by anything — they were scaffolding for
// exactly this step.

// StatusStaffReviewed is the state a (period, TA, course) row reaches once
// staff have checked the hours and money and released it for export.
const StatusStaffReviewed = "staff_reviewed"

// ReviewQueueRow is one unit of staff review work: a single TA's month on a
// single course. Carries the totals so the queue can be triaged without
// opening each row.
type ReviewQueueRow struct {
	PeriodID         uuid.UUID `json:"period_id"`
	PeriodLabel      string    `json:"period_label"`
	YearMonth        string    `json:"year_month"`
	TAID             uuid.UUID `json:"ta_id"`
	TAName           string    `json:"ta_name"`
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	CourseCode       string    `json:"course_code"`
	CourseNameTH     string    `json:"course_name_th"`
	Status           string    `json:"status"`
	// Hours/baht already approved by the lecturer for this month.
	ApprovedHours float64 `json:"approved_hours"`
	ApprovedBaht  float64 `json:"approved_baht"`
	// Rows still in the TA's or lecturer's hands. Non-zero means the month is
	// not ready for staff to sign off, and the queue says so rather than
	// letting staff approve a moving target.
	OpenRows int `json:"open_rows"`
	// Forfeited counts rows the TA never sent before their period closed. They
	// read as "TA ยังไม่ส่ง" on the staff grid until 04/08/2026, which was both
	// wrong — nobody is waiting on the TA, the deadline has passed — and a dead
	// end: a month whose only open rows were forfeited could never be signed off.
	Forfeited int `json:"forfeited"`
	// Who the month is actually waiting on. OpenRows alone could not say, so the
	// merged payout screen blamed the lecturer for rows the TA had not even
	// submitted — and offered a "remind the lecturer" button the server then
	// refused, because there was nothing in their queue.
	//   WaitingTA       = draft + rejected, still with the TA
	//   WaitingLecturer = submitted, sitting in the lecturer's approval queue
	WaitingTA       int `json:"waiting_ta"`
	WaitingLecturer int `json:"waiting_lecturer"`
	// What is inside the month, so the grid cell can say whether opening it is
	// worth the click. RowCount is the approved rows behind ApprovedHours;
	// ManualCount is how many of them the TA typed themselves.
	//
	// ManualCount is the only number on that screen that selects work for a
	// human: a generated row copies class times the lecturer already entered and
	// holds nothing a second person can verify, so a month that is entirely
	// generated can be signed off without reading it. Without this the officer
	// had to open all five months of every TA to find out which one had a claim
	// in it.
	RowCount    int `json:"row_count"`
	ManualCount int `json:"manual_count"`
	// NeedsStaff is the one answer to "is this month work for the officer".
	//
	// Three things have to be true: staff have not signed it off, nobody else
	// still holds a row, and there IS approved work in it. That last clause is
	// the one both screens have to agree on — a month whose rows were all
	// forfeited has nothing to sign and nothing to pay, and the staff grid
	// already skipped it while the payout LIST still counted it, so two courses
	// sat under "รอคุณดำเนินการ" over months worth ฿0 with no way to clear them.
	NeedsStaff bool `json:"needs_staff"`
}

// needsStaff decides whether this month is work for the officer. Kept as one
// function so the flag the screens read and the rule anybody edits are the same
// thing — the two screens deriving it independently is the bug it exists to
// close.
func (r ReviewQueueRow) needsStaff() bool {
	return r.Status == "pending" && // nobody has signed it off yet
		r.OpenRows == 0 && //   nothing still with the TA or the lecturer
		r.RowCount > 0 //       and there IS approved work to sign
}

// ListReviewQueue returns every (period, TA, course) whose month has approved
// work and has not yet been exported, so staff can see what is waiting on them
// and what they have already released.
//
// Scoped to a term because staff work one term at a time and the cross-term
// list would be dominated by history.
func (s *SubmissionPeriodService) ListReviewQueue(ctx context.Context, termID uuid.UUID) ([]ReviewQueueRow, error) {
	rows, err := s.pool.Query(ctx, `
		WITH`+mergedSittingsCTE+`,
		-- Approved hours are counted as SITTINGS, matching the signed claim form:
		-- two sections taught in one sitting are paid once (billable_hours.go).
		-- The open/waiting counts below stay row-based — they are workload, not
		-- money, and an officer chasing four unsubmitted rows wants four.
		month_sittings AS (
		    SELECT ta_id, teaching_course_id,
		           to_char(work_date, 'MM') AS mm,
		           SUM(hours) AS approved_hours
		    FROM sittings
		    GROUP BY ta_id, teaching_course_id, to_char(work_date, 'MM')
		),
		month_logs AS (
		    SELECT sp.id   AS period_id,
		           a.ta_id AS ta_id,
		           tc.id   AS tc_id,
		           RIGHT(sp.year_month, 2) AS mm,
		           -- Rows the TA can no longer move: unsent when their period
		           -- closed. They are NOT outstanding work — nobody is going to
		           -- send them — so they must not sit in open_rows, where they
		           -- would block staff from ever signing the month off and the
		           -- course from ever exporting.
		           COUNT(*)      FILTER (WHERE wl.status IN ('draft','rejected')
		                                   AND `+unsubmittableMonthSQL("wl")+`)      AS forfeited,
		           COUNT(*)      FILTER (WHERE `+outstandingRowSQL("wl")+`)   AS open_rows,
		           COUNT(*)      FILTER (WHERE `+waitingTASQL("wl")+`)         AS waiting_ta,
		           COUNT(*)      FILTER (WHERE `+waitingLecturerSQL("wl")+`)   AS waiting_lecturer,
		           COUNT(*)      FILTER (WHERE wl.status = 'approved')                        AS row_count,
		           COUNT(*)      FILTER (WHERE wl.status = 'approved' AND wl.source = 'manual') AS manual_count
		    FROM teaching_courses tc
		    JOIN submission_periods sp ON sp.term_id = tc.term_id
		    JOIN sections sec          ON sec.teaching_course_id = tc.id
		    JOIN ta_request_assignments a ON a.section_id = sec.id AND a.state <> 'dropped'
		    JOIN ta_requests r         ON r.id = a.request_id AND r.status = 'approved'
		    JOIN work_logs wl          ON wl.assignment_id = a.id
		                              AND to_char(wl.work_date, 'MM') = RIGHT(sp.year_month, 2)
		    WHERE tc.term_id = $1
		      -- Nothing to review until the appointment order is printed: the
		      -- work is not yet payable, and signing it off here would release
		      -- it to an export the finance office cannot accept.
		      AND `+AppointedSQL("tc.id", "a.ta_id")+`
		    GROUP BY sp.id, a.ta_id, tc.id, RIGHT(sp.year_month, 2)
		)
		SELECT sp.id, sp.label, sp.year_month,
		       u.id, u.first_name || ' ' || u.last_name,
		       tc.id, tc.code, tc.name_th,
		       COALESCE(st.status, 'pending'),
		       COALESCE(ms.approved_hours, 0),
		       COALESCE(ml.forfeited, 0),
		       COALESCE(ml.open_rows, 0),
		       COALESCE(ml.waiting_ta, 0),
		       COALESCE(ml.waiting_lecturer, 0),
		       COALESCE(ml.row_count, 0),
		       COALESCE(ml.manual_count, 0)
		FROM month_logs ml
		LEFT JOIN month_sittings ms
		       ON ms.ta_id = ml.ta_id AND ms.teaching_course_id = ml.tc_id
		      AND ms.mm = ml.mm
		JOIN submission_periods sp ON sp.id = ml.period_id
		JOIN users u               ON u.id = ml.ta_id
		JOIN teaching_courses tc   ON tc.id = ml.tc_id
		LEFT JOIN submission_period_status st
		       ON st.submission_period_id = sp.id
		      AND st.ta_id = ml.ta_id
		      AND st.teaching_course_id = tc.id
		WHERE COALESCE(st.status, 'pending') NOT IN ('finance_sent', 'skipped')
		ORDER BY sp.year_month, tc.code, u.first_name`, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ReviewQueueRow{}
	for rows.Next() {
		var r ReviewQueueRow
		if err := rows.Scan(&r.PeriodID, &r.PeriodLabel, &r.YearMonth,
			&r.TAID, &r.TAName, &r.TeachingCourseID, &r.CourseCode, &r.CourseNameTH,
			&r.Status, &r.ApprovedHours, &r.Forfeited, &r.OpenRows,
			&r.WaitingTA, &r.WaitingLecturer,
			&r.RowCount, &r.ManualCount); err != nil {
			return nil, err
		}
		r.NeedsStaff = r.needsStaff()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Money is derived per row rather than in the aggregate above: the rate
	// depends on the TA's level and the section's track, which the GROUP BY
	// would have to carry through every join. Row counts here are per month per
	// course, so this stays small.
	for i := range out {
		baht, err := s.approvedBahtForMonth(ctx, out[i].TAID, out[i].TeachingCourseID, out[i].YearMonth)
		if err != nil {
			return nil, err
		}
		out[i].ApprovedBaht = baht
	}
	return out, nil
}

// CountAwaitingAppointment counts the (period, TA, course) rows that WOULD be in
// the review queue but for the missing appointment order.
//
// Without this the queue can only go silently empty, and an officer with work
// waiting has no way to tell "nothing to do" from "the order has not been printed
// yet" — a difference of one action on a different screen. Counted the same way
// ListReviewQueue counts, with the appointment test inverted, so the two numbers
// always describe the same population.
func (s *SubmissionPeriodService) CountAwaitingAppointment(ctx context.Context, termID uuid.UUID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM (
		    SELECT sp.id, a.ta_id, tc.id
		    FROM teaching_courses tc
		    JOIN submission_periods sp ON sp.term_id = tc.term_id
		    JOIN sections sec          ON sec.teaching_course_id = tc.id
		    JOIN ta_request_assignments a ON a.section_id = sec.id AND a.state <> 'dropped'
		    JOIN ta_requests r         ON r.id = a.request_id AND r.status = 'approved'
		    JOIN work_logs wl          ON wl.assignment_id = a.id
		                              AND to_char(wl.work_date, 'MM') = RIGHT(sp.year_month, 2)
		    LEFT JOIN submission_period_status st
		           ON st.submission_period_id = sp.id
		          AND st.ta_id = a.ta_id
		          AND st.teaching_course_id = tc.id
		    WHERE tc.term_id = $1
		      AND COALESCE(st.status, 'pending') NOT IN ('finance_sent', 'skipped')
		      AND NOT `+AppointedSQL("tc.id", "a.ta_id")+`
		    GROUP BY sp.id, a.ta_id, tc.id
		) q`, termID).Scan(&n)
	return n, err
}

// approvedBahtForMonth totals the approved hourly-billed pay for one TA's month
// on one course. Grad-special contributes 0 — it is a flat monthly lump sum
// handled by the export, not an hourly accrual.
func (s *SubmissionPeriodService) approvedBahtForMonth(ctx context.Context, taID, tcID uuid.UUID, yearMonth string) (float64, error) {
	var baht float64
	err := s.pool.QueryRow(ctx, `
		WITH latest AS (SELECT * FROM pay_rates ORDER BY effective_from DESC LIMIT 1),`+
		mergedSittingsCTE+`
		-- Priced from SITTINGS, not from work_log rows: the same rule the claim
		-- form bills by (billable_hours.go).
		SELECT COALESCE(SUM(st.hours *
		    CASE
		        WHEN st.level = 'undergrad' AND st.track = 'regular' THEN pr.undergrad_regular
		        WHEN st.level = 'undergrad' AND st.track = 'special' THEN pr.undergrad_special
		        WHEN st.level IN ('master','phd') AND st.track = 'regular' THEN pr.graduate_regular_hourly
		        ELSE 0
		    END), 0)
		FROM sittings st
		CROSS JOIN latest pr
		WHERE st.ta_id = $1 AND st.teaching_course_id = $2
		  AND to_char(st.work_date, 'MM') = RIGHT($3, 2)`,
		taID, tcID, yearMonth).Scan(&baht)
	return baht, err
}

// MarkStaffReviewed records that staff checked this TA's month and released it
// for export.
//
// Refuses while any row is still draft/submitted/rejected: signing off on a
// month the TA can still edit would make the signature meaningless, and the
// export gate downstream trusts this state.
func (s *SubmissionPeriodService) MarkStaffReviewed(ctx context.Context, actor, periodID, taID, tcID uuid.UUID, comment string) error {
	if err := s.assertPrivileged(ctx, actor); err != nil {
		return err
	}
	if err := s.assertSignTarget(ctx, periodID, taID, tcID); err != nil {
		return err
	}

	var yearMonth string
	if err := s.pool.QueryRow(ctx,
		`SELECT year_month FROM submission_periods WHERE id = $1`, periodID).Scan(&yearMonth); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	total, unapproved, err := s.monthWorklogReadiness(ctx, taID, tcID, yearMonth)
	if err != nil {
		return err
	}
	if total == 0 {
		return Invalid("เดือนนี้ยังไม่มีรายการบันทึกเวลาที่อนุมัติแล้ว จึงยังตรวจสอบไม่ได้")
	}
	if unapproved > 0 {
		return Invalid(fmt.Sprintf(
			"ยังมี %d รายการที่อาจารย์ยังไม่อนุมัติ — ต้องให้ครบก่อน จึงจะตรวจสอบเบิกจ่ายได้", unapproved))
	}

	name := s.userDisplayName(ctx, actor)
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{
			ActorID: &actor, Action: "submission.staff_reviewed",
			Entity: "submission_period_status", EntityID: periodID.String(),
			Note: fmt.Sprintf("ta=%s course=%s", taID, tcID),
		},
		func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
				INSERT INTO submission_period_status
				    (id, submission_period_id, ta_id, teaching_course_id, status,
				     staff_reviewed_by, staff_reviewed_name, staff_comment)
				VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, NULLIF($7, ''))
				ON CONFLICT (submission_period_id, ta_id, teaching_course_id) DO UPDATE
				SET status              = $4,
				    staff_reviewed_by   = EXCLUDED.staff_reviewed_by,
				    staff_reviewed_name = EXCLUDED.staff_reviewed_name,
				    staff_comment       = EXCLUDED.staff_comment
				WHERE submission_period_status.status = 'pending'`,
				periodID, taID, tcID, StatusStaffReviewed, actor, name, comment)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return Invalid("รายการนี้ผ่านการตรวจสอบหรือส่งออกไปแล้ว")
			}
			return nil
		})
}

// UnreviewedCourseNames lists the courses in a term that still have months
// waiting on staff review, so the export screen can name them instead of
// silently offering fewer downloads than expected.
func (s *SubmissionPeriodService) UnreviewedCourseNames(ctx context.Context, tcID uuid.UUID) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT sp.label
		FROM teaching_courses tc
		JOIN submission_periods sp ON sp.term_id = tc.term_id
		JOIN sections sec          ON sec.teaching_course_id = tc.id
		JOIN ta_request_assignments a ON a.section_id = sec.id AND a.state <> 'dropped'
		JOIN ta_requests r         ON r.id = a.request_id AND r.status = 'approved'
		JOIN work_logs wl          ON wl.assignment_id = a.id
		                          AND to_char(wl.work_date, 'MM') = RIGHT(sp.year_month, 2)
		                          AND wl.status = 'approved'
		LEFT JOIN submission_period_status st
		       ON st.submission_period_id = sp.id
		      AND st.ta_id = a.ta_id
		      AND st.teaching_course_id = tc.id
		WHERE tc.id = $1
		  AND COALESCE(st.status, 'pending') = 'pending'
		ORDER BY sp.label`, tcID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, err
		}
		out = append(out, label)
	}
	return out, rows.Err()
}

// RemindLecturerUnapproved nudges a course's lecturers that TA work is still
// sitting unapproved in their queue.
//
// Added 31/07/2026 with the merged payout screen. The "waiting on someone else"
// group is, by definition, work the officer cannot do — and until now the screen
// offered them nothing but the word "waiting". A month can sit there because a
// lecturer forgot, and the officer's only recourse was to leave the system and
// send a message by hand.
//
// Throttled to one reminder per course per day, read off audit_logs rather than
// a new table: the reminder IS an auditable act, so the record it must leave is
// the same record the throttle needs.
func (s *SubmissionPeriodService) RemindLecturerUnapproved(ctx context.Context, actor, tcID uuid.UUID) error {
	var code, nameTH string
	if err := s.pool.QueryRow(ctx,
		`SELECT code, COALESCE(name_th,'') FROM teaching_courses WHERE id = $1`, tcID).
		Scan(&code, &nameTH); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}

	var lastSent *time.Time
	if err := s.pool.QueryRow(ctx, `
		SELECT MAX(at) FROM audit_logs
		 WHERE action = 'payout.remind_lecturer' AND entity_id = $1`,
		tcID.String()).Scan(&lastSent); err != nil {
		return err
	}
	if lastSent != nil && time.Since(*lastSent) < 24*time.Hour {
		return Invalid("แจ้งเตือนอาจารย์วิชานี้ไปแล้ววันนี้ — ส่งได้อีกครั้งใน 24 ชั่วโมง")
	}

	// How much is actually outstanding. Sending "please approve" without the
	// number is what makes reminders easy to ignore.
	var openRows int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM sections sec
		JOIN ta_request_assignments a ON a.section_id = sec.id AND a.state <> 'dropped'
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		JOIN work_logs wl  ON wl.assignment_id = a.id AND wl.status = 'submitted'
		WHERE sec.teaching_course_id = $1`, tcID).Scan(&openRows); err != nil {
		return err
	}
	if openRows == 0 {
		return Invalid("ไม่มีรายการที่รออาจารย์อนุมัติในวิชานี้แล้ว")
	}

	rows, err := s.pool.Query(ctx,
		`SELECT lecturer_id FROM teaching_lecturers WHERE teaching_course_id = $1`, tcID)
	if err != nil {
		return err
	}
	var lecturerIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		lecturerIDs = append(lecturerIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(lecturerIDs) == 0 {
		return Invalid("วิชานี้ยังไม่มีอาจารย์ผู้สอนในระบบ จึงยังแจ้งเตือนไม่ได้")
	}

	title := "มีบันทึกเวลา TA รออนุมัติ"
	body := fmt.Sprintf(
		"%s %s — มี %d รายการที่ TA ส่งมาแล้วและรอการอนุมัติของอาจารย์ "+
			"เจ้าหน้าที่ยังตรวจเบิกจ่ายเดือนนั้นไม่ได้จนกว่าจะอนุมัติครบ",
		code, nameTH, openRows)
	for _, id := range lecturerIDs {
		// The lecturer approves on .../reports; there is no .../worklog for them.
		s.notify.Send(ctx, id, title, body, "/lecturer/courses/"+tcID.String()+"/reports")
	}

	if err := s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "payout.remind_lecturer",
		Entity: "teaching_course", EntityID: tcID.String(),
		Note: fmt.Sprintf("%s — %d รายการรออนุมัติ, แจ้ง %d คน", code, openRows, len(lecturerIDs)),
	}); err != nil {
		return err
	}
	return nil
}
