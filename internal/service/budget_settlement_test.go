package service

import (
	"fmt"
	"math"
	"testing"

	"github.com/google/uuid"
)

// The rule: คาบ in chronological order until the pool runs out. Everyone on the
// course shares one cutoff, so the outcome cannot depend on who submitted first
// — that was the whole point of choosing it over pro-rata.

// months builds one คาบ per month, so these cases still read as "whole months"
// — the behaviour a month-sized คาบ must keep.
func months(vals ...float64) []SlotSettlement {
	names := []string{"2026-06", "2026-07", "2026-08", "2026-09", "2026-10"}
	out := make([]SlotSettlement, 0, len(vals))
	for i, v := range vals {
		out = append(out, SlotSettlement{
			Date: names[i] + "-01", StartTime: "09:00", YearMonth: names[i], Baht: v,
		})
	}
	return out
}

// slots builds several คาบ inside ONE month, which is where the cutoff can now
// land part-way.
func slots(vals ...float64) []SlotSettlement {
	out := make([]SlotSettlement, 0, len(vals))
	for i, v := range vals {
		out = append(out, SlotSettlement{
			Date: fmt.Sprintf("2026-06-%02d", i+1), StartTime: "09:00",
			YearMonth: "2026-06", Baht: v,
		})
	}
	return out
}

func paidFlags(t TrackSettlement) []bool {
	out := make([]bool, len(t.Months))
	for i, m := range t.Months {
		out[i] = m.Paid
	}
	return out
}

func slotPaidFlags(t TrackSettlement) []bool {
	out := make([]bool, len(t.Slots))
	for i, sl := range t.Slots {
		out[i] = sl.Paid
	}
	return out
}

func TestSettleTrack_PaysWholeMonthsUntilTheMoneyRunsOut(t *testing.T) {
	// 5,000 + 5,000 + 5,000 = 15,000 fits; the fourth month does not.
	got := settleTrack("regular", 16000, 0, months(5000, 5000, 5000, 5000))

	want := []bool{true, true, true, false}
	for i, w := range want {
		if paidFlags(got)[i] != w {
			t.Fatalf("paid flags = %v, want %v", paidFlags(got), want)
		}
	}
	if got.CutoffMonth != "2026-09" {
		t.Errorf("cutoff = %q, want 2026-09", got.CutoffMonth)
	}
	if math.Abs(got.PaidBaht-15000) > .01 || math.Abs(got.DroppedBaht-5000) > .01 {
		t.Errorf("paid=%.2f dropped=%.2f, want 15000/5000", got.PaidBaht, got.DroppedBaht)
	}
}

// The month after the cutoff is NOT paid even when it would fit in the
// leftover. Paying November but not October is impossible to explain to
// somebody who worked both.
func TestSettleTrack_DoesNotSkipAheadToASmallerMonth(t *testing.T) {
	got := settleTrack("regular", 16000, 0, months(5000, 5000, 5000, 5000, 500))

	if got.Months[4].Paid {
		t.Error("the last month was paid out of order — the cutoff must stop everything after it")
	}
	if math.Abs(got.DroppedBaht-5500) > .01 {
		t.Errorf("dropped = %.2f, want 5500 (both months after the cutoff)", got.DroppedBaht)
	}
}

// Everything fits: no cutoff, nothing dropped, and the leftover is simply not
// claimed.
func TestSettleTrack_UnderBudgetPaysEverything(t *testing.T) {
	got := settleTrack("regular", 30000, 0, months(5000, 5000, 5000))
	if got.CutoffMonth != "" || got.DroppedBaht != 0 {
		t.Errorf("cutoff=%q dropped=%.2f, want none", got.CutoffMonth, got.DroppedBaht)
	}
	if math.Abs(got.PaidBaht-15000) > .01 {
		t.Errorf("paid = %.2f, want 15000", got.PaidBaht)
	}
}

