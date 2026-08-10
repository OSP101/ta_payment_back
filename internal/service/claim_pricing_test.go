package service

import (
	"math"
	"testing"

	"github.com/google/uuid"
)

// One pricing source (claimCostByTAMonth) feeds the pay column, the settlement,
// and the dropped-month arithmetic. These tests pin its two special-side rules:
// the B2 overlap comes off, and the ป.ตรี-พิเศษ monthly cap holds.

func claimCosts(t *testing.T, f *fixture) map[string]float64 {
	t.Helper()
	svc := exportSvcFor(f)
	pr, err := (&CourseService{pool: f.Pool}).LatestPayRate(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	costs, err := svc.claimCostByTASlot(f.ctx, f.CourseID, *pr, mergedSittingsCTE)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{} // "ym/track" → baht
	for _, c := range costs {
		out[c.YearMonth+"/"+c.Track] += c.Baht
	}
	return out
}

func insertLog(f *fixture, assignID uuid.UUID, date, from, to string, hrs float64) {
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES (gen_random_uuid(), $1, $2::date, $3, $4, $5, 'lecture', 'approved')`,
		assignID, date, from, to, hrs)
}

func TestClaimCost_OverlapHoursAreNotBilledOnTheSpecialSide(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	special := f.siblingAssignment("special", nil)

	// The same two clock hours on both tracks, plus one special-only hour.
	insertLog(f, f.AssignmentID, day(10), "09:00", "11:00", 2)
	insertLog(f, special, day(10), "09:00", "11:00", 2)
	insertLog(f, special, day(10), "13:00", "14:00", 1)

	pr, _ := (&CourseService{pool: f.Pool}).LatestPayRate(f.ctx)
	got := claimCosts(t, f)
	ym := day(10)[:7]
	if want := 2 * pr.UndergradRegular; math.Abs(got[ym+"/regular"]-want) > 0.01 {
		t.Errorf("regular = %.2f, want %.2f", got[ym+"/regular"], want)
	}
	// Only the 13:00 hour is billable on the special side.
	if want := 1 * pr.UndergradSpecial; math.Abs(got[ym+"/special"]-want) > 0.01 {
		t.Errorf("special = %.2f, want %.2f — the co-taught 09:00-11:00 must not be billed twice", got[ym+"/special"], want)
	}
}

func TestClaimCost_MonthlyCapHoldsOnTheSpecialSide(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	special := f.siblingAssignment("special", nil)

	pr, _ := (&CourseService{pool: f.Pool}).LatestPayRate(f.ctx)
	if pr.UGSpecialMonthlyCap <= 0 {
		t.Skip("no monthly cap configured in fixture pay rates")
	}
	// Enough special-only hours in one month to sail past the cap.
	need := int(pr.UGSpecialMonthlyCap/pr.UndergradSpecial) + 5
	for d, left := 1, need; left > 0; d, left = d+1, left-6 {
		insertLog(f, special, day(d), "09:00", "15:00", 6)
	}

	got := claimCosts(t, f)
	ym := day(1)[:7]
	if math.Abs(got[ym+"/special"]-pr.UGSpecialMonthlyCap) > 0.01 {
		t.Errorf("special = %.2f, want capped at %.2f", got[ym+"/special"], pr.UGSpecialMonthlyCap)
	}
}

// The settlement must see the same numbers — a fully co-taught special section
// costs the special pool NOTHING, so it can never trigger a phantom cutoff.
func TestSettleCourse_FullyCoTaughtSpecialAddsNoSpecialCost(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	special := f.siblingAssignment("special", nil)
	insertLog(f, f.AssignmentID, day(10), "09:00", "12:00", 3)
	insertLog(f, special, day(10), "09:00", "12:00", 3)

	out, err := exportSvcFor(f).SettleCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Special.Months) != 0 {
		t.Errorf("special months = %+v, want none — the co-taught hours belong to the regular side only", out.Special.Months)
	}
	if out.OverBudget {
		t.Error("a fully co-taught special section must not push the course over budget")
	}
}

// The printed workbook must agree: the special sheet loses the co-taught
// minutes (they are already on the regular sheet), keeping any remainder.
func TestCollectCombinedBook_SpecialSheetExcludesCoTaughtTime(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	special := f.siblingAssignment("special", nil)
	insertLog(f, f.AssignmentID, day(10), "09:00", "11:00", 2)
	insertLog(f, special, day(10), "09:00", "13:00", 4) // 2 co-taught + 2 own

	d, err := exportSvcFor(f).collectCombinedBook(f.ctx, f.CourseID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Special) != 1 {
		t.Fatalf("special claimants = %d, want 1", len(d.Special))
	}
	var total float64
	for _, r := range d.Special[0].Rows {
		total += claimHours(r.Range)
		if r.Range == "09.00 - 13.00" || r.Range == "09.00 - 11.00" {
			t.Errorf("special sheet still prints co-taught time: %q", r.Range)
		}
	}
	if math.Abs(total-2) > 0.001 {
		t.Errorf("special sheet bills %.1f hrs, want 2 (only 11:00-13:00 is special-only)", total)
	}
}

func TestSubtractIntervals(t *testing.T) {
	cases := []struct {
		iv   [2]int
		cuts [][2]int
		want [][2]int
	}{
		{[2]int{540, 660}, nil, [][2]int{{540, 660}}},                              // untouched
		{[2]int{540, 660}, [][2]int{{540, 660}}, nil},                              // fully covered
		{[2]int{540, 780}, [][2]int{{540, 660}}, [][2]int{{660, 780}}},             // head clipped
		{[2]int{540, 780}, [][2]int{{600, 660}}, [][2]int{{540, 600}, {660, 780}}}, // split
		{[2]int{540, 660}, [][2]int{{500, 560}, {600, 700}}, [][2]int{{560, 600}}}, // both ends
		{[2]int{540, 660}, [][2]int{{700, 800}}, [][2]int{{540, 660}}},             // disjoint cut
	}
	for i, c := range cases {
		got := subtractIntervals(c.iv, c.cuts)
		if len(got) != len(c.want) {
			t.Errorf("case %d: got %v, want %v", i, got, c.want)
			continue
		}
		for j := range got {
			if got[j] != c.want[j] {
				t.Errorf("case %d: got %v, want %v", i, got, c.want)
			}
		}
	}
}

// squeezeToSlots prices an hour so the course's regular pool buys only part of
// the fixture's work, forcing a คาบ-level cutoff.
func squeezeToSlots(t *testing.T, f *fixture, affordableHours float64) {
	t.Helper()
	snap, err := (&BudgetService{pool: f.Pool}).Compute(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.TermPayRegular <= 0 {
		t.Fatalf("fixture course has no regular budget to squeeze (%.2f)", snap.TermPayRegular)
	}
	f.exec(`UPDATE pay_rates SET undergrad_regular = $1`, snap.TermPayRegular/affordableHours)
}

// The คาบ cutoff must land in the middle of a month, not on its boundary — the
// whole reason the rule changed.
func TestSettleCourse_CutsInsideAMonthNotAtItsEdge(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	// Four 2-hour คาบ in one month; the pool affords three of them.
	for d := 1; d <= 4; d++ {
		insertLog(f, f.AssignmentID, day(d), "09:00", "11:00", 2)
	}
	squeezeToSlots(t, f, 6)

	out, err := exportSvcFor(f).SettleCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if !out.OverBudget {
		t.Fatal("precondition: the course must be over budget")
	}
	if len(out.Regular.Slots) != 4 {
		t.Fatalf("slots = %d, want 4 the ledger must be per คาบ", len(out.Regular.Slots))
	}
	paid := 0
	for _, sl := range out.Regular.Slots {
		if sl.Paid {
			paid++
		}
	}
	if paid != 3 {
		t.Errorf("paid คาบ = %d, want 3 the budget affords three of the four", paid)
	}
	// The month is part-paid, so it is NOT in unpaid_months: telling the TA they
	// get nothing for a month they are partly paid for would be a lie.
	if len(out.UnpaidMonths) != 0 {
		t.Errorf("unpaid_months = %v, want none — the month is partly paid", out.UnpaidMonths)
	}
	if len(out.PartialMonths) != 1 {
		t.Errorf("partial_months = %v, want the one month", out.PartialMonths)
	}
}

// The sheet prints EVERY คาบ taught (office instruction, ส.ค. 2569 — "เขียน
// เวลามาให้ครบที่สอนจริง"); the budget cutoff decides only the funded figure
// that ขอเบิกจ่ายเพียง carries, and THAT must equal what the payout transfers.
func TestCombinedBook_PrintsAllHoursAndFundsOnlyWhatTheBudgetReached(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	for d := 1; d <= 4; d++ {
		insertLog(f, f.AssignmentID, day(d), "09:00", "11:00", 2)
	}
	squeezeToSlots(t, f, 6)
	svc := exportSvcFor(f)

	d, err := svc.collectCombinedBook(f.ctx, f.CourseID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Regular) != 1 {
		t.Fatalf("regular claimants = %d, want 1", len(d.Regular))
	}
	var hrs float64
	for _, r := range d.Regular[0].Rows {
		hrs += claimHours(r.Range)
	}
	if hrs != 8 {
		t.Errorf("the sheet prints %.1f hrs, want all 8 taught — the budget must not"+
			" remove rows from the record of what was taught", hrs)
	}

	// The funded figure is cut at the settlement's คาบ (6 of 8 hours afford)…
	pr, _ := (&CourseService{pool: f.Pool}).LatestPayRate(f.ctx)
	if want := 6 * pr.UndergradRegular; math.Abs(d.Regular[0].PaidBaht-want) > 0.01 {
		t.Errorf("PaidBaht = %.2f, want %.2f ขอเบิกจ่ายเพียง must stop where the budget did",
			d.Regular[0].PaidBaht, want)
	}
	if want := 8 * pr.UndergradRegular; math.Abs(d.Regular[0].FullBaht-want) > 0.01 {
		t.Errorf("FullBaht = %.2f, want %.2f (all 8 hours)", d.Regular[0].FullBaht, want)
	}
	if !d.Regular[0].underfunded() {
		t.Error("claimant must read as underfunded that is what makes ขอเบิกจ่ายเพียง print")
	}
	// …and must equal what the payout actually transfers.
	comp, err := svc.buildExportRows(f.ctx, f.CourseID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(comp.records[0].actualPaid-d.Regular[0].PaidBaht) > 0.01 {
		t.Errorf("actual_paid = %.2f but ขอเบิกจ่ายเพียง prints %.2f document and money disagree",
			comp.records[0].actualPaid, d.Regular[0].PaidBaht)
	}
}
