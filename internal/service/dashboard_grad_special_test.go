package service

import "testing"

// Same leftover-row hazard as the export gate: a grad-special (master/phd,
// track=special) assignment stopped logging work_logs, so a 'submitted' row
// left over from before that change can never move again. Counting it in
// PendingPayoutReviews would inflate the badge with work the review queue
// itself excludes — an officer clicking it would land on a queue with
// nothing matching.
func TestDashboardExecutive_ExcludesGradSpecialLeftoverRows(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master", Track: "special"})
	f.addAppointmentOrder()
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1`, f.AssignmentID)
	f.addSubmissionPeriod(currentMonthMM(), "2026-12-31", "", false)

	svc := &DashboardService{pool: f.Pool}
	budget := &BudgetService{pool: f.Pool}
	appts := &AppointmentOrderService{pool: f.Pool}

	sum, err := svc.Executive(f.ctx, &f.TermID, budget, appts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.PendingPayoutReviews != 0 {
		t.Errorf("pending_payout_reviews = %d, want 0 — grad-special leftover rows must not count",
			sum.PendingPayoutReviews)
	}
	if sum.PayoutCoursesActionable != 0 {
		t.Errorf("payout_courses_actionable = %d, want 0 — same leftover row must not make the course look actionable",
			sum.PayoutCoursesActionable)
	}
}
