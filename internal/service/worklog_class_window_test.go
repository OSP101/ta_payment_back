package service

import (
	"strings"
	"testing"
	"time"
)

// The manual "เพิ่มรายการ" path is the one place a TA types hours the generator
// did not produce, so it is the one place the numbers can be invented. Two rules
// keep a typed row honest, and both were missing for the undergrad majority:
//
//  1. The คาบ has to exist — a lecture/lab entry must land inside a period the
//     section actually runs on that weekday (section_schedules, as staff filed
//     it), or inside a makeup filed onto that date.
//  2. A คาบ declared "ไม่มีการชดเชย" never happened, so nothing may be billed on
//     it — and reading those rows must not fail, which is what broke every save
//     on a section after the first waive.
//
// The fixture section teaches Monday 09:00–12:00 (lecture) and 13:00–16:00 (lab).

// mondayInTerm is the first Monday of the fixture's term — a day the section
// really meets.
func mondayInTerm() time.Time {
	d := monthStart()
	for d.Weekday() != time.Monday {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

func classEntry(f *fixture, date, start, end string, hours float64, activity string) WorkLog {
	w := f.entry(date, start, end, hours)
	w.Activity = activity
	return w
}

// ---------------------------------------------------------------------------
// The คาบ must exist
// ---------------------------------------------------------------------------

// The normal case the generator produces: the last declared hour of the Monday
// lecture. Inside the period, so it stands.
func TestClassWindow_LectureInsideThePeriodIsAccepted(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mon := mondayInTerm().Format("2006-01-02")

	if _, err := f.upsert(classEntry(f, mon, "11:00", "12:00", 1, "lecture")); err != nil {
		t.Fatalf("an hour inside the 09:00–12:00 lecture must be accepted: %v", err)
	}
}

// A whole period is fine too — the entry has to FIT the คาบ, not be shorter than it.
func TestClassWindow_WholeLecturePeriodIsAccepted(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mon := mondayInTerm().Format("2006-01-02")

	if _, err := f.upsert(classEntry(f, mon, "09:00", "12:00", 3, "lecture")); err != nil {
		t.Fatalf("the full 09:00–12:00 lecture must be accepted: %v", err)
	}
}

// The hole this closes: an undergrad TA inventing a class on a day the section
// never meets. Tuesday has no period at all, so the row is refused outright —
// it used to be accepted, capped only by the weekly quota it then consumed.
func TestClassWindow_LectureOnANonClassDayIsRefused(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	tue := mondayInTerm().AddDate(0, 0, 1).Format("2006-01-02")

	_, err := f.upsert(classEntry(f, tue, "09:00", "10:00", 1, "lecture"))
	if err == nil {
		t.Fatal("a lecture on a day the section does not meet must be refused")
	}
	if !strings.Contains(err.Error(), "ตารางสอน") {
		t.Errorf("the refusal should point at the timetable, got: %v", err)
	}
}

// Right day, wrong hours: 20:00 is outside the 09:00–12:00 lecture. Refused, and
// the message names the real window so the TA can correct it.
func TestClassWindow_LectureOutsideThePeriodHoursIsRefused(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mon := mondayInTerm().Format("2006-01-02")

	_, err := f.upsert(classEntry(f, mon, "20:00", "21:00", 1, "lecture"))
	if err == nil {
		t.Fatal("a lecture outside the scheduled period hours must be refused")
	}
	if !strings.Contains(err.Error(), "09:00") {
		t.Errorf("the refusal should name the real period, got: %v", err)
	}
}

// An entry that starts inside the คาบ but runs past its end is billing time the
// class did not run. Containment, not overlap, is the rule.
func TestClassWindow_LectureSpillingPastThePeriodIsRefused(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mon := mondayInTerm().Format("2006-01-02")

	if _, err := f.upsert(classEntry(f, mon, "11:00", "14:00", 3, "lecture")); err == nil {
		t.Fatal("a lecture running past the period's end must be refused")
	}
}

// The lab's own window is separate from the lecture's: 09:00 is a lecture hour,
// not a lab hour, so a lab row there is refused even though the day is right.
func TestClassWindow_LabMustUseTheLabPeriod(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mon := mondayInTerm().Format("2006-01-02")

	if _, err := f.upsert(classEntry(f, mon, "09:00", "10:00", 1, "lab")); err == nil {
		t.Fatal("a lab logged during the lecture period must be refused")
	}
	if _, err := f.upsert(classEntry(f, mon, "13:00", "15:00", 2, "lab")); err != nil {
		t.Fatalf("a lab inside 13:00–16:00 must be accepted: %v", err)
	}
}

// Grading is off-site work, so it is not tied to the CLASS grid — this passes on
// a Tuesday, when the section only meets on Monday. It is tied to the TA's own
// grading timetable instead (see the ตรวจงาน tests below); the fixture declares
// wide grading slots, so what this pins is the independence from the class grid.
func TestClassWindow_ReviewIsNotTiedToTheClassGrid(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	tue := mondayInTerm().AddDate(0, 0, 1).Format("2006-01-02")

	if _, err := f.upsert(classEntry(f, tue, "20:00", "21:00", 1, "review")); err != nil {
		t.Fatalf("review is not timetabled and must stay loggable: %v", err)
	}
}

// A makeup is the legitimate way duty hours land off the weekly grid. The window
// the lecturer filed becomes the legal window for that date — including on a
// Saturday, which no section_schedules row describes.
func TestClassWindow_MakeupOpensItsOwnWindow(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mon := mondayInTerm()
	sat := mon.AddDate(0, 0, 5) // Saturday of the same week

	f.exec(`INSERT INTO makeup_schedules (section_id, original_date, makeup_date, kind, start_time, end_time)
	        VALUES ($1, $2::date, $3::date, 'lecture', '13:00', '15:00')`,
		f.SectionID, mon.Format("2006-01-02"), sat.Format("2006-01-02"))

	satISO := sat.Format("2006-01-02")
	if _, err := f.upsert(classEntry(f, satISO, "13:00", "15:00", 2, "lecture")); err != nil {
		t.Fatalf("the filed makeup window must be loggable: %v", err)
	}
	// Outside that window the Saturday is still an ordinary non-class day.
	if _, err := f.upsert(classEntry(f, satISO, "18:00", "19:00", 1, "lecture")); err == nil {
		t.Fatal("hours outside the filed makeup window must still be refused")
	}
}

// A makeup filed with only a date keeps the คาบ's normal length, so the original
// weekday's window is what authorises the hours.
func TestClassWindow_MakeupWithoutTimesFallsBackToTheOriginalPeriod(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mon := mondayInTerm()
	sat := mon.AddDate(0, 0, 5)

	f.exec(`INSERT INTO makeup_schedules (section_id, original_date, makeup_date, kind)
	        VALUES ($1, $2::date, $3::date, 'lecture')`,
		f.SectionID, mon.Format("2006-01-02"), sat.Format("2006-01-02"))

	satISO := sat.Format("2006-01-02")
	if _, err := f.upsert(classEntry(f, satISO, "11:00", "12:00", 1, "lecture")); err != nil {
		t.Fatalf("the original 09:00–12:00 period should authorise the makeup: %v", err)
	}
	if _, err := f.upsert(classEntry(f, satISO, "20:00", "21:00", 1, "lecture")); err == nil {
		t.Fatal("hours outside the original period must still be refused")
	}
}

// A section whose timetable staff have not filed yet has no grid to judge
// against. The caps still bound the claim; this must not become a wall.
func TestClassWindow_NoTimetableDefersToTheCaps(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.exec(`DELETE FROM section_schedules WHERE section_id = $1`, f.SectionID)
	mon := mondayInTerm().Format("2006-01-02")

	if _, err := f.upsert(classEntry(f, mon, "09:00", "10:00", 1, "lecture")); err != nil {
		t.Fatalf("with no timetable filed the entry must not be blocked: %v", err)
	}
}

// ---------------------------------------------------------------------------
// "ไม่มีการชดเชย" — makeup_date IS NULL
// ---------------------------------------------------------------------------

// The regression: WaiveMakeup writes makeup_date = NULL, loadMakeupIndex read it
// into a bare time.Time, and the scan error failed the whole Upsert. One waived
// คาบ made every work-log save on the section impossible — including entries on
// unrelated dates.
func TestWaivedMakeup_DoesNotBreakUnrelatedSaves(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mon := mondayInTerm()

	f.exec(`INSERT INTO makeup_schedules (section_id, original_date, kind, waived)
	        VALUES ($1, $2::date, 'lecture', true)`,
		f.SectionID, mon.Format("2006-01-02"))

	other := mon.AddDate(0, 0, 1).Format("2006-01-02")
	if _, err := f.upsert(classEntry(f, other, "20:00", "21:00", 1, "review")); err != nil {
		t.Fatalf("a waived คาบ elsewhere must not block an unrelated entry: %v", err)
	}
}

// The คาบ was cancelled with no replacement, so there are no duty hours on it.
func TestWaivedMakeup_RefusesTheCancelledPeriod(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mon := mondayInTerm().Format("2006-01-02")

	f.exec(`INSERT INTO makeup_schedules (section_id, original_date, kind, waived)
	        VALUES ($1, $2::date, 'lecture', true)`,
		f.SectionID, mon)

	_, err := f.upsert(classEntry(f, mon, "11:00", "12:00", 1, "lecture"))
	if err == nil {
		t.Fatal("a คาบ declared ไม่มีการชดเชย must not be billable")
	}
	if !strings.Contains(err.Error(), "ไม่มีการชดเชย") {
		t.Errorf("the refusal should say the คาบ was waived, got: %v", err)
	}
}

// Generate read the same NULL through a swallowed scan error, so a waived คาบ was
// invisible to it and got planted as an ordinary billable row.
func TestWaivedMakeup_GeneratorSkipsTheCancelledPeriod(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mon := mondayInTerm().Format("2006-01-02")

	f.exec(`INSERT INTO makeup_schedules (section_id, original_date, kind, waived)
	        VALUES ($1, $2::date, 'lecture', true)`,
		f.SectionID, mon)

	if _, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var n int
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT COUNT(*) FROM work_logs
		WHERE assignment_id = $1 AND work_date = $2::date AND activity = 'lecture'`,
		f.AssignmentID, mon).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("generated %d lecture row(s) on a waived คาบ, want 0", n)
	}
	// The lab that day was NOT waived, so it must still be generated — the skip
	// is per period, not per date.
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT COUNT(*) FROM work_logs
		WHERE assignment_id = $1 AND work_date = $2::date AND activity = 'lab'`,
		f.AssignmentID, mon).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n == 0 {
		t.Error("the un-waived lab on the same date should still be generated")
	}
}

// ---------------------------------------------------------------------------
// ตรวจงาน must sit in a slot somebody declared
// ---------------------------------------------------------------------------

// Grading is off-site work, so it is not tied to the CLASS timetable — but it is
// now tied to the grading timetable the TA files themselves. Without that, a
// declared "2 ชม./สัปดาห์ ตรวจงาน" could be billed at any hour of any day, and
// the record said nothing true about when the work happened.
func TestReviewWindow_InsideTheDeclaredSlotIsAccepted(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoReviewSchedule: true})
	tue := mondayInTerm().AddDate(0, 0, 1)
	f.exec(`INSERT INTO ta_review_schedules (assignment_id, kind, day_of_week, start_time, end_time)
	        VALUES ($1, 'review', $2, '08:00', '10:00')`, f.AssignmentID, int(tue.Weekday()))

	if _, err := f.upsert(classEntry(f, tue.Format("2006-01-02"), "08:00", "10:00", 2, "review")); err != nil {
		t.Fatalf("grading inside the declared slot must be accepted: %v", err)
	}
}

