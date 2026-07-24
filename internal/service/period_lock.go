package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// period_lock.go maps work_log writes onto the monthly submission workflow so
// the two state machines stop drifting apart:
//   - a month whose submission_period_status reached 'exported' or
//     'finance_sent' is frozen for EVERY role (staff already generated the
//     payout file; the numbers must not change under it). Admin can send it
//     back to pending to reopen for edits;
//   - a month whose submission_period is closed (is_closed, or past the
//     due_date + 1-day grace used by AutoCloseExpired) is frozen for TA actors
//     only — lecturers can still approve stragglers and staff can still fix
//     rows on the TA's behalf.
//
// year_month mapping: submission_periods.year_month is the term's Buddhist
// ACADEMIC year + submission month (see BulkCreateForTerm — semester-2 months
// Jan–Mar keep the term's academic_year even though the Gregorian calendar
// year has advanced), so a work_date resolves to its period via the course's
// term: academic_year::text || '-' || to_char(work_date, 'MM'). Never derive
// the year from the work_date itself.

// periodState is the resolved submission-period condition covering one
// (teaching_course, ta, work_date). Found=false when the term simply has no
// period defined for that month — treated as unrestricted for backward
// compatibility with terms that never adopted the monthly workflow.
type periodState struct {
	Found    bool
	Label    string
	IsClosed bool   // is_closed flag OR past due_date + 1-day grace
	Status   string // submission_period_status.status, 'pending' when no row
}

func resolvePeriodState(ctx context.Context, pool *pgxpool.Pool, tcID, taID uuid.UUID, workDate string) (periodState, error) {
	rows, err := pool.Query(ctx, `
		SELECT sp.label,
		       (sp.is_closed OR CURRENT_DATE > sp.due_date + INTERVAL '1 day'),
		       COALESCE(st.status, 'pending')
		FROM teaching_courses tc
		JOIN academic_terms trm ON trm.id = tc.term_id
		JOIN submission_periods sp ON sp.term_id = tc.term_id
		 AND sp.year_month = trm.academic_year::text || '-' || to_char($3::date, 'MM')
		LEFT JOIN submission_period_status st
		  ON st.submission_period_id = sp.id
		 AND st.ta_id = $2
		 AND st.teaching_course_id = tc.id
		WHERE tc.id = $1`, tcID, taID, workDate)
	if err != nil {
		return periodState{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return periodState{}, rows.Err()
	}
	st := periodState{Found: true}
	if err := rows.Scan(&st.Label, &st.IsClosed, &st.Status); err != nil {
		return periodState{}, err
	}
	return st, nil
}

// assertWorklogWritable rejects a write touching workDate when its month is
// finance-locked (every role) or closed (TA actors only). No period defined
// for that month → allowed.
func assertWorklogWritable(ctx context.Context, pool *pgxpool.Pool, tcID, taID uuid.UUID, workDate string, taActor bool) error {
	st, err := resolvePeriodState(ctx, pool, tcID, taID, workDate)
	if err != nil {
		return err
	}
	if !st.Found {
		return nil
	}
	if st.Status == "finance_sent" {
		return Conflict(fmt.Sprintf(
			"บันทึกเวลาเดือน %s ถูกส่งการเงินแล้ว — แก้ไขไม่ได้ (ผู้ดูแลระบบปลดล็อกได้เท่านั้น)", st.Label))
	}
	if st.Status == "exported" {
		return Conflict(fmt.Sprintf(
			"บันทึกเวลาเดือน %s ถูกส่งออกไฟล์เบิกจ่ายแล้ว — แก้ไขไม่ได้ (เจ้าหน้าที่ตีกลับหรือผู้ดูแลระบบปลดล็อกได้)", st.Label))
	}
	if taActor && st.IsClosed {
		return Invalid(fmt.Sprintf(
			"งวดส่งบันทึกเวลาเดือน %s ปิดรับแล้ว — ไม่สามารถเพิ่ม/แก้ไข/ส่งรายการของเดือนนี้ได้ กรุณาติดต่อเจ้าหน้าที่", st.Label))
	}
	return nil
}

// financeLockedMonths returns the labels of every locked (exported or
// finance_sent) month that contains at least one of the assignment's work_logs
// in the given statuses. Used as a batch pre-check by Approve/Reject so a
// whole-batch transition cannot touch rows already committed to a payout file.
func financeLockedMonths(ctx context.Context, pool *pgxpool.Pool, assignmentID uuid.UUID, statuses []string, yearMonth string) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT sp.label
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		JOIN sections sec ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		JOIN academic_terms trm ON trm.id = tc.term_id
		JOIN submission_periods sp ON sp.term_id = tc.term_id
		 AND sp.year_month = trm.academic_year::text || '-' || to_char(wl.work_date, 'MM')
		JOIN submission_period_status st
		  ON st.submission_period_id = sp.id
		 AND st.ta_id = a.ta_id
		 AND st.teaching_course_id = tc.id
		 AND st.status IN ('exported','finance_sent')
		WHERE wl.assignment_id = $1 AND wl.status = ANY($2)
		  AND ($3 = '' OR to_char(wl.work_date, 'YYYY-MM') = $3)
		ORDER BY sp.label`, assignmentID, statuses, yearMonth)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// assertNoFinanceLockedRows is the Conflict-raising form of financeLockedMonths
// for whole-batch transitions (Approve/Reject).
// yearMonth ("YYYY-MM") narrows the check to a single month; "" = whole batch.
func assertNoFinanceLockedRows(ctx context.Context, pool *pgxpool.Pool, assignmentID uuid.UUID, statuses []string, yearMonth string) error {
	months, err := financeLockedMonths(ctx, pool, assignmentID, statuses, yearMonth)
	if err != nil {
		return err
	}
	if len(months) > 0 {
		return Conflict(fmt.Sprintf(
			"ดำเนินการไม่ได้ — มีรายการในเดือนที่ส่งออกไฟล์/ส่งการเงินแล้ว: %s", strings.Join(months, ", ")))
	}
	return nil
}

// blockedMonth is one month's lock condition, keyed by its 2-digit month
// string ("06"). A term's periods all share one academic year, so MM alone is
// unambiguous within a course. Locked is true once the month is exported or
// finance_sent (frozen for every role).
type blockedMonth struct {
	Label    string
	IsClosed bool
	Locked   bool
}

// loadBlockedMonths returns every submission period of the course's term with
// its closed/locked state for one TA, keyed by "MM". Used by Generate to skip
// whole months in one preload instead of a per-date query.
func loadBlockedMonths(ctx context.Context, pool *pgxpool.Pool, tcID, taID uuid.UUID) (map[string]blockedMonth, error) {
	rows, err := pool.Query(ctx, `
		SELECT RIGHT(sp.year_month, 2), sp.label,
		       (sp.is_closed OR CURRENT_DATE > sp.due_date + INTERVAL '1 day'),
		       (COALESCE(st.status, '') IN ('exported','finance_sent'))
		FROM teaching_courses tc
		JOIN submission_periods sp ON sp.term_id = tc.term_id
		LEFT JOIN submission_period_status st
		  ON st.submission_period_id = sp.id
		 AND st.ta_id = $2
		 AND st.teaching_course_id = tc.id
		WHERE tc.id = $1`, tcID, taID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]blockedMonth{}
	for rows.Next() {
		var mm string
		var bm blockedMonth
		if err := rows.Scan(&mm, &bm.Label, &bm.IsClosed, &bm.Locked); err != nil {
			return nil, err
		}
		out[mm] = bm
	}
	return out, rows.Err()
}
