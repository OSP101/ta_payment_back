package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/timeutil"
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

// loadHolidaysInRange loads every public_holidays row in [start, end] into the
// date→closures index. Called on the pay-critical Upsert path with no cache:
// table is small (<200 rows) and the query is indexed on holiday_date, so
// freshness matters more than saving microseconds.
func (s *WorkLogService) loadHolidaysInRange(ctx context.Context, start, end time.Time) (holidaySet, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT TO_CHAR(holiday_date,'YYYY-MM-DD'), name_th,
		        TO_CHAR(start_time,'HH24:MI'), TO_CHAR(end_time,'HH24:MI')
		 FROM public_holidays
		 WHERE holiday_date BETWEEN $1::date AND $2::date
		 ORDER BY holiday_date, start_time NULLS FIRST`,
		start.Format("2006-01-02"), end.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := holidaySet{}
	for rows.Next() {
		var d, n string
		var st, et *string
		if err := rows.Scan(&d, &n, &st, &et); err != nil {
			return nil, err
		}
		w := holidayWindow{name: n, allDay: st == nil || et == nil}
		if !w.allDay {
			sm, ok1 := parseHM(*st)
			em, ok2 := parseHM(*et)
			if !ok1 || !ok2 {
				// Unparseable window: fall back to closing the whole day. The
				// CHECK constraint makes this unreachable, and if it ever is
				// reached, over-blocking is the side to fail on — a TA who
				// cannot log asks; a TA who is wrongly paid does not.
				w.allDay = true
			} else {
				w.startMin, w.endMin = sm, em
			}
		}
		out[d] = append(out[d], w)
	}
	return out, nil
}

// loadMakeupIndex loads every filed makeup for a section into both direction
// maps needed by the validator. Time-of-day is ignored here — the validator
// gates on the date bucket only; time-vs-schedule checks happen at makeup
// creation, not on each worklog Upsert.
func (s *WorkLogService) loadMakeupIndex(ctx context.Context, sectionID uuid.UUID) (makeupIndex, error) {
	idx := makeupIndex{
		byOriginal: map[string][]time.Time{},
		byMakeup:   map[string]bool{},
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
		ok := orig.Format("2006-01-02")
		idx.byOriginal[ok] = append(idx.byOriginal[ok], mk)
		idx.byMakeup[mk.Format("2006-01-02")] = true
	}
	return idx, nil
}

// holidayWindow is one closure on one date. A faculty holiday may cover only
// part of the day (migration 0058) — the college holds a ceremony in the
// morning and teaches in the afternoon — so a closure is a time range, not a
// flag. allDay is the pre-0058 shape (NULL/NULL in the table) and every
// national/university holiday; it blocks the date outright.
type holidayWindow struct {
	name     string
	allDay   bool
	startMin int // minutes from midnight, meaningless when allDay
	endMin   int
}

// labelTH names the closure for a refusal message, with its hours when partial.
// The hours are not decoration: told only "วันกีฬาคณะ", a TA whose lab starts at
// 13:00 has no way to tell whether the block is a mistake.
func (w holidayWindow) labelTH() string {
	if w.allDay {
		return w.name
	}
	return fmt.Sprintf("%s %02d:%02d–%02d:%02d",
		w.name, w.startMin/60, w.startMin%60, w.endMin/60, w.endMin%60)
}

// holidaySet is the compact view of the public_holidays table used by the
// worklog validator and generator: date key "YYYY-MM-DD" -> the closures on
// that date. Populated per Upsert call so a freshly-added holiday takes effect
// immediately (no caching on the pay-critical path).
//
// A date holds a SLICE because closures now compose: a national holiday and a
// faculty half-day can share a date, as can two faculty windows (a morning
// ceremony and an evening event, teaching in between).
type holidaySet map[string][]holidayWindow

// overlapping returns the first closure on `date` that covers any part of
// [startMin, endMin), the half-open span of the work being logged.
//
// Half-open on both sides is what makes back-to-back slots legal: a ceremony
// ending at 12:00 does not block a lab starting at 12:00. The same comparison
// is written in SQL in ImpactsForCourse and UnresolvedMakeupsSQL; all three
// must agree or the page, the badge and the validator start contradicting each
// other about whether a period survived.
func (h holidaySet) overlapping(date string, startMin, endMin int) (holidayWindow, bool) {
	for _, w := range h[date] {
		if w.allDay || (w.startMin < endMin && w.endMin > startMin) {
			return w, true
		}
	}
	return holidayWindow{}, false
}

// makeupIndex is the by-section lookup of makeup_schedules built once per
// Upsert. Both directions are needed: byOriginal answers "was the class on
// this day shifted to another day?" (block logging on the original) and
// byMakeup answers "is this day someone's approved makeup?" (allow logging on
// a normally-non-class day, e.g. a Saturday).
//
// byOriginal holds a SLICE because one cancelled day can have several makeups —
// one per period (see migration 0055). It used to be a single time.Time, so a
// section that moved its lecture and its lab to different days kept only whichever
// row the query happened to read last, and the refusal message named the wrong
// date. Membership is all byMakeup is ever asked, so it is a plain set.
type makeupIndex struct {
	byOriginal map[string][]time.Time
	byMakeup   map[string]bool
}

// activityGate is the compact "what's this TA authorized to log" packet the
// worklog validator needs. Kept as a struct (rather than a bag of args) so
// callers pick fields off the assignmentContext they already have and don't
// have to remember which positional argument means what.
//
//	Scope        = ta_requests.reimburse_scope (lecture/lab/both)
//	AllowLecture = ta_workload_forms declares attendance/help-teach hours > 0
//	AllowLab     = ta_workload_forms declares lab/help-teach hours > 0
//	AllowReview  = ta_workload_forms declares check-work/grade hours > 0
//	AllowOther   = ta_workload_forms declares ug_other/other/prep hours > 0
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
			return Invalid("บันทึกย้อนหลังในเดือนที่ผ่านไปแล้วไม่ได้ ลงเวลาได้ตั้งแต่วันที่ 1 ของเดือนปัจจุบันเป็นต้นไป")
		}
	}
	// Exam-window blackout — faculty publishes the range on the term. TA
	// worklog stops accruing pay during exams even if the section still has
	// scheduled meetings that week.
	if midterm.contains(d) {
		return Invalid("วันที่ทำงานตรงกับช่วงสอบกลางภาค ลงเวลาไม่ได้")
	}
	if final.contains(d) {
		return Invalid("วันที่ทำงานตรงกับช่วงสอบปลายภาค ลงเวลาไม่ได้")
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
			return Invalid("คำขอนี้อนุมัติเฉพาะปฏิบัติการ ลง 'บรรยาย' ไม่ได้")
		}
	case "lab":
		if scope == "lecture" {
			return Invalid("คำขอนี้อนุมัติเฉพาะบรรยาย ลง 'ปฏิบัติการ' ไม่ได้")
		}
	case "other":
		if w.ParentKind != nil {
			if scope == "lab" && *w.ParentKind == "lecture" {
				return Invalid("คำขอนี้อนุมัติเฉพาะปฏิบัติการ ลง 'อื่นๆ (คู่กับบรรยาย)' ไม่ได้")
			}
			if scope == "lecture" && *w.ParentKind == "lab" {
				return Invalid("คำขอนี้อนุมัติเฉพาะบรรยาย ลง 'อื่นๆ (คู่กับปฏิบัติการ)' ไม่ได้")
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
			return Invalid("อาจารย์ไม่ได้ระบุชั่วโมงเช็คชื่อบรรยายในคำขอ ลง 'บรรยาย' ไม่ได้")
		}
	case "lab":
		if !gate.AllowLab {
			return Invalid("อาจารย์ไม่ได้ระบุชั่วโมงปฏิบัติการในคำขอ ลง 'ปฏิบัติการ' ไม่ได้")
		}
	case "review":
		if !gate.AllowReview {
			return Invalid("อาจารย์ไม่ได้ระบุชั่วโมงตรวจงานในคำขอ ลง 'ตรวจงาน' ไม่ได้")
		}
	case "other":
		if !gate.AllowOther {
			return Invalid("อาจารย์ไม่ได้ระบุชั่วโมงงานอื่นๆ ในคำขอ ลง 'อื่นๆ' ไม่ได้")
		}
	case "makeup":
		if !gate.AllowLecture && !gate.AllowLab {
			return Invalid("อาจารย์ไม่ได้ระบุชั่วโมงคาบเรียนในคำขอ ลงชดเชยไม่ได้")
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
	//
	// "On a holiday" is decided against THIS ENTRY'S HOURS, not its date: a
	// faculty closure covering 08:00–12:00 leaves the 13:00–16:00 lab running,
	// and blocking it would deny the TA pay for a class that actually met.
	// All-day closures (every national/university holiday) overlap everything,
	// so the rules above are unchanged for them.
	dkey := w.WorkDate
	holiday, isHoliday := holidays.overlapping(dkey, sm, em)
	isMakeupDay := mk.byMakeup[dkey]
	shiftedTo, isMovedAway := mk.byOriginal[dkey]

	switch w.Activity {
	case "lecture", "lab":
		if isMovedAway {
			// Name every replacement date, not just one: the lecture and the lab
			// of this day may have moved to different days, and telling the TA
			// only one of them sends half of them to the wrong date.
			dates := make([]string, 0, len(shiftedTo))
			seen := map[string]bool{}
			for _, d := range shiftedTo {
				s := d.Format("2006-01-02")
				if !seen[s] {
					seen[s] = true
					dates = append(dates, s)
				}
			}
			sort.Strings(dates)
			return Invalid(fmt.Sprintf(
				"วันนี้ (%s) ถูกย้ายไปเป็นวันชดเชย %s แล้ว กรุณาลงในวันชดเชยแทน",
				dkey, strings.Join(dates, " / ")))
		}
		if isHoliday && !isMakeupDay {
			if holiday.allDay {
				return Invalid(fmt.Sprintf(
					"วันที่ %s ตรงกับวันหยุด (%s) ต้องกำหนดวันชดเชยก่อน จึงจะลงเวลาได้ (ไปที่หน้า “วันหยุดและวันชดเชย”)",
					dkey, holiday.labelTH()))
			}
			// Partial closure: the fix may be as small as moving the entry to the
			// free part of the same day, so say which hours are closed instead of
			// sending the TA to wait for a makeup they may not need.
			return Invalid(fmt.Sprintf(
				"เวลา %s–%s ของวันที่ %s อยู่ในช่วงวันหยุด (%s) ลงเวลาได้เฉพาะช่วงที่ไม่ตรงกับวันหยุด หรือกำหนดวันชดเชยที่หน้า “วันหยุดและวันชดเชย”",
				w.StartTime, w.EndTime, dkey, holiday.labelTH()))
		}
	case "makeup":
		if !isMakeupDay {
			return Invalid("วันที่เลือกยังไม่ได้ถูกกำหนดเป็นวันชดเชย กรุณาระบุวันชดเชยที่หน้า “วันหยุดและวันชดเชย” ก่อน")
		}
	case "review":
		// Intentional no-op — reviewing homework can happen on any date.
	case "other":
		if isHoliday && !isMakeupDay {
			// parent_kind is validated non-nil above for activity="other".
			if w.ParentKind != nil && *w.ParentKind == "lab" {
				return Invalid(fmt.Sprintf(
					"วันหยุด (%s) กิจกรรม 'อื่นๆ' ที่คู่กับปฏิบัติการ ทำได้เฉพาะช่วงเวลาที่มีคาบเรียนจริง",
					holiday.labelTH()))
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
	// export owns the budget settlement. Approving is the moment the committed
	// figure moves, so it is where the shortfall warning is worth recomputing —
	// not on every work-log write, which would recompute it dozens of times a
	// day to say the same thing.
	export *ExportService
}

// warnBudgetAfterApproval fires the shortfall notice, once per change. Runs
// after the approval has committed and never affects its outcome: a warning
// that failed to send must not undo work that was correctly approved.
func (s *WorkLogService) warnBudgetAfterApproval(ctx context.Context, courseIDs ...uuid.UUID) {
	if s.export == nil {
		return
	}
	seen := map[uuid.UUID]bool{}
	for _, c := range courseIDs {
		if c == uuid.Nil || seen[c] {
			continue
		}
		seen[c] = true
		s.export.NotifyBudgetShortfall(ctx, c)
	}
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
	// Source is 'auto' (generated from the section timetable) or 'manual' (typed
	// by the TA). The payout-review screen shows the two differently: a generated
	// row is a copy of times the lecturer entered, so there is nothing in it to
	// check, while a hand-typed row is a claim with no other source. See
	// migration 0057.
	Source string `json:"source"`
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

// SkipGroup counts the occurrences auto-generation refused for one reason.
// Grouped rather than listed per date: a weekly clash produces fifteen-odd
// identical skips over a term, and fifteen identical lines read as noise.
type SkipGroup struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// GenerateResult carries what auto-generation produced AND what it deliberately
// left out. The meeting asked for the "why" to be explicit: a TA who presses
// generate and gets fewer rows than expected must be able to see that their own
// timetable is the reason, rather than assuming the system is broken.
type GenerateResult struct {
	Entries         []WorkLog   `json:"entries"`
	SkippedOwnClass []SkipGroup `json:"skipped_own_class"`
	// StoppedAtTermCeiling is set when generation stopped early because the next
	// class occurrence would exceed the TA's total hours for the term. Without it
	// the TA just gets fewer rows than their timetable has, with no reason given.
	StoppedAtTermCeiling bool    `json:"stopped_at_term_ceiling,omitempty"`
	TermHourCeiling      float64 `json:"term_hour_ceiling,omitempty"`
}

func (s *WorkLogService) Generate(ctx context.Context, actor, assignmentID uuid.UUID) (*GenerateResult, error) {
	ac, err := s.assertTAOwnsAssignment(ctx, actor, assignmentID)
	if err != nil {
		return nil, err
	}
	if err := s.assertOwnScheduleForCourse(ctx, ac); err != nil {
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
		return nil, Invalid("ยังไม่ได้กำหนดวันเริ่ม/วันสิ้นสุดของเทอมหรือรายวิชา เจ้าหน้าที่ต้องตั้งช่วงเวลาก่อน จึงจะสร้างรายการอัตโนมัติได้")
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
	// Total hours already committed by this run. Generate enforced every OTHER
	// limit — daily hours, daily baht, activity scope, holidays, own-class clashes
	// — but never the term total, which is the one the TA is actually paid
	// against. Nothing stopped it walking the calendar past the ceiling, and on
	// the real data every TA of SC362102 ended the term 2 hours over.
	//
	// The ceiling itself is separately fixed (see WeeksInTerm): with the real
	// calendar it now sits above what a full timetable produces, so this is the
	// net rather than the thing doing the cutting. It stays because a schedule
	// that changes later must not be able to silently overshoot again.
	termCeiling := 0.0
	if ac.HasWorkloadForm && ac.WeeklyTotalHours > 0 {
		termCeiling = ac.WeeklyTotalHours * s.weeksInTermForCourse(ctx, ac.TeachingCourseID)
	}
	termHours := 0.0
	stoppedAtCeiling := false
	termBlocks := func(hrs float64) bool {
		if termCeiling <= 0 {
			return false
		}
		if termHours+hrs > termCeiling+0.01 {
			stoppedAtCeiling = true
			return true
		}
		return false
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
	// Rows that will SURVIVE the wipe below — submitted/approved ones, plus
	// drafts parked in blocked months — still count against the term total.
	// Counting every existing row instead double-counted the drafts this very
	// run is about to delete and recreate: the first regeneration after a full
	// term of drafts found the ceiling "already spent" and produced almost
	// nothing.
	if termCeiling > 0 {
		if err := s.pool.QueryRow(ctx,
			`SELECT COALESCE(SUM(hours),0) FROM work_logs
			  WHERE assignment_id=$1
			    AND (status <> 'draft' OR to_char(work_date,'MM') = ANY($2))`,
			assignmentID, blockedMM).Scan(&termHours); err != nil {
			return nil, err
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
	// Keyed by (original date, PERIOD kind): a section that teaches a lecture and
	// a lab on the same cancelled day files a makeup for each, and they may land on
	// different days. Keyed by date alone, one row overwrote the other and both
	// periods were generated on whichever date was read last — the same defect the
	// holidays page had (see migration 0055).
	type makeupKey struct{ date, kind string }
	// A makeup may carry its own time window (a Tuesday class moved to Saturday
	// evening). Generate used to read only the DATE and keep the original
	// period's times — so the row landed at an hour when nothing happened, and a
	// makeup deliberately placed outside a partial closure was re-planted inside
	// it and skipped. The lecturer's explicit window, when present, IS the duty.
	type makeupTo struct {
		date       time.Time
		start, end string
		hasTime    bool
	}
	makeup := map[makeupKey]makeupTo{}
	if rows, err := s.pool.Query(ctx,
		`SELECT original_date, makeup_date, kind,
		        TO_CHAR(start_time,'HH24:MI'), TO_CHAR(end_time,'HH24:MI')
		 FROM makeup_schedules WHERE section_id=$1`,
		sectionID); err == nil {
		for rows.Next() {
			var orig, mk time.Time
			var kind string
			var st, en *string
			if err := rows.Scan(&orig, &mk, &kind, &st, &en); err == nil {
				to := makeupTo{date: mk}
				if st != nil && en != nil {
					to.start, to.end, to.hasTime = *st, *en, true
				}
				makeup[makeupKey{orig.Format("2006-01-02"), kind}] = to
			}
		}
		rows.Close()
	}

	// Load schedules
	type sch struct {
		kind  string
		day   int
		start string
		end   string
		hours float64
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

	// The TA's own timetable, loaded once. An empty list means nothing to
	// check — a TA who has not filed a timetable is handled at request time,
	// not here.
	genTermID, err := courseTermID(ctx, s.pool, ac.TeachingCourseID)
	if err != nil {
		return nil, err
	}
	ownClasses, err := loadOwnClassBlocks(ctx, s.pool, ac.TAID, genTermID)
	if err != nil {
		return nil, err
	}
	// Counted per clashing class rather than per occurrence, so the TA reads
	// "ข้ามไป 14 คาบ เพราะตรงกับ X" instead of fourteen identical lines.
	skippedByClass := map[string]int{}

	out := []WorkLog{}
	dailyHrs := map[string]float64{}
	for d := startsOn; !d.After(endsOn); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		// Whether the day is "closed" is now a per-period question — a faculty
		// closure may cover the morning lecture and leave the afternoon lab
		// running — so it is asked inside the schedule loop, with that period's
		// hours, rather than once for the whole date here.
		//
		// Schedules are matched against the ORIGINAL weekday, and each matched
		// period is then shifted by ITS OWN makeup below. Previously the day was
		// shifted first and schedules matched against the shifted weekday, which
		// only worked when a makeup kept the same weekday: a Tuesday class moved to
		// a Saturday looked for Saturday schedules, found none, and generated
		// nothing — while the weekend gate below discarded it anyway.
		origWeekday := int(d.Weekday())
		if origWeekday == 0 || origWeekday == 6 {
			// No class is scheduled on a weekend, so there is nothing to shift.
			// A makeup that LANDS on a weekend is still generated — it is reached
			// through its original weekday, and the validator likewise permits
			// logging on a makeup Saturday.
			continue
		}
		for _, sc := range schs {
			if sc.day != origWeekday {
				continue
			}
			// This period's own replacement date, if the lecturer filed one.
			// rowStart/rowEnd/rowHours are what the generated ROW will carry —
			// they start as the period itself and may be replaced by the
			// makeup's explicit window, or narrowed to the attendance window
			// below. The closure tests keep using the PERIOD's window: whether
			// the class happened is a question about the class, not the duty.
			useDate := d
			rowStart, rowEnd, rowHours := sc.start, sc.end, sc.hours
			explicitTime := false
			if mk, ok := makeup[makeupKey{key, sc.kind}]; ok {
				useDate = mk.date
				if mk.hasTime {
					rowStart, rowEnd = mk.start, mk.end
					if sm, ok1 := parseHM(mk.start); ok1 {
						if em, ok2 := parseHM(mk.end); ok2 {
							rowHours = float64(em-sm) / 60.0
						}
					}
					explicitTime = true
				}
			}
			// เช็คชื่อ occupies the LAST attendance_hrs of the lecture, not the
			// whole period: the faculty's own claim forms bill a 13.00–15.00
			// lecture as "14.00 - 15.00 เช็คชื่อ" when the declared attendance is
			// one hour. Generating the full span both contradicted the signed
			// form and immediately blew the weekly attendance cap it was
			// declared under. A makeup with an explicit window is exempt — the
			// lecturer already picked the exact duty hour.
			if sc.kind == "lecture" && !explicitTime && ac.Level == "undergrad" &&
				ac.HasWorkloadForm && ac.WeeklyCapLecture > 0 && ac.WeeklyCapLecture < rowHours-0.01 {
				if em, ok := parseHM(rowEnd); ok {
					rowStart = hhmm(em - int(ac.WeeklyCapLecture*60+0.5))
					rowHours = ac.WeeklyCapLecture
				}
			}
			// This period's hours, used by the holiday overlap tests and reused
			// by the own-class clash check below. An unparseable schedule time
			// degrades to "the whole day", which blocks rather than pays.
			scStart, okS := parseHM(sc.start)
			scEnd, okE := parseHM(sc.end)
			hStart, hEnd := scStart, scEnd
			if !okS || !okE {
				hStart, hEnd = 0, 24*60
			}
			// Holiday gate: the original slot is closed and THIS period has no
			// makeup, so it simply does not happen. The TA sees the gap on the
			// holidays page and can nudge the lecturer; the next regeneration
			// picks it up on the shifted date. Checked per period because the
			// lecture may be rescheduled while the lab is not — and now also
			// because a partial closure may cover one period of the day and not
			// the other, in which case the untouched period generates normally.
			// "Has a makeup" means the date moved OR the time did — a same-day
			// evening makeup keeps the date, and judging it by date alone
			// re-cancelled exactly the class the lecturer had rescued.
			if _, closed := holidays.overlapping(key, hStart, hEnd); closed && useDate.Equal(d) && !explicitTime {
				continue
			}
			// If the SHIFTED slot itself lands on a closure (nested holiday —
			// lecturer picked a makeup that also happens to be closed), skip too.
			// AddMakeup rejects this at create time; this is the defensive check
			// for a holiday added AFTER a makeup was filed. Compared against the
			// ROW's window, which is the makeup's own when it carries one — a
			// class moved to the evening of a daytime closure is exactly the
			// arrangement the explicit window exists to express.
			shStart, shEnd := hStart, hEnd
			if explicitTime {
				if v, ok := parseHM(rowStart); ok {
					shStart = v
				}
				if v, ok := parseHM(rowEnd); ok {
					shEnd = v
				}
			}
			if _, closed := holidays.overlapping(useDate.Format("2006-01-02"), shStart, shEnd); closed {
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
			// The TA's own class wins. Evaluated against useDate (the makeup
			// date when one was filed), not the original slot: a class moved
			// to Saturday afternoon can clash where the Monday slot did not,
			// and vice versa. scStart/scEnd were parsed above for the holiday
			// overlap test.
			//
			// Same exemption as the request gate and the manual-entry gate: a
			// lecture period may overlap the TA's own class. Skipping it here too
			// would generate a short month for hours the TA is allowed to work.
			if okS && okE && clashBlockingKind(sc.kind, nil) {
				if clash := findOwnClassClash(ownClasses, int(useDate.Weekday()), scStart, scEnd); clash != nil {
					skippedByClass[clash.describe()]++
					continue
				}
			}
			// enforce per-track daily hour cap (Q&A rule 6d) — on the hours the
			// row will actually carry, not the full period's.
			if dailyHrs[useDate.Format("2006-01-02")]+rowHours > dailyHourCap {
				continue
			}
			if bahtBlocks(useDate.Format("2006-01-02"), rowHours) {
				continue
			}
			if termBlocks(rowHours) {
				continue
			}
			id := uuid.New()
			// Detect whether this row landed on a makeup date so the note
			// picks up the "(ชดเชย)" suffix. A same-day makeup with a new time
			// (a daytime closure pushing the class to the evening) is still a
			// makeup, so the explicit window counts too.
			note := autoNoteFor(sc.kind, !useDate.Equal(d) || explicitTime)
			if _, err := tx.Exec(ctx,
				`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, note, status, source)
				 VALUES ($1,$2,$3::date,$4::time,$5::time,$6,$7,$8,'draft','auto')`,
				id, assignmentID, useDate.Format("2006-01-02"), rowStart, rowEnd, rowHours, sc.kind, note); err != nil {
				return nil, err
			}
			out = append(out, WorkLog{ID: id, AssignmentID: assignmentID,
				WorkDate: useDate.Format("2006-01-02"), StartTime: rowStart, EndTime: rowEnd,
				Hours: rowHours, Activity: sc.kind, Note: &note, Status: "draft"})
			dailyHrs[useDate.Format("2006-01-02")] += rowHours
			dailyBaht[useDate.Format("2006-01-02")] += rowHours * genRate
			termHours += rowHours
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
			// Generate INSERTs directly, bypassing Upsert's gates, so it must
			// apply the class-timetable rule itself — otherwise it would plant
			// rows the TA can neither edit nor legitimately submit.
			if rd, err := timeutil.ParseDate(dstr); err == nil {
				rs, okS := parseHM(start)
				re, okE := parseHM(end)
				if okS && okE {
					if clash := findOwnClassClash(ownClasses, int(rd.Weekday()), rs, re); clash != nil {
						skippedByClass[clash.describe()]++
						continue
					}
				}
			}
			if dailyHrs[dstr]+hrs > dailyHourCap {
				continue
			}
			if bahtBlocks(dstr, hrs) {
				continue
			}
			if termBlocks(hrs) {
				continue
			}
			id := uuid.New()
			note := autoNoteFor("review", false)
			if _, err := tx.Exec(ctx,
				`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, note, status, source)
				 VALUES ($1,$2,$3::date,$4::time,$5::time,$6,'review',$7,'draft','auto')`,
				id, assignmentID, dstr, start, end, hrs, note); err != nil {
				reviewRows.Close()
				return nil, err
			}
			out = append(out, WorkLog{ID: id, AssignmentID: assignmentID,
				WorkDate: dstr, StartTime: start, EndTime: end,
				Hours: hrs, Activity: "review", Note: &note, Status: "draft"})
			dailyHrs[dstr] += hrs
			dailyBaht[dstr] += hrs * genRate
			termHours += hrs
		}
		reviewRows.Close()

	}

	// TA-owned weekly duty slots. Same weekly expansion as class schedules, but
	// the source is per-assignment and the kind decides which activity it lands
	// on. This block sits OUTSIDE the AllowReview gate on purpose: "อื่น ๆ" is
	// authorized by ug_other_hrs / lab_other_hrs, and keeping it inside meant a
	// TA whose lecturer asked only for other-work generated nothing at all.
	//
	// These are allowed on holidays (Q&A rule 7) — the work happens off-site —
	// so holiday dates are NOT skipped here; only exam windows and the hour and
	// baht ceilings can stop an entry.
	type trs struct {
		kind       string
		activity   string
		parentKind *string
		day        int
		start      string
		end        string
		hours      float64
	}
	var trss []trs
	trRows, err := tx.Query(ctx,
		`SELECT kind, day_of_week, start_time::text, end_time::text,
		        EXTRACT(EPOCH FROM (end_time - start_time))/3600
		 FROM ta_review_schedules WHERE assignment_id=$1`, assignmentID)
	if err != nil {
		return nil, err
	}
	for trRows.Next() {
		var r trs
		if err := trRows.Scan(&r.kind, &r.day, &r.start, &r.end, &r.hours); err != nil {
			trRows.Close()
			return nil, err
		}
		// Workload gate, per kind rather than per table.
		switch r.kind {
		case DutyReview:
			if !ac.AllowReview {
				continue
			}
			r.activity = "review"
		case DutyOtherLecture, DutyOtherLab:
			if !ac.AllowOther {
				continue
			}
			r.activity = "other"
			parent := "lecture"
			if r.kind == DutyOtherLab {
				parent = "lab"
			}
			// A request approved for one side only must not grow rows on the
			// other — same rule the manual-entry path applies.
			if (ac.ReimburseScope == "lecture" && parent == "lab") ||
				(ac.ReimburseScope == "lab" && parent == "lecture") {
				continue
			}
			r.parentKind = &parent
		default:
			continue
		}
		trss = append(trss, r)
	}
	trRows.Close()
	if err := trRows.Err(); err != nil {
		return nil, err
	}

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
				// Add/Update already refuse a clashing slot, but a timetable
				// edited AFTER the slot was created can make an existing one
				// clash.
				if rs, okS := parseHM(r.start); okS {
					if re, okE := parseHM(r.end); okE {
						if clash := findOwnClassClash(ownClasses, weekday, rs, re); clash != nil {
							skippedByClass[clash.describe()]++
							continue
						}
					}
				}
				if dailyHrs[dkey]+r.hours > dailyHourCap {
					continue
				}
				if bahtBlocks(dkey, r.hours) {
					continue
				}
				if termBlocks(r.hours) {
					continue
				}
				id := uuid.New()
				note := autoNoteFor(r.activity, false)
				if _, err := tx.Exec(ctx,
					`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, parent_kind, note, status, source)
					 VALUES ($1,$2,$3::date,$4::time,$5::time,$6,$7,$8,$9,'draft','auto')`,
					id, assignmentID, dkey, r.start, r.end, r.hours, r.activity, r.parentKind, note); err != nil {
					return nil, err
				}
				out = append(out, WorkLog{ID: id, AssignmentID: assignmentID,
					WorkDate: dkey, StartTime: r.start, EndTime: r.end,
					Hours: r.hours, Activity: r.activity, ParentKind: r.parentKind,
					Note: &note, Status: "draft"})
				dailyHrs[dkey] += r.hours
				dailyBaht[dkey] += r.hours * genRate
				termHours += r.hours
			}
		}
	}

	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "worklog.generate", Entity: "assignment", EntityID: assignmentID.String(), After: map[string]int{"count": len(out)}}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	skips := make([]SkipGroup, 0, len(skippedByClass))
	for reason, n := range skippedByClass {
		skips = append(skips, SkipGroup{Reason: reason, Count: n})
	}
	// Deterministic order so the same generation reads the same way twice.
	sort.Slice(skips, func(i, j int) bool {
		if skips[i].Count != skips[j].Count {
			return skips[i].Count > skips[j].Count
		}
		return skips[i].Reason < skips[j].Reason
	})
	return &GenerateResult{
		Entries:              out,
		SkippedOwnClass:      skips,
		StoppedAtTermCeiling: stoppedAtCeiling,
		TermHourCeiling:      termCeiling,
	}, nil
}

// -----------------------------------------------------------------------------
// TA-owned weekly review pattern (วันตรวจการบ้าน)
// -----------------------------------------------------------------------------

// TAReviewSchedule is one weekly slot the TA sets aside for grading homework.
// Auto-generate expands each slot into individual review entries across the
// term. Persistent across regenerations — TAs configure once per term.
// dutyKind is which declared duty a weekly slot belongs to. The three share one
// table because they are the same shape — a recurring slot the TA nominates —
// and because the conflict check has to see all of them at once.
//
//	review        → activity='review'                    (ช่วยตรวจงาน)
//	other_lecture → activity='other', parent_kind=lecture (อื่น ๆ ของบรรยาย)
//	other_lab     → activity='other', parent_kind=lab     (อื่น ๆ ของปฏิบัติการ)
const (
	DutyReview       = "review"
	DutyOtherLecture = "other_lecture"
	DutyOtherLab     = "other_lab"
)

func validDutyKind(k string) bool {
	return k == DutyReview || k == DutyOtherLecture || k == DutyOtherLab
}

// dutyKindTH names the duty the way the request form does, for error messages.
func dutyKindTH(k string) string {
	switch k {
	case DutyReview:
		return "ช่วยตรวจงาน"
	case DutyOtherLecture:
		return "อื่น ๆ (บรรยาย)"
	case DutyOtherLab:
		return "อื่น ๆ (ปฏิบัติการ)"
	}
	return k
}

type TAReviewSchedule struct {
	ID           uuid.UUID `json:"id"`
	AssignmentID uuid.UUID `json:"assignment_id"`
	Kind         string    `json:"kind"`
	DayOfWeek    int       `json:"day_of_week"` // 0=Sunday..6=Saturday
	StartTime    string    `json:"start_time"`  // "HH:MM"
	EndTime      string    `json:"end_time"`
	Room         string    `json:"room,omitempty"`
	Note         string    `json:"note,omitempty"`
}

type TAReviewScheduleInput struct {
	Kind      string `json:"kind"`
	DayOfWeek int    `json:"day_of_week"`
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
	Room      string `json:"room"`
	Note      string `json:"note"`
}

func validateReviewInput(in TAReviewScheduleInput) error {
	if !validDutyKind(in.Kind) {
		return Invalid("ประเภทงานไม่ถูกต้อง")
	}
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

// TAReviewScheduleList wraps the recurring slots along with the weekly budget
// per duty, so the UI can show "2/3 ชม./สัปดาห์", disable "add" when full, and
// — the part that was missing — render a card ONLY for a duty the lecturer
// actually asked this TA to do.
//
// The budget is the DECLARED hours from ta_workload_forms, not the section's
// timetable. It used to be the timetable's lecture hours, which let a TA
// nominate far more grading time than the lecturer requested and had nothing
// to say about "อื่น ๆ" at all.
type TAReviewScheduleList struct {
	Items []TAReviewSchedule `json:"items"`
	// Declared[kind] is the lecturer's weekly ceiling; a kind absent from the
	// map (or zero) was not requested and must not get a card.
	Declared map[string]float64 `json:"declared_hours_per_week"`
	// Used[kind] is what the TA has already nominated.
	Used map[string]float64 `json:"used_hours_per_week"`
}

// declaredDutyHours maps each duty kind to the weekly hours the lecturer
// declared for this assignment. Undergrad only — grad TAs declare a single
// grade_hrs / other_hrs pair, mapped onto the same duties.
func (s *WorkLogService) declaredDutyHours(ctx context.Context, assignmentID uuid.UUID) (map[string]float64, error) {
	var level string
	var grade, other, prep, check, ugOther, labOther float64
	err := s.pool.QueryRow(ctx, `
		SELECT a.level::text,
		       COALESCE(wf.grade_hrs,0), COALESCE(wf.other_hrs,0), COALESCE(wf.prep_hrs,0),
		       COALESCE(wf.check_work_hrs,0), COALESCE(wf.ug_other_hrs,0),
		       COALESCE(wf.lab_other_hrs,0)
		FROM ta_request_assignments a
		LEFT JOIN ta_workload_forms wf ON wf.assignment_id = a.id
		WHERE a.id = $1`, assignmentID).Scan(
		&level, &grade, &other, &prep, &check, &ugOther, &labOther)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if level == "master" || level == "phd" {
		// Grad has no lecture/lab split for other-work; both flavours draw on
		// the one pool so the TA can place it wherever it belongs.
		return map[string]float64{
			DutyReview:       grade,
			DutyOtherLecture: other + prep,
			DutyOtherLab:     other + prep,
		}, nil
	}
	return map[string]float64{
		DutyReview:       check,
		DutyOtherLecture: ugOther,
		DutyOtherLab:     labOther,
	}, nil
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
// The sum is scoped to ONE kind: the three duties have separate ceilings, so
// grading time must not eat into the other-work allowance or vice versa.
func (s *WorkLogService) weeklyReviewHoursForAssignment(ctx context.Context, assignmentID uuid.UUID, kind string, excludeID *uuid.UUID) (float64, error) {
	var h float64
	q := `SELECT COALESCE(SUM(EXTRACT(EPOCH FROM (end_time - start_time))/3600), 0)
	      FROM ta_review_schedules WHERE assignment_id=$1 AND kind=$2`
	args := []any{assignmentID, kind}
	if excludeID != nil {
		q += " AND id <> $3"
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
		`SELECT id, assignment_id, kind, day_of_week, start_time::text, end_time::text,
		        COALESCE(room,''), COALESCE(note,'')
		 FROM ta_review_schedules WHERE assignment_id=$1
		 ORDER BY kind, day_of_week, start_time`, assignmentID)
	if err != nil {
		return TAReviewScheduleList{}, err
	}
	defer rows.Close()
	items := []TAReviewSchedule{}
	used := map[string]float64{}
	for rows.Next() {
		var r TAReviewSchedule
		if err := rows.Scan(&r.ID, &r.AssignmentID, &r.Kind, &r.DayOfWeek,
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
		used[r.Kind] += slotHours(r.StartTime, r.EndTime)
		items = append(items, r)
	}
	if err := rows.Err(); err != nil {
		return TAReviewScheduleList{}, err
	}
	declared, err := s.declaredDutyHours(ctx, assignmentID)
	if err != nil {
		return TAReviewScheduleList{}, err
	}
	return TAReviewScheduleList{Items: items, Declared: declared, Used: used}, nil
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
		       tc2.code || ' sec ' || sec2.sec_no
		FROM ta_request_assignments a2
		JOIN ta_requests r2 ON r2.id = a2.request_id AND r2.status = 'approved'
		JOIN sections sec2 ON sec2.id = a2.section_id
		JOIN teaching_courses tc2 ON tc2.id = sec2.teaching_course_id AND tc2.term_id = $2
		JOIN section_schedules ss ON ss.section_id = sec2.id
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
		       tc2.code || ' sec ' || sec2.sec_no
		FROM ta_review_schedules rs
		JOIN ta_request_assignments a2 ON a2.id = rs.assignment_id
		JOIN sections sec2 ON sec2.id = a2.section_id
		JOIN teaching_courses tc2 ON tc2.id = sec2.teaching_course_id AND tc2.term_id = $2
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
	if err := s.enforceReviewCap(ctx, assignmentID, in.Kind, nil, slotHours(in.StartTime, in.EndTime)); err != nil {
		return uuid.Nil, err
	}
	id := uuid.New()
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "ta_review_schedule.add", Entity: "assignment", EntityID: assignmentID.String(), After: in},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO ta_review_schedules (id, assignment_id, kind, day_of_week, start_time, end_time, room, note)
				 VALUES ($1,$2,$3,$4,$5::time,$6::time,NULLIF($7,''),NULLIF($8,''))`,
				id, assignmentID, in.Kind, in.DayOfWeek, in.StartTime, in.EndTime, in.Room, in.Note)
			if err != nil {
				// Postgres 23505 = unique_violation — friendly message for the dup case.
				if isUniqueViolation(err) {
					return Invalid(fmt.Sprintf("มีตารางงาน '%s' ช่วงเวลานี้อยู่แล้ว", dutyKindTH(in.Kind)))
				}
				return err
			}
			return nil
		}); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// reviewDayNames maps day_of_week (0=Sunday..6=Saturday, the convention shared
