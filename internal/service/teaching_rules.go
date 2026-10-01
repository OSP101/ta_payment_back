package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// teaching_rules.go is the ONE set of plausibility rules for the numbers a
// course carries — credits, weekly hours, headcounts and dates — shared by every
// path that writes them: the manual "เปิดรายวิชา" form (Create), section
// add/edit, the /info and /num-students corrections and the registrar import.
//
// Each path used to carry its own idea of "valid": Create checked nothing (a
// section of −50 students and a 2,000,000-hour course both saved, the second
// pricing the course at 2.4e14 ฿), sections only refused negatives, /info
// capped hours at 30 and /num-students capped headcount at 3000 per track. The
// same course could therefore be written through one door with numbers another
// door refused. They are ceilings against typos, not policy.
const (
	// maxStudentsPerSection / maxStudentsPerCourse: the largest real section in
	// the registrar file is 150; an extra zero is the typo these catch.
	maxStudentsPerSection = 3000
	maxStudentsPerCourse  = 3000
	maxCourseCredits      = 30
	// maxWeeklyHours is 60, not 30: the registrar's own file carries
	// "6 (0-36-18)" and "9 (0-36-18)" (graduate project/thesis courses), so a
	// 30-hour cap would refuse real courses on import and lock them out of the
	// /info editor. 60 still stops a mistyped 300 or 2,000,000.
	maxWeeklyHours = 60
)

// courseNumbers is the credit notation "C (L-P-S)" as stored on a course.
type courseNumbers struct {
	Credits, LectureHrs, LabHrs, SelfHrs int
}

// validateCourseNumbers checks each figure's range, then that the notation
// hangs together: a lecture hour a week is one credit, so a course cannot have
// more weekly lecture hours than credits ("3 (9-9-9)" is a typo, not a course).
// Every course in the registrar file satisfies this; credits = 0 means "not
// stated" and skips the cross-check.
func validateCourseNumbers(n courseNumbers) error {
	if n.Credits < 0 || n.Credits > maxCourseCredits {
		return Invalid(fmt.Sprintf("หน่วยกิตต้องอยู่ระหว่าง 0 ถึง %d", maxCourseCredits))
	}
	for _, f := range []struct {
		v     int
		label string
	}{
		{n.LectureHrs, "ชั่วโมงบรรยาย"},
		{n.LabHrs, "ชั่วโมงปฏิบัติการ"},
		{n.SelfHrs, "ชั่วโมงศึกษาด้วยตนเอง"},
	} {
		if f.v < 0 || f.v > maxWeeklyHours {
			return Invalid(fmt.Sprintf("%sต้องอยู่ระหว่าง 0 ถึง %d ชั่วโมงต่อสัปดาห์", f.label, maxWeeklyHours))
		}
	}
	if n.Credits > 0 && n.LectureHrs > n.Credits {
		return Invalid(fmt.Sprintf(
			"หน่วยกิตไม่สอดคล้องกัน: %d (%d-%d-%d) มีชั่วโมงบรรยายมากกว่าหน่วยกิต (บรรยาย 1 ชั่วโมงต่อสัปดาห์ = 1 หน่วยกิต) กรุณาตรวจตัวเลขอีกครั้ง",
			n.Credits, n.LectureHrs, n.LabHrs, n.SelfHrs))
	}
	return nil
}

// validateSectionStudents is the per-section headcount rule. label names the
// section in the message ("Sec 2") so a form with several rows says which.
func validateSectionStudents(n int, label string) error {
	if n < 0 {
		return Invalid(strings.TrimSpace(label + " จำนวนนักศึกษาต้องไม่ติดลบ"))
	}
	if n > maxStudentsPerSection {
		return Invalid(strings.TrimSpace(fmt.Sprintf("%s จำนวนนักศึกษาต้องไม่เกิน %d คนต่อกลุ่มเรียน กรุณาตรวจตัวเลขอีกครั้ง", label, maxStudentsPerSection)))
	}
	return nil
}

// validateCourseStudents is the per-course headcount rule (all sections, both
// tracks together) — the figure the budget is multiplied by.
func validateCourseStudents(n int) error {
	if n < 0 {
		return Invalid("จำนวนนักศึกษาต้องไม่ติดลบ")
	}
	if n > maxStudentsPerCourse {
		return Invalid(fmt.Sprintf("จำนวนนักศึกษารวมทั้งรายวิชาต้องไม่เกิน %d คน กรุณาตรวจตัวเลขอีกครั้ง", maxStudentsPerCourse))
	}
	return nil
}

