package service

import "testing"

// A draft left unsent when its period closed is forfeited ("ไม่ประสงค์ลงเวลา").
// It used to count as "not yet approved", so one forgotten draft kept the whole
// month from being signed off by staff or locked by the export — however
// complete the approved rest was. Found in the UAT follow-up review.
func TestForfeitedDraft_DoesNotBlockSignOffOrExport(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.addAppointmentOrder()
	payoutReady(f)
	pid := f.addSubmissionPeriod(currentMonthMM(), "2026-12-31", "", false)
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, "", false); err != nil {
		t.Fatal(err)
	}
	// A later day the TA wrote but never sent, then the period closed.
	f.exec(`INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES ($1, $2, '09:00', '10:00', 1, 'review', 'draft')`, f.AssignmentID, day(12))
	f.exec(`UPDATE submission_periods SET is_closed = TRUE WHERE id = $1`, pid)

	if err := f.Periods.MarkStaffReviewed(f.ctx, f.StaffID, pid, f.TAID, f.CourseID, ""); err != nil {
		t.Fatalf("a forfeited draft must not block the staff sign-off: %v", err)
	}
	n, err := f.Periods.MarkCourseExported(f.ctx, f.StaffID, f.CourseID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("locked %d cells, want 1 — the forfeited draft must not hold the month open", n)
	}
}

// While the period is still open, an unsent draft is work in progress and
// still holds the sign-off, as before.
func TestOpenPeriodDraft_StillBlocksSignOff(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	pid := f.addSubmissionPeriod(currentMonthMM(), "2026-12-31", "", false)
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, "", false); err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES ($1, $2, '09:00', '10:00', 1, 'review', 'draft')`, f.AssignmentID, day(12))

	if err := f.Periods.MarkStaffReviewed(f.ctx, f.StaffID, pid, f.TAID, f.CourseID, ""); err == nil {
		t.Fatal("an unsent draft in an OPEN period must still hold the sign-off")
	}
}