// by ta_review_schedules / ta_class_schedules / section_schedules) to Thai.
var reviewDayNames = []string{"อาทิตย์", "จันทร์", "อังคาร", "พุธ", "พฤหัสบดี", "ศุกร์", "เสาร์"}

// enforceReviewNoConflict rejects a review slot that overlaps anything already
// on the TA's weekly schedule for the term:
//
//	(a) their own study timetable  (ta_class_schedules, WBA rows excluded),
//	(b) the classes they help-teach across ALL their approved assignments
//	    (section_schedules of every section the TA holds this term), and
//	(c) their other review slots  (ta_review_schedules across assignments).
//
// Overlap uses the half-open predicate start1 < end2 AND end1 > start2, gated
// on equal day_of_week — the same pattern ta_request.go uses for section
// conflicts. excludeID skips the row being edited so it doesn't clash with
// itself. A conflict returns a Thai message naming the day + clashing slot.
func (s *WorkLogService) enforceReviewNoConflict(ctx context.Context, assignmentID uuid.UUID, in TAReviewScheduleInput, excludeID *uuid.UUID) error {
	// Resolve the owning TA + term for this assignment, plus its co-taught
	// group: two sections graded in the same sitting (sec 2 ภาคปกติ + sec 3
	// โครงการพิเศษ) legitimately share one weekly grading hour, and check (c)
	// below must not treat that as a double booking. Same exemption, same
	// marker, as enforceNoOverlap.
	var taID, termID, reqID uuid.UUID
	var coGroup *int
	if err := s.pool.QueryRow(ctx, `
		SELECT a.ta_id, tc.term_id, a.request_id, a.cotaught_group
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		WHERE a.id = $1`, assignmentID).Scan(&taID, &termID, &reqID, &coGroup); err != nil {
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
			"เวลาตรวจการบ้านชนกับ%s (%s %s %s–%s) เลือกช่วงเวลาที่ไม่ทับกัน",
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
			SELECT tc2.code || ' sec ' || sec2.sec_no,
			       to_char(ss.start_time,'HH24:MI'), to_char(ss.end_time,'HH24:MI')
			FROM ta_request_assignments a2
			JOIN ta_requests r2 ON r2.id = a2.request_id AND r2.status = 'approved'
			JOIN sections sec2 ON sec2.id = a2.section_id
			JOIN teaching_courses tc2 ON tc2.id = sec2.teaching_course_id AND tc2.term_id = $2
			JOIN section_schedules ss ON ss.section_id = sec2.id
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
			SELECT tc2.code || ' sec ' || sec2.sec_no,
			       to_char(rs.start_time,'HH24:MI'), to_char(rs.end_time,'HH24:MI')
			FROM ta_review_schedules rs
			JOIN ta_request_assignments a2 ON a2.id = rs.assignment_id
			JOIN sections sec2 ON sec2.id = a2.section_id
			JOIN teaching_courses tc2 ON tc2.id = sec2.teaching_course_id AND tc2.term_id = $2
			WHERE a2.ta_id = $1 AND rs.id <> $6
			  AND rs.day_of_week = $3
			  AND rs.start_time < $5::time AND rs.end_time > $4::time
			  -- Co-taught sections of the same request share the grading
			  -- sitting; their slots may (and on the faculty's forms, do)
			  -- coincide. NULL group = not co-taught, keeps the old rule.
			  AND NOT (a2.id <> $7
			           AND a2.request_id = $8
			           AND a2.cotaught_group IS NOT NULL
			           AND a2.cotaught_group = $9::int)
			LIMIT 1`, taID, termID, in.DayOfWeek, in.StartTime, in.EndTime, exclude,
			assignmentID, reqID, coGroup).Scan(&label, &st, &en)
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
// enforceReviewCap bounds a duty's weekly slots by what the LECTURER declared
// for it, and refuses the kind entirely when nothing was declared.
//
// That second half is the gate that was missing: a TA could nominate grading
// days for an assignment whose lecturer never ticked ช่วยตรวจงาน, then press
// "สร้างอัตโนมัติ" and receive nothing, with no explanation anywhere.
func (s *WorkLogService) enforceReviewCap(ctx context.Context, assignmentID uuid.UUID, kind string, excludeID *uuid.UUID, addedHours float64) error {
	if addedHours <= 0 {
		return nil
	}
	declared, err := s.declaredDutyHours(ctx, assignmentID)
	if err != nil {
		return err
	}
	capH := declared[kind]
	if capH <= 0 {
		return Invalid(fmt.Sprintf(
			"อาจารย์ไม่ได้ระบุชั่วโมง '%s' ในคำขอ จึงกำหนดตารางงานนี้ไม่ได้", dutyKindTH(kind)))
	}
	existing, err := s.weeklyReviewHoursForAssignment(ctx, assignmentID, kind, excludeID)
	if err != nil {
		return err
	}
	if existing+addedHours > capH+0.01 {
		return Invalid(fmt.Sprintf(
			"ชั่วโมง '%s' รวมต่อสัปดาห์ (%.1f ชม.) ต้องไม่เกินที่อาจารย์ระบุไว้ (%.1f ชม.) ปัจจุบันใช้ไปแล้ว %.1f ชม.",
			dutyKindTH(kind), existing+addedHours, capH, existing))
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
	if err := s.enforceReviewCap(ctx, assignmentID, in.Kind, &rsID, slotHours(in.StartTime, in.EndTime)); err != nil {
		return err
	}
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "ta_review_schedule.update", Entity: "ta_review_schedule", EntityID: rsID.String(), After: in},
		func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx,
				`UPDATE ta_review_schedules
				 SET kind=$3, day_of_week=$4, start_time=$5::time, end_time=$6::time,
				     room=NULLIF($7,''), note=NULLIF($8,'')
				 WHERE id=$1 AND assignment_id=$2`,
				rsID, assignmentID, in.Kind, in.DayOfWeek, in.StartTime, in.EndTime, in.Room, in.Note)
			if err != nil {
				if isUniqueViolation(err) {
					return Invalid(fmt.Sprintf("มีตารางงาน '%s' ช่วงเวลานี้อยู่แล้ว", dutyKindTH(in.Kind)))
				}
				return err
			}
			if tag.RowsAffected() == 0 {
				return Invalid("ไม่พบตารางงานที่ต้องการแก้ไข")
			}
			return nil
		})
}

func (s *WorkLogService) DeleteTAReviewSchedule(ctx context.Context, actor, assignmentID, rsID uuid.UUID) error {
	if _, err := s.assertTAOwnsAssignment(ctx, actor, assignmentID); err != nil {
		return err
	}
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "ta_review_schedule.delete", Entity: "ta_review_schedule", EntityID: rsID.String()},
		func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx,
				`DELETE FROM ta_review_schedules WHERE id=$1 AND assignment_id=$2`,
				rsID, assignmentID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return Invalid("ไม่พบตารางตรวจการบ้านที่ต้องการลบ")
			}
			return nil
		})
}

// dailyHourCapFor resolves the daily hour cap that applies to an assignment
// based on (level, track). Falls back to 7 if pay_rates is missing.
//
//	ป.ตรี ปกติ  → ug_regular_daily_hour_cap    (default 7)
//	ป.ตรี พิเศษ → ug_special_daily_hour_cap    (default 6)
//	บัณฑิต ปกติ → grad_regular_daily_hour_cap  (default 6)
//	บัณฑิต พิเศษ → 24 (flat monthly, hours not billed)
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
func (s *WorkLogService) enforceDailyBahtCap(ctx context.Context, taID uuid.UUID, w WorkLog) error {
	var capBaht float64
	if err := s.pool.QueryRow(ctx,
		`SELECT daily_pay_cap_baht FROM pay_rates ORDER BY effective_from DESC LIMIT 1`).Scan(&capBaht); err != nil {
		return nil // no pay_rates → skip cap
	}
	if capBaht <= 0 {
		return nil
	}

	// The day is priced ONCE, with the candidate row folded in — not as
	// "existing + this row".
	//
	// A sitting taught to sec 1 (ภาคปกติ) and sec 2 (โครงการพิเศษ) at the same
	// hour is written against both, because the tracks draw on separate budgets,
	// and export.go rule B2 then pays those shared hours once at the REGULAR
	// rate ("เบิกภาคปกติก่อน"). Adding the two rows up charged this cap for money
	// nobody is paid: one real Tuesday came to 360฿ against a 300฿ cap where the
	// payout engine settles at 160฿, and every row of that day became uneditable.
	//
	// Folding the candidate in is what makes the second half work. Priced
	// separately, the sec-2 row of a brand-new sitting still arrives as its own
	// 200฿ on top of the sec-1 row's 160฿ — the same double count, one step
	// later.
	var total float64
	if err := s.pool.QueryRow(ctx, `
		WITH latest AS (SELECT * FROM pay_rates ORDER BY effective_from DESC LIMIT 1),
		cand AS (
		    SELECT $3::uuid AS id, $4::time AS start_time, $5::time AS end_time,
		           $6::numeric AS hours, a.request_id, a.cotaught_group,
		           a.level::text AS level, sec.track::text AS track
		    FROM ta_request_assignments a
		    JOIN sections sec ON sec.id = a.section_id
		    WHERE a.id = $7
		),
		day_rows AS (
		    SELECT wl.id, wl.start_time, wl.end_time, wl.hours, a.request_id,
		           a.cotaught_group, a.level::text, sec.track::text
		    FROM work_logs wl
		    JOIN ta_request_assignments a ON a.id = wl.assignment_id
		    JOIN sections sec ON sec.id = a.section_id
		    WHERE a.ta_id = $1 AND wl.work_date = $2::date
		      AND wl.status <> 'rejected' AND wl.id <> $3
		    UNION ALL
		    SELECT * FROM cand
		),
		-- One row per sitting: same co-taught group, same request, same clock
		-- interval. The regular-track row wins so the price matches what B2 pays.
		-- COALESCE on the row id keeps ungrouped rows from merging with anything.
		sitting AS (
		    SELECT DISTINCT ON (
		               COALESCE(cotaught_group::text, id::text),
		               request_id, start_time, end_time)
		           hours, level, track
		    FROM day_rows
		    ORDER BY COALESCE(cotaught_group::text, id::text),
		             request_id, start_time, end_time, (track = 'regular') DESC
		)
		SELECT COALESCE(SUM(sitting.hours *
		    CASE
		        WHEN sitting.level='undergrad' AND sitting.track='regular' THEN pr.undergrad_regular
		        WHEN sitting.level='undergrad' AND sitting.track='special' THEN pr.undergrad_special
		        WHEN sitting.level IN ('master','phd') AND sitting.track='regular' THEN pr.graduate_regular_hourly
		        ELSE 0  -- grad special = flat monthly, does not count toward daily cap
		    END), 0)
		FROM sitting CROSS JOIN latest pr`,
		taID, w.WorkDate, w.ID, w.StartTime, w.EndTime, w.Hours, w.AssignmentID).Scan(&total); err != nil {
		return err
	}
	if total > capBaht+0.01 {
		return Invalid(fmt.Sprintf(
			"รวมค่าตอบแทนของวันนี้เกิน %.0f บาท (รวมรายการนี้แล้วเป็น %.2f บาท)",
			capBaht, total))
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
	// System-generated makeup rows are exempt from the DESTINATION week's cap:
	// the hour belongs to the origin week whose class was cancelled, and the
	// college's own claim forms show both the regular and the relocated hour in
	// one week. Scoped to source='auto' so a TA typing "ชดเชย" into a manual
	// note cannot dodge the cap.
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(hours), 0) FROM work_logs
		 WHERE assignment_id = $1
		   AND date_trunc('week', work_date) = date_trunc('week', $2::date)
		   AND activity = ANY($3)
		   AND id <> $4
		   AND NOT (source = 'auto' AND COALESCE(note,'') LIKE '%ชดเชย%')`,
		w.AssignmentID, w.WorkDate, activities, w.ID).Scan(&weekTotal); err != nil {
		return err
	}
	if weekTotal+w.Hours > cap+0.01 {
		return Invalid(fmt.Sprintf(
			"เกินโควตา %s ประจำสัปดาห์ (%.1f ชม./สัปดาห์) สัปดาห์นี้มี %.2f ชม. อยู่แล้ว",
			labelTH, cap, weekTotal))
	}
	return nil
}

// enforceNoOverlap rejects a row whose time range overlaps another of the same
// TA's rows on the same date — across every assignment the TA holds, so two
// courses can't both bill the same clock hours. Back-to-back rows
// (end == start) are allowed; rejected rows don't count.
//
// CO-TAUGHT SECTIONS ARE EXEMPT (31/07/2026). One lab serving sec 1 (ภาคปกติ)
// and sec 2 (โครงการพิเศษ) in the same room at the same hour is one sitting that
// must be recorded against both sections, because the two tracks are paid from
// separate budgets. The faculty's own signed form does exactly this, and so does
// the rest of this system:
//
//   - Generate never called this function, so it has been writing those
//     overlapping rows all along — 12 of them sit approved in the live database;
//   - export.go rule B2 then counts the shared hours ONCE at the regular rate
//     ("เบิกภาคปกติก่อน") and removes the duplicate from the special side.
//
// So the hours were never double-paid, and this gate was the only thing that
// disagreed: a TA could not hand-correct a row the generator had written for
// them. It blocked exactly the case the design depends on.
//
// The exemption keys on `cotaught_group`, the marker Create already computes and
// stores for this purpose (detectCotaughtGroups). Deliberately NOT re-derived
// from matching schedules here: a second definition of "same sitting" is how the
// clash rule ended up with three spellings that disagreed.
func (s *WorkLogService) enforceNoOverlap(ctx context.Context, taID uuid.UUID, w WorkLog) error {
	var code, st, en string
	err := s.pool.QueryRow(ctx, `
		SELECT tc.code, wl.start_time::text, wl.end_time::text
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		JOIN sections sec ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		-- The assignment the row being written belongs to.
		JOIN ta_request_assignments self ON self.id = $6
		WHERE a.ta_id = $1 AND wl.work_date = $2::date AND wl.id <> $3
		  AND wl.status <> 'rejected'
		  AND wl.start_time < $5::time AND wl.end_time > $4::time
		  -- ...unless both rows are the same sitting taught to co-scheduled
		  -- sections of the same request. NULL group = not co-taught, and NULL
		  -- never equals NULL here, so singletons keep the old behaviour.
		  --
		  -- a.id <> self.id is load-bearing: without it an assignment matches
		  -- ITSELF (same request, same group) and the exemption swallows the
		  -- plain same-assignment overlap rule entirely. The exemption is about
		  -- a second SECTION, never a second row on the same one.
		  AND NOT (a.id <> self.id
		           AND a.request_id = self.request_id
		           AND a.cotaught_group IS NOT NULL
		           AND a.cotaught_group = self.cotaught_group)
		LIMIT 1`, taID, w.WorkDate, w.ID, w.StartTime, w.EndTime, w.AssignmentID).Scan(&code, &st, &en)
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
	return Invalid(fmt.Sprintf("ช่วงเวลาซ้อนกับรายการเดิม (%s %s–%s) แก้ไขเวลาให้ไม่ทับกัน", code, st, en))
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
			"อนุมัติไม่ได้ ชั่วโมงรวมต่อวันเกิน %.1f ชม. ในวันที่: %s", dailyCap, strings.Join(badDays, ", ")))
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
		// Same makeup exemption as enforceWeeklyActivityCap: a relocated class
		// hour counts against its origin week, not the week it landed in.
		wrows, err := tx.Query(ctx, `
			SELECT TO_CHAR(date_trunc('week', work_date),'YYYY-MM-DD')
			FROM work_logs
			WHERE assignment_id=$1 AND status IN ('submitted','approved') AND activity = ANY($2)
			  AND NOT (source = 'auto' AND COALESCE(note,'') LIKE '%ชดเชย%')
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
				"อนุมัติไม่ได้ เกินโควตา %s ประจำสัปดาห์ (%.1f ชม.) ในสัปดาห์ที่เริ่ม: %s",
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
		SELECT CASE WHEN $2='lecture' THEN tc.lecture_hrs ELSE tc.lab_hrs END
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		WHERE a.id = $1`, assignmentID, parentKind).Scan(&capHrs)
	return capHrs, err
}

