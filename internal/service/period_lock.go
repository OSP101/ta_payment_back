package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// period_lock.go maps work_log writes onto the monthly submission workflow so
// the two state machines stop drifting apart:
//   - a month whose submission_period_status reached 'exported' or
//     'finance_sent' is frozen for EVERY role (staff already generated the
//     payout file; the numbers must not change under it). Admin can send it
//     back to pending to reopen for edits;
//   - a month whose submission_period is closed (is_closed, or past the
//     due_date + 1-day grace used by AutoCloseExpired) is frozen for EVERY
//     role. It used to be frozen for TA actors only, so staff could still edit
//     on a TA's behalf; that was closed on 03/08/2026 at the staff's own
//     request — no back-dated entry at all, by anyone. Rows not submitted by
//     the deadline are treated as not claimed (ไม่ประสงค์ลงเวลา).
//
//     Approving is NOT a write: a lecturer can still approve rows a TA
//     submitted before the deadline, which is the whole point of having one.
//
// year_month mapping: submission_periods.year_month is the term's Buddhist
// ACADEMIC year + submission month (see BulkCreateForTerm — semester-2 months
// Jan–Mar keep the term's academic_year even though the Gregorian calendar
// year has advanced), so a work_date resolves to its period via the course's
// term: academic_year::text || '-' || to_char(work_date, 'MM'). Never derive
// the year from the work_date itself.

// workLogInPeriodSQL คืนเงื่อนไข join ระหว่าง work_logs กับ submission_periods
//
// มีที่เดียว เพราะ ~15 query เคยเขียนซ้ำกันเองด้วย MM อย่างเดียว
// (to_char(work_date,'MM') = RIGHT(sp.year_month, 2)) ซึ่งทิ้งปีทิ้ง และ
// ปลอดภัยเฉพาะโดยบังเอิญที่ subquery ทุกอันถูกจำกัดด้วย teaching_course_id
// เดียว (คอร์สหนึ่งอยู่ในเทอมเดียวซึ่งสั้นกว่า 12 เดือน จึงไม่มีเดือนซ้ำ) —
// มันจะพังทันทีที่มีคอร์สข้ามปีปฏิทิน, แถว teaching_courses ถูกใช้ซ้ำ, หรือมี
// ภาคฤดูร้อนต่อท้ายคอร์สเดิม
//
// year_month เป็นปีการศึกษาพุทธศักราช ไม่ใช่ปีปฏิทิน (ดูคอมเมนต์ข้างบนเรื่อง
// year_month mapping) จึงเทียบกับ to_char(work_date,'YYYY-MM') ตรง ๆ ไม่ได้ —
// ต้องประกอบจากปีการศึกษาของเทอมเหมือนที่ resolvePeriodState/financeLockedMonths
// ทำอยู่แล้ว
func workLogInPeriodSQL(wlAlias, termAlias, spAlias string) string {
	return fmt.Sprintf("%s.academic_year::text || '-' || to_char(%s.work_date,'MM') = %s.year_month",
		termAlias, wlAlias, spAlias)
}

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

// monthClosedForCourse is the TA-agnostic half of resolvePeriodState: whether
// the submission_periods row covering workDate's month is closed (is_closed,
// or past due_date + 1-day grace), without the per-TA submission_period_status
// join. found=false means no period is defined for that month at all —
// callers treat that as unrestricted, matching resolvePeriodState's own
// backward-compatibility stance for terms that never adopted the monthly
// workflow.
func monthClosedForCourse(ctx context.Context, pool *pgxpool.Pool, tcID uuid.UUID, workDate string) (closed bool, label string, found bool, err error) {
	err = pool.QueryRow(ctx, `
		SELECT sp.label, (sp.is_closed OR CURRENT_DATE > sp.due_date + INTERVAL '1 day')
		FROM teaching_courses tc
		JOIN academic_terms trm ON trm.id = tc.term_id
		JOIN submission_periods sp ON sp.term_id = tc.term_id
		 AND sp.year_month = trm.academic_year::text || '-' || to_char($2::date, 'MM')
		WHERE tc.id = $1`, tcID, workDate).Scan(&label, &closed)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, "", false, nil
		}
		return false, "", false, err
	}
	return closed, label, true, nil
}

// assertMonthOpenForCourse rejects an action dated in a month whose
// submission_periods row is closed — for call sites (like filing a makeup
// class) that act on a whole section rather than one TA's worklog.
func assertMonthOpenForCourse(ctx context.Context, pool *pgxpool.Pool, tcID uuid.UUID, workDate string) error {
	closed, label, found, err := monthClosedForCourse(ctx, pool, tcID, workDate)
	if err != nil {
		return err
	}
	if found && closed {
		return Invalid(fmt.Sprintf(
			"งวดส่งบันทึกเวลาเดือน %s ปิดแล้ว กำหนดวันชดเชยย้อนหลังเข้าไปไม่ได้", label))
	}
	return nil
}