// validateCourseDates checks a course's own date override against its term:
// both are optional (nil or "" = inherit the term's), each must be a real
// date, start ≤ end, and both inside the term. A course running outside its
// term is unloggable — the worklog refuses any date outside the term — and a
// reversed range (start 2030, end 2020) used to save without complaint.
func validateCourseDates(ctx context.Context, q querier, termID uuid.UUID, startsOn, endsOn *string) error {
	parse := func(p *string, label string) (*time.Time, error) {
		if p == nil || strings.TrimSpace(*p) == "" {
			return nil, nil
		}
		d, err := time.Parse("2006-01-02", strings.TrimSpace(*p))
		if err != nil {
			return nil, Invalid(label + "ไม่ถูกต้อง (ต้องเป็น YYYY-MM-DD)")
		}
		return &d, nil
	}
	st, err := parse(startsOn, "วันเริ่มของรายวิชา")
	if err != nil {
		return err
	}
	en, err := parse(endsOn, "วันสิ้นสุดของรายวิชา")
	if err != nil {
		return err
	}
	var tStart, tEnd *time.Time
	if err := q.QueryRow(ctx, `SELECT starts_on, ends_on FROM academic_terms WHERE id = $1`, termID).
		Scan(&tStart, &tEnd); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Invalid("ไม่พบภาคการศึกษา")
		}
		return err
	}
	// Effective range = override, else the term's own bound.
	effStart, effEnd := st, en
	if effStart == nil {
		effStart = tStart
	}
	if effEnd == nil {
		effEnd = tEnd
	}
	if effStart != nil && effEnd != nil && effStart.After(*effEnd) {
		return Invalid("วันเริ่มของรายวิชาต้องไม่อยู่หลังวันสิ้นสุด")
	}
	if st != nil && ((tStart != nil && st.Before(*tStart)) || (tEnd != nil && st.After(*tEnd))) {
		return Invalid(fmt.Sprintf("วันเริ่มของรายวิชาต้องอยู่ภายในภาคการศึกษา (%s)", termRangeTH(tStart, tEnd)))
	}
	if en != nil && ((tStart != nil && en.Before(*tStart)) || (tEnd != nil && en.After(*tEnd))) {
		return Invalid(fmt.Sprintf("วันสิ้นสุดของรายวิชาต้องอยู่ภายในภาคการศึกษา (%s)", termRangeTH(tStart, tEnd)))
	}
	return nil
}

func termRangeTH(st, en *time.Time) string {
	if st == nil || en == nil {
		return "ยังไม่ได้กำหนดช่วงวันที่"
	}
	return thaiLongDateISO(st.Format("2006-01-02")) + " ถึง " + thaiLongDateISO(en.Format("2006-01-02"))
}

// assertDateInCourseWindow refuses a date outside the course's effective
// window (its own dates, else its term's). Makeups and waivers outside it can
// never be acted on: the TA's worklog refuses any date outside the term
// ("วันที่ทำงานต้องอยู่ในช่วงภาคการศึกษา"), so a makeup on 2045-12-31 was a
// class nobody could ever log. what names the date in the message.
func assertDateInCourseWindow(ctx context.Context, q querier, tcID uuid.UUID, date, what string) error {
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		return Invalid("รูปแบบวันที่ไม่ถูกต้อง")
	}
	var st, en *time.Time
	if err := q.QueryRow(ctx, `SELECT `+CourseStartSQL("tc")+`, `+CourseEndSQL("tc")+`
		FROM teaching_courses tc WHERE tc.id = $1`, tcID).Scan(&st, &en); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if st == nil || en == nil {
		return Invalid("ยังไม่ได้กำหนดวันเริ่ม/วันสิ้นสุดของภาคการศึกษาหรือรายวิชา เจ้าหน้าที่ต้องตั้งช่วงวันที่ก่อน")
	}
	if d.Before(*st) || d.After(*en) {
		return Invalid(fmt.Sprintf("%s (%s) ต้องอยู่ในช่วงของรายวิชา %s",
			what, thaiLongDateISO(date), termRangeTH(st, en)))
	}
	return nil
}

// BudgetConfirmPrefix opens every "this edit moves an approved course's
// budget" refusal. The staff UI keys its confirm dialog on it (409 + this
// prefix → show the message, resend with confirm=true); any other 409 is a
// plain refusal. Keep in sync with app/staff/teaching/budgetConfirm.ts.
const BudgetConfirmPrefix = "ต้องยืนยันการเปลี่ยนงบประมาณ:"

