package service

import (
	"fmt"
	"math"
	"testing"
)

// The spread rule: the pool is divided equally between the months that have
// work, each month is filled chronologically out of its own share, and the
// remainder nobody could spend is handed back out in calendar order.
//
// What it buys is the promise in its name — every month with work is paid
// something. What it must not cost is money: a rule that guarantees each month
// a share but leaves more in the account than the rule it replaced has taken
// from the TAs to buy a nicer-looking calendar.

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

// THE PROMISE. Under the old rule this exact course pays June through August in
// full and October nothing; under the spread rule every month is paid.
func TestSpread_EveryMonthWithWorkIsPaidSomething(t *testing.T) {
	// Five months × 10 คาบ × 100฿ = 5,000฿ of work against a 3,000฿ pool.
	ledger := concat(
		monthOf("2026-06", 10, 100),
		monthOf("2026-07", 10, 100),
		monthOf("2026-08", 10, 100),
		monthOf("2026-09", 10, 100),
		monthOf("2026-10", 10, 100),
	)

	chrono := settleTrack(SettleChronological, "regular", 3000, 0, ledger)
	if got := monthPaid(chrono)["2026-10"]; got != 0 {
		t.Fatalf("the case is not set up: October should be unpaid under the old rule, got %.0f", got)
	}

	spread := settleTrack(SettleSpread, "regular", 3000, 0, cloneSlots(ledger))
	for ym, paid := range monthPaid(spread) {
		if paid <= 0 {
			t.Errorf("%s was paid nothing — the spread rule exists to prevent exactly this", ym)
		}
	}
}

// THE MONEY THAT USED TO BE LEFT BEHIND.
//
// A remainder of 500฿ facing คาบ of 700 then 300 bought nothing until
// 07/09/2026, because the fill stopped at the first คาบ it could not afford and
// never reached past it. The college chose the money over the tidier claim
// form — "เงินสำคัญกว่า" — so the 300฿ คาบ must now be paid.
//
// The visible cost, pinned here too: the form reads unpaid-then-paid. The gap is
// never arbitrary — it is always a คาบ dearer than what was left.
func TestSettle_SkipsAheadToSpendTheLastOfThePool(t *testing.T) {
	ledger := []SlotSettlement{
		{Date: "2026-06-01", StartTime: "09:00", YearMonth: "2026-06", Baht: 700},
		{Date: "2026-06-02", StartTime: "09:00", YearMonth: "2026-06", Baht: 300},
	}
	got := settleTrack(SettleChronological, "regular", 500, 0, ledger)

	if got.PaidBaht != 300 {
		t.Errorf("paid %.0f, want 300 — the 700฿ คาบ does not fit but the 300฿ one "+
			"does, and leaving the whole 500฿ unspent is money owed to a TA that "+
			"simply never left the account", got.PaidBaht)
	}
	if got.Slots[0].Paid || !got.Slots[1].Paid {
		t.Errorf("paid flags %v %v, want the dear คาบ skipped and the cheap one bought",
			got.Slots[0].Paid, got.Slots[1].Paid)
	}
}

// The promise that skipping buys: after settling, no unpaid คาบ anywhere is
// cheap enough to have been afforded. This is the strong form — the one the
// old rule could not honour.
func TestSettle_LeavesNothingAnyKapCouldHaveBought(t *testing.T) {
	ledger := concat(
		monthOf("2026-06", 3, 700),
		monthOf("2026-07", 5, 300),
		monthOf("2026-08", 4, 550),
		monthOf("2026-09", 6, 250),
	)
	for _, mode := range []SettlementMode{SettleChronological, SettleSpread} {
		got := settleTrack(mode, "regular", 4000, 0, cloneSlots(ledger))
		left := 4000 - got.PaidBaht
		cheapest := math.Inf(1)
		for _, sl := range got.Slots {
			if !sl.Paid && sl.Baht < cheapest {
				cheapest = sl.Baht
			}
		}
		if left >= cheapest {
			t.Errorf("[%s] %.2f฿ left while an unpaid คาบ costs %.2f฿", mode, left, cheapest)
		}
	}
}

// The pool is a hard ceiling under either rule. Guaranteeing every month a share
// must not become a licence to overspend the budget the lecturer approved.
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
	if math.Abs(got.PaidBaht+got.DroppedBaht-10800) > 0.01 {
		t.Errorf("paid + dropped = %.2f, want the full 10,800 of work — "+
			"every คาบ must be accounted for on one side or the other",
			got.PaidBaht+got.DroppedBaht)
	}
}

// The committed lump (graduate-special) comes off the top before the months are
// given their shares — it is not monthly money and cannot be spread.
func TestSpread_DividesOnlyWhatIsLeftAfterTheCommittedLump(t *testing.T) {
	ledger := concat(
		monthOf("2026-06", 2, 500),
		monthOf("2026-07", 2, 500),
	)
	got := settleTrack(SettleSpread, "special", 3000, 2000, ledger)
	// 3,000 − 2,000 committed = 1,000 to divide, so 500 a month: one คาบ each.
	if got.PaidBaht != 1000 {
		t.Errorf("paid %.2f, want 1,000 — the 2,000 lump is spent already", got.PaidBaht)
	}
	for ym, paid := range monthPaid(got) {
		if paid != 500 {
			t.Errorf("%s paid %.2f, want 500", ym, paid)
		}
	}
}

// Inside a month the fill skips too: 1,000฿ against 600 + 500 + 100 buys the
// 600 and the 100, and leaves the 500 it cannot afford.
func TestSpread_SkipsPastAnExpensiveKapInsideAMonth(t *testing.T) {
	got := settleTrack(SettleSpread, "regular", 1000, 0, slots(600, 500, 100))
	if want := []bool{true, false, true}; !equalBools(slotPaidFlags(got), want) {
		t.Errorf("paid flags %v, want %v — 600 + 100 fits in 1,000 and the 500 "+
			"does not; leaving the 100 unpaid strands money", slotPaidFlags(got), want)
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

// The document asks unpaidFrom, one คาบ at a time. Under the spread rule the
// unpaid คาบ are no longer a suffix in time, so an answer derived from the
// cutoff date alone would print คาบ the settlement did not pay for — money out
// of the door against a budget that had already run out.
func TestSpread_UnpaidFromMatchesTheSettledSlots(t *testing.T) {
	ledger := concat(
		monthOf("2026-06", 4, 400),
		monthOf("2026-07", 4, 400),
		monthOf("2026-08", 4, 400),
	)
	got := settleTrack(SettleSpread, "regular", 3000, 0, ledger)

	sawUnpaidBeforePaid := false
	unpaidSeen := false
	for _, sl := range got.Slots {
		if got.unpaidFor(sl.TA, sl.Date, sl.StartTime) != !sl.Paid {
			t.Fatalf("unpaidFrom disagrees with the ledger at %s %s: "+
				"the printed document and the settlement would pay different people",
				sl.Date, sl.StartTime)
		}
		if !sl.Paid {
			unpaidSeen = true
		} else if unpaidSeen {
			sawUnpaidBeforePaid = true
		}
	}
	if !sawUnpaidBeforePaid {
		t.Error("this case never puts a paid คาบ after an unpaid one, so it does " +
			"not actually exercise what makes the spread rule different")
	}
}

// cloneSlots keeps one ledger reusable across two settlements — settleTrack
// writes Paid onto the slice it is handed.
func cloneSlots(in []SlotSettlement) []SlotSettlement {
	out := make([]SlotSettlement, len(in))
	copy(out, in)
	return out
}
