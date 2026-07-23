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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

// isUniqueViolation reports whether err is a Postgres 23505 (unique_violation).
func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

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
// that a work_date falls within the term. Falls back to the parent term's
// range when the course doesn't override it (same COALESCE chain as Generate)
// — without the term fallback a course with blank dates would silently disable
// the in-term check altogether.
func (s *WorkLogService) courseDateRange(ctx context.Context, tcID uuid.UUID) (start, end time.Time, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT COALESCE(tc.starts_on, at.starts_on, '0001-01-01'::date),
		       COALESCE(tc.ends_on,   at.ends_on,   '9999-12-31'::date)
		FROM teaching_courses tc
		JOIN academic_terms at ON at.id = tc.term_id
		WHERE tc.id = $1`, tcID).Scan(&start, &end)
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

// loadHolidaysInRange loads every public_holidays row in [start, end] into a
// date→name map. Called on the pay-critical Upsert path with no cache: table
// is small (<200 rows) and the query is indexed on holiday_date, so freshness
// matters more than saving microseconds.
func (s *WorkLogService) loadHolidaysInRange(ctx context.Context, start, end time.Time) (holidaySet, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT TO_CHAR(holiday_date,'YYYY-MM-DD'), name_th
		 FROM public_holidays
		 WHERE holiday_date BETWEEN $1::date AND $2::date`,
		start.Format("2006-01-02"), end.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := holidaySet{}
	for rows.Next() {
		var d, n string
		if err := rows.Scan(&d, &n); err != nil {
			return nil, err
		}
		if _, exists := out[d]; !exists {
			out[d] = n
		}
	}
	return out, nil
}

// loadMakeupIndex loads every filed makeup for a section into both direction
// maps needed by the validator. Time-of-day is ignored here — the validator
// gates on the date bucket only; time-vs-schedule checks happen at makeup
// creation, not on each worklog Upsert.
func (s *WorkLogService) loadMakeupIndex(ctx context.Context, sectionID uuid.UUID) (makeupIndex, error) {
	idx := makeupIndex{
		byOriginal: map[string]time.Time{},
		byMakeup:   map[string]time.Time{},
	}
	rows, err := s.pool.Query(ctx,
		`SELECT original_date, makeup_date FROM makeup_schedules WHERE section_id = $1`, sectionID)
	if err != nil {
		return idx, err
	}
	defer rows.Close()
	for rows.Next() {
		var orig, mk time.Time
		if err := rows.Scan(&orig, &mk); err != nil {
			return idx, err
		}
		idx.byOriginal[orig.Format("2006-01-02")] = mk
		idx.byMakeup[mk.Format("2006-01-02")] = orig
	}
	return idx, nil
}

// holidaySet is the compact view of the public_holidays table used by the
// worklog validator: date key "YYYY-MM-DD" -> Thai display name. Populated per
// Upsert call so a freshly-added holiday takes effect immediately (no caching
// on the pay-critical path).
type holidaySet map[string]string

// makeupIndex is the by-section lookup of makeup_schedules built once per
// Upsert. Both directions are needed: byOriginal answers "was the class on
// this day shifted to another day?" (block logging on the original) and
// byMakeup answers "is this day someone's approved makeup?" (allow logging on
// a normally-non-class day, e.g. a Saturday).
type makeupIndex struct {
	byOriginal map[string]time.Time
	byMakeup   map[string]time.Time
}

// activityGate is the compact "what's this TA authorized to log" packet the
// worklog validator needs. Kept as a struct (rather than a bag of args) so
// callers pick fields off the assignmentContext they already have and don't
// have to remember which positional argument means what.
//
//   Scope        = ta_requests.reimburse_scope (lecture/lab/both)
//   AllowLecture = ta_workload_forms declares attendance/help-teach hours > 0
//   AllowLab     = ta_workload_forms declares lab/help-teach hours > 0
//   AllowReview  = ta_workload_forms declares check-work/grade hours > 0
//   AllowOther   = ta_workload_forms declares ug_other/other/prep hours > 0
type activityGate struct {
	Scope        string
	AllowLecture bool
	AllowLab     bool
	AllowReview  bool
	AllowOther   bool
}

// maxRowHours caps a single work_log row. Kept in sync with the frontend's
// MAX_ROW_HOURS (app/ta/courses/[tcId]/worklog/page.tsx).
const maxRowHours = 7.0

