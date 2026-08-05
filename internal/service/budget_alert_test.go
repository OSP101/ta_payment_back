package service

import (
	"testing"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
)

// The warning has to arrive once, not on every recomputation — the shortfall is
// recalculated on each approval, and a stateless "notify if over budget" would
// re-send it every time. It also has to arrive AGAIN when the answer changes,
// because "October too" is news.

func exportSvcWithNotify(f *fixture) *ExportService {
	notify := &NotifyService{pool: f.Pool, mailer: mail.New(config.Config{})}
	return &ExportService{
		pool: f.Pool, aud: audit.New(f.Pool),
		budget: &BudgetService{pool: f.Pool}, notify: notify,
	}
}

func budgetNoticeCount(t *testing.T, f *fixture) int {
	t.Helper()
	var n int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM notifications WHERE title LIKE 'งบไม่พอ%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNotifyBudgetShortfall_SendsOnceThenStaysQuiet(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1`, f.AssignmentID)
	f.exec(`UPDATE pay_rates SET undergrad_regular = 999999`) // far over any budget

	svc := exportSvcWithNotify(f)
	svc.NotifyBudgetShortfall(f.ctx, f.CourseID)
	first := budgetNoticeCount(t, f)
	if first == 0 {
		t.Fatal("no warning was sent for a course that cannot pay its months")
	}

	// Same shortfall, recomputed: nothing new to say.
	svc.NotifyBudgetShortfall(f.ctx, f.CourseID)
	if got := budgetNoticeCount(t, f); got != first {
		t.Errorf("notices = %d after a second run, want %d — the shortfall had not "+
			"changed, so nobody should have been told again", got, first)
	}
}

// Both the lecturer and the TA are told: it decides the TA's pay and the
// lecturer is the one who can stop approving into it.
func TestNotifyBudgetShortfall_ReachesLecturerAndTA(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1`, f.AssignmentID)
	f.exec(`UPDATE pay_rates SET undergrad_regular = 999999`)

	exportSvcWithNotify(f).NotifyBudgetShortfall(f.ctx, f.CourseID)

	for who, id := range map[string]interface{ String() string }{
		"lecturer": f.LecturerID, "TA": f.TAID,
	} {
		var n int
		if err := f.Pool.QueryRow(f.ctx,
			`SELECT COUNT(*) FROM notifications WHERE user_id=$1 AND title LIKE 'งบไม่พอ%'`,
			id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Errorf("the %s was not told", who)
		}
	}
}

// A course that fits its budget says nothing at all.
func TestNotifyBudgetShortfall_SilentWhenWithinBudget(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1`, f.AssignmentID)

	exportSvcWithNotify(f).NotifyBudgetShortfall(f.ctx, f.CourseID)
	if got := budgetNoticeCount(t, f); got != 0 {
		t.Errorf("sent %d warnings for a course that fits its budget", got)
	}
}

// The forecast is what the warning reads, so a shortfall that exists only in
// unapproved work still warns — which is the whole point of warning early.
func TestForecastCourse_SeesTroubleBeforeApprovalDoes(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	// Nothing approved: everything is still a draft.
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.exec(`UPDATE pay_rates SET undergrad_regular = 999999`)

	svc := exportSvcFor(f)
	committed, err := svc.SettleCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if committed.OverBudget {
		t.Error("the settled figure must see nothing — no hours are approved yet")
	}

	forecast, err := svc.ForecastCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if !forecast.OverBudget {
		t.Error("the forecast must see the drafts — warning only once the money has " +
			"already been committed is warning too late")
	}
}

// A rejected row is not work anybody plans to pay for, so it must not inflate
// the forecast into a false alarm.
func TestForecastCourse_IgnoresRejectedRows(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='rejected' WHERE assignment_id=$1`, f.AssignmentID)
	f.exec(`UPDATE pay_rates SET undergrad_regular = 999999`)

	forecast, err := exportSvcFor(f).ForecastCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if forecast.OverBudget {
		t.Error("rejected hours counted towards the forecast — the TA would be warned " +
			"about money nobody intends to spend")
	}
}