func (s *WorkLogService) List(ctx context.Context, actor, assignmentID uuid.UUID, privileged bool) ([]WorkLog, error) {
	if err := s.assertCanView(ctx, actor, assignmentID, privileged); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, assignment_id, TO_CHAR(work_date,'YYYY-MM-DD'), start_time::text, end_time::text, hours, activity, parent_kind, room, note, status::text, reject_reason, source
		 FROM work_logs WHERE assignment_id=$1 ORDER BY work_date, start_time`, assignmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkLog{}
	for rows.Next() {
		var w WorkLog
		if err := rows.Scan(&w.ID, &w.AssignmentID, &w.WorkDate, &w.StartTime, &w.EndTime, &w.Hours, &w.Activity, &w.ParentKind, &w.Room, &w.Note, &w.Status, &w.RejectReason, &w.Source); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

// assertOwnClassScheduleFilled blocks worklog writes until the TA has entered
// their OWN class timetable for the term.
//
// The faculty's signed form ("ตารางเรียนและตารางปฏิบัติงาน TA") puts the TA's
// classes and their TA duties in one weekly grid, and the whole point of that
// layout is that a duty scheduled on top of a lecture the TA has to attend is
// visible at a glance. With the class half empty the form is not just less
// useful — it is not the document the faculty signs, and the clash it exists to
// catch cannot be seen by anyone.
//
// Gating the write rather than warning afterwards is deliberate: hours logged
// against an unknown timetable would have to be re-checked later by hand, which
// is the work this is meant to remove.
func (s *WorkLogService) assertOwnClassScheduleFilled(ctx context.Context, taID, termID uuid.UUID) error {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ta_class_schedules WHERE user_id = $1 AND term_id = $2`,
		taID, termID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return Invalid("กรุณากรอกตารางเรียนของคุณในเทอมนี้ก่อน จึงจะลงเวลาปฏิบัติงานได้ " +
			"ระบบใช้ตารางเรียนเพื่อออกแบบฟอร์มตารางปฏิบัติงาน และตรวจว่างาน TA ไม่ชนกับคาบเรียนของคุณ (เมนู “ตารางเรียนของฉัน”)")
	}
	return nil
}