// validateWorkLogEntry enforces sane hours/time/date on a single manual entry
// plus the holiday-and-makeup gate that keeps TAs from billing on days the
// university was closed. Callers are responsible for loading the holiday +
// makeup contexts for the assignment's section and passing them in — the
// validator is stateless so it stays trivially testable.
//
// `gate` narrows which activity kinds are legal for THIS assignment. The
// frontend already hides unauthorized activities from the picker; the server
// needs the same rule so a stale tab or scripted client can't POST outside
// the authorized set. See activityGate for the full field-by-field meaning.
//
// The per-day baht cap and per-track hour cap are enforced separately in
// Upsert (they need DB context — pay_rates + assignment level/track).
func validateWorkLogEntry(
	w WorkLog,
	gate activityGate,
	termStart, termEnd time.Time,
	midterm, final examWindow,
	holidays holidaySet,
	mk makeupIndex,
	todayRef time.Time,
) error {
	scope := gate.Scope
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
	// Hard per-row ceiling (mirrors the frontend's MAX_ROW_HOURS). The per-day
	// caps aggregate per track, but grad-special's daily cap is 24 — without
	// this a scripted client could post a single absurdly long row.
	if w.Hours > maxRowHours+0.01 {
		return Invalid(fmt.Sprintf("ชั่วโมงต่อรายการต้องไม่เกิน %.0f ชม.", maxRowHours))
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
	// No back-dating into a month that has already passed. Compared by
	// (year, month) so it is timezone-robust. Future dates within the term are
	// allowed (a TA may fill the whole term ahead of time). Disabled when
	// todayRef is the zero value (staff override + unit tests).
	if !todayRef.IsZero() {
		dy, dm, _ := d.Date()
		ty, tm, _ := todayRef.Date()
		if dy < ty || (dy == ty && dm < tm) {
			return Invalid("บันทึกย้อนหลังในเดือนที่ผ่านไปแล้วไม่ได้ — ลงเวลาได้ตั้งแต่วันที่ 1 ของเดือนปัจจุบันเป็นต้นไป")
		}
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
	// Scope gate — the lecturer's ta_request narrows what activity types the TA
	// may log. review/makeup always pass (grading + rescheduled classes are
	// scope-neutral per Q&A rule 7). For lecture/lab and for "other" (whose
	// parent_kind stands in), reject anything the request doesn't cover.
	switch w.Activity {
	case "lecture":
		if scope == "lab" {
			return Invalid("คำขอนี้อนุมัติเฉพาะปฏิบัติการ — ลง 'บรรยาย' ไม่ได้")
		}
	case "lab":
		if scope == "lecture" {
			return Invalid("คำขอนี้อนุมัติเฉพาะบรรยาย — ลง 'ปฏิบัติการ' ไม่ได้")
		}
	case "other":
		if w.ParentKind != nil {
			if scope == "lab" && *w.ParentKind == "lecture" {
				return Invalid("คำขอนี้อนุมัติเฉพาะปฏิบัติการ — ลง 'อื่นๆ (คู่กับบรรยาย)' ไม่ได้")
			}
			if scope == "lecture" && *w.ParentKind == "lab" {
				return Invalid("คำขอนี้อนุมัติเฉพาะบรรยาย — ลง 'อื่นๆ (คู่กับปฏิบัติการ)' ไม่ได้")
			}
		}
	}
	// Workload gate — even within the authorized scope, the lecturer's
	// declared workload (ta_workload_forms) says what specific work this TA
	// was hired for. Block activities whose corresponding hour field is 0.
	// makeup follows its parent kind's authorization; if the parent activity
	// wasn't declared, the makeup shouldn't exist either — approximate that
	// by treating makeup like lecture+lab: allowed if either is allowed.
	switch w.Activity {
	case "lecture":
		if !gate.AllowLecture {
			return Invalid("อาจารย์ไม่ได้ระบุชั่วโมงเช็คชื่อบรรยายในคำขอ — ลง 'บรรยาย' ไม่ได้")
		}
	case "lab":
		if !gate.AllowLab {
			return Invalid("อาจารย์ไม่ได้ระบุชั่วโมงปฏิบัติการในคำขอ — ลง 'ปฏิบัติการ' ไม่ได้")
		}
	case "review":
		if !gate.AllowReview {
			return Invalid("อาจารย์ไม่ได้ระบุชั่วโมงตรวจงานในคำขอ — ลง 'ตรวจงาน' ไม่ได้")
		}
	case "other":
		if !gate.AllowOther {
			return Invalid("อาจารย์ไม่ได้ระบุชั่วโมงงานอื่นๆ ในคำขอ — ลง 'อื่นๆ' ไม่ได้")
		}
	case "makeup":
		if !gate.AllowLecture && !gate.AllowLab {
			return Invalid("อาจารย์ไม่ได้ระบุชั่วโมงคาบเรียนในคำขอ — ลงชดเชยไม่ได้")
		}
	}
	// Holiday + makeup gate. See plans/1-sharded-hopcroft.md § 2 for the full
	// rule table — summary:
	//   lecture/lab   → block on holiday unless the day IS a filed makeup;
	//                   block if the original class day was shifted elsewhere.
	//   makeup        → must land on a filed makeup date.
	//   review        → allowed anytime (grading happens off-site, Q&A rule 7).
	//   other+lecture → allowed on holidays (prep/admin can be off-site).
	//   other+lab     → blocked on holidays (lab admin is space-bound).
	dkey := w.WorkDate
	holidayName, isHoliday := holidays[dkey]
	_, isMakeupDay := mk.byMakeup[dkey]
	shiftedTo, isMovedAway := mk.byOriginal[dkey]

	switch w.Activity {
	case "lecture", "lab":
		if isMovedAway {
			return Invalid(fmt.Sprintf(
				"วันนี้ (%s) ถูกย้ายไปเป็นวันชดเชย %s แล้ว กรุณาลงในวันชดเชยแทน",
				dkey, shiftedTo.Format("2006-01-02")))
		}
		if isHoliday && !isMakeupDay {
			return Invalid(fmt.Sprintf(
				"วันที่ %s ตรงกับวันหยุด (%s) — กรุณาให้อาจารย์กำหนดวันชดเชยก่อน จึงจะลงเวลาได้",
				dkey, holidayName))
		}
	case "makeup":
		if !isMakeupDay {
			return Invalid("วันที่เลือกไม่ได้ถูกกำหนดเป็นวันชดเชยโดยอาจารย์ — โปรดให้อาจารย์ระบุวันชดเชยในระบบก่อน")
		}
	case "review":
		// Intentional no-op — reviewing homework can happen on any date.
	case "other":
		if isHoliday && !isMakeupDay {
			// parent_kind is validated non-nil above for activity="other".
			if w.ParentKind != nil && *w.ParentKind == "lab" {
				return Invalid(fmt.Sprintf(
					"วันหยุด (%s) — กิจกรรม 'อื่นๆ' ที่คู่กับปฏิบัติการ ทำได้เฉพาะวันที่มีคาบเรียนจริง",
					holidayName))
			}
			// parent_kind=lecture → admin/prep work is allowed off-campus.
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
	ParentKind *string `json:"parent_kind,omitempty"`
	Room       *string `json:"room,omitempty"`
	Note       *string `json:"note,omitempty"`
	Status     string  `json:"status"`
	// RejectReason is populated when the lecturer sends the batch back to the
	// TA. Reject() stamps every submitted row in the assignment with the same
	// reason so the TA-facing UI can surface the message on entry — otherwise
	// they only see "rejected" chips with no explanation.
	RejectReason *string `json:"reject_reason,omitempty"`
}

// Generate auto-creates a draft set of work logs from a section's schedule between two dates.
// Rules:
//   - Skip weekends (Sat/Sun) unless a makeup row moves it
//   - Skip exam dates
//   - Apply makeup: if original_date falls in a skipped day, move to makeup_date
//   - Cap daily hours per pay_rates (per-track: ป.ตรี regular=7, others=6)
//   - Review entries (kind='review' in lecture_review_dates) are inserted on
//     their ORIGINAL date — no makeup shift (Q&A rule 7).

// autoNoteFor returns the Thai default หมายเหตุ that Generate stamps on each
// generated row so the TA doesn't have to fill in "what did I do here" for
// every entry. Matches the labels the payroll document uses (เช็คชื่อ /
// สอนปฏิบัติ / ตรวจงาน / ชดเชย). `shifted` is true when the row was moved to
// a makeup date — the note is suffixed with "(ชดเชย)" so the payroll clerk
// sees at a glance that this row rescheduled a holiday class.
func autoNoteFor(activity string, shifted bool) string {
	base := ""
	switch activity {
	case "lecture":
		base = "เช็คชื่อ"
	case "lab":
		base = "สอนปฏิบัติ"
	case "review":
		base = "ตรวจงาน"
	case "makeup":
		base = "ชดเชย"
	case "other":
		base = "งานอื่นๆ"
	default:
		return ""
	}
	if shifted {
		return base + "(ชดเชย)"
	}
	return base
}

func (s *WorkLogService) Generate(ctx context.Context, actor, assignmentID uuid.UUID) ([]WorkLog, error) {
	ac, err := s.assertTAOwnsAssignment(ctx, actor, assignmentID)
	if err != nil {
		return nil, err
	}
	if err := s.assertStudentCountFilled(ctx, ac.TeachingCourseID); err != nil {
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
	// Prefer the course-level date range, but fall back to the parent term's
	// range when the course itself doesn't override it. Without this fallback
	// a course that inherits its dates from the term would silently produce
	// zero rows (CURRENT_DATE range = a one-day loop).
	err = s.pool.QueryRow(ctx, `
		SELECT a.section_id,
		       COALESCE(tc.starts_on, at.starts_on, CURRENT_DATE),
		       COALESCE(tc.ends_on,   at.ends_on,   CURRENT_DATE)
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		JOIN academic_terms  at ON at.id = tc.term_id
		WHERE a.id = $1`, assignmentID).Scan(&sectionID, &startsOn, &endsOn)
	if err != nil {
		return nil, err
	}
	// Safety net: if both the course and the term left dates blank (nothing to
	// COALESCE against but CURRENT_DATE), the caller gets a helpful message
	// instead of a silent no-op.
	if !endsOn.After(startsOn) {
		return nil, Invalid("ยังไม่ได้กำหนดวันเริ่ม/วันสิ้นสุดของเทอมหรือรายวิชา — เจ้าหน้าที่ต้องตั้งช่วงเวลาก่อน จึงจะสร้างรายการอัตโนมัติได้")
	}
	// Resolve the per-track daily hour cap once (used by both loops below).
	dailyHourCap := s.dailyHourCapFor(ctx, assignmentID)
	// Term exam windows (midterm/final): auto-generation must not create rows
	// inside them — the same blackout the manual Upsert path enforces
	// (validateWorkLogEntry). Without this, Generate silently mints billable
	// rows a lecturer would then approve straight through the blackout.
	midterm, final, err := s.courseExamWindows(ctx, ac.TeachingCourseID)
	if err != nil {
		return nil, err
	}
	inExamWindow := func(d time.Time) bool { return midterm.contains(d) || final.contains(d) }
	inExamWindowStr := func(dstr string) bool {
		t, perr := time.Parse("2006-01-02", dstr)
		return perr == nil && (midterm.contains(t) || final.contains(t))
	}
	// Daily baht ceiling (Q&A rule 6a, 300฿/day): skip rows that could never be
	// approved because they'd blow the per-day baht cap. Grad-special is flat
	// monthly (rate 0) and exempt, matching enforceDailyBahtCap.
	genRate := s.assignmentRate(ctx, assignmentID)
	var dailyBahtCap float64
	if genRate > 0 {
		_ = s.pool.QueryRow(ctx,
			`SELECT daily_pay_cap_baht FROM pay_rates ORDER BY effective_from DESC LIMIT 1`).Scan(&dailyBahtCap)
	}
	dailyBaht := map[string]float64{}
	bahtBlocks := func(dkey string, hrs float64) bool {
		return genRate > 0 && dailyBahtCap > 0 && dailyBaht[dkey]+hrs*genRate > dailyBahtCap+0.01
	}
	// Months whose submission period is closed or locked (exported/finance_sent)
	// are skipped entirely (silent, mirroring the holiday skip below) — the TA
	// can no longer write there, so generating rows would only create stuck
	// drafts.
	blockedMonths, err := loadBlockedMonths(ctx, s.pool, ac.TeachingCourseID, ac.TAID)
	if err != nil {
		return nil, err
	}
	monthBlocked := func(mm string) bool {
		bm, ok := blockedMonths[mm]
		return ok && (bm.IsClosed || bm.Locked)
	}
	blockedMM := []string{} // non-nil: `<> ALL(NULL)` would delete nothing
	for mm, bm := range blockedMonths {
		if bm.IsClosed || bm.Locked {
			blockedMM = append(blockedMM, mm)
		}
	}
	// Holidays that fall in the term — used to skip auto-generation on days
	// the university is closed. If the lecturer has already filed a makeup for
	// that day, the shift happens through the existing `makeup` map below; if
	// not, the day is silently dropped and the TA sees the gap in the
	// /holiday-impacts page.
	holidays, err := s.loadHolidaysInRange(ctx, startsOn, endsOn)
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
	// wipe existing draft logs for this assignment — except rows in blocked
	// months, which the TA can no longer touch (and we won't regenerate).
	if _, err := tx.Exec(ctx,
		`DELETE FROM work_logs WHERE assignment_id=$1 AND status='draft'
		 AND to_char(work_date,'MM') <> ALL($2)`, assignmentID, blockedMM); err != nil {
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
		// Holiday gate: if the ORIGINAL scheduled day is a public holiday and
		// the lecturer hasn't filed a makeup that moves us elsewhere, skip
		// generation for that day. The TA sees the gap on the holidays page
		// and can nudge the lecturer to file a makeup, at which point the
		// next regeneration picks it up on the shifted date.
		if _, isHoliday := holidays[key]; isHoliday {
			if _, hasMakeup := makeup[key]; !hasMakeup {
				continue
			}
		}
		// If the SHIFTED useDate itself lands on a holiday (nested holiday —
		// lecturer picked a makeup that also happens to be closed), skip too.
		// AddMakeup rejects this at create time, but defensive-check here in
		// case a holiday is added AFTER a makeup was filed.
		if _, isHoliday := holidays[useDate.Format("2006-01-02")]; isHoliday {
			continue
		}
		if examDates[useDate.Format("2006-01-02")] {
			continue
		}
		if inExamWindow(useDate) {
			continue
		}
		if monthBlocked(useDate.Format("01")) {
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
			// Scope gate: the lecturer's ta_request authorizes only certain
			// activity kinds. Skip section_schedules rows whose kind isn't
			// covered so the generator doesn't create bogus lab entries for a
			// lecture-only request (and vice versa). Review is unaffected —
			// it's expanded from separate tables below.
			if ac.ReimburseScope == "lecture" && sc.kind == "lab" {
				continue
			}
			if ac.ReimburseScope == "lab" && sc.kind == "lecture" {
				continue
			}
			// Workload gate: even within the authorized scope, only generate
			// activities the lecturer declared this TA does (ta_workload_forms).
			// E.g. undergrad with attendance_hrs=0 → don't generate lecture
			// attendance rows even if the section has lecture schedules.
			if sc.kind == "lecture" && !ac.AllowLecture {
				continue
			}
			if sc.kind == "lab" && !ac.AllowLab {
				continue
			}
			// enforce per-track daily hour cap (Q&A rule 6d)
			if dailyHrs[useDate.Format("2006-01-02")]+sc.hours > dailyHourCap {
				continue
			}
			if bahtBlocks(useDate.Format("2006-01-02"), sc.hours) {
				continue
			}
			id := uuid.New()
			// Detect whether this row landed on a makeup date so the note
			// picks up the "(ชดเชย)" suffix. useDate != d only when the
			// original class day had a makeup filed above.
			note := autoNoteFor(sc.kind, !useDate.Equal(d))
			if _, err := tx.Exec(ctx,
				`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, note, status)
				 VALUES ($1,$2,$3::date,$4::time,$5::time,$6,$7,$8,'draft')`,
				id, assignmentID, useDate.Format("2006-01-02"), sc.start, sc.end, sc.hours, sc.kind, note); err != nil {
				return nil, err
			}
			out = append(out, WorkLog{ID: id, AssignmentID: assignmentID,
				WorkDate: useDate.Format("2006-01-02"), StartTime: sc.start, EndTime: sc.end,
				Hours: sc.hours, Activity: sc.kind, Note: &note, Status: "draft"})
			dailyHrs[useDate.Format("2006-01-02")] += sc.hours
			dailyBaht[useDate.Format("2006-01-02")] += sc.hours * genRate
		}
	}

	// Q&A rule 7: review entries reference the ORIGINAL date and are NOT shifted
	// by the makeup mapping. They live in a separate table (lecture_review_dates).
	// Workload gate: skip both review sources entirely if the lecturer didn't
	// declare grading / check-work hours for this TA — otherwise the generator
	// would create review rows the TA isn't authorized to log.
	if ac.AllowReview {
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
			if inExamWindowStr(dstr) {
				continue
			}
			if len(dstr) >= 7 && monthBlocked(dstr[5:7]) {
				continue
			}
			if dailyHrs[dstr]+hrs > dailyHourCap {
				continue
			}
			if bahtBlocks(dstr, hrs) {
				continue
			}
			id := uuid.New()
			note := autoNoteFor("review", false)
			if _, err := tx.Exec(ctx,
				`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, note, status)
				 VALUES ($1,$2,$3::date,$4::time,$5::time,$6,'review',$7,'draft')`,
				id, assignmentID, dstr, start, end, hrs, note); err != nil {
				reviewRows.Close()
				return nil, err
			}
			out = append(out, WorkLog{ID: id, AssignmentID: assignmentID,
				WorkDate: dstr, StartTime: start, EndTime: end,
				Hours: hrs, Activity: "review", Note: &note, Status: "draft"})
			dailyHrs[dstr] += hrs
			dailyBaht[dstr] += hrs * genRate
		}
		reviewRows.Close()

		// TA-owned weekly review pattern (วันตรวจการบ้าน). Same weekly-expansion
		// as class schedules, but source is per-assignment and activity=review.
		// Reviews are allowed on holidays (Q&A rule 7), so we do NOT skip holiday
		// dates here — only exam windows and the daily hour cap can prevent an
		// entry.
		type trs struct {
			day   int
			start string
			end   string
			hours float64
		}
		var trss []trs
		trRows, err := tx.Query(ctx,
			`SELECT day_of_week, start_time::text, end_time::text,
			        EXTRACT(EPOCH FROM (end_time - start_time))/3600
			 FROM ta_review_schedules WHERE assignment_id=$1`, assignmentID)
		if err != nil {
			return nil, err
		}
		for trRows.Next() {
			var r trs
			if err := trRows.Scan(&r.day, &r.start, &r.end, &r.hours); err != nil {
				trRows.Close()
				return nil, err
			}
			trss = append(trss, r)
		}
		trRows.Close()

		if len(trss) > 0 {
			for d := startsOn; !d.After(endsOn); d = d.AddDate(0, 0, 1) {
				dkey := d.Format("2006-01-02")
				if examDates[dkey] {
					continue
				}
				if inExamWindow(d) {
					continue
				}
				if monthBlocked(d.Format("01")) {
					continue
				}
				weekday := int(d.Weekday())
				for _, r := range trss {
					if r.day != weekday {
						continue
					}
					if dailyHrs[dkey]+r.hours > dailyHourCap {
						continue
					}
					if bahtBlocks(dkey, r.hours) {
						continue
					}
					id := uuid.New()
					note := autoNoteFor("review", false)
					if _, err := tx.Exec(ctx,
						`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, note, status)
						 VALUES ($1,$2,$3::date,$4::time,$5::time,$6,'review',$7,'draft')`,
						id, assignmentID, dkey, r.start, r.end, r.hours, note); err != nil {
						return nil, err
					}
					out = append(out, WorkLog{ID: id, AssignmentID: assignmentID,
						WorkDate: dkey, StartTime: r.start, EndTime: r.end,
						Hours: r.hours, Activity: "review", Note: &note, Status: "draft"})
					dailyHrs[dkey] += r.hours
					dailyBaht[dkey] += r.hours * genRate
				}
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "worklog.generate", Entity: "assignment", EntityID: assignmentID.String(), After: map[string]int{"count": len(out)}})
	return out, nil
}

// -----------------------------------------------------------------------------
// TA-owned weekly review pattern (วันตรวจการบ้าน)
// -----------------------------------------------------------------------------

// TAReviewSchedule is one weekly slot the TA sets aside for grading homework.
// Auto-generate expands each slot into individual review entries across the
// term. Persistent across regenerations — TAs configure once per term.
type TAReviewSchedule struct {
	ID           uuid.UUID `json:"id"`
	AssignmentID uuid.UUID `json:"assignment_id"`
	DayOfWeek    int       `json:"day_of_week"` // 0=Sunday..6=Saturday
	StartTime    string    `json:"start_time"`  // "HH:MM"
	EndTime      string    `json:"end_time"`
	Room         string    `json:"room,omitempty"`
	Note         string    `json:"note,omitempty"`
}

type TAReviewScheduleInput struct {
	DayOfWeek int    `json:"day_of_week"`
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
	Room      string `json:"room"`
	Note      string `json:"note"`
}

func validateReviewInput(in TAReviewScheduleInput) error {
	if in.DayOfWeek < 0 || in.DayOfWeek > 6 {
		return Invalid("วันของสัปดาห์ไม่ถูกต้อง")
	}
	s, ok := parseHM(in.StartTime)
	if !ok {
		return Invalid("รูปแบบเวลาเริ่มไม่ถูกต้อง")
	}
	e, ok := parseHM(in.EndTime)
	if !ok {
		return Invalid("รูปแบบเวลาสิ้นสุดไม่ถูกต้อง")
	}
	if e <= s {
		return Invalid("เวลาสิ้นสุดต้องหลังเวลาเริ่ม")
	}
	return nil
}

// TAReviewScheduleList wraps the recurring pattern along with the weekly
// budget so the UI can show "2/3 ชม./สัปดาห์" and disable "add" when full.
// Business rule (per user): total review hours per week must not exceed the
// section's weekly lecture hours (summed from section_schedules kind='lecture').
type TAReviewScheduleList struct {
	Items                []TAReviewSchedule `json:"items"`
	LectureHoursPerWeek  float64            `json:"lecture_hours_per_week"`
	ReviewHoursPerWeek   float64            `json:"review_hours_per_week"`
}

// weeklyLectureHoursForAssignment sums the section's kind='lecture' hours from
// section_schedules. Returns 0 when the lecturer hasn't entered any lecture
// slot yet — callers should treat 0 as "no review time allowed at all".
func (s *WorkLogService) weeklyLectureHoursForAssignment(ctx context.Context, assignmentID uuid.UUID) (float64, error) {
	var h float64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(EXTRACT(EPOCH FROM (ss.end_time - ss.start_time))/3600), 0)
		FROM ta_request_assignments a
		JOIN section_schedules ss ON ss.section_id = a.section_id
		WHERE a.id = $1 AND ss.kind = 'lecture'`, assignmentID).Scan(&h)
	return h, err
}

// weeklyReviewHoursForAssignment sums existing review-slot hours. When
// excludeID is non-nil, that row is left out — used when updating an existing
// slot so it doesn't count against itself.
func (s *WorkLogService) weeklyReviewHoursForAssignment(ctx context.Context, assignmentID uuid.UUID, excludeID *uuid.UUID) (float64, error) {
	var h float64
	q := `SELECT COALESCE(SUM(EXTRACT(EPOCH FROM (end_time - start_time))/3600), 0)
	      FROM ta_review_schedules WHERE assignment_id=$1`
	args := []any{assignmentID}
	if excludeID != nil {
		q += " AND id <> $2"
		args = append(args, *excludeID)
	}
	err := s.pool.QueryRow(ctx, q, args...).Scan(&h)
	return h, err
}

// slotHours parses "HH:MM" bounds into a float count of hours, or 0 on error.
func slotHours(startHM, endHM string) float64 {
	s, ok1 := parseHM(startHM)
	e, ok2 := parseHM(endHM)
	if !ok1 || !ok2 || e <= s {
		return 0
	}
	return float64(e-s) / 60
}

func (s *WorkLogService) ListTAReviewSchedules(ctx context.Context, actor, assignmentID uuid.UUID) (TAReviewScheduleList, error) {
	if _, err := s.assertTAOwnsAssignment(ctx, actor, assignmentID); err != nil {
		return TAReviewScheduleList{}, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, assignment_id, day_of_week, start_time::text, end_time::text,
		        COALESCE(room,''), COALESCE(note,'')
		 FROM ta_review_schedules WHERE assignment_id=$1
		 ORDER BY day_of_week, start_time`, assignmentID)
	if err != nil {
		return TAReviewScheduleList{}, err
	}
	defer rows.Close()
	items := []TAReviewSchedule{}
	var totalReview float64
	for rows.Next() {
		var r TAReviewSchedule
		if err := rows.Scan(&r.ID, &r.AssignmentID, &r.DayOfWeek,
			&r.StartTime, &r.EndTime, &r.Room, &r.Note); err != nil {
			return TAReviewScheduleList{}, err
		}
		// Store as HH:MM for the UI even though the DB column keeps seconds.
		if len(r.StartTime) >= 5 {
			r.StartTime = r.StartTime[:5]
		}
		if len(r.EndTime) >= 5 {
			r.EndTime = r.EndTime[:5]
		}
		totalReview += slotHours(r.StartTime, r.EndTime)
		items = append(items, r)
	}
	lecH, err := s.weeklyLectureHoursForAssignment(ctx, assignmentID)
	if err != nil {
		return TAReviewScheduleList{}, err
	}
	return TAReviewScheduleList{
		Items:               items,
		LectureHoursPerWeek: lecH,
		ReviewHoursPerWeek:  totalReview,
	}, nil
}

// ScheduleBusyBlock is one occupied weekly slot on the TA's timetable, used by
// the review-schedule editor to preview time conflicts before saving. Kind is
// "class" (their own study timetable), "teaching" (a section they TA), or
// "review" (an existing review slot). ReviewID is set only for kind="review"
// so the editor can exclude the row being edited from its overlap check.
type ScheduleBusyBlock struct {
	DayOfWeek int     `json:"day_of_week"`
	StartTime string  `json:"start_time"` // "HH:MM"
	EndTime   string  `json:"end_time"`
	Kind      string  `json:"kind"`  // class | teaching | review
	Label     string  `json:"label"` // course code/name for display
	ReviewID  *string `json:"review_id,omitempty"`
}

// ScheduleBusyBlocks returns every weekly slot that already occupies the TA's
// timetable for the assignment's term — their class timetable, the classes
// they TA across all approved assignments, and their review slots. The editor
// uses this to warn about overlaps inline (the server still enforces the same
// rule authoritatively via enforceReviewNoConflict on save).
func (s *WorkLogService) ScheduleBusyBlocks(ctx context.Context, actor, assignmentID uuid.UUID) ([]ScheduleBusyBlock, error) {
	if _, err := s.assertTAOwnsAssignment(ctx, actor, assignmentID); err != nil {
		return nil, err
	}
	var taID, termID uuid.UUID
	if err := s.pool.QueryRow(ctx, `
		SELECT a.ta_id, tc.term_id
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		WHERE a.id = $1`, assignmentID).Scan(&taID, &termID); err != nil {
		return nil, err
	}
	out := []ScheduleBusyBlock{}

	// (a) the TA's own class timetable (WBA rows excluded).
	rows, err := s.pool.Query(ctx, `
		SELECT cs.day_of_week,
		       to_char(cs.start_time,'HH24:MI'), to_char(cs.end_time,'HH24:MI'),
		       COALESCE(NULLIF(cs.course_code,''), NULLIF(cs.course_name,''),
		                NULLIF(cs.course_label,''), 'คาบเรียน')
		FROM ta_class_schedules cs
		WHERE cs.user_id = $1 AND cs.term_id = $2 AND NOT cs.is_wba`, taID, termID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b ScheduleBusyBlock
		if err := rows.Scan(&b.DayOfWeek, &b.StartTime, &b.EndTime, &b.Label); err != nil {
			rows.Close()
			return nil, err
		}
		b.Kind = "class"
		out = append(out, b)
	}
	rows.Close()

	// (b) classes the TA help-teaches across all approved assignments.
	rows, err = s.pool.Query(ctx, `
		SELECT ss.day_of_week,
		       to_char(ss.start_time,'HH24:MI'), to_char(ss.end_time,'HH24:MI'),
		       fc.code || ' sec ' || sec2.sec_no
		FROM ta_request_assignments a2
		JOIN ta_requests r2 ON r2.id = a2.request_id AND r2.status = 'approved'
		JOIN sections sec2 ON sec2.id = a2.section_id
		JOIN teaching_courses tc2 ON tc2.id = sec2.teaching_course_id AND tc2.term_id = $2
		JOIN section_schedules ss ON ss.section_id = sec2.id
		JOIN faculty_courses fc ON fc.id = tc2.faculty_course_id
		WHERE a2.ta_id = $1`, taID, termID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b ScheduleBusyBlock
		if err := rows.Scan(&b.DayOfWeek, &b.StartTime, &b.EndTime, &b.Label); err != nil {
			rows.Close()
			return nil, err
		}
		b.Kind = "teaching"
		out = append(out, b)
	}
	rows.Close()

	// (c) the TA's existing review slots across assignments.
	rows, err = s.pool.Query(ctx, `
		SELECT rs.id::text, rs.day_of_week,
		       to_char(rs.start_time,'HH24:MI'), to_char(rs.end_time,'HH24:MI'),
		       fc.code || ' sec ' || sec2.sec_no
		FROM ta_review_schedules rs
		JOIN ta_request_assignments a2 ON a2.id = rs.assignment_id
		JOIN sections sec2 ON sec2.id = a2.section_id
		JOIN teaching_courses tc2 ON tc2.id = sec2.teaching_course_id AND tc2.term_id = $2
		JOIN faculty_courses fc ON fc.id = tc2.faculty_course_id
		WHERE a2.ta_id = $1`, taID, termID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b ScheduleBusyBlock
		var id string
		if err := rows.Scan(&id, &b.DayOfWeek, &b.StartTime, &b.EndTime, &b.Label); err != nil {
			rows.Close()
			return nil, err
		}
		b.Kind = "review"
		b.ReviewID = &id
		out = append(out, b)
	}
	rows.Close()

	return out, nil
}

func (s *WorkLogService) AddTAReviewSchedule(ctx context.Context, actor, assignmentID uuid.UUID, in TAReviewScheduleInput) (uuid.UUID, error) {
	if _, err := s.assertTAOwnsAssignment(ctx, actor, assignmentID); err != nil {
		return uuid.Nil, err
	}
	if err := validateReviewInput(in); err != nil {
		return uuid.Nil, err
	}
	if err := s.enforceReviewNoConflict(ctx, assignmentID, in, nil); err != nil {
		return uuid.Nil, err
	}
	if err := s.enforceReviewCap(ctx, assignmentID, nil, slotHours(in.StartTime, in.EndTime)); err != nil {
		return uuid.Nil, err
	}
	id := uuid.New()
	_, err := s.pool.Exec(ctx,
		`INSERT INTO ta_review_schedules (id, assignment_id, day_of_week, start_time, end_time, room, note)
		 VALUES ($1,$2,$3,$4::time,$5::time,NULLIF($6,''),NULLIF($7,''))`,
		id, assignmentID, in.DayOfWeek, in.StartTime, in.EndTime, in.Room, in.Note)
	if err != nil {
		// Postgres 23505 = unique_violation — friendly message for the dup case.
		if isUniqueViolation(err) {
			return uuid.Nil, Invalid("มีตารางตรวจการบ้านนี้อยู่แล้ว")
		}
		return uuid.Nil, err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "ta_review_schedule.add", Entity: "assignment", EntityID: assignmentID.String(), After: in})
	return id, nil
}

// reviewDayNames maps day_of_week (0=Sunday..6=Saturday, the convention shared
// by ta_review_schedules / ta_class_schedules / section_schedules) to Thai.
var reviewDayNames = []string{"อาทิตย์", "จันทร์", "อังคาร", "พุธ", "พฤหัสบดี", "ศุกร์", "เสาร์"}

// enforceReviewNoConflict rejects a review slot that overlaps anything already
// on the TA's weekly schedule for the term:
//   (a) their own study timetable  (ta_class_schedules, WBA rows excluded),
//   (b) the classes they help-teach across ALL their approved assignments
//       (section_schedules of every section the TA holds this term), and
//   (c) their other review slots  (ta_review_schedules across assignments).
// Overlap uses the half-open predicate start1 < end2 AND end1 > start2, gated
// on equal day_of_week — the same pattern ta_request.go uses for section
// conflicts. excludeID skips the row being edited so it doesn't clash with
// itself. A conflict returns a Thai message naming the day + clashing slot.
func (s *WorkLogService) enforceReviewNoConflict(ctx context.Context, assignmentID uuid.UUID, in TAReviewScheduleInput, excludeID *uuid.UUID) error {
	// Resolve the owning TA + term for this assignment.
	var taID, termID uuid.UUID
	if err := s.pool.QueryRow(ctx, `
		SELECT a.ta_id, tc.term_id
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		WHERE a.id = $1`, assignmentID).Scan(&taID, &termID); err != nil {
		return err
	}

	dayTH := "วันที่เลือก"
	if in.DayOfWeek >= 0 && in.DayOfWeek < len(reviewDayNames) {
		dayTH = reviewDayNames[in.DayOfWeek]
	}
	conflict := func(kind, label, st, en string) error {
		if label == "" {
			label = kind
		}
		return Invalid(fmt.Sprintf(
			"เวลาตรวจการบ้านชนกับ%s (%s %s %s–%s) — เลือกช่วงเวลาที่ไม่ทับกัน",
			kind, label, dayTH, st, en))
	}

	// (a) TA's own class timetable.
	{
		var label, st, en string
		err := s.pool.QueryRow(ctx, `
			SELECT COALESCE(NULLIF(cs.course_code,''), NULLIF(cs.course_name,''),
			                NULLIF(cs.course_label,''), ''),
			       to_char(cs.start_time,'HH24:MI'), to_char(cs.end_time,'HH24:MI')
			FROM ta_class_schedules cs
			WHERE cs.user_id = $1 AND cs.term_id = $2 AND NOT cs.is_wba
			  AND cs.day_of_week = $3
			  AND cs.start_time < $5::time AND cs.end_time > $4::time
			LIMIT 1`, taID, termID, in.DayOfWeek, in.StartTime, in.EndTime).Scan(&label, &st, &en)
		if err == nil {
			return conflict("ตารางเรียนของคุณ", label, st, en)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}

	// (b) Classes the TA help-teaches (all their approved assignments' sections).
	{
		var label, st, en string
		err := s.pool.QueryRow(ctx, `
			SELECT fc.code || ' sec ' || sec2.sec_no,
			       to_char(ss.start_time,'HH24:MI'), to_char(ss.end_time,'HH24:MI')
			FROM ta_request_assignments a2
			JOIN ta_requests r2 ON r2.id = a2.request_id AND r2.status = 'approved'
			JOIN sections sec2 ON sec2.id = a2.section_id
			JOIN teaching_courses tc2 ON tc2.id = sec2.teaching_course_id AND tc2.term_id = $2
			JOIN section_schedules ss ON ss.section_id = sec2.id
			JOIN faculty_courses fc ON fc.id = tc2.faculty_course_id
			WHERE a2.ta_id = $1
			  AND ss.day_of_week = $3
			  AND ss.start_time < $5::time AND ss.end_time > $4::time
			LIMIT 1`, taID, termID, in.DayOfWeek, in.StartTime, in.EndTime).Scan(&label, &st, &en)
		if err == nil {
			return conflict("คาบสอนที่คุณเป็น TA", label, st, en)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}

	// (c) The TA's other review slots (across every assignment they hold).
	{
		var exclude uuid.UUID
		if excludeID != nil {
			exclude = *excludeID
		}
		var label, st, en string
		err := s.pool.QueryRow(ctx, `
			SELECT fc.code || ' sec ' || sec2.sec_no,
			       to_char(rs.start_time,'HH24:MI'), to_char(rs.end_time,'HH24:MI')
			FROM ta_review_schedules rs
			JOIN ta_request_assignments a2 ON a2.id = rs.assignment_id
			JOIN sections sec2 ON sec2.id = a2.section_id
			JOIN teaching_courses tc2 ON tc2.id = sec2.teaching_course_id AND tc2.term_id = $2
			JOIN faculty_courses fc ON fc.id = tc2.faculty_course_id
			WHERE a2.ta_id = $1 AND rs.id <> $6
			  AND rs.day_of_week = $3
			  AND rs.start_time < $5::time AND rs.end_time > $4::time
			LIMIT 1`, taID, termID, in.DayOfWeek, in.StartTime, in.EndTime, exclude).Scan(&label, &st, &en)
		if err == nil {
			return conflict("ตารางตรวจการบ้านอื่นของคุณ", label, st, en)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}

	return nil
}

// enforceReviewCap rejects an add/update that would push the section's total
// weekly review hours past the section's lecture hours per week.
func (s *WorkLogService) enforceReviewCap(ctx context.Context, assignmentID uuid.UUID, excludeID *uuid.UUID, addedHours float64) error {
	if addedHours <= 0 {
		return nil
	}
	lecH, err := s.weeklyLectureHoursForAssignment(ctx, assignmentID)
	if err != nil {
		return err
	}
	if lecH <= 0 {
		return Invalid("ยังไม่พบชั่วโมงบรรยายของ section นี้ในระบบ — อาจารย์ต้องกำหนดตารางสอนก่อน จึงจะเพิ่มวันตรวจการบ้านได้")
	}
	existing, err := s.weeklyReviewHoursForAssignment(ctx, assignmentID, excludeID)
	if err != nil {
		return err
	}
	if existing+addedHours > lecH+0.01 {
		return Invalid(fmt.Sprintf(
			"ชั่วโมงตรวจการบ้านรวมต่อสัปดาห์ (%.1f ชม.) ต้องไม่เกินชั่วโมงบรรยายของ section (%.1f ชม.) — ปัจจุบันใช้ไปแล้ว %.1f ชม.",
			existing+addedHours, lecH, existing))
	}
	return nil
}

func (s *WorkLogService) UpdateTAReviewSchedule(ctx context.Context, actor, assignmentID, rsID uuid.UUID, in TAReviewScheduleInput) error {
	if _, err := s.assertTAOwnsAssignment(ctx, actor, assignmentID); err != nil {
		return err
	}
	if err := validateReviewInput(in); err != nil {
		return err
	}
	if err := s.enforceReviewNoConflict(ctx, assignmentID, in, &rsID); err != nil {
		return err
	}
	if err := s.enforceReviewCap(ctx, assignmentID, &rsID, slotHours(in.StartTime, in.EndTime)); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE ta_review_schedules
		 SET day_of_week=$3, start_time=$4::time, end_time=$5::time,
		     room=NULLIF($6,''), note=NULLIF($7,'')
		 WHERE id=$1 AND assignment_id=$2`,
		rsID, assignmentID, in.DayOfWeek, in.StartTime, in.EndTime, in.Room, in.Note)
	if err != nil {
		if isUniqueViolation(err) {
			return Invalid("มีตารางตรวจการบ้านนี้อยู่แล้ว")
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return Invalid("ไม่พบตารางตรวจการบ้านที่ต้องการแก้ไข")
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "ta_review_schedule.update", Entity: "ta_review_schedule", EntityID: rsID.String(), After: in})
	return nil
}

func (s *WorkLogService) DeleteTAReviewSchedule(ctx context.Context, actor, assignmentID, rsID uuid.UUID) error {
	if _, err := s.assertTAOwnsAssignment(ctx, actor, assignmentID); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM ta_review_schedules WHERE id=$1 AND assignment_id=$2`,
		rsID, assignmentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return Invalid("ไม่พบตารางตรวจการบ้านที่ต้องการลบ")
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "ta_review_schedule.delete", Entity: "ta_review_schedule", EntityID: rsID.String()})
	return nil
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

// enforceWeeklyActivityCap rejects a worklog upsert that would push the TA's
// weekly total for the same activity past what the lecturer declared in the
// workload form. Week bounds follow PostgreSQL's date_trunc('week', ...) —
// Monday through Sunday. Grad assignments share a single help_teach cap
// across lecture+lab, so those two activities are summed together when
// ac.WeeklyLectureLabShared is true. Called from both Upsert paths (TA + staff).
//
// Skipped when the workload form hasn't been filled yet or the cap is zero
// (the AllowX gates in validateWorkLogEntry already reject those cases with a
// clearer message).
func (s *WorkLogService) enforceWeeklyActivityCap(ctx context.Context, ac *assignmentContext, w WorkLog) error {
	if !ac.HasWorkloadForm {
		return nil
	}
	var (
		cap        float64
		activities []string
		labelTH    string
	)
	switch w.Activity {
	case "lecture", "lab":
		if ac.WeeklyLectureLabShared {
			cap = ac.WeeklyCapLecture
			activities = []string{"lecture", "lab"}
			labelTH = "ช่วยสอน (บรรยาย+ปฏิบัติการ)"
		} else if w.Activity == "lecture" {
			cap = ac.WeeklyCapLecture
			activities = []string{"lecture"}
			labelTH = "เช็คชื่อบรรยาย"
		} else {
			cap = ac.WeeklyCapLab
			activities = []string{"lab"}
			labelTH = "ปฏิบัติการ"
		}
	case "review":
		cap = ac.WeeklyCapReview
		activities = []string{"review"}
		labelTH = "ตรวจงาน"
	case "other":
		cap = ac.WeeklyCapOther
		activities = []string{"other"}
		labelTH = "อื่นๆ"
	default:
		// makeup and any future kinds — no weekly cap enforced (they follow
		// the original class occurrence which is already bounded by the
		// section schedule).
		return nil
	}
	if cap <= 0 {
		return nil
	}
	var weekTotal float64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(hours), 0) FROM work_logs
		 WHERE assignment_id = $1
		   AND date_trunc('week', work_date) = date_trunc('week', $2::date)
		   AND activity = ANY($3)
		   AND id <> $4`,
		w.AssignmentID, w.WorkDate, activities, w.ID).Scan(&weekTotal); err != nil {
		return err
	}
	if weekTotal+w.Hours > cap+0.01 {
		return Invalid(fmt.Sprintf(
			"เกินโควตา %s ประจำสัปดาห์ (%.1f ชม./สัปดาห์) — สัปดาห์นี้มี %.2f ชม. อยู่แล้ว",
			labelTH, cap, weekTotal))
	}
	return nil
}

// enforceNoOverlap rejects a row whose time range overlaps another of the same
// TA's rows on the same date — across every assignment the TA holds, so two
// courses can't both bill the same clock hours. Back-to-back rows
// (end == start) are allowed; rejected rows don't count.
func (s *WorkLogService) enforceNoOverlap(ctx context.Context, taID uuid.UUID, w WorkLog) error {
	var code, st, en string
	err := s.pool.QueryRow(ctx, `
		SELECT fc.code, wl.start_time::text, wl.end_time::text
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		JOIN sections sec ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		JOIN faculty_courses fc ON fc.id = tc.faculty_course_id
		WHERE a.ta_id = $1 AND wl.work_date = $2::date AND wl.id <> $3
		  AND wl.status <> 'rejected'
		  AND wl.start_time < $5::time AND wl.end_time > $4::time
		LIMIT 1`, taID, w.WorkDate, w.ID, w.StartTime, w.EndTime).Scan(&code, &st, &en)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(st) > 5 {
		st = st[:5]
	}
	if len(en) > 5 {
		en = en[:5]
	}
	return Invalid(fmt.Sprintf("ช่วงเวลาซ้อนกับรายการเดิม (%s %s–%s) — แก้ไขเวลาให้ไม่ทับกัน", code, st, en))
}

// recheckCapsForApproval re-validates the per-day and per-week hour caps over
// the assignment's submitted+approved rows inside the approval transaction.
// The caps are normally enforced at Upsert time, but staff edits or workload
// changes between submit and approve can push the totals past them — approval
// is the last gate before the hours become billable.
func (s *WorkLogService) recheckCapsForApproval(ctx context.Context, tx pgx.Tx, ac *assignmentContext, assignmentID uuid.UUID) error {
	dailyCap := s.dailyHourCapFor(ctx, assignmentID)
	rows, err := tx.Query(ctx, `
		SELECT TO_CHAR(work_date,'YYYY-MM-DD')
		FROM work_logs
		WHERE assignment_id=$1 AND status IN ('submitted','approved')
		GROUP BY work_date
		HAVING SUM(hours) > $2 + 0.01
		ORDER BY work_date`, assignmentID, dailyCap)
	if err != nil {
		return err
	}
	var badDays []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return err
		}
		badDays = append(badDays, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(badDays) > 0 {
		return Conflict(fmt.Sprintf(
			"อนุมัติไม่ได้ — ชั่วโมงรวมต่อวันเกิน %.1f ชม. ในวันที่: %s", dailyCap, strings.Join(badDays, ", ")))
	}

	if !ac.HasWorkloadForm {
		return nil
	}
	type bucket struct {
		cap   float64
		acts  []string
		label string
	}
	var buckets []bucket
	if ac.WeeklyLectureLabShared {
		buckets = append(buckets, bucket{ac.WeeklyCapLecture, []string{"lecture", "lab"}, "ช่วยสอน (บรรยาย+ปฏิบัติการ)"})
	} else {
		buckets = append(buckets,
			bucket{ac.WeeklyCapLecture, []string{"lecture"}, "เช็คชื่อบรรยาย"},
			bucket{ac.WeeklyCapLab, []string{"lab"}, "ปฏิบัติการ"})
	}
	buckets = append(buckets,
		bucket{ac.WeeklyCapReview, []string{"review"}, "ตรวจงาน"},
		bucket{ac.WeeklyCapOther, []string{"other"}, "อื่นๆ"})
	for _, b := range buckets {
		if b.cap <= 0 {
			continue
		}
		wrows, err := tx.Query(ctx, `
			SELECT TO_CHAR(date_trunc('week', work_date),'YYYY-MM-DD')
			FROM work_logs
			WHERE assignment_id=$1 AND status IN ('submitted','approved') AND activity = ANY($2)
			GROUP BY date_trunc('week', work_date)
			HAVING SUM(hours) > $3 + 0.01
			ORDER BY 1`, assignmentID, b.acts, b.cap)
		if err != nil {
			return err
		}
		var badWeeks []string
		for wrows.Next() {
			var wk string
			if err := wrows.Scan(&wk); err != nil {
				wrows.Close()
				return err
			}
			badWeeks = append(badWeeks, wk)
		}
		wrows.Close()
		if err := wrows.Err(); err != nil {
			return err
		}
		if len(badWeeks) > 0 {
			return Conflict(fmt.Sprintf(
				"อนุมัติไม่ได้ — เกินโควตา %s ประจำสัปดาห์ (%.1f ชม.) ในสัปดาห์ที่เริ่ม: %s",
				b.label, b.cap, strings.Join(badWeeks, ", ")))
		}
	}
	return nil
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
		`SELECT id, assignment_id, TO_CHAR(work_date,'YYYY-MM-DD'), start_time::text, end_time::text, hours, activity, parent_kind, room, note, status::text, reject_reason
		 FROM work_logs WHERE assignment_id=$1 ORDER BY work_date, start_time`, assignmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkLog{}
	for rows.Next() {
		var w WorkLog
		if err := rows.Scan(&w.ID, &w.AssignmentID, &w.WorkDate, &w.StartTime, &w.EndTime, &w.Hours, &w.Activity, &w.ParentKind, &w.Room, &w.Note, &w.Status, &w.RejectReason); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

// assertStudentCountFilled blocks worklog writes until staff record the
// course's student count. Payout is derived from num_students, so allowing
// entries before it is set would bill against a zero budget. Mirrors the export
// gate in BuildCourseZip.
func (s *WorkLogService) assertStudentCountFilled(ctx context.Context, teachingCourseID uuid.UUID) error {
	var numStudents int
	if err := s.pool.QueryRow(ctx,
		`SELECT num_students FROM teaching_courses WHERE id=$1`, teachingCourseID).Scan(&numStudents); err != nil {
		return err
	}
	if numStudents <= 0 {
		return Invalid("ยังไม่ได้กรอกจำนวนนักศึกษาของวิชานี้ กรุณาให้เจ้าหน้าที่กรอกจำนวนนักศึกษาก่อน จึงจะลงบันทึกเวลาได้")
	}
	return nil
}

// weeksInTermForCourse returns the term length in weeks (months × 4), matching
// the export payout basis (export.go). Defaults to 16 weeks (4 months) when the
// term's month count is unset.
func (s *WorkLogService) weeksInTermForCourse(ctx context.Context, teachingCourseID uuid.UUID) float64 {
	var months int
	_ = s.pool.QueryRow(ctx, `
		SELECT COALESCE(NULLIF(t.months, 0), 4)
		FROM academic_terms t JOIN teaching_courses tc ON tc.term_id = t.id
		WHERE tc.id = $1`, teachingCourseID).Scan(&months)
	if months <= 0 {
		months = 4
	}
	return float64(months) * 4.0
}

// enforceTermHourCeiling caps the TOTAL hours a TA may log for one assignment
// across the whole term at (declared weekly workload hours × weeks-in-term).
// The lecturer's workload declaration is the agreed workload, so logging beyond
// weekly-hours × term-weeks means over-billing. Skipped when no workload form is
// filed (nothing to bound against). excludeID excludes the row being edited.
func (s *WorkLogService) enforceTermHourCeiling(ctx context.Context, ac *assignmentContext, assignmentID, excludeID uuid.UUID, newHours float64) error {
	if !ac.HasWorkloadForm || ac.WeeklyTotalHours <= 0 {
		return nil
	}
	ceiling := ac.WeeklyTotalHours * s.weeksInTermForCourse(ctx, ac.TeachingCourseID)
	var used float64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(hours),0) FROM work_logs
		 WHERE assignment_id=$1 AND status <> 'rejected' AND id <> $2`,
		assignmentID, excludeID).Scan(&used); err != nil {
		return err
	}
	if used+newHours > ceiling+0.01 {
		remaining := ceiling - used
		if remaining < 0 {
			remaining = 0
		}
		return Invalid(fmt.Sprintf(
			"เกินเพดานชั่วโมงรวมทั้งเทอมของภาระงานนี้ (%.1f ชม.) — ลงได้อีก %.2f ชม.", ceiling, remaining))
	}
	return nil
}

func (s *WorkLogService) Upsert(ctx context.Context, actor uuid.UUID, w WorkLog) (uuid.UUID, error) {
	ac, err := s.assertTAOwnsAssignment(ctx, actor, w.AssignmentID)
	if err != nil {
		return uuid.Nil, err
	}
	if err := s.assertStudentCountFilled(ctx, ac.TeachingCourseID); err != nil {
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
	holidays, err := s.loadHolidaysInRange(ctx, termStart, termEnd)
	if err != nil {
		return uuid.Nil, err
	}
	mk, err := s.loadMakeupIndex(ctx, ac.SectionID)
	if err != nil {
		return uuid.Nil, err
	}
	if err := validateWorkLogEntry(w, activityGate{
		Scope:        ac.ReimburseScope,
		AllowLecture: ac.AllowLecture,
		AllowLab:     ac.AllowLab,
		AllowReview:  ac.AllowReview,
		AllowOther:   ac.AllowOther,
	}, termStart, termEnd, midterm, final, holidays, mk, time.Now()); err != nil {
		return uuid.Nil, err
	}
	// Month lock: a finance_sent month is frozen for everyone; a closed period
	// additionally blocks TA writes. Check the incoming date, and for edits also
	// the row's current date so an entry can't be moved into or out of a locked
	// month from either side.
	if err := assertWorklogWritable(ctx, s.pool, ac.TeachingCourseID, ac.TAID, w.WorkDate, true); err != nil {
		return uuid.Nil, err
	}
	if w.ID != uuid.Nil {
		var oldDate string
		if err := s.pool.QueryRow(ctx,
			`SELECT TO_CHAR(work_date,'YYYY-MM-DD') FROM work_logs WHERE id=$1 AND assignment_id=$2`,
			w.ID, w.AssignmentID).Scan(&oldDate); err == nil && oldDate[:7] != w.WorkDate[:7] {
			if err := assertWorklogWritable(ctx, s.pool, ac.TeachingCourseID, ac.TAID, oldDate, true); err != nil {
				return uuid.Nil, err
			}
		}
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

	// Weekly per-activity cap — the lecturer's workload declaration bounds
	// how many hours per week the TA may bill each activity for.
	if err := s.enforceWeeklyActivityCap(ctx, ac, w); err != nil {
		return uuid.Nil, err
	}

	// Term-total ceiling — declared weekly workload × weeks-in-term bounds the
	// cumulative hours across the whole term (not just per week).
	if err := s.enforceTermHourCeiling(ctx, ac, w.AssignmentID, w.ID, w.Hours); err != nil {
		return uuid.Nil, err
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

	// The same clock hours must not be billed twice across the TA's courses.
	if err := s.enforceNoOverlap(ctx, ac.TAID, w); err != nil {
		return uuid.Nil, err
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
	ac, err := s.assertTAOwnsAssignment(ctx, actor, assignmentID)
	if err != nil {
		return err
	}
	// Include 'rejected' so pressing "ส่งอนุมัติ" resubmits the whole batch —
	// TAs typically fix a handful of rows and expect the rest (which the
	// lecturer bounced together) to go along with them. reject_reason is
	// cleared so a stale message from the previous review doesn't leak into
	// the fresh submission if the lecturer bounces it a second time without
	// entering a new reason.
	//
	// Rows whose month is closed or already finance_sent are excluded (not
	// errored) so a stale draft in a locked month can never wedge the TA's
	// submission for the months that are still open.
	tag, err := s.pool.Exec(ctx, `
		UPDATE work_logs wl SET status='submitted', submitted_at=NOW(), reject_reason=NULL
		WHERE wl.assignment_id=$1 AND wl.status IN ('draft','rejected')
		  AND NOT EXISTS (
			SELECT 1
			FROM ta_request_assignments a
			JOIN sections sec ON sec.id = a.section_id
			JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
			JOIN academic_terms trm ON trm.id = tc.term_id
			JOIN submission_periods sp ON sp.term_id = tc.term_id
			 AND sp.year_month = trm.academic_year::text || '-' || to_char(wl.work_date, 'MM')
			LEFT JOIN submission_period_status st
			  ON st.submission_period_id = sp.id
			 AND st.ta_id = a.ta_id
			 AND st.teaching_course_id = tc.id
			WHERE a.id = wl.assignment_id
			  AND (sp.is_closed OR CURRENT_DATE > sp.due_date + INTERVAL '1 day'
			       OR COALESCE(st.status, '') IN ('exported','finance_sent'))
		  )`, assignmentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Distinguish "nothing to submit" from "everything is in a locked month"
		// so the TA isn't left guessing why the button did nothing.
		var candidates int
		if err := s.pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM work_logs WHERE assignment_id=$1 AND status IN ('draft','rejected')`,
			assignmentID).Scan(&candidates); err == nil && candidates > 0 {
			return Invalid("รายการทั้งหมดอยู่ในงวดที่ปิดรับหรือส่งการเงินไปแล้ว — ส่งอนุมัติไม่ได้ กรุณาติดต่อเจ้าหน้าที่")
		}
		return Invalid("ไม่มีรายการที่ส่งอนุมัติได้")
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "worklog.submit", Entity: "assignment", EntityID: assignmentID.String()})
	// Tell the course's lecturers there is work waiting — without this the
	// approver only discovers pending batches by polling their reports page.
	if s.notify != nil {
		if lects, err := courseLecturerIDs(ctx, s.pool, ac.TeachingCourseID); err == nil {
			for _, lid := range lects {
				s.notify.Send(ctx, lid,
					"มีบันทึกเวลารอการอนุมัติ",
					"TA ส่งบันทึกเวลาปฏิบัติงานรอการอนุมัติจากคุณ",
					"/lecturer/approvals")
			}
		}
	}
	return nil
}

func (s *WorkLogService) Approve(ctx context.Context, actor, assignmentID uuid.UUID, privileged bool) error {
	ac, err := s.assertCanReview(ctx, actor, assignmentID, privileged)
	if err != nil {
		return err
	}
	// Whole-batch guard: approval flips every submitted row at once, so refuse
	// outright if any of them falls in a month already handed to finance.
	if err := assertNoFinanceLockedRows(ctx, s.pool, assignmentID, []string{"submitted"}); err != nil {
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

	// Last-gate cap recheck: staff edits after submit can push a day/week past
	// its cap; refuse rather than approve hours that violate the pay rules.
	if err := s.recheckCapsForApproval(ctx, tx, ac, assignmentID); err != nil {
		return err
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

// ApprovalHistoryEntry is one row on the lecturer's "ประวัติการอนุมัติ"
// panel — an approve or reject action they've performed on a TA's worklog
// batch. Sourced from the audit_logs table so the record survives even if
// the underlying assignment is later modified.
type ApprovalHistoryEntry struct {
	ID           int64     `json:"id"`
	At           time.Time `json:"at"`
	Action       string    `json:"action"` // "worklog.approve" | "worklog.reject"
	AssignmentID uuid.UUID `json:"assignment_id"`
	TAName       string    `json:"ta_name"`
	SecNo        string    `json:"sec_no"`
	Track        string    `json:"track"`
	// Note holds the reject reason for reject actions; empty for approvals.
	Note string `json:"note,omitempty"`
}

// ListApprovalHistory returns the lecturer's own approve/reject actions on
// worklog batches within one teaching course, newest first. Authorization:
// the lecturer must currently teach the course — otherwise a rotated-off
// lecturer could still read history for courses they no longer own.
func (s *WorkLogService) ListApprovalHistory(ctx context.Context, actor, tcID uuid.UUID) ([]ApprovalHistoryEntry, error) {
	owns, err := lecturerOwnsCourse(ctx, s.pool, actor, tcID)
	if err != nil {
		return nil, err
	}
	if !owns {
		return nil, ErrForbidden
	}
	rows, err := s.pool.Query(ctx, `
		SELECT al.id, al.at, al.action, a.id, sec.sec_no, sec.track::text,
		       u.first_name || ' ' || u.last_name, COALESCE(al.note, '')
		FROM audit_logs al
		JOIN ta_request_assignments a ON a.id::text = al.entity_id
		JOIN sections sec ON sec.id = a.section_id
		JOIN users u ON u.id = a.ta_id
		WHERE al.actor_id = $1
		  AND al.entity = 'assignment'
		  AND al.action IN ('worklog.approve','worklog.reject')
		  AND sec.teaching_course_id = $2
		ORDER BY al.at DESC
		LIMIT 100`, actor, tcID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ApprovalHistoryEntry, 0)
	for rows.Next() {
		var e ApprovalHistoryEntry
		if err := rows.Scan(&e.ID, &e.At, &e.Action, &e.AssignmentID,
			&e.SecNo, &e.Track, &e.TAName, &e.Note); err != nil {
			return nil, err
		}
		out = append(out, e)
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
	// Locked is true when this row's month has reached exported/finance_sent for
	// the TA — the row is frozen for every role (assertWorklogWritable). The
	// editor renders locked rows read-only instead of letting a save 409.
	Locked bool `json:"locked"`
}

// StaffAssignment is one approved (TA × section) slot in a course, used by the
// staff editor's "add row" picker so a new work_log can target any TA/section
// — even one that has no rows yet.
type StaffAssignment struct {
	AssignmentID uuid.UUID `json:"assignment_id"`
	TAID         uuid.UUID `json:"ta_id"`
	TAName       string    `json:"ta_name"`
	StudentID    string    `json:"student_id"`
	SectionNo    string    `json:"section_no"`
	Track        string    `json:"track"`
	Level        string    `json:"level"`
	// IsReturning is true when the TA held an approved assignment in an earlier
	// academic term — the "เก่า/ใหม่" badge, same rule as the export coversheet.
	IsReturning bool `json:"is_returning"`
}

// StaffListByCourse returns every work_log tied to a teaching course, joined
// with the assignment's TA + section metadata. Staff-only.
func (s *WorkLogService) StaffListByCourse(ctx context.Context, tcID uuid.UUID) ([]WorkLogWithTA, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT wl.id, wl.assignment_id, TO_CHAR(wl.work_date,'YYYY-MM-DD'),
		       wl.start_time::text, wl.end_time::text, wl.hours, wl.activity,
		       wl.parent_kind, wl.room, wl.note, wl.status::text,
		       a.ta_id, u.first_name || ' ' || u.last_name,
		       fc.code, sec.sec_no, sec.track, a.level,
		       -- Month is frozen for this TA once it reaches exported/finance_sent.
		       -- Same (ta_id, tc, year_month, status) predicate as financeLockedMonths.
		       EXISTS (
		         SELECT 1 FROM submission_periods sp
		         JOIN submission_period_status st
		           ON st.submission_period_id = sp.id
		          AND st.ta_id = a.ta_id
		          AND st.teaching_course_id = tc.id
		          AND st.status IN ('exported','finance_sent')
		         WHERE sp.term_id = tc.term_id
		           AND sp.year_month = trm.academic_year::text || '-' || to_char(wl.work_date, 'MM')
		       ) AS locked
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		JOIN users u ON u.id = a.ta_id
		JOIN sections sec ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		JOIN academic_terms trm ON trm.id = tc.term_id
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
			&w.TAID, &w.TAName, &w.CourseCode, &w.SectionNo, &w.Track, &w.Level,
			&w.Locked); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// StaffListAssignments returns every approved (TA × section) slot in a course
// for the editor's add-row picker. Staff-only.
func (s *WorkLogService) StaffListAssignments(ctx context.Context, tcID uuid.UUID) ([]StaffAssignment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.ta_id, u.first_name || ' ' || u.last_name,
		       COALESCE(u.student_id,''), sec.sec_no, sec.track::text, a.level::text,
		       EXISTS(
		         SELECT 1 FROM ta_request_assignments pa
		         JOIN ta_requests pr ON pr.id = pa.request_id AND pr.status = 'approved'
		         JOIN sections psec ON psec.id = pa.section_id
		         JOIN teaching_courses ptc ON ptc.id = psec.teaching_course_id
		         JOIN academic_terms pt ON pt.id = ptc.term_id
		         JOIN teaching_courses ctc ON ctc.id = $1
		         JOIN academic_terms cur ON cur.id = ctc.term_id
		         WHERE pa.ta_id = a.ta_id
		           AND (pt.academic_year < cur.academic_year
		                OR (pt.academic_year = cur.academic_year AND pt.semester < cur.semester))
		       ) AS is_returning
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		JOIN users u ON u.id = a.ta_id
		JOIN sections sec ON sec.id = a.section_id
		WHERE sec.teaching_course_id = $1
		ORDER BY u.first_name, sec.sec_no`, tcID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StaffAssignment{}
	for rows.Next() {
		var a StaffAssignment
		if err := rows.Scan(&a.AssignmentID, &a.TAID, &a.TAName, &a.StudentID, &a.SectionNo, &a.Track, &a.Level, &a.IsReturning); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// StaffUpsert lets staff edit or add a work_log entry regardless of the row's
// current status. Enforces the same business rules as TA Upsert (hour cap /
// baht cap / other-cap-per-session / parent_kind). Preserves the current
// status (so an approved row stays approved) — the intent is "fix a typo
// before export" not "undo review". Audits before/after and notifies the TA.
func (s *WorkLogService) StaffUpsert(ctx context.Context, staffID uuid.UUID, w WorkLog) (uuid.UUID, error) {
	// For an existing row the assignment is authoritative from the DB, never the
	// request body. Otherwise a caller could send a locked row's id together with
	// some OTHER (unlocked) assignment_id: every lock/cap check below would run
	// against the unlocked assignment while the UPDATE (matched by id) rewrote the
	// locked row. Pin w.AssignmentID to the row's real owner up front.
	if w.ID != uuid.Nil {
		var realAssignment uuid.UUID
		if err := s.pool.QueryRow(ctx,
			`SELECT assignment_id FROM work_logs WHERE id=$1`, w.ID).Scan(&realAssignment); err != nil {
			return uuid.Nil, Invalid("ไม่พบรายการที่ต้องการแก้ไข")
		}
		w.AssignmentID = realAssignment
	}
	ac, err := loadAssignmentContext(ctx, s.pool, w.AssignmentID)
	if err != nil {
		return uuid.Nil, err
	}
	if err := s.assertStudentCountFilled(ctx, ac.TeachingCourseID); err != nil {
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
	holidays, err := s.loadHolidaysInRange(ctx, termStart, termEnd)
	if err != nil {
		return uuid.Nil, err
	}
	mk, err := s.loadMakeupIndex(ctx, ac.SectionID)
	if err != nil {
		return uuid.Nil, err
	}
	if err := validateWorkLogEntry(w, activityGate{
		Scope:        ac.ReimburseScope,
		AllowLecture: ac.AllowLecture,
		AllowLab:     ac.AllowLab,
		AllowReview:  ac.AllowReview,
		AllowOther:   ac.AllowOther,
	}, termStart, termEnd, midterm, final, holidays, mk, time.Time{}); err != nil {
		return uuid.Nil, err
	}
	// Finance lock applies to staff too — once a month reached finance_sent the
	// paperwork is out the door; edits must go through the admin unlock first.
	// (Closed-but-not-sent months stay staff-editable: taActor=false.)
	if err := assertWorklogWritable(ctx, s.pool, ac.TeachingCourseID, ac.TAID, w.WorkDate, false); err != nil {
		return uuid.Nil, err
	}
	if w.ID != uuid.Nil {
		var oldDate string
		if err := s.pool.QueryRow(ctx,
			`SELECT TO_CHAR(work_date,'YYYY-MM-DD') FROM work_logs WHERE id=$1 AND assignment_id=$2`,
			w.ID, w.AssignmentID).Scan(&oldDate); err == nil && oldDate[:7] != w.WorkDate[:7] {
			if err := assertWorklogWritable(ctx, s.pool, ac.TeachingCourseID, ac.TAID, oldDate, false); err != nil {
				return uuid.Nil, err
			}
		}
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
	if err := s.enforceWeeklyActivityCap(ctx, ac, w); err != nil {
		return uuid.Nil, err
	}
	if err := s.enforceTermHourCeiling(ctx, ac, w.AssignmentID, w.ID, w.Hours); err != nil {
		return uuid.Nil, err
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
	if err := s.enforceNoOverlap(ctx, ac.TAID, w); err != nil {
		return uuid.Nil, err
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
		// The assignment_id predicate is defence-in-depth on top of the pin above.
		tag, err := s.pool.Exec(ctx,
			`UPDATE work_logs SET work_date=$1::date, start_time=$2::time, end_time=$3::time, hours=$4, activity=$5, parent_kind=$6, room=$7, note=$8
			 WHERE id=$9 AND assignment_id=$10`,
			w.WorkDate, w.StartTime, w.EndTime, w.Hours, w.Activity, w.ParentKind, w.Room, w.Note, w.ID, w.AssignmentID)
		if err != nil {
			return uuid.Nil, err
		}
		if tag.RowsAffected() == 0 {
			return uuid.Nil, Invalid("ไม่พบรายการที่ต้องการแก้ไข")
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

// Delete removes a single work_log entry owned by the calling TA. Draft and
// rejected rows are the only removable states — submitted/approved must go
// through Reject or staff intervention so the audit trail isn't lost.
func (s *WorkLogService) Delete(ctx context.Context, actor, logID uuid.UUID) error {
	// Resolve the owning assignment + status in one query. When the row is
	// missing the join returns zero rows and RowsAffected on the follow-up
	// DELETE tells us the same, so we don't need a separate not-found branch.
	var assignmentID uuid.UUID
	var status, workDate string
	if err := s.pool.QueryRow(ctx,
		`SELECT assignment_id, status::text, TO_CHAR(work_date,'YYYY-MM-DD') FROM work_logs WHERE id = $1`, logID,
	).Scan(&assignmentID, &status, &workDate); err != nil {
		return Invalid("ไม่พบรายการที่ต้องการลบ")
	}
	ac, err := s.assertTAOwnsAssignment(ctx, actor, assignmentID)
	if err != nil {
		return err
	}
	if status != "draft" && status != "rejected" {
		return Invalid("ลบไม่ได้: รายการนี้ถูกส่งอนุมัติหรืออนุมัติแล้ว")
	}
	if err := assertWorklogWritable(ctx, s.pool, ac.TeachingCourseID, ac.TAID, workDate, true); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM work_logs WHERE id=$1 AND status IN ('draft','rejected')`, logID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return Invalid("ลบไม่ได้: รายการอาจถูกดำเนินการไปแล้ว")
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "worklog.delete", Entity: "work_log", EntityID: logID.String()})
	return nil
}

// StaffDelete removes a work_log entry. Only draft/rejected can be removed;
// submitted/approved rows must be handled through Reject or a manual DB fix
// so the audit trail stays intact.
func (s *WorkLogService) StaffDelete(ctx context.Context, staffID, id uuid.UUID) error {
	var (
		taID, tcID         uuid.UUID
		workDate, timeSpan string
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT a.ta_id, sec.teaching_course_id,
		       TO_CHAR(wl.work_date,'YYYY-MM-DD'),
		       wl.start_time::text || '–' || wl.end_time::text
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		JOIN sections sec ON sec.id = a.section_id
		WHERE wl.id = $1`, id).Scan(&taID, &tcID, &workDate, &timeSpan); err != nil {
		return Invalid("ไม่พบรายการที่ต้องการลบ")
	}
	if err := assertWorklogWritable(ctx, s.pool, tcID, taID, workDate, false); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM work_logs WHERE id=$1 AND status IN ('draft','rejected')`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return Invalid("ลบไม่ได้: รายการอาจถูกส่งอนุมัติหรืออนุมัติแล้ว")
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &staffID, Action: "worklog.staff_delete", Entity: "work_log", EntityID: id.String()})
	if s.notify != nil {
		s.notify.Send(ctx, taID,
			"เจ้าหน้าที่ลบบันทึกเวลา",
			fmt.Sprintf("เจ้าหน้าที่ลบรายการบันทึกเวลาวันที่ %s เวลา %s", workDate, timeSpan),
			"/ta/worklog")
	}
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
	// Mirror Approve: a month at finance_sent is settled — bouncing its rows
	// back would desync the paperwork already at the finance office.
	if err := assertNoFinanceLockedRows(ctx, s.pool, assignmentID, []string{"submitted"}); err != nil {
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
