package service

import (
	"strings"
	"testing"
)

// The rule the 24/07/2026 meeting put at the centre of the system: a TA
// attends their own class rather than teaching. Before this, ta_class_schedules
// was never consulted on the write path, so these hours reached the payout.
//
// Exercised through the real Upsert so the guarantee covers the path the TA
// actually uses, not just the predicate underneath it.

func TestUpsert_RejectsHoursClashingWithOwnClass(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := day(10)
	f.addOwnClass("CP101 Programming", weekdayOf(t, d), "09:00", "12:00")

	_, err := f.upsert(f.entry(d, "10:00", "11:00", 1))
	if err == nil {
		t.Fatal("logging hours during the TA's own class must be refused")
	}
	// The message has to name the class, or the TA cannot tell whether to move
	// the entry or fix a stale timetable row.
	for _, want := range []string{"CP101 Programming", "ตารางเรียน"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message should mention %q, got: %v", want, err)
		}
	}
	if n := f.countLogs(); n != 0 {
		t.Errorf("refused write must not persist, found %d rows", n)
	}
}

func TestUpsert_AllowsHoursOutsideOwnClass(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := day(10)
	f.addOwnClass("CP101", weekdayOf(t, d), "09:00", "12:00")

	// Immediately before and immediately after the class — touching edges are
	// free, so a TA whose class ends at noon can start teaching at noon.
	f.mustUpsert(f.entry(d, "08:00", "09:00", 1))
	f.mustUpsert(f.entry(d, "12:00", "14:00", 2))

	if n := f.countLogs(); n != 2 {
		t.Fatalf("both non-clashing rows should persist, found %d", n)
	}
}

// The clash is weekday-based, so the same clock hours on another day are fine.
func TestUpsert_OwnClassOnlyBlocksItsOwnWeekday(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	blocked := day(10)
	other := day(11)
	if weekdayOf(t, blocked) == weekdayOf(t, other) {
		t.Fatal("fixture precondition: the two days must differ")
	}
	f.addOwnClass("CP101", weekdayOf(t, blocked), "09:00", "12:00")

	if _, err := f.upsert(f.entry(blocked, "10:00", "11:00", 1)); err == nil {
		t.Fatal("the class weekday must be blocked")
	}
	f.mustUpsert(f.entry(other, "10:00", "11:00", 1))
}

// Rule C5: the WBA sentinel spans no real time and must never block anything.
func TestUpsert_WBARowDoesNotBlock(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.addWBARow()

	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if n := f.countLogs(); n != 1 {
		t.Fatalf("a WBA row must not block work logs, found %d rows", n)
	}
}

// POLICY CHANGE. This used to assert the opposite — that a TA with no timetable
// could still log time, so the work-log path would not deadlock them. The rule is
// now that they cannot, because the faculty's signed form is a grid of the TA's
// classes AND their duties: with the class half empty it is neither the document
// the faculty signs nor able to show a duty scheduled on top of a lecture.
//
// The old test's concern still stands and is asserted below: refusing must not
// strand the TA, so the message has to name the way out.
func TestUpsert_RefusedUntilTimetableIsFiled(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoOwnClassSchedule: true})

	_, err := f.upsert(f.entry(day(10), "09:00", "11:00", 2))
	if err == nil {
		t.Fatal("logging time with no timetable on file must be refused")
	}
	if !strings.Contains(err.Error(), "ตารางเรียนของฉัน") {
		t.Errorf("the refusal must name where to fix it, or the TA is stuck: %v", err)
	}
}

// Staff edit on the TA's behalf. Staff may back-date, which is a paperwork
// concession; they may not enter hours the TA physically could not have
// worked.
func TestStaffUpsert_AlsoRespectsOwnClass(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	staff := f.insertUser("staff", "officer")
	d := day(10)
	f.addOwnClass("CP101", weekdayOf(t, d), "09:00", "12:00")

	w := f.entry(d, "10:00", "11:00", 1)
	if _, err := f.Svc.StaffUpsert(f.ctx, staff, true, w); err == nil {
		t.Fatal("staff must not be able to enter hours clashing with the TA's class")
	}
	if n := f.countLogs(); n != 0 {
		t.Errorf("refused staff write must not persist, found %d rows", n)
	}
}

// Editing an existing row must be re-checked: a timetable filed after the row
// was created can turn a legal entry into an impossible one.
func TestUpsert_EditIntoClashIsRejected(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := day(10)

	// A timetable must exist before any write is allowed at all; this one is on a
	// different day so the first entry is still legal.
	f.addOwnClass("ZZ999", (weekdayOf(t, d)+2)%7, "08:00", "09:00")
	id := f.mustUpsert(f.entry(d, "13:00", "15:00", 2))
	f.addOwnClass("CP101", weekdayOf(t, d), "09:00", "12:00")

	moved := f.entry(d, "10:00", "12:00", 2)
	moved.ID = id
	if _, err := f.upsert(moved); err == nil {
		t.Fatal("moving a row into the TA's class time must be refused")
	}
}