// assertOwnScheduleForCourse resolves the course's term and applies the
// own-class-schedule gate. Wrapped so the three write paths take the same one
// line and cannot drift on how the term is derived.
func (s *WorkLogService) assertOwnScheduleForCourse(ctx context.Context, ac *assignmentContext) error {
	termID, err := courseTermID(ctx, s.pool, ac.TeachingCourseID)
	if err != nil {
		return err
	}
	return s.assertOwnClassScheduleFilled(ctx, ac.TAID, termID)
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
	return WeeksInTerm(ctx, s.pool, teachingCourseID)
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
			"เกินเพดานชั่วโมงรวมทั้งเทอมของภาระงานนี้ (%.1f ชม.) ลงได้อีก %.2f ชม.", ceiling, remaining))
	}
	return nil
}

// validateGradRegularClassWindow requires a grad-regular (level master/phd,
// track regular) TA's เช็คชื่อ/สอนปฏิบัติการ entry to fall within that
// section's real scheduled class period for that weekday — per the 2026
// staff meeting: attendance hours reference the actual lecture period, and
// lab-teaching hours reference the actual class schedule, rather than being
// freely typed the way manual entry allows today. Undergrad and grad-special
// are untouched; ตรวจงาน (review/other) stays free-form, matching the
// meeting's own scope ("ตรวจงานวันเวลาไหน" — no schedule tie called for there).
// A makeup session logs as activity='makeup', not 'lecture'/'lab', so it
// never reaches this check — its date legitimately differs from the weekly
// pattern.
func (s *WorkLogService) validateGradRegularClassWindow(ctx context.Context, ac *assignmentContext, w WorkLog) error {
	if ac.Level == "undergrad" || ac.Track != "regular" {
		return nil
	}
	if w.Activity != "lecture" && w.Activity != "lab" {
		return nil
	}
	d, err := time.Parse("2006-01-02", w.WorkDate)
	if err != nil {
		return nil // date format already validated upstream
	}
	sm, sok := parseHM(w.StartTime)
	em, eok := parseHM(w.EndTime)
	if !sok || !eok {
		return nil // time format already validated upstream
	}
	rows, err := s.pool.Query(ctx, `
		SELECT start_time::text, end_time::text FROM section_schedules
		WHERE section_id = $1 AND kind = $2 AND day_of_week = $3`,
		ac.SectionID, w.Activity, int(d.Weekday()))
	if err != nil {
		return err
	}
	defer rows.Close()
	label := "เช็คชื่อ (คาบบรรยาย)"
	if w.Activity == "lab" {
		label = "สอนปฏิบัติการ"
	}
	var windows []string
	for rows.Next() {
		var st, et string
		if err := rows.Scan(&st, &et); err != nil {
			return err
		}
		stm, stok := parseHM(st)
		etm, etok := parseHM(et)
		if !stok || !etok {
			continue
		}
		windows = append(windows, fmt.Sprintf("%02d:%02d-%02d:%02d", stm/60, stm%60, etm/60, etm%60))
		if sm >= stm && em <= etm {
			return nil // fits within this scheduled period
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(windows) == 0 {
		return Invalid(fmt.Sprintf("วันนี้ไม่มีคาบ%sตามตารางสอนจริงของกลุ่มนี้", label))
	}
	return Invalid(fmt.Sprintf("เวลาที่กรอกต้องอยู่ในคาบ%sจริง (%s)", label, strings.Join(windows, ", ")))
}

func (s *WorkLogService) Upsert(ctx context.Context, actor uuid.UUID, w WorkLog) (uuid.UUID, error) {
	ac, err := s.assertTAOwnsAssignment(ctx, actor, w.AssignmentID)
	if err != nil {
		return uuid.Nil, err
	}
	if err := s.assertOwnScheduleForCourse(ctx, ac); err != nil {
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
		// "today" must be Thailand's calendar day, not the host's. On a UTC
		// server time.Now() still reports yesterday until 07:00 local, which
		// would reopen a month the back-date rule should already have closed.
	}, termStart, termEnd, midterm, final, holidays, mk, timeutil.Now()); err != nil {
		return uuid.Nil, err
	}
	if err := s.validateGradRegularClassWindow(ctx, ac, w); err != nil {
		return uuid.Nil, err
	}
	// Month lock: a finance_sent month is frozen for everyone; a closed period
	// additionally blocks TA writes. Check the incoming date, and for edits also
	// the row's current date so an entry can't be moved into or out of a locked
	// month from either side.
	if err := assertWorklogWritable(ctx, s.pool, ac.TeachingCourseID, ac.TAID, w.WorkDate); err != nil {
		return uuid.Nil, err
	}
	if w.ID != uuid.Nil {
		var oldDate string
		if err := s.pool.QueryRow(ctx,
			`SELECT TO_CHAR(work_date,'YYYY-MM-DD') FROM work_logs WHERE id=$1 AND assignment_id=$2`,
			w.ID, w.AssignmentID).Scan(&oldDate); err == nil && oldDate[:7] != w.WorkDate[:7] {
			if err := assertWorklogWritable(ctx, s.pool, ac.TeachingCourseID, ac.TAID, oldDate); err != nil {
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
	if err := s.enforceDailyBahtCap(ctx, ac.TAID, w); err != nil {
		return uuid.Nil, err
	}

	// The same clock hours must not be billed twice across the TA's courses.
	if err := s.enforceNoOverlap(ctx, ac.TAID, w); err != nil {
		return uuid.Nil, err
	}

	// The TA's own class wins over any teaching duty (24/07/2026 meeting).
	// Checked here rather than only at request time because a makeup class
	// lands on a different date and time than the slot the request approved.
	if err := s.enforceNoOwnClassConflict(ctx, ac, w); err != nil {
		return uuid.Nil, err
	}

	if w.ID == uuid.Nil {
		// Block adding new rows once THAT MONTH has entered review, so hours
		// cannot be inflated after the lecturer and staff have signed it off.
		//
		// Scoped to the month (03/08/2026). It used to look at the whole
		// assignment, which meant submitting June closed October too: a TA who
		// picked up an extra session mid-term could never log it, all term. The
		// month is the unit everything downstream uses — approval, staff review
		// and export are all per (assignment, month) — so it is the unit that
		// should close.
		var reviewed int
		if err := s.pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM work_logs
			 WHERE assignment_id=$1 AND status IN ('submitted','approved')
			   AND to_char(work_date, 'YYYY-MM') = $2`,
			w.AssignmentID, w.WorkDate[:7]).Scan(&reviewed); err != nil {
			return uuid.Nil, err
		}
		if reviewed > 0 {
			return uuid.Nil, Invalid("เดือนนี้ส่งอนุมัติหรืออนุมัติไปแล้ว เพิ่มรายการใหม่ในเดือนนี้ไม่ได้")
		}
		w.ID = uuid.New()
		if err := writeAudited(ctx, s.pool, s.aud,
			audit.Entry{ActorID: &actor, Action: "worklog.create", Entity: "work_log", EntityID: w.ID.String(), After: w},
			func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx,
					`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, parent_kind, room, note, status)
					 VALUES ($1,$2,$3::date,$4::time,$5::time,$6,$7,$8,$9,$10,'draft')`,
					w.ID, w.AssignmentID, w.WorkDate, w.StartTime, w.EndTime, w.Hours, w.Activity, w.ParentKind, w.Room, w.Note)
				return err
			}); err != nil {
			return uuid.Nil, err
		}
		return w.ID, nil
	}
	// Editable states: draft (in progress) and rejected (TA fixing after a
	// bounce — the update resets it to draft so it can be resubmitted).
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "worklog.update", Entity: "work_log", EntityID: w.ID.String(), After: w},
		func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx,
				`UPDATE work_logs SET work_date=$1::date, start_time=$2::time, end_time=$3::time, hours=$4, activity=$5, parent_kind=$6, room=$7, note=$8, status='draft'
				 WHERE id=$9 AND assignment_id=$10 AND status IN ('draft','rejected')`,
				w.WorkDate, w.StartTime, w.EndTime, w.Hours, w.Activity, w.ParentKind, w.Room, w.Note, w.ID, w.AssignmentID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return Invalid("ไม่พบรายการที่แก้ไขได้ (อาจถูกส่งอนุมัติหรืออนุมัติแล้ว)")
			}
			return nil
		}); err != nil {
		return uuid.Nil, err
	}
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
		  AND NOT `+unsubmittableMonthSQL("wl"), assignmentID)
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
			// No back-dated submission (staff decision, 03/08/2026): a month
			// whose deadline passed unsent is a month the TA did not claim. The
			// message says so instead of offering an appeal that will be refused.
			return Invalid("รายการทั้งหมดอยู่ในงวดที่ปิดไปแล้ว ถือว่าไม่ประสงค์ลงเวลา ส่งย้อนหลังไม่ได้")
		}
		return Invalid("ไม่มีรายการที่ส่งอนุมัติได้")
	}
	if err := s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "worklog.submit", Entity: "assignment", EntityID: assignmentID.String()}); err != nil {
		return err
	}
	// Tell the course's lecturers there is work waiting — without this the
	// approver only discovers pending batches by polling their reports page.
	if s.notify != nil {
		if lects, err := courseLecturerIDs(ctx, s.pool, ac.TeachingCourseID); err == nil {
			for _, lid := range lects {
				s.notify.Send(ctx, lid,
					"มีบันทึกเวลารอการอนุมัติ",
					"TA ส่งบันทึกเวลาปฏิบัติงานรอการอนุมัติจากคุณ",
					"/lecturer/courses/"+ac.TeachingCourseID.String()+"/reports")
			}
		}
	}
	return nil
}

