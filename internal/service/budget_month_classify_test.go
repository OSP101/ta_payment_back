package service

import (
	"strings"
	"testing"
)

// What a month's shortfall is CALLED, which is what a TA reads and believes.
//
// ภาคปกติ and ภาคพิเศษ are separate budgets that run out at different points, so
// the classification cannot be made one pool at a time — the same month can be
// complete on one and empty on the other.

// trackWith builds a settled pool from per-month (cost, paid) pairs.
func trackWith(track string, rows ...[2]float64) TrackSettlement {
	names := []string{"2026-06", "2026-07", "2026-08", "2026-09", "2026-10"}
	t := TrackSettlement{Track: track}
	for i, r := range rows {
		t.Months = append(t.Months, MonthSettlement{
			YearMonth: names[i], Baht: r[0], PaidBaht: r[1],
			Paid: r[1] >= r[0]-0.01,
		})
	}
	return t
}

func joined(ss []string) string { return strings.Join(ss, ",") }

// THE REGRESSION. CP363205 announced "ตุลาคม ไม่ได้รับค่าตอบแทน" while paying its
// regular track ฿2,040 in full that month. A pool paid IN FULL set none of the
// old flags, so a month complete on one pool and empty on the other was
// indistinguishable from a month nobody was paid for.
func TestClassifyMonths_APoolPaidInFullIsNotAMonthNobodyWasPaidFor(t *testing.T) {
	reg := trackWith("regular", [2]float64{2040, 2040}) // paid in full
	spc := trackWith("special", [2]float64{1800, 0})    // paid nothing

	unpaid, partial, zeroed := classifyMonths(reg, spc)

	if len(unpaid) != 0 {
		t.Errorf("unpaid = %v, want none — ภาคปกติ was paid in full that month, "+
			"and telling those TAs they get nothing is simply false", unpaid)
	}
	if joined(partial) != "2026-06" {
		t.Errorf("partial = %v, want the month — it is short, but not empty", partial)
	}
	if len(zeroed) != 1 || zeroed[0].YearMonth != "2026-06" ||
		joined(zeroed[0].ZeroTracks) != "special" {
		t.Errorf("track detail = %+v, want ภาคพิเศษ named — “ได้บางส่วน” is true of "+
			"the course and a lie to everyone on the empty pool", zeroed)
	}
}

// The genuine total loss must still be called one.
func TestClassifyMonths_EveryPoolEmptyIsStillUnpaid(t *testing.T) {
	reg := trackWith("regular", [2]float64{2040, 0})
	spc := trackWith("special", [2]float64{1800, 0})

	unpaid, partial, zeroed := classifyMonths(reg, spc)

	if joined(unpaid) != "2026-06" {
		t.Errorf("unpaid = %v, want the month — nobody was paid anything", unpaid)
	}
	if len(partial) != 0 {
		t.Errorf("partial = %v, want none", partial)
	}
	// Not a per-pool story when every pool tells the same one.
	if len(zeroed) != 0 {
		t.Errorf("track detail = %+v, want none — naming a pool suggests another "+
			"pool fared better, and none did", zeroed)
	}
}

// A course with no ภาคพิเศษ section must not be reported as having a pool that
// went unpaid — a pool with no work has no view.
func TestClassifyMonths_APoolWithNoWorkDoesNotVote(t *testing.T) {
	reg := trackWith("regular", [2]float64{2040, 0})
	spc := trackWith("special", [2]float64{0, 0})

	unpaid, _, zeroed := classifyMonths(reg, spc)

	if joined(unpaid) != "2026-06" {
		t.Errorf("unpaid = %v, want the month — the only pool with work got nothing", unpaid)
	}
	if len(zeroed) != 0 {
		t.Errorf("track detail = %+v, want none — the special pool had no work to "+
			"go unpaid for", zeroed)
	}
}

// Short on both, empty on neither: the ordinary partial month.
func TestClassifyMonths_ShortOnBothIsPartial(t *testing.T) {
	reg := trackWith("regular", [2]float64{2000, 1200})
	spc := trackWith("special", [2]float64{1000, 400})

	unpaid, partial, zeroed := classifyMonths(reg, spc)

	if len(unpaid) != 0 || joined(partial) != "2026-06" || len(zeroed) != 0 {
		t.Errorf("unpaid=%v partial=%v tracks=%+v, want partial only", unpaid, partial, zeroed)
	}
}

// A month everybody was paid for in full is not news at all.
func TestClassifyMonths_FullyPaidMonthIsNotReported(t *testing.T) {
	reg := trackWith("regular", [2]float64{2000, 2000})
	spc := trackWith("special", [2]float64{1000, 1000})

	unpaid, partial, zeroed := classifyMonths(reg, spc)

	if len(unpaid) != 0 || len(partial) != 0 || len(zeroed) != 0 {
		t.Errorf("unpaid=%v partial=%v tracks=%+v, want silence", unpaid, partial, zeroed)
	}
}

// Months come back in calendar order — the lists are read out to people as
// sentences, and an unordered month list reads as a mistake.
func TestClassifyMonths_ReportsInCalendarOrder(t *testing.T) {
	reg := trackWith("regular",
		[2]float64{100, 0}, [2]float64{100, 100}, [2]float64{100, 0},
		[2]float64{100, 100}, [2]float64{100, 0})

	unpaid, _, _ := classifyMonths(reg)

	if joined(unpaid) != "2026-06,2026-08,2026-10" {
		t.Errorf("unpaid = %v, want calendar order", unpaid)
	}
}
