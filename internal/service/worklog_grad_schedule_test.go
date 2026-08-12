package service

import "testing"

// Per the 2026 staff meeting: a grad-regular (level master/phd, track
// regular) TA's เช็คชื่อ/สอนปฏิบัติการ entries must reference the section's
// real scheduled class period — not be freely typed the way undergrad and
// grad-special entries still are. The fixture's default section schedule
// (fixture_test.go's insertSectionSchedule) gives every test a Monday
// lecture 09:00-12:00 and a Monday lab 13:00-16:00 to validate against.

func TestUpsert_GradRegularLecture_WithinScheduledWindow_Succeeds(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master", Track: "regular"})
	mon, _ := sameWeekDays()
	if _, err := f.upsert(WorkLog{
		AssignmentID: f.AssignmentID, WorkDate: mon,
		StartTime: "10:00", EndTime: "11:00", Hours: 1, Activity: "lecture",
	}); err != nil {
		t.Fatalf("Upsert within the real lecture window should succeed: %v", err)
	}
}

func TestUpsert_GradRegularLecture_OutsideScheduledWindow_Rejected(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master", Track: "regular"})
	mon, _ := sameWeekDays()
	// 13:00-14:00 on the same Monday is the LAB window, not the lecture one —
	// entering it as "lecture" must be rejected, not silently accepted because
	// some schedule row exists that day.
	if _, err := f.upsert(WorkLog{
		AssignmentID: f.AssignmentID, WorkDate: mon,
		StartTime: "13:00", EndTime: "14:00", Hours: 1, Activity: "lecture",
	}); err == nil {
		t.Fatal("Upsert outside the real lecture window should be rejected")
	}
}

func TestUpsert_GradRegularLecture_NoScheduleThatWeekday_Rejected(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master", Track: "regular"})
	_, tue := sameWeekDays() // Tuesday — the fixture only schedules Monday
	if _, err := f.upsert(WorkLog{
		AssignmentID: f.AssignmentID, WorkDate: tue,
		StartTime: "10:00", EndTime: "11:00", Hours: 1, Activity: "lecture",
	}); err == nil {
		t.Fatal("Upsert on a weekday with no scheduled class should be rejected")
	}
}

func TestUpsert_GradRegularLab_WithinScheduledWindow_Succeeds(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master", Track: "regular"})
	mon, _ := sameWeekDays()
	if _, err := f.upsert(WorkLog{
		AssignmentID: f.AssignmentID, WorkDate: mon,
		StartTime: "13:00", EndTime: "15:00", Hours: 2, Activity: "lab",
	}); err != nil {
		t.Fatalf("Upsert within the real lab window should succeed: %v", err)
	}
}

// Grading stays free-form — the 2026 meeting only tied เช็คชื่อ/สอนปฏิบัติการ
// to the real schedule, not ตรวจงาน ("ตรวจงานวันเวลาไหน" asks for a specific
// date/time, not one matching a class period).
func TestUpsert_GradRegularReview_NotConstrainedToSchedule(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master", Track: "regular"})
	_, tue := sameWeekDays()
	if _, err := f.upsert(WorkLog{
		AssignmentID: f.AssignmentID, WorkDate: tue,
		StartTime: "20:00", EndTime: "21:00", Hours: 1, Activity: "review",
	}); err != nil {
		t.Fatalf("grading must stay free-form regardless of the class schedule: %v", err)
	}
}

// Undergrad is untouched by this rule — only grad-regular gets the schedule
// tie.
func TestUpsert_Undergrad_NotConstrainedToSchedule(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "undergrad", Track: "regular"})
	_, tue := sameWeekDays()
	if _, err := f.upsert(WorkLog{
		AssignmentID: f.AssignmentID, WorkDate: tue,
		StartTime: "10:00", EndTime: "11:00", Hours: 1, Activity: "lecture",
	}); err != nil {
		t.Fatalf("undergrad lecture entries must stay unconstrained by this rule: %v", err)
	}
}