var yearMonthRe = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])$`)

// validateYearMonth accepts "" (whole batch) or a strict "YYYY-MM". The value
// reaches SQL as a to_char() comparison, so a malformed one would silently
// match nothing and look like "ไม่มีรายการที่รออนุมัติ".
func validateYearMonth(ym string) error {
	if ym == "" || yearMonthRe.MatchString(ym) {
		return nil
	}
	return Invalid("รูปแบบเดือนไม่ถูกต้อง (ต้องเป็น YYYY-MM)")
}

// pendingApprovalBaht is the extra pay the not-yet-approved rows of one
// assignment represent. Read on the CALLER'S transaction so a batch sees the
// rows it has already flipped in the same transaction.
//
// Both undergrad (hourly, per-track) AND graduate-regular (hourly per ประกาศ)
// are billed by hours. Grad-special is flat monthly, so its hours contribute 0.
func pendingApprovalBaht(ctx context.Context, tx pgx.Tx, assignmentID uuid.UUID, yearMonth string) (float64, error) {
	var addBaht float64
	err := tx.QueryRow(ctx, `
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
		WHERE wl.assignment_id = $1 AND wl.status = 'submitted'
		  AND ($2 = '' OR to_char(wl.work_date, 'YYYY-MM') = $2)`,
		assignmentID, yearMonth).Scan(&addBaht)
	return addBaht, err
}

// ApproveMany approves several assignments in ONE transaction: all of them land
// or none does.
//
// The lecturer screen's "อนุมัติทั้งคนนี้" covers a TA who helps with several
// sections, which is several assignments. Looping over the single-assignment
// endpoint left a half-approved TA whenever the second call was refused — the
// caller saw an error for something that had partly happened, and retrying
// silently skipped whatever had already gone through.
//
// It also fixes a check the loop could not do. The per-assignment budget guard
// compares each assignment against the course cap on its own; two sections that
// each fit can still exceed it together. Here the batch's cost is summed per
// course FIRST, then compared once.
func (s *WorkLogService) ApproveMany(ctx context.Context, actor uuid.UUID, assignmentIDs []uuid.UUID, yearMonth string, privileged bool) error {
	if len(assignmentIDs) == 0 {
		return Invalid("ไม่ได้ระบุรายการที่จะอนุมัติ")
	}
	if err := validateYearMonth(yearMonth); err != nil {
		return err
	}

	// Permission and finance-lock checks first, on the pool: they are reads, and
	// a refusal here should not have opened a transaction at all.
	ctxs := make(map[uuid.UUID]*assignmentContext, len(assignmentIDs))
	ids := make([]uuid.UUID, 0, len(assignmentIDs))
	for _, id := range assignmentIDs {
		if _, seen := ctxs[id]; seen {
			continue // the same assignment named twice is one assignment
		}
		ac, err := s.assertCanReview(ctx, actor, id, privileged)
		if err != nil {
			return err
		}
		if err := assertNoFinanceLockedRows(ctx, s.pool, id, []string{"submitted"}, yearMonth); err != nil {
			return err
		}
		ctxs[id] = ac
		ids = append(ids, id)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Serialize on every course involved, so two reviewers cannot both push the
	// same course over budget. Sorted to give every caller the same lock order
	// and rule out a deadlock between two overlapping batches.
	courses := make([]uuid.UUID, 0, len(ids))
	seenCourse := map[uuid.UUID]bool{}
	for _, id := range ids {
		if c := ctxs[id].TeachingCourseID; !seenCourse[c] {
			seenCourse[c] = true
			courses = append(courses, c)
		}
	}
	sort.Slice(courses, func(i, j int) bool { return courses[i].String() < courses[j].String() })
	for _, c := range courses {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 42))`, c); err != nil {
			return err
		}
	}

	// No budget guard here, by design (04/08/2026).
	//
	// It used to price the batch and refuse the whole approval when the course
	// would go over its cap. The work had already happened, so refusing did not
	// un-happen it — it left real hours with nowhere to go and the lecturer with
	// a button that could not be pressed. What the budget decides now is which
	// MONTHS get paid (budget_settlement.go), settled at export; recording what
	// somebody did is not the place to enforce it.
	//
	// The advisory lock above stays: it still serialises concurrent approvals on
	// one course, which the cap recheck below depends on.

	var affected int64
	for _, id := range ids {
		if err := s.recheckCapsForApproval(ctx, tx, ctxs[id], id); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			`UPDATE work_logs SET status='approved', approved_at=NOW(), approved_by=$1
			 WHERE assignment_id=$2 AND status='submitted'
			   AND ($3 = '' OR to_char(work_date, 'YYYY-MM') = $3)`, actor, id, yearMonth)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// One section of a co-taught pair can legitimately be finished
			// already. Only an entirely empty batch is worth refusing.
			continue
		}
		affected += tag.RowsAffected()
		if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "worklog.approve",
			Entity: "assignment", EntityID: id.String(), Note: yearMonth}); err != nil {
			return err
		}
	}
	if affected == 0 {
		return Invalid("ไม่มีรายการที่รออนุมัติ (อาจถูกดำเนินการไปแล้ว)")
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	// The course budget just moved. Warn if the term no longer fits — after the
	// commit, so a warning cannot roll back an approval.
	courseIDs := make([]uuid.UUID, 0, len(courses))
	courseIDs = append(courseIDs, courses...)
	s.warnBudgetAfterApproval(ctx, courseIDs...)

	// After the commit, and once per PERSON — a TA on two sections should not be
	// told twice about one decision.
	if s.notify != nil {
		scope := "บันทึกเวลาปฏิบัติงานของคุณได้รับการอนุมัติแล้ว"
		if yearMonth != "" {
			scope = "บันทึกเวลาเดือน " + yearMonth + " ของคุณได้รับการอนุมัติแล้ว"
		}
		told := map[uuid.UUID]bool{}
		for _, id := range ids {
			ta := ctxs[id].TAID
			if told[ta] {
				continue
			}
			told[ta] = true
			t := s.notifyTarget(ctx, ctxs[id].TeachingCourseID)
			s.notify.Send(ctx, ta, "อนุมัติบันทึกเวลา "+t.Code, scope, t.Link)
		}
	}
	return nil
}

