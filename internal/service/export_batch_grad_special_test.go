package service

import (
	"testing"

	"ta-payment-back/internal/audit"
)

// Grad-special (master/phd, track=special) no longer logs work_logs at all —
// pay is computed automatically from the regular track's class schedule. A
// work_logs row left over from before that change can never be reviewed
// again (ListReviewQueue excludes grad-special, so staff have no screen to
// clear it on), so it must not count as "unreviewed" — that would hold the
// course's ReviewComplete/ExportEligible flags false forever.
func TestExportBatchDashboard_ExcludesGradSpecialLeftoverRows(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master", Track: "special"})
	f.addAppointmentOrder()
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1`, f.AssignmentID)
	f.addSubmissionPeriod(currentMonthMM(), "2026-12-31", "", false)

	svc := &ExportBatchService{pool: f.Pool, aud: audit.New(f.Pool)}
	budget := &BudgetService{pool: f.Pool}
	export := exportSvcFor(f)

	out, err := svc.DashboardSummary(f.ctx, budget, export, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range out.Courses {
		if c.TeachingCourseID == f.CourseID && len(c.UnreviewedMonths) != 0 {
			t.Fatalf("grad-special leftover approved row counted as unreviewed: %+v", c.UnreviewedMonths)
		}
	}
}
