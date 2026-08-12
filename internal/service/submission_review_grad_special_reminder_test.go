package service

import "testing"

// RemindLecturerUnapproved counts 'submitted' work_logs to tell a lecturer
// how many rows await their approval. A grad-special (master/phd,
// track=special) TA no longer logs work_logs — they are also excluded from
// ListPending, the lecturer's own approval screen — so a leftover
// 'submitted' row from before that change must not be counted: it would
// send a lecturer a reminder about hours their own approval screen shows
// none of.
func TestRemindLecturerUnapproved_ExcludesGradSpecial(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master", Track: "special"})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='submitted', submitted_at=now() WHERE assignment_id=$1`, f.AssignmentID)

	err := f.Periods.RemindLecturerUnapproved(f.ctx, f.StaffID, f.CourseID)
	if err == nil {
		t.Fatal("expected an Invalid error saying nothing is waiting, got nil")
	}
}

// A grad-regular (master/phd, track=regular) submitted row is a real,
// approvable row and must still be counted.
func TestRemindLecturerUnapproved_StillCountsGradRegular(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master", Track: "regular"})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='submitted', submitted_at=now() WHERE assignment_id=$1`, f.AssignmentID)

	if err := f.Periods.RemindLecturerUnapproved(f.ctx, f.StaffID, f.CourseID); err != nil {
		t.Fatalf("grad-regular submitted row must still count as waiting: %v", err)
	}
}