// yearMonth ("YYYY-MM") approves only that month's submitted rows — lecturers
// asked for per-month decisions because a batch is usually wrong in one month
// only. "" keeps the legacy whole-batch behaviour.
func (s *WorkLogService) Approve(ctx context.Context, actor, assignmentID uuid.UUID, yearMonth string, privileged bool) error {
	if err := validateYearMonth(yearMonth); err != nil {
		return err
	}
	ac, err := s.assertCanReview(ctx, actor, assignmentID, privileged)
	if err != nil {
		return err
	}
	// Refuse if any row being approved falls in a month already handed to
	// finance. Scoped to yearMonth so one settled month can't freeze the rest.
	if err := assertNoFinanceLockedRows(ctx, s.pool, assignmentID, []string{"submitted"}, yearMonth); err != nil {
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

	// No budget guard — see ApproveMany for why. The month cutoff at export is
	// what the budget controls, not whether the hours may be recorded.

	// Last-gate cap recheck: staff edits after submit can push a day/week past
	// its cap; refuse rather than approve hours that violate the pay rules.
	if err := s.recheckCapsForApproval(ctx, tx, ac, assignmentID); err != nil {
		return err
	}

	tag, err := tx.Exec(ctx,
		`UPDATE work_logs SET status='approved', approved_at=NOW(), approved_by=$1
		 WHERE assignment_id=$2 AND status='submitted'
		   AND ($3 = '' OR to_char(work_date, 'YYYY-MM') = $3)`, actor, assignmentID, yearMonth)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return Invalid("ไม่มีรายการที่รออนุมัติ (อาจถูกดำเนินการไปแล้ว)")
	}
	// Audit INSIDE the transaction, before the commit. Written afterwards on a
	// pool connection it can fail on its own, and then there is no honest answer
	// left: the hours are approved and the record says they never were, or we
	// report a failure for something that did happen and the lecturer retries
	// into "ไม่มีรายการที่รออนุมัติ". Here the two land together or neither does.
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "worklog.approve", Entity: "assignment", EntityID: assignmentID.String(), Note: yearMonth}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if s.notify != nil {
		t := s.notifyTarget(ctx, ac.TeachingCourseID)
		scope := "บันทึกเวลาปฏิบัติงานวิชา " + t.Code + " ของคุณได้รับการอนุมัติแล้ว"
		if when := thaiYearMonth(yearMonth); when != "" {
			scope = "บันทึกเวลาวิชา " + t.Code + " เดือน" + when + " ของคุณได้รับการอนุมัติแล้ว"
		}
		s.notify.Send(ctx, ac.TAID, "อนุมัติบันทึกเวลา "+t.Code, scope, t.Link)
	}
	s.warnBudgetAfterApproval(ctx, ac.TeachingCourseID)
	return nil
}

