package service

import (
	"math"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// Approving hours that put a course over its budget used to be refused
// outright, which left work that had already happened with nowhere to go. It is
// allowed now; the budget decides which MONTHS get paid, at export.

func exportSvcFor(f *fixture) *ExportService {
	return &ExportService{
		pool:   f.Pool,
		aud:    audit.New(f.Pool),
		budget: &BudgetService{pool: f.Pool},
	}
}

// squeezeBudget prices an hour so the course's whole budget buys just over one
// month of the fixture's work, forcing a cutoff.
func squeezeBudget(t *testing.T, f *fixture, monthsAffordable float64, hoursPerMonth float64) {
	t.Helper()
	snap, err := (&BudgetService{pool: f.Pool}).Compute(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.TermPayRegular <= 0 {
		t.Fatalf("fixture course has no regular budget to squeeze (%.2f)", snap.TermPayRegular)
	}
	// rate × hoursPerMonth × monthsAffordable = the whole regular pool
	f.exec(`UPDATE pay_rates SET undergrad_regular = $1`,
		snap.TermPayRegular/(hoursPerMonth*monthsAffordable))
}

func TestApprove_NoLongerRefusedWhenItWouldExceedTheBudget(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	// Make the course's budget far too small for even this one entry.
	f.exec(`UPDATE pay_rates SET undergrad_regular = 999999`)

	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, "", false); err != nil {
		t.Fatalf("approval must not be refused on budget any more: %v", err)
	}
	if got := f.worklogStatusOf(t, f.AssignmentID); got != "approved" {
		t.Errorf("status = %q, want approved", got)
	}
}

// The cutoff is by month and shared by the whole course, so the same hours are
// worth the same to everyone regardless of who submitted first.
func TestSettleCourse_CutsWholeMonthsNotPeople(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	// Two months of identical work: 2 hours in each.
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	next := monthStart().AddDate(0, 1, 9).Format("2006-01-02")
	f.mustUpsert(f.entry(next, "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1`, f.AssignmentID)

	// Budget buys ~1.4 months of the 2 h/month above → the second month falls off.
	squeezeBudget(t, f, 1.4, 2)

	got, err := exportSvcFor(f).SettleCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.OverBudget {
		t.Fatalf("expected the course to be over budget, got %+v", got.Regular)
	}
	if len(got.Regular.Months) != 2 {
		t.Fatalf("months = %d, want 2", len(got.Regular.Months))
	}
	if !got.Regular.Months[0].Paid || got.Regular.Months[1].Paid {
		t.Errorf("paid = [%v %v], want the first month only",
			got.Regular.Months[0].Paid, got.Regular.Months[1].Paid)
	}
	if len(got.UnpaidMonths) != 1 || got.UnpaidMonths[0] != got.Regular.Months[1].YearMonth {
		t.Errorf("unpaid months = %v, want just %q — this is the list the screens name "+
			"to the lecturer and the TA", got.UnpaidMonths, got.Regular.Months[1].YearMonth)
	}
}

// Under budget, nothing is dropped and nothing is scaled.
func TestSettleCourse_UnderBudgetChangesNothing(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1`, f.AssignmentID)

	got, err := exportSvcFor(f).SettleCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OverBudget || len(got.UnpaidMonths) != 0 {
		t.Errorf("settlement = %+v, want everything paid", got)
	}
}

// The money a TA actually receives drops by exactly the cut months' worth —
// not by a scaling factor, so it still reconciles against the claim form.
func TestBuildExportRows_DropsTheCutMonthsFromActualPaid(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	next := monthStart().AddDate(0, 1, 9).Format("2006-01-02")
	f.mustUpsert(f.entry(next, "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1`, f.AssignmentID)
	squeezeBudget(t, f, 1.4, 2)

	comp, err := exportSvcFor(f).buildExportRows(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(comp.records) != 1 {
		t.Fatalf("records = %d, want 1", len(comp.records))
	}
	r := comp.records[0]
	if r.actualPaid >= r.payBaht {
		t.Fatalf("actualPaid %.2f is not below payBaht %.2f — the cut month was still paid",
			r.actualPaid, r.payBaht)
	}
	// The exact invariant: what a TA loses is the cut months' own cost, not a
	// share of some scaled total. Compared against the settlement rather than
	// against "half", because the two months are priced independently and a
	// scaling factor would also land near half.
	settlement, err := exportSvcFor(f).SettleCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	lost := r.payBaht - r.actualPaid
	if math.Abs(lost-settlement.DroppedBaht) > 0.02 {
		t.Errorf("TA lost %.2f but the settlement dropped %.2f — the two must be the "+
			"same money", lost, settlement.DroppedBaht)
	}
}

var _ = uuid.Nil