// The graduate-special lump is a flat term figure that cannot be cut by month,
// so it comes off the top and the monthly cutoff works on what is left.
func TestSettleTrack_CommittedSpendComesOffTheTop(t *testing.T) {
	got := settleTrack("special", 16000, 12000, months(3000, 3000))

	if !got.Months[0].Paid || got.Months[1].Paid {
		t.Errorf("paid = %v, want only the first month — 12,000 is already committed",
			paidFlags(got))
	}
	if got.CutoffMonth != "2026-07" {
		t.Errorf("cutoff = %q, want 2026-07", got.CutoffMonth)
	}
}

// A cap of zero means the student count has not been entered, not that the
// course has no money. Zeroing everyone on that basis would be a silent wipe.
func TestSettleTrack_ZeroCapIsUnconfiguredNotBroke(t *testing.T) {
	got := settleTrack("regular", 0, 0, months(5000, 5000))
	if got.DroppedBaht != 0 || got.CutoffMonth != "" {
		t.Errorf("an unconfigured cap dropped %.2f at %q — it must pay everything and "+
			"leave the export's own student-count gate to refuse", got.DroppedBaht, got.CutoffMonth)
	}
}

// A month costing exactly the remaining budget fits. Off-by-one here would
// drop a month for a rounding cent.
func TestSettleTrack_AnExactFitIsPaid(t *testing.T) {
	got := settleTrack("regular", 10000, 0, months(6000, 4000))
	if !got.Months[1].Paid {
		t.Error("a month that costs exactly the remainder must be paid")
	}
}

// The concurrent-section spill (B2). Only pay for hours worked on both tracks
// at once may borrow, and only from what the special pool has not spent.

func TestSpillAllowance_RegularBorrowsExactlyItsShortfall(t *testing.T) {
	// Regular is 300 short; special has 1000 spare and 500 is spillable.
	if got := spillAllowance(1000, 2000, 0, 1300, 1000, 500); got != 300 {
		t.Errorf("spill = %v, want 300 (the shortfall is the binding limit)", got)
	}
}

func TestSpillAllowance_CannotBorrowMoreThanSpecialHasLeft(t *testing.T) {
	// Regular is 800 short but special has only 200 unspent.
	if got := spillAllowance(1000, 2000, 0, 1800, 1800, 5000); got != 200 {
		t.Errorf("spill = %v, want 200 (special's spare room is the limit)", got)
	}
}

func TestSpillAllowance_OnlyConcurrentSectionPayMayBorrow(t *testing.T) {
	// Plenty short and plenty spare, but only 50 baht of it is co-taught pay.
	if got := spillAllowance(1000, 2000, 0, 1800, 0, 50); got != 50 {
		t.Errorf("spill = %v, want 50 — ordinary regular work must not borrow", got)
	}
}

func TestSpillAllowance_NoSpillWhenRegularFits(t *testing.T) {
	if got := spillAllowance(1000, 2000, 0, 900, 0, 500); got != 0 {
		t.Errorf("spill = %v, want 0 — nothing to rescue", got)
	}
}

func TestSpillAllowance_APoolThatIsItselfShortLendsNothing(t *testing.T) {
	// Special is over its own cap, so its "unused" is negative, not lendable.
	if got := spillAllowance(1000, 2000, 0, 1500, 2600, 500); got != 0 {
		t.Errorf("spill = %v, want 0 — an over-budget pool has nothing to lend", got)
	}
}

func TestSpillAllowance_CommittedSpecialSpendIsNotLendable(t *testing.T) {
	// 2000 cap, 1800 already committed elsewhere, 100 of new special work:
	// only 100 is left to lend even though the regular side is 500 short.
	if got := spillAllowance(1000, 2000, 1800, 1500, 100, 500); got != 100 {
		t.Errorf("spill = %v, want 100 — committed spend is already gone", got)
	}
}

func TestSpillAllowance_UnsetCapMeansNothingToRescue(t *testing.T) {
	if got := spillAllowance(0, 2000, 0, 5000, 0, 500); got != 0 {
		t.Errorf("spill = %v, want 0 — an unlimited pool is never short", got)
	}
	if got := spillAllowance(1000, 0, 0, 5000, 0, 500); got != 0 {
		t.Errorf("spill = %v, want 0 — an unconfigured special pool lends nothing", got)
	}
}