// PendingReport is one row on the lecturer's "อนุมัติรายงาน TA" list — an
// assignment that currently has at least one submitted work-log row awaiting
// review.
type PendingReport struct {
	ID     uuid.UUID `json:"id"`
	TAID   uuid.UUID `json:"ta_id"`
	TAName string    `json:"ta_name"`
	// StudyLevel ("undergrad" | "master" | "phd") lets the reviewer tell a ป.ตรี
	// claimant from a บัณฑิตศึกษา one at a glance — the two are reviewed under
	// different rules (per-hour caps vs. the 10–12 ชม./สัปดาห์ regulation).
	StudyLevel       string    `json:"study_level"`
	CourseCode       string    `json:"course_code"`
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	// Which section this row is for. The list is one row PER ASSIGNMENT, and a
	// TA who helps with two sections of one course produces two rows carrying
	// the same name, course and (when the sections are co-taught) the same hour
	// count. Without the section they are indistinguishable on screen, and the
	// reviewer's only reading is that the same request arrived twice.
	SecNo string `json:"sec_no"`
	Track string `json:"track"`
	// CoTaughtGroup marks sections taught in ONE sitting. Rows sharing a group
	// are the same hours written against each section, and export rule B2 pays
	// them once at the regular rate — so their hours must never be added up.
	CoTaughtGroup *int `json:"cotaught_group,omitempty"`
	// TotalHours is this ASSIGNMENT's submitted hours. GroupHours is what the
	// whole co-taught group is worth once each shared sitting is counted a
	// single time — identical on every row of the group, and equal to
	// TotalHours when there is no group.
	//
	// Both are needed and neither can be derived from the other: co-taught
	// sections are not copies of each other. On CP321002 the pair holds 98 rows
	// but only 82 distinct sittings, so the honest total is 164h — not 196
	// (adding them) and not 98 (assuming one is a duplicate of the other).
	TotalHours  float64 `json:"total_hours"`
	GroupHours  float64 `json:"group_hours"`
	PeriodLabel string  `json:"period_label,omitempty"`
	// FirstDate / LastDate are the same span as PeriodLabel, unformatted, so the
	// client can render it in Thai like every other date in the app.
	FirstDate string `json:"first_date,omitempty"`
	LastDate  string `json:"last_date,omitempty"`
}

