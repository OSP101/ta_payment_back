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