// guardBudgetChange runs INSIDE an edit's transaction, after the write, and
// compares the course's formula budget with what it was before (`before`,
// read on the same transaction before the write).
//
// Why this exists: PATCH /info {lecture_hrs:0} or /num-students
// {num_students_regular:0} on a course whose TAs were approved and whose hours
// were approved returned 200 and silently zeroed the ceiling the approvals had
// been made against. Now:
//   - a change to the ceiling is refused outright once any month of the course
//     has been exported — the issued payout file was priced against it;
//   - otherwise, if the course has approved TAs or approved hours, the caller
//     must resend with confirm=true after seeing old vs new and who it affects.
//
// An edit that does not move the ceiling (a name, a self-study hour) passes.
func guardBudgetChange(ctx context.Context, tx pgx.Tx, tcID uuid.UUID, before float64, confirm bool) error {
	after, err := courseFormulaBudget(ctx, tx, tcID)
	if err != nil {
		return err
	}
	if math.Abs(after-before) < 0.5 {
		return nil
	}
	var exportedLabel string
	err = tx.QueryRow(ctx, `
		SELECT sp.label
		  FROM submission_period_status st
		  JOIN submission_periods sp ON sp.id = st.submission_period_id
		 WHERE st.teaching_course_id = $1 AND st.status IN ('exported','finance_sent')
		 ORDER BY sp.year_month
		 LIMIT 1`, tcID).Scan(&exportedLabel)
	if err == nil {
		return Conflict(fmt.Sprintf(
			"แก้ไขไม่ได้ การแก้ไขนี้เปลี่ยนงบประมาณของรายวิชาจาก %s บาท เป็น %s บาท แต่เดือน %s ส่งออกเอกสารเบิกจ่ายไปแล้ว ซึ่งคำนวณจากงบเดิม (เจ้าหน้าที่ตีกลับเดือนนั้นก่อน หรือผู้ดูแลระบบปลดล็อก)",
			thaiBaht(before), thaiBaht(after), exportedLabel))
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if confirm {
		return nil
	}
	var tas []string
	var approvedHrs float64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE((
		         SELECT array_agg(n ORDER BY n) FROM (
		           SELECT DISTINCT u.first_name || ' ' || u.last_name AS n
		             FROM ta_request_assignments a
		             JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		             JOIN users u ON u.id = a.ta_id
		            WHERE r.teaching_course_id = $1 AND a.state <> 'dropped') x), '{}'),
		       COALESCE((
		         SELECT SUM(wl.hours)
		           FROM work_logs wl
		           JOIN ta_request_assignments a ON a.id = wl.assignment_id
		           JOIN sections s ON s.id = a.section_id
		          WHERE s.teaching_course_id = $1 AND wl.status = 'approved'), 0)`,
		tcID).Scan(&tas, &approvedHrs); err != nil {
		return err
	}
	if len(tas) == 0 && approvedHrs == 0 {
		return nil
	}
	who := "ยังไม่มี TA ที่อนุมัติ"
	if len(tas) > 0 {
		who = fmt.Sprintf("TA ที่ได้รับอนุมัติแล้ว %d คน (%s)", len(tas), strings.Join(tas, ", "))
	}
	return Conflict(fmt.Sprintf(
		"%s งบประมาณของรายวิชาจะเปลี่ยนจาก %s บาท เป็น %s บาท รายวิชานี้มี%s และมีชั่วโมงที่อนุมัติแล้ว %s ชั่วโมง ซึ่งอนุมัติไว้ตามงบเดิม การแก้ไขนี้จะบันทึกเมื่อยืนยันเท่านั้น (confirm)",
		BudgetConfirmPrefix, thaiBaht(before), thaiBaht(after), who, trimHours(approvedHrs)))
}

func trimHours(h float64) string {
	s := fmt.Sprintf("%.2f", h)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

// TermMonthSpan is how many calendar months the dates [start, end] touch —
// Jun 1–Sep 30 is 4, Jul 6–Nov 8 is 5. It is the most months a term can be
// billed for: academic_terms.months multiplies every course budget, and a
// 4-month term saved with months = 12 tripled them all.
func TermMonthSpan(start, end time.Time) int {
	if end.Before(start) {
		return 0
	}
	return (end.Year()-start.Year())*12 + int(end.Month()) - int(start.Month()) + 1
}

// termOverlapAllowedKey marks a context whose UpsertTerm may create a term
// overlapping another. Only the demo sandbox sets it — see AllowTermOverlap.
type termOverlapAllowedKey struct{}

// AllowTermOverlap returns a context in which UpsertTerm skips the
// "ช่วงภาคเรียนทับซ้อน" check. For the demo sandbox ONLY: its guided walkthrough
// and its presentation dataset are both anchored on today and are designed to
// sit side by side in one isolated slot (the presentation's term is never the
// active one). No HTTP path can set a context value, so real data keeps the
// rule.
func AllowTermOverlap(ctx context.Context) context.Context {
	return context.WithValue(ctx, termOverlapAllowedKey{}, true)
}

func termOverlapAllowed(ctx context.Context) bool {
	v, _ := ctx.Value(termOverlapAllowedKey{}).(bool)
	return v
}

// assertCourseTotalStudents re-checks the per-course ceiling after a section
// write has refreshed the aggregate (recomputeAggregate) on the same
// transaction — ten sections of 3000 are each fine and together are not.
func assertCourseTotalStudents(ctx context.Context, tx pgx.Tx, tcID uuid.UUID) error {
	var total int
	if err := tx.QueryRow(ctx, `SELECT num_students FROM teaching_courses WHERE id = $1`, tcID).Scan(&total); err != nil {
		return err
	}
	return validateCourseStudents(total)
}
