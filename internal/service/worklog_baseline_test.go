package service

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// worklog_baseline_test.go pins the rules that decide how much a TA is paid.
//
// These are characterisation tests: they record what the system does TODAY so
// that phase 1–2 of the meeting plan (per-section workload hours, the
// class-schedule conflict rule) can refactor worklog.go with a signal that
// something changed. A test failing here after a refactor is not automatically
// a bug — it is a behaviour change that must be an intentional one.
//
// Each test tightens exactly one limit and leaves the others slack (see
// fixture_test.go), so a failure names the rule that broke.

// ---------------------------------------------------------------------------
// Gate: who may write at all
// ---------------------------------------------------------------------------

func TestUpsert_RejectsNonOwner(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	stranger := f.insertUser("ta", "stranger")

	_, err := f.Svc.Upsert(f.ctx, stranger, f.entry(day(10), "09:00", "11:00", 2))
	if err != ErrForbidden {
		t.Fatalf("a TA writing to someone else's assignment must get ErrForbidden, got: %v", err)
	}
	if n := f.countLogs(); n != 0 {
		t.Fatalf("rejected write must not persist, found %d rows", n)
	}
}

func TestUpsert_RejectsUnapprovedRequest(t *testing.T) {
	f := newFixture(t, fixtureOpts{RequestStatus: "rejected"})

	_, err := f.upsert(f.entry(day(10), "09:00", "11:00", 2))
	if err == nil {
		t.Fatal("logging against a non-approved request must be rejected")
	}
	if !strings.Contains(err.Error(), "ยังไม่ได้รับการอนุมัติ") {
		t.Errorf("message should explain the request is not approved, got: %v", err)
	}
}

// Payout is computed per student head-count, so a course with none set cannot
// produce a defensible number.
func TestUpsert_RequiresStudentCount(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoStudents: true})

	_, err := f.upsert(f.entry(day(10), "09:00", "11:00", 2))
	if err == nil {
		t.Fatal("a course with no student count must reject work logs")
	}
	if !strings.Contains(err.Error(), "จำนวนนักศึกษา") {
		t.Errorf("message should mention the student count, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Per-day hour cap — ประกาศ 731/2565 + 1080/2565
// ป.ตรี ปกติ ≤ 7 ชม./วัน · ป.ตรี พิเศษ ≤ 6 · บัณฑิต ปกติ ≤ 6
// ---------------------------------------------------------------------------

func TestUpsert_DailyHourCap_UndergradRegularIs7(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "undergrad", Track: "regular"})
	d := day(10)

	// 4h + 3h == exactly 7h is allowed.
	f.mustUpsert(f.entry(d, "08:00", "12:00", 4))
	f.mustUpsert(f.entry(d, "13:00", "16:00", 3))

	// The next 30 minutes crosses the cap.
	_, err := f.upsert(f.entry(d, "16:00", "16:30", 0.5))
	if err == nil {
		t.Fatal("a 7.5h day must be rejected for undergrad/regular")
	}
	if !strings.Contains(err.Error(), "7.0") && !strings.Contains(err.Error(), "7") {
		t.Errorf("message should name the 7h cap, got: %v", err)
	}
	if n := f.countLogs(); n != 2 {
		t.Errorf("expected the two accepted rows only, found %d", n)
	}
}

func TestUpsert_DailyHourCap_UndergradSpecialIs6(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "undergrad", Track: "special"})
	d := day(10)

	f.mustUpsert(f.entry(d, "08:00", "14:00", 6))

	_, err := f.upsert(f.entry(d, "14:00", "15:00", 1))
	if err == nil {
		t.Fatal("a 7h day must be rejected for undergrad/special (cap 6)")
	}
}

func TestUpsert_DailyHourCap_GradRegularIs6(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master", Track: "regular"})
	d := day(10)

	f.mustUpsert(f.entry(d, "08:00", "14:00", 6))

	if _, err := f.upsert(f.entry(d, "14:00", "15:00", 1)); err == nil {
		t.Fatal("a 7h day must be rejected for graduate/regular (cap 6)")
	}
}

// The cap counts the whole day, not each row, and it must not leak across days.
func TestUpsert_DailyHourCap_IsPerDay(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	f.mustUpsert(f.entry(day(10), "08:00", "15:00", 7))
	// A different day starts a fresh budget.
	f.mustUpsert(f.entry(day(11), "08:00", "15:00", 7))

	if n := f.countLogs(); n != 2 {
		t.Fatalf("expected 2 rows across two days, found %d", n)
	}
}

// ---------------------------------------------------------------------------
// Per-day baht cap (Q&A rule 6a) — 300฿/day across every assignment the TA
// holds, so a TA cannot bill two courses to route around it.
// ---------------------------------------------------------------------------

