package service

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// Equal work, equal pay — the fairness the college chose on 07/09/2026 and
// tightened to the baht on 11/09/2026: "ต่อให้งบขาด เงินไม่พอ ก็ต้องได้เท่า ๆ
// กัน ... ถ้าทำงานเวลาเท่า ๆ กัน", and "ใครทำมาก ก็ได้มาก".
//
// The pool is shared as money in proportion to what each person is owed. The
// rule this replaced bought whole คาบ out of each share, which left two TAs
// with identical hours a คาบ apart and stranded change in every pool.

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
		out[sl.TA] += sl.PaidBaht
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
// the term, come out level to the baht — and the pool is spent in full.
func TestEqualPay_SameHoursInDifferentPartsOfTheTermArePaidTheSame(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	var firstDates, secondDates []string
	for d := 1; d <= 10; d++ {
		firstDates = append(firstDates, fmt.Sprintf("2026-06-%02d", d))
		secondDates = append(secondDates, fmt.Sprintf("2026-07-%02d", d))
	}
	ledger := ledgerSorted(
		personSlots(first, 500, firstDates...),
		personSlots(second, 500, secondDates...),
	)

	// A 6,000฿ pool against 10,000฿ of work: everybody loses 40%.
	got := settleTrack(SettleChronological, "regular", 6000, 0, ledger)
	paid := paidPerTA(got)

	if paid[first] != 3000 || paid[second] != 3000 {
		t.Errorf("first-half TA paid %.2f, second-half TA paid %.2f — same hours, want 3,000 each",
			paid[first], paid[second])
	}
	if got.PaidBaht != 6000 {
		t.Errorf("paid %.2f, want the whole 6,000 pool", got.PaidBaht)
	}
}

