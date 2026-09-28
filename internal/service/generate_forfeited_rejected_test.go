package service

import (
	"context"
	"testing"
)

// A lecturer bounced the TA's month, the TA did not fix it before the period
// closed, and the rows became forfeited: the TA can no longer edit, resend or
// delete them. Generate used to count every rejected row as "in review" and
// refuse, so that TA could never auto-generate the rest of the term again —
// and nothing on screen could clear the block.
func TestGenerate_IgnoresRejectedRowsInClosedMonth(t *testing.T) {
	f := scheduleFixture(t)
	f.addOwnClassInTerm(3, "08:00", "09:00")
	ctx := context.Background()

	pid := f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "", false)
	f.exec(`INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity, status, reject_reason)
	        VALUES ($1, $2, '09:00', '10:00', 1, 'review', 'rejected', 'ไม่ถูกต้อง')`,
		f.AssignmentID, day(12))

	// Still inside an open period the TA can fix it, so regenerating stays
	// refused — it would mint a fresh draft beside the rejected slot.
	if _, err := f.Svc.Generate(ctx, f.TAID, f.AssignmentID); err == nil {
		t.Fatal("a rejected row in an OPEN month must still block Generate")
	}

	f.exec(`UPDATE submission_periods SET is_closed = TRUE WHERE id = $1`, pid)
	if _, err := f.Svc.Generate(ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("a forfeited rejected row (period closed) must not block Generate: %v", err)
	}

	// Nothing may be generated into the closed month, and the forfeited row stays.
	var inClosed, rejected int
	if err := f.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FILTER (WHERE status = 'draft'), COUNT(*) FILTER (WHERE status = 'rejected')
		   FROM work_logs WHERE assignment_id = $1 AND to_char(work_date,'MM') = $2`,
		f.AssignmentID, currentMonthMM()).Scan(&inClosed, &rejected); err != nil {
		t.Fatal(err)
	}
	if inClosed != 0 {
		t.Errorf("Generate wrote %d drafts into the closed month", inClosed)
	}
	if rejected != 1 {
		t.Errorf("rejected rows in the closed month = %d, want the 1 forfeited row kept", rejected)
	}
}

// The ฿300/day cap is per TA per day across every course (rule 6a). Generate
// used to count only the rows it was writing, so a TA already carrying a day in
// another course got a generated row that pushed it over — and the lecturer's
// approval then failed on a day their own course priced well under the cap.
func TestGenerate_DailyBahtCountsOtherCourses(t *testing.T) {
	f := newFixture(t, fixtureOpts{Rates: rateOverrides{UndergradRegular: 40, DailyPayCapBaht: 300}, Workload: workloadHours{CheckWork: 2}})
	f.addDutySlot(DutyReview, 3, "17:00", "18:00") // Wednesday evening

	rows, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var target string
	for _, r := range rows.Entries {
		if r.Activity == "review" {
			target = r.WorkDate
			break
		}
	}
	if target == "" {
		t.Fatal("fixture generated no review row to collide with")
	}

	// Another course already fills that day to 280฿ (7 ชม. × 40).
	other := f.secondCourseAssignment(fixtureOpts{})
	f.exec(`INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES ($1, $2, '08:00', '15:00', 7, 'review', 'approved')`, other, target)

	if _, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	var n int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM work_logs WHERE assignment_id = $1 AND work_date = $2`,
		f.AssignmentID, target).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("Generate wrote %d row(s) on %s, where another course already holds 280฿ of a 300฿ day", n, target)
	}
}