// The hole this closes: the same two hours, moved to 03:00 on a Sunday.
func TestReviewWindow_OutsideTheDeclaredSlotIsRefused(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoReviewSchedule: true})
	mon := mondayInTerm()
	tue := mon.AddDate(0, 0, 1)
	f.exec(`INSERT INTO ta_review_schedules (assignment_id, kind, day_of_week, start_time, end_time)
	        VALUES ($1, 'review', $2, '08:00', '10:00')`, f.AssignmentID, int(tue.Weekday()))

	// Right weekday, wrong hours.
	if _, err := f.upsert(classEntry(f, tue.Format("2006-01-02"), "20:00", "21:00", 1, "review")); err == nil {
		t.Error("grading outside the declared hours must be refused")
	}
	// Right hours, wrong weekday.
	if _, err := f.upsert(classEntry(f, mon.Format("2006-01-02"), "08:00", "10:00", 2, "review")); err == nil {
		t.Error("grading on a day with no declared slot must be refused")
	}
}

// A grading date the lecturer filed against the section authorises its own hours
// — the generator writes rows from that table too, so they must stay loggable.
func TestReviewWindow_LecturerFiledDateIsAccepted(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoReviewSchedule: true})
	tue := mondayInTerm().AddDate(0, 0, 1)
	tueISO := tue.Format("2006-01-02")
	f.exec(`INSERT INTO lecture_review_dates (section_id, review_date, start_time, end_time, hours)
	        VALUES ($1, $2::date, '13:00', '15:00', 2)`, f.SectionID, tueISO)

	if _, err := f.upsert(classEntry(f, tueISO, "13:00", "15:00", 2, "review")); err != nil {
		t.Fatalf("a grading date the lecturer filed must be loggable: %v", err)
	}
	// It authorises its own window and no more.
	if _, err := f.upsert(classEntry(f, tueISO, "20:00", "21:00", 1, "review")); err == nil {
		t.Error("hours outside the filed grading window must still be refused")
	}
}

// Unlike the class grid — which staff own, so its absence is not the TA's fault
// — this table is the TA's own and the remedy is in their hands. They are told
// to declare the slot rather than quietly allowed to bill against nothing.
func TestReviewWindow_NoDeclaredSlotTellsTheTAToDeclareOne(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoReviewSchedule: true})
	tue := mondayInTerm().AddDate(0, 0, 1).Format("2006-01-02")

	_, err := f.upsert(classEntry(f, tue, "08:00", "10:00", 2, "review"))
	if err == nil {
		t.Fatal("grading with no declared slot anywhere must be refused")
	}
	if !strings.Contains(err.Error(), "ตารางตรวจการบ้าน") {
		t.Errorf("the refusal should point at the grading timetable, got: %v", err)
	}
}
