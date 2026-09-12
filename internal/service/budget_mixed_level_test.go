package service

import (
	"math"
	"testing"

	"github.com/google/uuid"
)

// The share is of MONEY owed, priced by the real rate table: a graduate TA's
// hour costs more than an undergraduate's, so for the same hours the graduate
// is owed more and — when the pool is short — is paid more, by exactly the
// ratio of the two rates. Both are cut by the same percentage.
//
// End to end through SettleCourse and the payout rows, not settleTrack alone,
// so the pricing (claimCostByTASlot) and the split are checked together.

// fourHoursEach gives the fixture's own TA and a second undergraduate on the
// same course four approved hours apiece. secondTAOnSameCourse already logs
// two of the second person's hours.
func fourHoursEach(f *fixture) uuid.UUID {
	insertLog(f, f.AssignmentID, day(10), "09:00", "11:00", 2)
	insertLog(f, f.AssignmentID, day(12), "09:00", "11:00", 2)
	second := f.secondTAOnSameCourse()
	var assign uuid.UUID
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT id FROM ta_request_assignments WHERE ta_id = $1`, second).Scan(&assign); err != nil {
		f.t.Fatal(err)
	}
	insertLog(f, assign, day(13), "09:00", "11:00", 2)
	return second
}

// paidByTA reads what the payout actually transfers to each person.
func paidByTA(t *testing.T, f *fixture) (map[uuid.UUID]float64, map[uuid.UUID]float64) {
	t.Helper()
	comp, err := exportSvcFor(f).buildExportRows(f.ctx, f.CourseID, nil)
	if err != nil {
		t.Fatal(err)
	}
	paid, earned := map[uuid.UUID]float64{}, map[uuid.UUID]float64{}
	for _, r := range comp.records {
		paid[r.taID] += r.actualPaid
		earned[r.taID] += r.payBaht
	}
	return paid, earned
}

// Two undergraduates, same hours, same rate: the same money, and the pool spent
// to the baht.
func TestSettleCourse_TwoUndergradsWithTheSameHoursArePaidTheSame(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	second := fourHoursEach(f)
	// Price the hour so 8 hours cost twice the pool: everybody loses half.
	snap, err := (&BudgetService{pool: f.Pool}).Compute(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	cap := snap.TermPayRegular
	f.exec(`UPDATE pay_rates SET undergrad_regular = $1`, 2*cap/8)

	paid, earned := paidByTA(t, f)
	if earned[f.TAID] != earned[second] {
		t.Fatalf("earned %.2f vs %.2f — the case must start with equal claims", earned[f.TAID], earned[second])
	}
	want := math.Floor(cap / 2)
	if paid[f.TAID] != want || paid[second] != want {
		t.Errorf("paid %.2f and %.2f, want %.2f each (half the pool, whole baht)",
			paid[f.TAID], paid[second], want)
	}
	if total := paid[f.TAID] + paid[second]; total < cap-2 || total > cap+0.01 {
		t.Errorf("paid %.2f of a %.2f pool — must be spent to within the rounding", total, cap)
	}
}

// A graduate and an undergraduate, same hours: the graduate is owed more
// (higher hourly rate) and is paid more by that same ratio; both lose the same
// percentage.
func TestSettleCourse_GraduateAndUndergradAreCutByTheSamePercentage(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master"})
	ug := fourHoursEach(f) // the second TA is always an undergraduate
	snap, err := (&BudgetService{pool: f.Pool}).Compute(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	cap := snap.TermPayRegular
	// Graduate rate = 2 × undergraduate rate; 4 h each → owed 8r + 4r = 12r.
	// Choose r so the work costs twice the pool: 12r = 2·cap.
	r := 2 * cap / 12
	f.exec(`UPDATE pay_rates SET undergrad_regular = $1, graduate_regular_hourly = $2`, r, 2*r)

	paid, earned := paidByTA(t, f)
	if math.Abs(earned[f.TAID]-2*earned[ug]) > 0.01 {
		t.Fatalf("graduate earned %.2f, undergrad %.2f — the case must start 2:1", earned[f.TAID], earned[ug])
	}
	wantGrad, wantUG := math.Floor(cap*2/3), math.Floor(cap/3)
	t.Logf("pool %.2f: graduate owed %.2f → paid %.2f; undergrad owed %.2f → paid %.2f",
		cap, earned[f.TAID], paid[f.TAID], earned[ug], paid[ug])
	if paid[f.TAID] != wantGrad || paid[ug] != wantUG {
		t.Errorf("paid graduate %.2f, undergrad %.2f, want %.2f and %.2f — "+
			"the graduate's share is bigger by exactly the rate ratio",
			paid[f.TAID], paid[ug], wantGrad, wantUG)
	}
	// Same percentage cut for both (to within the whole-baht rounding).
	fracGrad, fracUG := paid[f.TAID]/earned[f.TAID], paid[ug]/earned[ug]
	if math.Abs(fracGrad-fracUG) > 1/earned[ug] {
		t.Errorf("graduate kept %.1f%%, undergrad kept %.1f%% — everyone is short by the same proportion",
			100*fracGrad, 100*fracUG)
	}
	if total := paid[f.TAID] + paid[ug]; total < cap-2 || total > cap+0.01 {
		t.Errorf("paid %.2f of a %.2f pool", total, cap)
	}
}

// Two graduates, same hours: a level's rate is not a tie-break between people —
// equal work at equal rate is equal money at any level.
func TestSettleCourse_TwoGraduatesWithTheSameHoursArePaidTheSame(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master"})
	second := fourHoursEach(f)
	f.exec(`UPDATE ta_request_assignments SET level = 'master' WHERE ta_id = $1`, second)
	snap, err := (&BudgetService{pool: f.Pool}).Compute(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	cap := snap.TermPayRegular
	f.exec(`UPDATE pay_rates SET graduate_regular_hourly = $1`, 2*cap/8)

	paid, earned := paidByTA(t, f)
	if earned[f.TAID] != earned[second] {
		t.Fatalf("earned %.2f vs %.2f — the case must start with equal claims", earned[f.TAID], earned[second])
	}
	want := math.Floor(cap / 2)
	if paid[f.TAID] != want || paid[second] != want {
		t.Errorf("paid %.2f and %.2f, want %.2f each", paid[f.TAID], paid[second], want)
	}
}

// The per-person rows the lecturer's screen reads must be the same money the
// payout transfers, month by month, with the person's level on the row.
func TestSettleCourse_PeopleRowsMatchThePayoutPerPersonAndMonth(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master"})
	ug := fourHoursEach(f)
	snap, err := (&BudgetService{pool: f.Pool}).Compute(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	r := 2 * snap.TermPayRegular / 12
	f.exec(`UPDATE pay_rates SET undergrad_regular = $1, graduate_regular_hourly = $2`, r, 2*r)

	st, err := exportSvcFor(f).SettleCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	paid, earned := paidByTA(t, f)
	if len(st.Regular.People) != 2 {
		t.Fatalf("people = %d, want 2", len(st.Regular.People))
	}
	for _, p := range st.Regular.People {
		if p.Name == "" {
			t.Errorf("person %s has no name", p.TAID)
		}
		wantLevel := "undergrad"
		if p.TAID == f.TAID {
			wantLevel = "master"
		}
		if p.Level != wantLevel {
			t.Errorf("person %s level = %q, want %q", p.TAID, p.Level, wantLevel)
		}
		if math.Abs(p.PaidBaht-paid[p.TAID]) > 0.01 || math.Abs(p.Baht-earned[p.TAID]) > 0.01 {
			t.Errorf("person %s row paid %.2f/earned %.2f, payout paid %.2f/earned %.2f",
				p.TAID, p.PaidBaht, p.Baht, paid[p.TAID], earned[p.TAID])
		}
		var sum float64
		for _, m := range p.Months {
			sum += m.PaidBaht
		}
		if math.Abs(sum-p.PaidBaht) > 0.01 {
			t.Errorf("person %s months sum to %.2f, row says %.2f", p.TAID, sum, p.PaidBaht)
		}
	}
	_ = ug
}