func TestUpsert_DailyBahtCap_BlocksWithinOneCourse(t *testing.T) {
	// 50฿/h against a 300฿ day = 6 payable hours, below the 7h hour cap, so the
	// baht rule is the binding constraint.
	f := newFixture(t, fixtureOpts{
		Rates: rateOverrides{UndergradRegular: 50, DailyPayCapBaht: 300},
	})
	d := day(10)

	f.mustUpsert(f.entry(d, "08:00", "14:00", 6)) // 300฿ exactly

	_, err := f.upsert(f.entry(d, "14:00", "15:00", 1))
	if err == nil {
		t.Fatal("exceeding the 300฿ daily cap must be rejected")
	}
	if !strings.Contains(err.Error(), "300") {
		t.Errorf("message should name the baht cap, got: %v", err)
	}
}

func TestUpsert_DailyBahtCap_SpansCourses(t *testing.T) {
	f := newFixture(t, fixtureOpts{
		Rates: rateOverrides{UndergradRegular: 50, DailyPayCapBaht: 300},
	})
	other := f.secondCourseAssignment(fixtureOpts{})
	d := day(10)

	// 5h on course A = 250฿.
	f.mustUpsert(f.entry(d, "08:00", "13:00", 5))

	// 2h on course B would add 100฿ → 350฿ total. Different assignment, same
	// TA, same day: the cap is per person per day.
	second := f.entry(d, "14:00", "16:00", 2)
	second.AssignmentID = other
	if _, err := f.upsert(second); err == nil {
		t.Fatal("the daily baht cap must aggregate across the TA's courses")
	}
}

// Grad-special is paid a flat monthly lump sum, so its hours contribute 0 baht
// and must not consume another assignment's daily allowance.
func TestUpsert_DailyBahtCap_ExemptsGradSpecial(t *testing.T) {
	f := newFixture(t, fixtureOpts{
		Level: "master", Track: "special",
		Rates: rateOverrides{DailyPayCapBaht: 300, GradRegularDailyCap: 6},
	})

	// Six hours would be 300฿+ at any hourly rate; as a lump-sum row it costs
	// nothing against the cap and is bounded only by the 6h/day hour cap.
	f.mustUpsert(f.entry(day(10), "08:00", "14:00", 6))
	if n := f.countLogs(); n != 1 {
		t.Fatalf("grad-special row should have been accepted, found %d rows", n)
	}
}

// ---------------------------------------------------------------------------
// Weekly per-activity cap — bounded by the lecturer's declared workload
// ---------------------------------------------------------------------------

func TestUpsert_WeeklyActivityCap(t *testing.T) {
	// Only 3h/week of ตรวจงาน declared. Everything else stays generous.
	f := newFixture(t, fixtureOpts{
		Workload: workloadHours{Attendance: 20, Lab: 20, CheckWork: 3, UGOther: 20},
	})
	mon, tue := sameWeekDays()

	f.mustUpsert(f.entry(mon, "08:00", "10:00", 2))

	// 2h + 2h = 4h > 3h declared for this activity, in the same Mon–Sun week.
	_, err := f.upsert(f.entry(tue, "08:00", "10:00", 2))
	if err == nil {
		t.Fatal("exceeding the declared weekly hours for an activity must be rejected")
	}
	if !strings.Contains(err.Error(), "สัปดาห์") {
		t.Errorf("message should mention the weekly quota, got: %v", err)
	}
}

// An activity the lecturer never declared hours for cannot be logged at all.
func TestUpsert_UndeclaredActivityBlocked(t *testing.T) {
	f := newFixture(t, fixtureOpts{
		Workload: workloadHours{Attendance: 20, Lab: 20, CheckWork: 0, UGOther: 20},
	})

	_, err := f.upsert(f.entry(day(10), "08:00", "10:00", 2)) // activity=review
	if err == nil {
		t.Fatal("an activity with no declared hours must be rejected")
	}
	if !strings.Contains(err.Error(), "ตรวจงาน") {
		t.Errorf("message should name the activity, got: %v", err)
	}
}

// With no workload form at all the gates stay open — the lecturer simply has
// not filled it in yet, and locking the TA out would be worse than permissive.
func TestUpsert_NoWorkloadFormAllowsEverything(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoWorkloadForm: true})

	f.mustUpsert(f.entry(day(10), "08:00", "10:00", 2))
	if n := f.countLogs(); n != 1 {
		t.Fatalf("expected the row to be accepted, found %d", n)
	}
}

// ---------------------------------------------------------------------------
// Term ceiling — declared weekly hours × weeks in term
// ---------------------------------------------------------------------------