// The probe that changed the rule (11/09/2026): two TAs × 10 คาบ × 150฿ against
// 2,000฿ came out 1,050 / 900 with 50฿ unspent under whole-คาบ billing.
func TestEqualPay_TheProbeCaseComesOutLevel(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	var dates []string
	for d := 1; d <= 10; d++ {
		dates = append(dates, fmt.Sprintf("2026-06-%02d", d))
	}
	ledger := ledgerSorted(personSlots(a, 150, dates...), personSlots(b, 150, dates...))
	got := settleTrack(SettleChronological, "regular", 2000, 0, ledger)
	paid := paidPerTA(got)
	if paid[a] != 1000 || paid[b] != 1000 || got.PaidBaht != 2000 {
		t.Errorf("paid a=%.2f b=%.2f total=%.2f, want 1,000 / 1,000 / 2,000", paid[a], paid[b], got.PaidBaht)
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

	if paid[big] != 1000 || paid[small] != 500 {
		t.Errorf("paid big=%.2f small=%.2f, want 1,000 and 500 — the 2:1 ratio of the work",
			paid[big], paid[small])
	}
}

// The proportion is of MONEY owed, not of hours: a graduate TA's hour costs
// more and is cut by the same percentage as everyone else's baht.
func TestEqualPay_ProportionIsOfBahtNotHours(t *testing.T) {
	ug, grad := uuid.New(), uuid.New()
	ledger := ledgerSorted(
		personSlots(ug, 100, "2026-06-01", "2026-06-02"),   // 2 คาบ at 100 = 200
		personSlots(grad, 300, "2026-06-03", "2026-06-04"), // 2 คาบ at 300 = 600
	)
	got := settleTrack(SettleChronological, "regular", 400, 0, ledger) // half of 800
	paid := paidPerTA(got)
	if paid[ug] != 100 || paid[grad] != 300 {
		t.Errorf("paid ug=%.2f grad=%.2f, want 100 and 300 — 50%% of what each is owed",
			paid[ug], paid[grad])
	}
}

// Each person's share is a whole baht; what the rounding strands stays in the
// pool and is the only money the rule leaves — under one baht a person.
func TestEqualPay_SharesAreWholeBahtAndTheChangeStaysInThePool(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	ledger := ledgerSorted(
		personSlots(a, 100, "2026-06-01"),
		personSlots(b, 100, "2026-06-02"),
		personSlots(c, 100, "2026-06-03"),
	)
	// 200 of 300: 66.67 each → 66 each, 198 paid, 2 left.
	got := settleTrack(SettleChronological, "regular", 200, 0, ledger)
	paid := paidPerTA(got)
	for _, ta := range []uuid.UUID{a, b, c} {
		if paid[ta] != 66 {
			t.Errorf("paid %.2f, want 66", paid[ta])
		}
	}
	if left := 200 - got.PaidBaht; left >= 3 {
		t.Errorf("%.2f left unspent, want under one baht a person", left)
	}
}

// The document sums each cost row × fundedShare; that must land on exactly what
// the ledger paid the person.
func TestEqualPay_FundedShareIsAnsweredPerPerson(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	ledger := ledgerSorted(
		personSlots(a, 400, "2026-06-01", "2026-06-02", "2026-06-03"),
		personSlots(b, 400, "2026-06-01", "2026-06-02", "2026-06-03"),
	)
	got := settleTrack(SettleChronological, "regular", 1600, 0, ledger)
	paid := paidPerTA(got)
	sum := map[uuid.UUID]float64{}
	for _, sl := range got.Slots {
		sum[sl.TA] += sl.Baht * got.fundedShare(sl.TA, sl.Date, sl.StartTime)
	}
	for _, ta := range []uuid.UUID{a, b} {
		if !near(sum[ta], paid[ta]) || paid[ta] != 800 {
			t.Errorf("rows × fundedShare = %.2f, ledger = %.2f, want 800", sum[ta], paid[ta])
		}
	}
}

// Spreading across months and sharing between people are independent: turning
// the month rule on must not change anybody's total.
func TestEqualPay_HoldsUnderTheSpreadRuleToo(t *testing.T) {
	early, late := uuid.New(), uuid.New()
	ledger := ledgerSorted(
		personSlots(early, 500, "2026-06-01", "2026-06-02", "2026-06-03", "2026-06-04"),
		personSlots(late, 500, "2026-07-01", "2026-07-02", "2026-07-03", "2026-07-04"),
	)
	got := settleTrack(SettleSpread, "regular", 2400, 0, ledger)
	paid := paidPerTA(got)

	if paid[early] != 1200 || paid[late] != 1200 {
		t.Errorf("early %.2f vs late %.2f under the spread rule, want 1,200 each", paid[early], paid[late])
	}
}

// The college's own worked example (11/09/2026): one ป.เอก and three ป.ตรี,
// identical hours, a 29,900฿ pool — at the live rate table (ป.ตรี 40฿/h,
// ป.โท/เอก 50฿/h). Everybody works 180 h, so the work costs 30,600฿ and the
// pool is 700฿ short.
func TestEqualPay_OnePhdThreeUndergradsAgainst29900(t *testing.T) {
	const ugRate, gradRate, hours = 40.0, 50.0, 180.0
	phd := uuid.New()
	ugs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}

	// 60 คาบ × 3 h each, spread over the term, same dates for everybody.
	slotsFor := func(ta uuid.UUID, rate float64) []SlotSettlement {
		var out []SlotSettlement
		for i := 0; i < 60; i++ {
			ym := []string{"2026-06", "2026-07", "2026-08", "2026-09", "2026-10"}[i/12]
			out = append(out, SlotSettlement{
				TA: ta, Date: fmt.Sprintf("%s-%02d", ym, i%12+1), StartTime: "09:00",
				YearMonth: ym, Baht: 3 * rate,
			})
		}
		return out
	}
	ledger := ledgerSorted(slotsFor(phd, gradRate),
		slotsFor(ugs[0], ugRate), slotsFor(ugs[1], ugRate), slotsFor(ugs[2], ugRate))

	got := settleTrack(SettleChronological, "regular", 29900, 0, ledger)
	paid := paidPerTA(got)
	t.Logf("pool 29,900: ป.เอก owed %.0f → paid %.0f; each ป.ตรี owed %.0f → paid %.0f; total %.0f, unspent %.0f",
		hours*gradRate, paid[phd], hours*ugRate, paid[ugs[0]], got.PaidBaht, 29900-got.PaidBaht)

	// Shares: 29,900 × 9,000/30,600 = 8,794.12 → 8,794; 29,900 × 7,200/30,600 = 7,035.29 → 7,035.
	if paid[phd] != 8794 {
		t.Errorf("ป.เอก paid %.2f, want 8,794", paid[phd])
	}
	for _, ug := range ugs {
		if paid[ug] != 7035 {
			t.Errorf("ป.ตรี paid %.2f, want 7,035", paid[ug])
		}
	}
	// Same hours, higher rate → more money, by exactly the rate ratio; and
	// everyone keeps the same 97.7%.
	if got.PaidBaht != 8794+3*7035 {
		t.Errorf("total %.2f, want 29,899 (29,900 less the rounding)", got.PaidBaht)
	}
}