// isMonthOpenForCourse reports whether workDate's month has a submission
// period that staff have EXPLICITLY confirmed is still open (found and not
// closed) — used to let validateWorkLogEntry's own back-dating rule defer to
// the real period configuration instead of a bare "is this the current
// calendar month" guess. A period staff extended past month-end (the
// "ระยะเวลาเบิกจ่ายรายเดือน" screen sets due_date independent of the calendar
// month) must count as open here even though the calendar month has passed.
// Errors and "no period configured" both resolve to false — the caller's
// existing calendar-month rule is the fallback for that case, not a reason to
// loosen anything.
func isMonthOpenForCourse(ctx context.Context, pool *pgxpool.Pool, tcID uuid.UUID, workDate string) bool {
	closed, _, found, err := monthClosedForCourse(ctx, pool, tcID, workDate)
	if err != nil || !found {
		return false
	}
	return !closed
}

// assertWorklogWritable rejects a write touching workDate when its month is
// finance-locked or closed. No period defined for that month → allowed.
//
// There is deliberately no actor parameter. There was one, and it made "closed"
// mean two different things depending on who asked — which is how a deadline
// stops being a deadline.
func assertWorklogWritable(ctx context.Context, pool *pgxpool.Pool, tcID, taID uuid.UUID, workDate string) error {
	st, err := resolvePeriodState(ctx, pool, tcID, taID, workDate)
	if err != nil {
		return err
	}
	if !st.Found {
		return nil
	}
	if st.Status == "finance_sent" {
		return Conflict(fmt.Sprintf(
			"บันทึกเวลาเดือน %s ถูกส่งการเงินแล้ว แก้ไขไม่ได้ (ผู้ดูแลระบบปลดล็อกได้เท่านั้น)", st.Label))
	}
	if st.Status == "exported" {
		return Conflict(fmt.Sprintf(
			"บันทึกเวลาเดือน %s ถูกส่งออกไฟล์เบิกจ่ายแล้ว แก้ไขไม่ได้ (เจ้าหน้าที่ตีกลับหรือผู้ดูแลระบบปลดล็อกได้)", st.Label))
	}
	if st.IsClosed {
		return Invalid(fmt.Sprintf(
			"งวดส่งบันทึกเวลาเดือน %s ปิดแล้ว เพิ่ม/แก้ไข/ส่งย้อนหลังไม่ได้ "+
				"รายการที่ไม่ได้ส่งภายในกำหนดถือว่าไม่ประสงค์ลงเวลา", st.Label))
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
			"ดำเนินการไม่ได้ มีรายการในเดือนที่ส่งออกไฟล์/ส่งการเงินแล้ว: %s", strings.Join(months, ", ")))
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

// unsubmittableMonthSQL is the predicate that decides whether a work_log's month
// is beyond a TA's reach: the period is closed (flag or past the grace day), or
// staff have already exported / sent it to finance.
//
// Shared between WorkLogService.Submit, which skips such rows, and the
// assignment tally the TA screen counts with. They used to differ: the screen
// counted every draft and offered a "ส่งอนุมัติ 10 รายการ" button that the
// server then refused in full, so a TA who missed a deadline was left pressing a
// live button that could never work. The button's number and the number the
// server acts on now come from one predicate.
//
// The parameter is the alias of the work_logs row in the caller's query.
func unsubmittableMonthSQL(wl string) string {
	return `EXISTS (
		SELECT 1
		FROM ta_request_assignments a2
		JOIN sections sec2 ON sec2.id = a2.section_id
		JOIN teaching_courses tc2 ON tc2.id = sec2.teaching_course_id
		JOIN academic_terms trm2 ON trm2.id = tc2.term_id
		JOIN submission_periods sp2 ON sp2.term_id = tc2.term_id
		 AND sp2.year_month = trm2.academic_year::text || '-' || to_char(` + wl + `.work_date, 'MM')
		LEFT JOIN submission_period_status st2
		  ON st2.submission_period_id = sp2.id
		 AND st2.ta_id = a2.ta_id
		 AND st2.teaching_course_id = tc2.id
		WHERE a2.id = ` + wl + `.assignment_id
		  AND (sp2.is_closed OR CURRENT_DATE > sp2.due_date + INTERVAL '1 day'
		       OR COALESCE(st2.status, '') IN ('exported','finance_sent'))
	)`
}

// waitingLecturerSQL / waitingTASQL / outstandingRowSQL are the one definition
// of who a work-log row is still waiting on.
//
// The two halves are disjoint and outstandingRowSQL is exactly their union, so
// "how many rows are open" and "who do I chase" can never drift apart. Rows the
// TA can no longer send — unsent when their period closed — are in NEITHER:
// nobody is going to move them, so counting them as open would block the month
// from ever being signed off and the course from ever exporting.
//
// The parameter is the alias of the work_logs row in the caller's query.
func waitingLecturerSQL(wl string) string { return wl + `.status = 'submitted'` }

func waitingTASQL(wl string) string {
	return `(` + wl + `.status IN ('draft','rejected') AND NOT ` + unsubmittableMonthSQL(wl) + `)`
}

func outstandingRowSQL(wl string) string {
	return `(` + waitingLecturerSQL(wl) + ` OR ` + waitingTASQL(wl) + `)`
}