// The spill exists to save a month from the cutoff; this is that end to end,
// through settleTrack rather than through the allowance alone.
func TestSettleTrack_ABorrowedBahtSavesTheLastMonth(t *testing.T) {
	months := []SlotSettlement{
		{Date: "2569-07-01", StartTime: "09:00", YearMonth: "2569-07", Baht: 600},
		{Date: "2569-08-01", StartTime: "09:00", YearMonth: "2569-08", Baht: 500},
	}
	if got := settleTrack("regular", 1000, 0, months); got.CutoffMonth != "2569-08" {
		t.Fatalf("without the spill the second month must be cut, got cutoff %q", got.CutoffMonth)
	}
	spill := spillAllowance(1000, 2000, 0, 1100, 0, 500)
	out := settleTrack("regular", 1000+spill, 0, months)
	if out.CutoffMonth != "" || out.DroppedBaht != 0 {
		t.Errorf("with %v borrowed both months must be paid, got cutoff %q dropped %v",
			spill, out.CutoffMonth, out.DroppedBaht)
	}
}

// A regular sitting can overlap SEVERAL special ones — a TA on two special
// sections that meet in the same room at the same hour. Summing each pair's
// intersection would charge that hour once per pair, inflating how much the
// regular pool is allowed to borrow.
func TestSpillableRegularBaht_CountsOverlappingHoursOnce(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := exportSvcFor(f)

	// One regular sitting, 09:00–11:00.
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES (gen_random_uuid(), $1, $2::date, '09:00', '11:00', 2, 'lecture', 'approved')`,
		f.AssignmentID, day(10))

	// Two special sections, both meeting at exactly the same hour.
	for _, secNo := range []string{"91", "92"} {
		sectionID := uuid.New()
		f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track)
		        VALUES ($1, $2, $3, 'special')`, sectionID, f.CourseID, secNo)
		assignID := uuid.New()
		f.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
		        VALUES ($1, $2, $3, $4, 'undergrad')`,
			assignID, f.RequestID, sectionID, f.TAID)
		f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
		        VALUES (gen_random_uuid(), $1, $2::date, '09:00', '11:00', 2, 'lecture', 'approved')`,
			assignID, day(10))
	}

	pr, err := (&CourseService{pool: f.Pool}).LatestPayRate(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.spillableRegularBaht(f.ctx, f.CourseID, *pr, mergedSittingsCTE)
	if err != nil {
		t.Fatal(err)
	}
	want := 2 * pr.UndergradRegular // two hours of clock time, not two pairs of it
	if math.Abs(got-want) > 0.01 {
		t.Errorf("spillable = %.2f, want %.2f — the same hour was counted per pair", got, want)
	}
}

// The merge must not swallow genuinely separate overlaps on the same day.
func TestSpillableRegularBaht_KeepsDisjointOverlapsSeparate(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := exportSvcFor(f)

	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES (gen_random_uuid(), $1, $2::date, '09:00', '11:00', 2, 'lecture', 'approved'),
	               (gen_random_uuid(), $1, $2::date, '13:00', '15:00', 2, 'lab', 'approved')`,
		f.AssignmentID, day(10))

	sectionID := uuid.New()
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track)
	        VALUES ($1, $2, '93', 'special')`, sectionID, f.CourseID)
	assignID := uuid.New()
	f.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
	        VALUES ($1, $2, $3, $4, 'undergrad')`, assignID, f.RequestID, sectionID, f.TAID)
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES (gen_random_uuid(), $1, $2::date, '09:00', '11:00', 2, 'lecture', 'approved'),
	               (gen_random_uuid(), $1, $2::date, '13:00', '15:00', 2, 'lab', 'approved')`,
		assignID, day(10))

	pr, err := (&CourseService{pool: f.Pool}).LatestPayRate(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.spillableRegularBaht(f.ctx, f.CourseID, *pr, mergedSittingsCTE)
	if err != nil {
		t.Fatal(err)
	}
	want := 4 * pr.UndergradRegular // morning and afternoon are two distinct blocks
	if math.Abs(got-want) > 0.01 {
		t.Errorf("spillable = %.2f, want %.2f — disjoint overlaps were merged", got, want)
	}
}

