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

// A short pool funds the month PARTLY — every คาบ at the same proportion — and
// spends the pool to the baht rather than stopping at a คาบ boundary.
func TestSettleCourse_FundsAShortMonthProportionally(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	// Four 2-hour คาบ in one month; the pool affords six of the eight hours.
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
	for _, sl := range out.Regular.Slots {
		if share := out.Regular.fundedShare(sl.TA, sl.Date, sl.StartTime); math.Abs(share-0.75) > 0.01 {
			t.Errorf("คาบ %s funded %.3f, want 0.75 — no คาบ is singled out", sl.Date, share)
		}
	}
	if want := math.Floor(out.Regular.Cap); out.Regular.PaidBaht != want {
		t.Errorf("paid %.2f, want %.2f — the pool spent to the baht", out.Regular.PaidBaht, want)
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

	// The funded figure is the whole pool (6 of the 8 hours' worth), rounded
	// down to a whole baht…
	pr, _ := (&CourseService{pool: f.Pool}).LatestPayRate(f.ctx)
	if want := math.Floor(6 * pr.UndergradRegular); math.Abs(d.Regular[0].PaidBaht-want) > 0.01 {
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

// The overlap comes off the special คาบ it actually falls in, not smeared
// across the month. SC362005 (11/09/2026): lecture and lab taught to sec 1-2
// (ปกติ) and sec 3 (พิเศษ) at the same clock time, plus special-only review
// hours — the co-taught คาบ must be worth 0 on the special side and the review
// คาบ its full rate, in whole baht, so the screen and the printed special
// sheet (clipSpecialOverlap) name the same คาบ with the same money.
func TestClaimCost_OverlapIsClippedFromTheSpecialSlotItFallsIn(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	special := f.siblingAssignment("special", nil)
	// A third section (ปกติ sec 03) on the same request, so both regular
	// sections and the special one meet at once — the SC362005 shape.
	sec2ID, sec2 := uuid.New(), uuid.New()
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track)
	        VALUES ($1, $2, '03', 'regular')`, sec2ID, f.CourseID)
	f.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
	        VALUES ($1, $2, $3, $4, 'undergrad')`, sec2, f.RequestID, sec2ID, f.TAID)

	// Lecture 11:30-12:30 and lab 13:00-15:00 on all three sections at once.
	for _, a := range []uuid.UUID{f.AssignmentID, sec2, special} {
		insertLog(f, a, day(10), "11:30", "12:30", 1)
		insertLog(f, a, day(10), "13:00", "15:00", 2)
	}
	// Review on the special section only, 10:00-12:00 — overlaps the lecture
	// for half an hour, so 1.5 h of it is special-only.
	insertLog(f, special, day(12), "10:00", "12:00", 2)
	insertLog(f, special, day(12), "10:30", "11:30", 1) // same-day lecture on sec 1 covers 10:30-11:30
	insertLog(f, f.AssignmentID, day(12), "10:30", "11:30", 1)

	svc := exportSvcFor(f)
	pr, _ := (&CourseService{pool: f.Pool}).LatestPayRate(f.ctx)
	costs, err := svc.claimCostByTASlot(f.ctx, f.CourseID, *pr, mergedSittingsCTE)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{} // "date st/track" → baht
	for _, c := range costs {
		got[c.Date+" "+c.StartTime+"/"+c.Track] += c.Baht
	}
	want := map[string]float64{
		day(10) + " 11:30/regular": 1 * pr.UndergradRegular, // sec 1+2 merged: once
		day(10) + " 13:00/regular": 2 * pr.UndergradRegular,
		day(12) + " 10:30/regular": 1 * pr.UndergradRegular,
		day(10) + " 11:30/special": 0, // fully co-taught
		day(10) + " 13:00/special": 0,
		day(12) + " 10:00/special": 1 * pr.UndergradSpecial, // 10:00-12:00 minus 10:30-11:30
	}
	for k, w := range want {
		if math.Abs(got[k]-w) > 0.001 {
			t.Errorf("%s = %.2f, want %.2f", k, got[k], w)
		}
	}
	for k, v := range got {
		if _, ok := want[k]; !ok && v != 0 {
			t.Errorf("unexpected priced คาบ %s = %.2f", k, v)
		}
	}
}

// ปกติกับพิเศษที่ไม่ทับซ้อนกันเบิกแยกกันได้ทั้งคู่ — each on its own pool,
// nothing borrowed, nothing dropped (11/09/2026).
func TestSettleCourse_DisjointRegularAndSpecialAreBothBilled(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	special := f.siblingAssignment("special", nil)
	f.exec(`UPDATE teaching_courses SET num_students_special = 5 WHERE id = $1`, f.CourseID)
	insertLog(f, f.AssignmentID, day(10), "09:00", "11:00", 2)
	insertLog(f, special, day(10), "13:00", "15:00", 2)

	pr, _ := (&CourseService{pool: f.Pool}).LatestPayRate(f.ctx)
	out, err := exportSvcFor(f).SettleCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if want := 2 * pr.UndergradRegular; math.Abs(out.Regular.PaidBaht-want) > 0.01 {
		t.Errorf("regular paid %.2f, want %.2f", out.Regular.PaidBaht, want)
	}
	if want := 2 * pr.UndergradSpecial; math.Abs(out.Special.PaidBaht-want) > 0.01 {
		t.Errorf("special paid %.2f, want %.2f", out.Special.PaidBaht, want)
	}
	if out.SpilledBaht != 0 || out.OverBudget {
		t.Errorf("spilled %.2f over=%v, want nothing borrowed and nothing dropped", out.SpilledBaht, out.OverBudget)
	}
}

// ปกติสองกลุ่มไม่ทับกันเอง แต่พิเศษไปทับกลุ่มปกติกลุ่มหนึ่ง: the overlap is billed
// on the regular side first, and only once the regular pool is exhausted may
// that overlap money draw on the special pool's unused room.
func TestSettleCourse_SpecialOverlappingOneRegularSectionBillsRegularFirst(t *testing.T) {
	f := newFixture(t, fixtureOpts{NumStudents: 1}) // regular cap: 0.3 × 1 × 300 × 4 = ฿360
	special := f.siblingAssignment("special", nil)
	sec2ID, sec2 := uuid.New(), uuid.New()
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track)
	        VALUES ($1, $2, '03', 'regular')`, sec2ID, f.CourseID)
	f.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
	        VALUES ($1, $2, $3, $4, 'undergrad')`, sec2, f.RequestID, sec2ID, f.TAID)
	f.exec(`UPDATE teaching_courses SET num_students_special = 5 WHERE id = $1`, f.CourseID) // special cap ฿1,800

	// Five days: sec 1 09-11, sec 3 13-15 (regular, disjoint), special 09-11
	// on top of sec 1 only.
	for d := 1; d <= 5; d++ {
		insertLog(f, f.AssignmentID, day(d), "09:00", "11:00", 2)
		insertLog(f, sec2, day(d), "13:00", "15:00", 2)
		insertLog(f, special, day(d), "09:00", "11:00", 2)
	}

	pr, _ := (&CourseService{pool: f.Pool}).LatestPayRate(f.ctx)
	snap, err := exportSvcFor(f).budget.Compute(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exportSvcFor(f).SettleCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	regularCost := 20 * pr.UndergradRegular // 4 h × 5 days, all on the regular side
	overlapPay := 10 * pr.UndergradRegular  // the 09-11 hours only
	if regularCost <= snap.TermPayRegular {
		t.Fatalf("fixture must make the regular pool short: cost %.2f cap %.2f", regularCost, snap.TermPayRegular)
	}
	// Special side owes nothing of its own — every special hour was co-taught.
	if len(out.Special.Months) != 0 || out.Special.PaidBaht != 0 {
		t.Errorf("special months %+v paid %.2f, want none — co-taught hours are regular money", out.Special.Months, out.Special.PaidBaht)
	}
	wantSpill := math.Min(regularCost-snap.TermPayRegular, overlapPay)
	if math.Abs(out.SpilledBaht-wantSpill) > 0.01 {
		t.Errorf("spilled %.2f, want %.2f (only the overlap pay may borrow, only once regular is used up)", out.SpilledBaht, wantSpill)
	}
	if want := snap.TermPayRegular + wantSpill; math.Abs(out.Regular.PaidBaht-want) > 1 {
		t.Errorf("regular paid %.2f, want %.2f (whole regular cap + the borrowed overlap money)", out.Regular.PaidBaht, want)
	}
}

// Same shape, but the regular pool is NOT short: the overlap stays regular
// money and the special pool is never touched, even though it has room.
func TestSettleCourse_OverlapDoesNotBorrowWhileRegularStillFits(t *testing.T) {
	f := newFixture(t, fixtureOpts{}) // 40 students → regular cap ฿14,400
	special := f.siblingAssignment("special", nil)
	f.exec(`UPDATE teaching_courses SET num_students_special = 5 WHERE id = $1`, f.CourseID)
	for d := 1; d <= 5; d++ {
		insertLog(f, f.AssignmentID, day(d), "09:00", "11:00", 2)
		insertLog(f, special, day(d), "09:00", "11:00", 2)
	}
	pr, _ := (&CourseService{pool: f.Pool}).LatestPayRate(f.ctx)
	out, err := exportSvcFor(f).SettleCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if want := 10 * pr.UndergradRegular; math.Abs(out.Regular.PaidBaht-want) > 0.01 || out.SpilledBaht != 0 || out.Special.PaidBaht != 0 {
		t.Errorf("regular %.2f spilled %.2f special %.2f — want %.2f / 0 / 0", out.Regular.PaidBaht, out.SpilledBaht, out.Special.PaidBaht, want)
	}
}
