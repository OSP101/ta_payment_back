package service

import (
	"fmt"
	"testing"
)

// The spread rule: every month a person worked is cut by the same proportion,
// so none of them is paid nothing. It changes only WHERE a share lands, never
// how big it is — the per-person total is the same as under the chronological
// rule, to the baht.

// monthOf builds n คาบ of equal cost inside one named month.
func monthOf(ym string, n int, baht float64) []SlotSettlement {
	out := make([]SlotSettlement, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, SlotSettlement{
			Date: fmt.Sprintf("%s-%02d", ym, i+1), StartTime: "09:00",
			YearMonth: ym, Baht: baht,
		})
	}
	return out
}

func concat(groups ...[]SlotSettlement) []SlotSettlement {
	var out []SlotSettlement
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// monthPaid maps year-month to what the settlement paid it.
func monthPaid(tr TrackSettlement) map[string]float64 {
	out := map[string]float64{}
	for _, m := range tr.Months {
		out[m.YearMonth] = m.PaidBaht
	}
	return out
}

// THE PROMISE. Under the chronological rule this course pays June through
// August in full and October nothing; under the spread rule every month gets
// the same 60%.
func TestSpread_EveryMonthIsCutByTheSameProportion(t *testing.T) {
	// Five months × 10 คาบ × 100฿ = 5,000฿ of work against a 3,000฿ pool.
	ledger := concat(
		monthOf("2026-06", 10, 100),
		monthOf("2026-07", 10, 100),
		monthOf("2026-08", 10, 100),
		monthOf("2026-09", 10, 100),
		monthOf("2026-10", 10, 100),
	)

	chrono := settleTrack(SettleChronological, "regular", 3000, 0, cloneSlots(ledger))
	if got := monthPaid(chrono)["2026-10"]; got != 0 {
		t.Fatalf("the case is not set up: October should be unpaid under the old rule, got %.0f", got)
	}

	spread := settleTrack(SettleSpread, "regular", 3000, 0, cloneSlots(ledger))
	for ym, paid := range monthPaid(spread) {
		if paid != 600 {
			t.Errorf("%s was paid %.2f, want 600 — every month carries the same 60%%", ym, paid)
		}
	}
	if spread.PaidBaht != chrono.PaidBaht {
		t.Errorf("spread paid %.2f, chronological %.2f — the rule moves money between "+
			"months, it must not change the total", spread.PaidBaht, chrono.PaidBaht)
	}
}

// Months of different sizes are cut by the same PROPORTION, not given the same
// baht — a month with twice the work carries twice the money.
func TestSpread_BigMonthsCarryMoreOfTheShare(t *testing.T) {
	ledger := concat(
		monthOf("2026-06", 2, 500), // 1,000
		monthOf("2026-07", 4, 500), // 2,000
	)
	got := settleTrack(SettleSpread, "regular", 1500, 0, ledger)
	paid := monthPaid(got)
	if paid["2026-06"] != 500 || paid["2026-07"] != 1000 {
		t.Errorf("paid June %.2f July %.2f, want 500 and 1,000 — half of each", paid["2026-06"], paid["2026-07"])
	}
}

// Every month's figure is a whole baht, and the baht the rounding strands go
// back to the earliest month, so the person's total is exactly their share.
func TestSpread_MonthFiguresAreWholeBahtAndSumToTheShare(t *testing.T) {
	ledger := concat(
		monthOf("2026-06", 1, 100),
		monthOf("2026-07", 1, 100),
		monthOf("2026-08", 1, 100),
	)
	// 200 of 300: 66.67 a month → 66 + 66 + 66 = 198, 2 left → June 68.
	got := settleTrack(SettleSpread, "regular", 200, 0, ledger)
	paid := monthPaid(got)
	if paid["2026-06"] != 68 || paid["2026-07"] != 66 || paid["2026-08"] != 66 {
		t.Errorf("paid %v, want June 68, July 66, August 66", paid)
	}
	if got.PaidBaht != 200 {
		t.Errorf("paid %.2f, want the whole 200", got.PaidBaht)
	}
}

// The pool is a hard ceiling under either rule, and every baht of work is on
// one side of the line or the other.
func TestSpread_NeverSpendsMoreThanThePool(t *testing.T) {
	ledger := concat(
		monthOf("2026-06", 4, 900),
		monthOf("2026-07", 4, 900),
		monthOf("2026-08", 4, 900),
	)
	got := settleTrack(SettleSpread, "regular", 5000, 0, ledger)
	if got.PaidBaht > 5000+0.01 {
		t.Errorf("paid %.2f against a 5,000 pool", got.PaidBaht)
	}
	if !near(got.PaidBaht+got.DroppedBaht, 10800) {
		t.Errorf("paid + dropped = %.2f, want the full 10,800 of work", got.PaidBaht+got.DroppedBaht)
	}
}

// The committed lump (graduate-special) comes off the top before the share is
// spread — it is not monthly money.
func TestSpread_DividesOnlyWhatIsLeftAfterTheCommittedLump(t *testing.T) {
	ledger := concat(
		monthOf("2026-06", 2, 500),
		monthOf("2026-07", 2, 500),
	)
	got := settleTrack(SettleSpread, "special", 3000, 2000, ledger)
	// 3,000 − 2,000 committed = 1,000 to divide over 2,000 of work: half.
	if got.PaidBaht != 1000 {
		t.Errorf("paid %.2f, want 1,000 — the 2,000 lump is spent already", got.PaidBaht)
	}
	for ym, paid := range monthPaid(got) {
		if paid != 500 {
			t.Errorf("%s paid %.2f, want 500", ym, paid)
		}
	}
}

// A course whose budget covers everything settles identically under both rules.
// The lecturer turning the switch on must not change a course that was never
// short in the first place.
func TestSpread_IsANoOpWhenTheBudgetCoversEverything(t *testing.T) {
	ledger := concat(
		monthOf("2026-06", 3, 400),
		monthOf("2026-07", 3, 400),
	)
	chrono := settleTrack(SettleChronological, "regular", 99000, 0, cloneSlots(ledger))
	spread := settleTrack(SettleSpread, "regular", 99000, 0, cloneSlots(ledger))
	if chrono.PaidBaht != spread.PaidBaht || chrono.DroppedBaht != 0 || spread.DroppedBaht != 0 {
		t.Errorf("chronological paid %.2f/dropped %.2f, spread paid %.2f/dropped %.2f — "+
			"a course that fits must settle the same either way",
			chrono.PaidBaht, chrono.DroppedBaht, spread.PaidBaht, spread.DroppedBaht)
	}
}

// An unconfigured cap (no student count yet) means "unknown", not "no money".
// Both rules pay everything rather than silently zeroing the course.
func TestSpread_UnconfiguredCapPaysEverything(t *testing.T) {
	got := settleTrack(SettleSpread, "regular", 0, 0, months(5000, 5000))
	if got.DroppedBaht != 0 || got.PaidBaht != 10000 {
		t.Errorf("paid %.2f dropped %.2f, want everything paid — a cap of 0 is "+
			"an unfilled student count, not an empty budget", got.PaidBaht, got.DroppedBaht)
	}
}

// The documents sum cost rows × fundedShare. Under the spread rule every คาบ
// is part-funded, and the ledger's answer must reproduce the month figures.
func TestSpread_FundedShareReproducesTheMonths(t *testing.T) {
	ledger := concat(
		monthOf("2026-06", 4, 400),
		monthOf("2026-07", 4, 400),
		monthOf("2026-08", 4, 400),
	)
	got := settleTrack(SettleSpread, "regular", 3000, 0, ledger)
	sum := map[string]float64{}
	for _, sl := range got.Slots {
		sum[sl.YearMonth] += sl.Baht * got.fundedShare(sl.TA, sl.Date, sl.StartTime)
	}
	for ym, paid := range monthPaid(got) {
		if !near(sum[ym], paid) {
			t.Errorf("%s: rows × fundedShare = %.2f, ledger month = %.2f", ym, sum[ym], paid)
		}
	}
}

// cloneSlots keeps one ledger reusable across two settlements — settleTrack
// writes PaidBaht onto the slice it is handed.
func cloneSlots(in []SlotSettlement) []SlotSettlement {
	out := make([]SlotSettlement, len(in))
	copy(out, in)
	return out
}