func TestUpsert_TermHourCeiling(t *testing.T) {
	// 1h/week declared over a 4-week term → 4h for the whole term.
	//
	// The dates are stated explicitly because the ceiling is now counted off the
	// CALENDAR, not off academic_terms.months × 4 — a term of "1 month" whose
	// dates spanned three was previously assigned a 4-week ceiling, which is the
	// approximation that put every TA of SC362102 over their limit.
	start := monthStart()
	f := newFixture(t, fixtureOpts{
		TermStart: start.Format("2006-01-02"),
		TermEnd:   start.AddDate(0, 0, 27).Format("2006-01-02"), // 28 days = 4 weeks
		Workload:  workloadHours{CheckWork: 1},
	})

	// Spread across four separate weeks so the weekly cap (1h) is never the
	// thing that fires; days 1, 8, 15, 22 are always in distinct weeks.
	for _, n := range []int{1, 8, 15, 22} {
		f.mustUpsert(f.entry(day(n), "08:00", "09:00", 1))
	}

	_, err := f.upsert(f.entry(day(28), "08:00", "09:00", 1))
	if err == nil {
		t.Fatal("exceeding weekly-hours × term-weeks must be rejected")
	}
	if !strings.Contains(err.Error(), "ทั้งเทอม") {
		t.Errorf("message should mention the term ceiling, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Clock overlap — the same hour must not be billed twice
// ---------------------------------------------------------------------------

func TestUpsert_RejectsOverlapWithinSameAssignment(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := day(10)

	f.mustUpsert(f.entry(d, "09:00", "11:00", 2))

	if _, err := f.upsert(f.entry(d, "10:00", "12:00", 2)); err == nil {
		t.Fatal("overlapping time ranges must be rejected")
	}
}

func TestUpsert_RejectsOverlapAcrossCourses(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	other := f.secondCourseAssignment(fixtureOpts{})
	d := day(10)

	f.mustUpsert(f.entry(d, "09:00", "11:00", 2))

	clash := f.entry(d, "10:00", "12:00", 2)
	clash.AssignmentID = other
	_, err := f.upsert(clash)
	if err == nil {
		t.Fatal("the same clock hours must not be billable to two courses")
	}
	if !strings.Contains(err.Error(), "ซ้อน") {
		t.Errorf("message should explain the overlap, got: %v", err)
	}
}

// Back-to-back rows share an instant but no duration, so they are legal.
func TestUpsert_AllowsBackToBackRows(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := day(10)

	f.mustUpsert(f.entry(d, "09:00", "11:00", 2))
	f.mustUpsert(f.entry(d, "11:00", "13:00", 2))

	if n := f.countLogs(); n != 2 {
		t.Fatalf("back-to-back rows should both persist, found %d", n)
	}
}

// ---------------------------------------------------------------------------
// Monthly write lock — the payout file must not change under staff's feet
// ---------------------------------------------------------------------------

func TestUpsert_BlockedAfterFinanceSent(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "finance_sent", false)

	_, err := f.upsert(f.entry(day(10), "09:00", "11:00", 2))
	if err == nil {
		t.Fatal("a month already sent to finance must be frozen")
	}
}

func TestUpsert_BlockedWhenPeriodClosed(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.addSubmissionPeriod(currentMonthMM(), day(28), "pending", true)

	if _, err := f.upsert(f.entry(day(10), "09:00", "11:00", 2)); err == nil {
		t.Fatal("a closed submission period must block TA writes")
	}
}

// A month with a period that is open and still pending stays writable — the
// lock must not fire merely because a period row exists.
func TestUpsert_AllowedWhenPeriodOpen(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "pending", false)

	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
}

// A term that never adopted the monthly workflow has no period rows at all and
// must remain fully writable.
func TestUpsert_AllowedWithNoPeriodDefined(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
}

// ---------------------------------------------------------------------------
// Edit semantics
// ---------------------------------------------------------------------------

// Re-saving a row must not double-count itself against the day's totals.
func TestUpsert_EditExcludesOwnRowFromCaps(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := day(10)

	id := f.mustUpsert(f.entry(d, "08:00", "15:00", 7)) // the full daily cap

	edited := f.entry(d, "08:00", "15:00", 7)
	edited.ID = id
	if _, err := f.upsert(edited); err != nil {
		t.Fatalf("re-saving an unchanged row must not trip the daily cap: %v", err)
	}
	if n := f.countLogs(); n != 1 {
		t.Fatalf("edit must update in place, found %d rows", n)
	}
}

// Once any row has been submitted or approved the assignment stops accepting
// new rows, so hours cannot be inflated after review has begun.
func TestUpsert_NoNewRowsAfterSubmission(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := day(10)

	f.mustUpsert(f.entry(d, "08:00", "10:00", 2))
	f.exec(`UPDATE work_logs SET status='submitted' WHERE assignment_id=$1`, f.AssignmentID)

	_, err := f.upsert(f.entry(day(11), "08:00", "10:00", 2))
	if err == nil {
		t.Fatal("new rows must be refused once the assignment is under review")
	}
}

func TestUpsert_RejectsUnknownAssignment(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	w := f.entry(day(10), "09:00", "11:00", 2)
	w.AssignmentID = uuid.New()
	if _, err := f.upsert(w); err != ErrNotFound {
		t.Fatalf("unknown assignment should be ErrNotFound, got: %v", err)
	}
}