// The คาบ cutoff (04/08/2026). Whole months wasted up to a full month's cost;
// cutting at the คาบ leaves under one slot behind.

func TestSettleTrack_FillsPartOfAMonthInsteadOfDroppingItWhole(t *testing.T) {
	// One month of five 1,000฿ คาบ against a 3,000฿ pool.
	got := settleTrack("regular", 3000, 0, slots(1000, 1000, 1000, 1000, 1000))

	if got.PaidBaht != 3000 {
		t.Errorf("paid = %v, want the pool filled to 3000", got.PaidBaht)
	}
	want := []bool{true, true, true, false, false}
	for i, w := range want {
		if slotPaidFlags(got)[i] != w {
			t.Fatalf("slot paid = %v, want %v", slotPaidFlags(got), want)
		}
	}
	if len(got.Months) != 1 || got.Months[0].Paid {
		t.Fatalf("the month must read as part-paid, got %+v", got.Months)
	}
	if got.Months[0].PaidBaht != 3000 || got.Months[0].Baht != 5000 {
		t.Errorf("month = %+v, want 3000 of 5000", got.Months[0])
	}
}

// The whole point: the old rule dropped the month entire and left the pool
// mostly unspent.
func TestSettleTrack_WastesFarLessThanTheMonthRuleDid(t *testing.T) {
	// SC362102's real regular months against a 4,000 pool.
	byMonth := settleTrack("regular", 4000, 0, months(400, 800, 3600, 4160, 2000))
	if byMonth.PaidBaht != 1200 {
		t.Fatalf("month-sized คาบ still cut whole: paid = %v, want 1200", byMonth.PaidBaht)
	}

	// Same money, but August is five คาบ the cutoff can walk into.
	fine := settleTrack("regular", 4000, 0, []SlotSettlement{
		{Date: "2026-06-01", StartTime: "09:00", YearMonth: "2026-06", Baht: 400},
		{Date: "2026-07-01", StartTime: "09:00", YearMonth: "2026-07", Baht: 800},
		{Date: "2026-08-01", StartTime: "09:00", YearMonth: "2026-08", Baht: 900},
		{Date: "2026-08-08", StartTime: "09:00", YearMonth: "2026-08", Baht: 900},
		{Date: "2026-08-15", StartTime: "09:00", YearMonth: "2026-08", Baht: 900},
		{Date: "2026-08-22", StartTime: "09:00", YearMonth: "2026-08", Baht: 900},
	})
	// 400 + 800 + 900×3 = 3,900; the fourth August คาบ needs 4,800.
	if fine.PaidBaht != 3900 {
		t.Errorf("paid = %v, want 3900 — the คาบ cutoff should reach far deeper", fine.PaidBaht)
	}
	if wasted := 4000 - fine.PaidBaht; wasted >= 900 {
		t.Errorf("wasted %v, want less than one คาบ", wasted)
	}
}

// Strict chronology survives the finer granularity: a cheap later คาบ must not
// jump the queue ahead of the expensive one that stopped the budget.
func TestSettleTrack_DoesNotSkipAheadToASmallerSlot(t *testing.T) {
	got := settleTrack("regular", 1000, 0, slots(600, 500, 100))
	if want := []bool{true, false, false}; !equalBools(slotPaidFlags(got), want) {
		t.Errorf("slot paid = %v, want %v — the 100฿ คาบ would fit, but it is later",
			slotPaidFlags(got), want)
	}
	if got.PaidBaht != 600 {
		t.Errorf("paid = %v, want 600", got.PaidBaht)
	}
}

