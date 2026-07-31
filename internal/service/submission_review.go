package service

import (
	"context"
	"errors"
	"fmt"

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
}

// ListReviewQueue returns every (period, TA, course) whose month has approved
// work and has not yet been exported, so staff can see what is waiting on them
// and what they have already released.
//
// Scoped to a term because staff work one term at a time and the cross-term
// list would be dominated by history.
func (s *SubmissionPeriodService) ListReviewQueue(ctx context.Context, termID uuid.UUID) ([]ReviewQueueRow, error) {
	rows, err := s.pool.Query(ctx, `
		WITH month_logs AS (
		    SELECT sp.id   AS period_id,
		           a.ta_id AS ta_id,
		           tc.id   AS tc_id,
		           SUM(wl.hours) FILTER (WHERE wl.status = 'approved')                      AS approved_hours,
		           COUNT(*)      FILTER (WHERE wl.status IN ('draft','submitted','rejected')) AS open_rows
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
		      AND ` + AppointedSQL("tc.id", "a.ta_id") + `
		    GROUP BY sp.id, a.ta_id, tc.id
		)
		SELECT sp.id, sp.label, sp.year_month,
		       u.id, u.first_name || ' ' || u.last_name,
		       tc.id, tc.code, tc.name_th,
		       COALESCE(st.status, 'pending'),
		       COALESCE(ml.approved_hours, 0),
		       COALESCE(ml.open_rows, 0)
		FROM month_logs ml
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
			&r.Status, &r.ApprovedHours, &r.OpenRows); err != nil {
			return nil, err
		}
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
		WITH latest AS (SELECT * FROM pay_rates ORDER BY effective_from DESC LIMIT 1)
		SELECT COALESCE(SUM(wl.hours *
		    CASE
		        WHEN a.level = 'undergrad' AND sec.track = 'regular' THEN pr.undergrad_regular
		        WHEN a.level = 'undergrad' AND sec.track = 'special' THEN pr.undergrad_special
		        WHEN a.level IN ('master','phd') AND sec.track = 'regular' THEN pr.graduate_regular_hourly
		        ELSE 0
		    END), 0)
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		JOIN sections sec ON sec.id = a.section_id
		CROSS JOIN latest pr
		WHERE a.ta_id = $1 AND sec.teaching_course_id = $2
		  AND wl.status = 'approved'
		  AND to_char(wl.work_date, 'MM') = RIGHT($3, 2)`,
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
	tag, err := s.pool.Exec(ctx, `
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

	s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "submission.staff_reviewed",
		Entity: "submission_period_status", EntityID: periodID.String(),
		Note: fmt.Sprintf("ta=%s course=%s", taID, tcID),
	})
	return nil
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
