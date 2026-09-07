package service

import (
	"fmt"
	"math"
	"testing"

	"github.com/google/uuid"
)

// Equal work, equal pay — the fairness the college chose on 07/09/2026:
// "ต่อให้งบขาด เงินไม่พอ ก็ต้องได้เท่า ๆ กัน ... ถ้าทำงานเวลาเท่า ๆ กัน".
//
// The rule these tests replaced cut the term at one moment for everybody, which
// made a TA's pay depend on WHEN they were timetabled. Whoever's work sat before
// the cutoff was paid in full and whoever's sat after it was not, for the same
// hours at the same rate.

// personSlots builds one TA's คาบ on the given dates, each worth the same.
func personSlots(ta uuid.UUID, baht float64, dates ...string) []SlotSettlement {
	out := make([]SlotSettlement, 0, len(dates))
	for _, d := range dates {
		out = append(out, SlotSettlement{
			TA: ta, Date: d, StartTime: "09:00", YearMonth: d[:7], Baht: baht,
		})
	}
	return out
}

// paidPerTA totals what each person actually receives.
func paidPerTA(tr TrackSettlement) map[uuid.UUID]float64 {
	out := map[uuid.UUID]float64{}
	for _, sl := range tr.Slots {
		if sl.Paid {
			out[sl.TA] += sl.Baht
		}
	}
	return out
}

// ledgerSorted mimics slotLedger's output ordering, which settleTrack relies on.
func ledgerSorted(groups ...[]SlotSettlement) []SlotSettlement {
	var out []SlotSettlement
	for _, g := range groups {
		out = append(out, g...)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			if a.Date < b.Date || (a.Date == b.Date && a.StartTime <= b.StartTime) {
				break
			}
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// THE ONE THAT MATTERS. Two TAs, identical hours, working different stretches of
// the term. Under the old whole-course cutoff the first one was paid in full and
// the second got the remainder; they must now come out level.
//
// Deliberately a split by PERIOD rather than by day of the week: TAs who
// alternate days inside the same weeks are interleaved in the ledger, so a
// chronological cutoff happens to fall evenly between them and the old rule
// looked fair. It is when one person's work sits mostly before the other's — a
// TA appointed late, a section that only runs in the second half — that a single
// cutoff quietly pays one of them and not the other.
func TestEqualPay_SameHoursInDifferentPartsOfTheTermArePaidTheSame(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	// Ten คาบ each at 500฿ = 5,000฿ owed apiece, 10,000฿ of work in all.
	var firstDates, secondDates []string
	for d := 1; d <= 10; d++ {
		firstDates = append(firstDates, fmt.Sprintf("2026-06-%02d", d))
		secondDates = append(secondDates, fmt.Sprintf("2026-07-%02d", d))
	}
	ledger := ledgerSorted(
		personSlots(first, 500, firstDates...),
		personSlots(second, 500, secondDates...),
	)

	// A 6,000฿ pool against 10,000฿ of work: everybody should lose 40%.
	// The old rule paid the June TA all 5,000 and the July TA 1,000.
	got := settleTrack(SettleChronological, "regular", 6000, 0, ledger)
	paid := paidPerTA(got)

	if math.Abs(paid[first]-paid[second]) > 500.01 {
		t.Errorf("first-half TA paid %.0f, second-half TA paid %.0f — same hours, "+
			"and the gap is wider than the one คาบ that whole-คาบ billing can "+
			"strand. Pay is still deciding on WHEN somebody was timetabled",
			paid[first], paid[second])
	}
	if got.PaidBaht > 6000.01 {
		t.Errorf("paid %.2f against a 6,000 pool", got.PaidBaht)
	}
}

// Unequal hours are not levelled — the promise is equal PROPORTION, not equal
// baht. A TA who worked twice as much is paid twice as much of a short budget.
func TestEqualPay_UnequalHoursKeepTheirProportion(t *testing.T) {
	big, small := uuid.New(), uuid.New()
	ledger := ledgerSorted(
		personSlots(big, 500, "2026-06-01", "2026-06-02", "2026-06-03", "2026-06-04"),
		personSlots(small, 500, "2026-06-05", "2026-06-08"),
	)
	// 3,000฿ of work, 1,500฿ pool: half of each person's due.
	got := settleTrack(SettleChronological, "regular", 1500, 0, ledger)
	paid := paidPerTA(got)

	if math.Abs(paid[big]-1000) > 500.01 || math.Abs(paid[small]-500) > 500.01 {
		t.Errorf("paid big=%.0f small=%.0f, want roughly 1,000 and 500 — "+
			"the 2:1 ratio of the work they did", paid[big], paid[small])
	}
}

// The shares strand small change, and change withheld from people who are
// already short is money the college owes and did not pay. Whatever no share
// could reach must be spent — and spent on whoever is furthest behind, so
// clearing it cannot re-open the gap the shares just closed.
func TestEqualPay_LeftoverGoesToWhoeverIsFurthestBehind(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	// Deliberately awkward: costs that do not divide into either share.
	ledger := ledgerSorted(
		personSlots(a, 700, "2026-06-01", "2026-06-03", "2026-06-05"),
		personSlots(b, 300, "2026-06-02", "2026-06-04", "2026-06-06", "2026-06-08"),
	)
	got := settleTrack(SettleChronological, "regular", 2000, 0, ledger)

	left := 2000 - got.PaidBaht
	cheapestUnpaid := math.Inf(1)
	for _, sl := range got.Slots {
		if !sl.Paid && sl.Baht < cheapestUnpaid {
			cheapestUnpaid = sl.Baht
		}
	}
	if left >= cheapestUnpaid {
		t.Errorf("%.2f฿ left unspent while an unpaid คาบ costs %.2f฿ — that is "+
			"money owed to somebody who was short and simply not paid", left, cheapestUnpaid)
	}
}

// The document filter must agree with the ledger PER PERSON. Two TAs sharing one
// co-taught คาบ can now be settled differently, so an answer that ignores who is
// asking would print คาบ the budget did not pay for.
func TestEqualPay_UnpaidForIsAnsweredPerPerson(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	ledger := ledgerSorted(
		personSlots(a, 400, "2026-06-01", "2026-06-02", "2026-06-03"),
		personSlots(b, 400, "2026-06-01", "2026-06-02", "2026-06-03"),
	)
	// Enough for four of the six คาบ.
	got := settleTrack(SettleChronological, "regular", 1600, 0, ledger)

	for _, sl := range got.Slots {
		if got.unpaidFor(sl.TA, sl.Date, sl.StartTime) != !sl.Paid {
			t.Fatalf("unpaidFor disagrees with the ledger for %s on %s — the "+
				"printed claim and the settlement would pay different people",
				sl.TA, sl.Date)
		}
	}
}

// Spreading across months and sharing between people are independent: turning
// the month rule on must not undo the person rule.
func TestEqualPay_HoldsUnderTheSpreadRuleToo(t *testing.T) {
	early, late := uuid.New(), uuid.New()
	ledger := ledgerSorted(
		personSlots(early, 500, "2026-06-01", "2026-06-02", "2026-06-03", "2026-06-04"),
		personSlots(late, 500, "2026-07-01", "2026-07-02", "2026-07-03", "2026-07-04"),
	)
	got := settleTrack(SettleSpread, "regular", 2400, 0, ledger)
	paid := paidPerTA(got)

	if math.Abs(paid[early]-paid[late]) > 500.01 {
		t.Errorf("early %.0f vs late %.0f under the spread rule — equal work must "+
			"still be equal pay whichever way the months are cut", paid[early], paid[late])
	}
}