func TestSettleTrack_CutoffNamesTheExactSlot(t *testing.T) {
	got := settleTrack("regular", 1500, 0, slots(1000, 1000))
	if got.CutoffDate != "2026-06-02" || got.CutoffStart != "09:00" {
		t.Errorf("cutoff = %q %q, want 2026-06-02 09:00", got.CutoffDate, got.CutoffStart)
	}
	if !got.unpaidFrom("2026-06-02", "09:00") {
		t.Error("the cutoff คาบ itself must be unpaid")
	}
	if got.unpaidFrom("2026-06-01", "09:00") {
		t.Error("an earlier คาบ must stay paid")
	}
	if !got.unpaidFrom("2026-07-01", "09:00") {
		t.Error("everything after the cutoff must be unpaid")
	}
}

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The boundary of the whole spill rule, in the staff's own words (04/08/2026):
// "ถ้าไม่ได้สอนพร้อมกัน ก็คือแต่ละกลุ่มอิสระต่อกันเลย สอนคนละวัน ก็ต้องแบ่งคนละก้อน".
//
// Two sections that never meet at the same time are two separate budgets, full
// stop — the regular pool may not touch the special one however short it is.
// Only pay for time billed on BOTH tracks at once is entitled to cross over.
func TestSpillableRegularBaht_SectionsTaughtOnDifferentDaysCannotBorrow(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	special := f.siblingAssignment("special", nil)

	// Same TA, both tracks, but never at the same time — different days.
	insertLog(f, f.AssignmentID, day(10), "09:00", "12:00", 3)
	insertLog(f, special, day(11), "09:00", "12:00", 3)

	pr, err := (&CourseService{pool: f.Pool}).LatestPayRate(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := exportSvcFor(f).spillableRegularBaht(f.ctx, f.CourseID, *pr, mergedSittingsCTE)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("spillable = %.2f, want 0 — nothing was co-taught, so the pools stay separate", got)
	}

	// And with nothing spillable, a short regular pool sitting next to a special
	// pool with plenty of room still borrows nothing.
	if s := spillAllowance(1000, 5000, 0, 4000, 0, got); s != 0 {
		t.Errorf("spill = %.2f, want 0 — a shortfall alone must not open the other pool", s)
	}
}

// Only the hours actually SHARED may borrow, not the whole sitting. A TA on a
// regular section 09:00–13:00 and a special one 11:00–15:00 co-taught two hours,
// not four — and the other two on each side belong to their own pool.
//
// This case exists because every other spill test uses times that coincide
// exactly, where the shared span happens to equal the whole sitting. That let
// the obvious wrong implementation — bill the whole regular sitting whenever a
// special row exists the same day — pass all of them.
func TestSpillableRegularBaht_OnlyTheSharedHoursCross(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	special := f.siblingAssignment("special", nil)
	insertLog(f, f.AssignmentID, day(10), "09:00", "13:00", 4)
	insertLog(f, special, day(10), "11:00", "15:00", 4)

	pr, err := (&CourseService{pool: f.Pool}).LatestPayRate(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := exportSvcFor(f).spillableRegularBaht(f.ctx, f.CourseID, *pr, mergedSittingsCTE)
	if err != nil {
		t.Fatal(err)
	}
	if want := 2 * pr.UndergradRegular; math.Abs(got-want) > 0.01 {
		t.Errorf("spillable = %.2f, want %.2f — only 11:00-13:00 is taught on both tracks",
			got, want)
	}
}

// Adjacent but not overlapping: 09:00–11:00 then 11:00–13:00 are two classes,
// not one co-taught hour, so nothing crosses.
func TestSpillableRegularBaht_BackToBackIsNotCoTaught(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	special := f.siblingAssignment("special", nil)
	insertLog(f, f.AssignmentID, day(10), "09:00", "11:00", 2)
	insertLog(f, special, day(10), "11:00", "13:00", 2)

	pr, _ := (&CourseService{pool: f.Pool}).LatestPayRate(f.ctx)
	got, err := exportSvcFor(f).spillableRegularBaht(f.ctx, f.CourseID, *pr, mergedSittingsCTE)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("spillable = %.2f, want 0 — touching at 11:00 is not teaching at the same time", got)
	}
}