// ListPending returns assignments with submitted (awaiting-review) work-logs.
// Lecturers see only assignments in courses they teach; privileged users
// (admin/staff) see all.
func (s *WorkLogService) ListPending(ctx context.Context, actor uuid.UUID, privileged bool) ([]PendingReport, error) {
	rows, err := s.pool.Query(ctx, `
		WITH pend AS (
		    SELECT a.id AS assignment_id, a.ta_id, a.cotaught_group,
		           sec.teaching_course_id,
		           wl.work_date, wl.start_time, wl.end_time, wl.hours
		    FROM work_logs wl
		    JOIN ta_request_assignments a ON a.id = wl.assignment_id
		    JOIN sections sec ON sec.id = a.section_id
		    WHERE wl.status = 'submitted' AND a.cotaught_group IS NOT NULL
		),
		-- One sitting taught to several sections is ONE thing to review and one
		-- thing to pay (export rule B2). Count it once per co-taught group
		-- before the group's hours are reported.
		sitting AS (
		    SELECT DISTINCT ON (ta_id, teaching_course_id, cotaught_group,
		                        work_date, start_time, end_time)
		           ta_id, teaching_course_id, cotaught_group, hours
		    FROM pend
		),
		grp AS (
		    SELECT ta_id, teaching_course_id, cotaught_group, SUM(hours) AS hours
		    FROM sitting
		    GROUP BY ta_id, teaching_course_id, cotaught_group
		)
		SELECT a.id,
		       a.ta_id,
		       u.first_name || ' ' || u.last_name,
		       a.level::text,
		       tc.code,
		       tc.id,
		       sec.sec_no,
		       sec.track::text,
		       a.cotaught_group,
		       COALESCE(SUM(wl.hours), 0),
		       COALESCE(MAX(g.hours), SUM(wl.hours), 0),
		       MIN(wl.work_date),
		       MAX(wl.work_date),
		       MIN(wl.submitted_at)
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		JOIN users u ON u.id = a.ta_id
		JOIN work_logs wl ON wl.assignment_id = a.id AND wl.status = 'submitted'
		LEFT JOIN grp g ON g.ta_id = a.ta_id
		                AND g.teaching_course_id = tc.id
		                AND g.cotaught_group = a.cotaught_group
		-- Grad-special (master/phd on a special-track section) no longer logs
		-- work_logs at all — the system computes their pay automatically from
		-- the regular track's class schedule (2026 meeting). Any leftover
		-- 'submitted' rows from before that change are inert and must not
		-- surface here: they'd sit forever unapproved and, since the batch
		-- approve call covers every assignment shown for a TA, one grad-special
		-- row failing its (now-meaningless) weekly cap check would block
		-- approving that TA's legitimate grad-regular/undergrad hours too.
		WHERE (a.level::text NOT IN ('master','phd') OR sec.track <> 'special')
		  AND ($1 = TRUE OR EXISTS (
		    SELECT 1 FROM teaching_lecturers tl
		    WHERE tl.teaching_course_id = tc.id AND tl.lecturer_id = $2
		  ))
		GROUP BY a.id, a.ta_id, u.first_name, u.last_name, a.level, tc.code, tc.id,
		         sec.sec_no, sec.track, a.cotaught_group
		ORDER BY MIN(wl.submitted_at) ASC NULLS LAST, tc.code, sec.sec_no`,
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
		if err := rows.Scan(&p.ID, &p.TAID, &p.TAName, &p.StudyLevel, &p.CourseCode, &p.TeachingCourseID,
			&p.SecNo, &p.Track, &p.CoTaughtGroup,
			&p.TotalHours, &p.GroupHours, &minD, &maxD, &submittedAt); err != nil {
			return nil, err
		}
		p.FirstDate = minD.Format("2006-01-02")
		p.LastDate = maxD.Format("2006-01-02")
		if minD.Equal(maxD) {
			p.PeriodLabel = p.FirstDate
		} else {
			p.PeriodLabel = p.FirstDate + " – " + p.LastDate
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
	// Locked is true when this row's month is closed to writes for EVERY role —
	// past its deadline, or already exported / sent to finance. The editor
	// renders locked rows read-only instead of letting a save 409.
	//
	// Widened on 03/08/2026 when the deadline stopped exempting staff: without
	// it the editor kept offering a pencil on rows the server would refuse,
	// which is the same trap the TA's submit button used to be.
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
		       tc.code, sec.sec_no, sec.track, a.level,
		       -- Exactly what assertWorklogWritable refuses, so the pencil the
		       -- editor draws and the write the server accepts cannot disagree.
		       `+unsubmittableMonthSQL("wl")+` AS locked
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		JOIN users u ON u.id = a.ta_id
		JOIN sections sec ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
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
// auditEditAction / auditDeleteAction keep the audit trail honest now that two
// different roles reach the same code path. "worklog.staff_edit" on a row a
// lecturer changed would send an investigator to the wrong desk.
func auditEditAction(privileged bool) string {
	if privileged {
		return "worklog.staff_edit"
	}
	return "worklog.lecturer_edit"
}

func auditDeleteAction(privileged bool) string {
	if privileged {
		return "worklog.staff_delete"
	}
	return "worklog.lecturer_delete"
}

// StaffUpsert edits a TA's work log on their behalf. Reachable by staff/admin
// (privileged) and, since the 24/07/2026 meeting, by the course's own lecturer.
func (s *WorkLogService) StaffUpsert(ctx context.Context, actor uuid.UUID, privileged bool, w WorkLog) (uuid.UUID, error) {
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
	// Lecturers may now correct their own course's rows (24/07/2026 meeting:
	// "อาจารย์ตรวจสอบ ตีกลับ อนุมัติ หรือ แก้ไขได้"). Previously the only
	// caller was staff and the route guard was the whole authorisation story;
	// with a second caller the service has to own the rule, or any lecturer
	// could edit any course's hours.
	if !privileged {
		owns, err := lecturerOwnsCourse(ctx, s.pool, actor, ac.TeachingCourseID)
		if err != nil {
			return uuid.Nil, err
		}
		if !owns {
			return uuid.Nil, ErrForbidden
		}
	}
	if err := s.assertOwnScheduleForCourse(ctx, ac); err != nil {
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
	if err := s.validateGradRegularClassWindow(ctx, ac, w); err != nil {
		return uuid.Nil, err
	}
	// Staff get no exemption from the monthly deadline. A closed month is closed
	// for them too (03/08/2026, at the staff's own request): the only way to
	// correct a month after it closes is to move the period's due date in
	// settings, which is a deliberate, visible act rather than a quiet edit.
	if err := assertWorklogWritable(ctx, s.pool, ac.TeachingCourseID, ac.TAID, w.WorkDate); err != nil {
		return uuid.Nil, err
	}
	if w.ID != uuid.Nil {
		var oldDate string
		if err := s.pool.QueryRow(ctx,
			`SELECT TO_CHAR(work_date,'YYYY-MM-DD') FROM work_logs WHERE id=$1 AND assignment_id=$2`,
			w.ID, w.AssignmentID).Scan(&oldDate); err == nil && oldDate[:7] != w.WorkDate[:7] {
			if err := assertWorklogWritable(ctx, s.pool, ac.TeachingCourseID, ac.TAID, oldDate); err != nil {
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
	if err := s.enforceDailyBahtCap(ctx, ac.TAID, w); err != nil {
		return uuid.Nil, err
	}
	if err := s.enforceNoOverlap(ctx, ac.TAID, w); err != nil {
		return uuid.Nil, err
	}
	// Staff edit on the TA's behalf. The class-timetable rule still applies —
	// the TA physically cannot be in two rooms, so staff must not be able to
	// enter hours the TA could not have worked. (Staff DO keep the back-dating
	// override above; that one is a paperwork concession, not a physical one.)
	if err := s.enforceNoOwnClassConflict(ctx, ac, w); err != nil {
		return uuid.Nil, err
	}

	isNew := w.ID == uuid.Nil
	if isNew {
		w.ID = uuid.New()
	}
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: auditEditAction(privileged), Entity: "work_log", EntityID: w.ID.String(), After: w},
		func(tx pgx.Tx) error {
			if isNew {
				_, err := tx.Exec(ctx,
					`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, parent_kind, room, note, status)
					 VALUES ($1,$2,$3::date,$4::time,$5::time,$6,$7,$8,$9,$10,'draft')`,
					w.ID, w.AssignmentID, w.WorkDate, w.StartTime, w.EndTime, w.Hours, w.Activity, w.ParentKind, w.Room, w.Note)
				return err
			}
			// Preserve status so approved rows stay approved; staff cannot silently unlock review state.
			// The assignment_id predicate is defence-in-depth on top of the pin above.
			tag, err := tx.Exec(ctx,
				`UPDATE work_logs SET work_date=$1::date, start_time=$2::time, end_time=$3::time, hours=$4, activity=$5, parent_kind=$6, room=$7, note=$8
				 WHERE id=$9 AND assignment_id=$10`,
				w.WorkDate, w.StartTime, w.EndTime, w.Hours, w.Activity, w.ParentKind, w.Room, w.Note, w.ID, w.AssignmentID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return Invalid("ไม่พบรายการที่ต้องการแก้ไข")
			}
			return nil
		}); err != nil {
		return uuid.Nil, err
	}
	if s.notify != nil {
		t := s.notifyTarget(ctx, ac.TeachingCourseID)
		s.notify.Send(ctx, ac.TAID,
			"เจ้าหน้าที่แก้ไขบันทึกเวลา "+t.Code,
			fmt.Sprintf("เจ้าหน้าที่ปรับข้อมูลบันทึกเวลาวิชา %s วันที่ %s เวลา %s–%s",
				t.Code, w.WorkDate, w.StartTime, w.EndTime),
			t.Link)
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
	if err := assertWorklogWritable(ctx, s.pool, ac.TeachingCourseID, ac.TAID, workDate); err != nil {
		return err
	}
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "worklog.delete", Entity: "work_log", EntityID: logID.String()},
		func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx,
				`DELETE FROM work_logs WHERE id=$1 AND status IN ('draft','rejected')`, logID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return Invalid("ลบไม่ได้: รายการอาจถูกดำเนินการไปแล้ว")
			}
			return nil
		})
}

// StaffDelete removes a work_log entry. Only draft/rejected can be removed;
// submitted/approved rows must be handled through Reject or a manual DB fix
// so the audit trail stays intact.
func (s *WorkLogService) StaffDelete(ctx context.Context, actor uuid.UUID, privileged bool, id uuid.UUID) error {
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
	// Same rule as StaffUpsert: a lecturer may only touch their own course.
	if !privileged {
		owns, err := lecturerOwnsCourse(ctx, s.pool, actor, tcID)
		if err != nil {
			return err
		}
		if !owns {
			return ErrForbidden
		}
	}
	if err := assertWorklogWritable(ctx, s.pool, tcID, taID, workDate); err != nil {
		return err
	}
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: auditDeleteAction(privileged), Entity: "work_log", EntityID: id.String()},
		func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx,
				`DELETE FROM work_logs WHERE id=$1 AND status IN ('draft','rejected')`, id)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return Invalid("ลบไม่ได้: รายการอาจถูกส่งอนุมัติหรืออนุมัติแล้ว")
			}
			return nil
		}); err != nil {
		return err
	}
	if s.notify != nil {
		t := s.notifyTarget(ctx, tcID)
		s.notify.Send(ctx, taID,
			"เจ้าหน้าที่ลบบันทึกเวลา "+t.Code,
			fmt.Sprintf("เจ้าหน้าที่ลบรายการบันทึกเวลาวิชา %s วันที่ %s เวลา %s", t.Code, workDate, timeSpan),
			t.Link)
	}
	return nil
}

// yearMonth ("YYYY-MM") sends back only that month's rows; "" = whole batch.
func (s *WorkLogService) Reject(ctx context.Context, actor, assignmentID uuid.UUID, reason, yearMonth string, privileged bool) error {
	if reason == "" {
		return Invalid("ต้องระบุเหตุผลการปฏิเสธ")
	}
	if err := validateYearMonth(yearMonth); err != nil {
		return err
	}
	ac, err := s.assertCanReview(ctx, actor, assignmentID, privileged)
	if err != nil {
		return err
	}
	// Mirror Approve: a month at finance_sent is settled — bouncing its rows
	// back would desync the paperwork already at the finance office.
	if err := assertNoFinanceLockedRows(ctx, s.pool, assignmentID, []string{"submitted"}, yearMonth); err != nil {
		return err
	}
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "worklog.reject", Entity: "assignment", EntityID: assignmentID.String(), Note: reason},
		func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx,
				`UPDATE work_logs SET status='rejected', reject_reason=$1
				 WHERE assignment_id=$2 AND status='submitted'
				   AND ($3 = '' OR to_char(work_date, 'YYYY-MM') = $3)`,
				reason, assignmentID, yearMonth)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return Invalid("ไม่มีรายการที่รออนุมัติ (อาจถูกดำเนินการไปแล้ว)")
			}
			return nil
		}); err != nil {
		return err
	}
	if s.notify != nil {
		t := s.notifyTarget(ctx, ac.TeachingCourseID)
		when := thaiYearMonth(yearMonth)
		if when != "" {
			when = " เดือน" + when
		}
		s.notify.Send(ctx, ac.TAID,
			"บันทึกเวลา "+t.Code+" ถูกตีกลับ",
			"อาจารย์ส่งบันทึกเวลาวิชา "+t.Code+when+" กลับมาให้แก้ไข: "+reason,
			t.Link)
	}
	return nil
}

// worklogNotifyTarget is the course a work-log notification is about, worded and
// linked so the TA can act on it.
//
// Every notification this service sent used to read "บันทึกเวลาของคุณถูกส่งกลับ
// ให้แก้ไข" and link to /ta/worklog — a route that does not exist, on a screen
// that is per-course, for a TA who assists three courses. Four identical lines
// in the bell told them something had happened somewhere. The course code and a
// working link are the whole difference between a notice and a nuisance.
type worklogNotifyTarget struct {
	Code string
	Link string
}

func (s *WorkLogService) notifyTarget(ctx context.Context, tcID uuid.UUID) worklogNotifyTarget {
	t := worklogNotifyTarget{Link: "/ta/courses/" + tcID.String() + "/worklog"}
	_ = s.pool.QueryRow(ctx, `SELECT code FROM teaching_courses WHERE id=$1`, tcID).Scan(&t.Code)
	return t
}

// thaiYearMonth turns "2026-09" into "กันยายน 2569". Empty stays empty — the
// caller then means "the whole batch", not one month.
func thaiYearMonth(ym string) string {
	if len(ym) != 7 {
		return ""
	}
	y, err1 := strconv.Atoi(ym[:4])
	m, err2 := strconv.Atoi(ym[5:])
	if err1 != nil || err2 != nil || m < 1 || m > 12 {
		return ""
	}
	return fmt.Sprintf("%s %d", thaiMonthNames[m], y+543)
}
