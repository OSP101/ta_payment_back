package service

import (
	"math"
	"testing"
)

func newRows(pays ...float64) []exportRow {
	rs := make([]exportRow, len(pays))
	for i, p := range pays {
		rs[i] = exportRow{payBaht: p, actualPaid: p}
	}
	return rs
}

func sumActual(rs []exportRow) float64 {
	var s float64
	for _, r := range rs {
		s += r.actualPaid
	}
	return s
}

func TestProrata_NoOpWhenUnderBudget(t *testing.T) {
	rs := newRows(3000, 4000, 5000)
	if applyProrataCap(rs, 20000) {
		t.Errorf("expected no scaling")
	}
	for _, r := range rs {
		if r.actualPaid != r.payBaht {
			t.Errorf("actualPaid should stay at payBaht, got %v vs %v", r.actualPaid, r.payBaht)
		}
	}
}

func TestProrata_NoOpWhenBudgetZero(t *testing.T) {
	rs := newRows(3000, 4000)
	if applyProrataCap(rs, 0) {
		t.Errorf("budgetMax=0 must be treated as unlimited")
	}
}

func TestProrata_NoOpEmpty(t *testing.T) {
	rs := []exportRow{}
	if applyProrataCap(rs, 1000) {
		t.Errorf("empty slice must not report scaling")
	}
}

// From plan: 3 TAs total 25,000฿, budget 20,000฿ → 8,000 / 6,400 / 5,600.
func TestProrata_PlanScenario(t *testing.T) {
	// Raw: 10,000 / 8,000 / 7,000 = 25,000; k = 0.8 → 8,000 / 6,400 / 5,600.
	rs := newRows(10000, 8000, 7000)
	if !applyProrataCap(rs, 20000) {
		t.Fatalf("expected scaling for over-budget total")
	}
	want := []float64{8000, 6400, 5600}
	for i, w := range want {
		if math.Abs(rs[i].actualPaid-w) > 0.01 {
			t.Errorf("row %d: got %.2f want %.2f", i, rs[i].actualPaid, w)
		}
	}
	if math.Abs(sumActual(rs)-20000) > 0.01 {
		t.Errorf("sum should equal budget exactly, got %.2f", sumActual(rs))
	}
}

// Rounding drift lands on the last row so sum equals budgetMax to the cent.
func TestProrata_RoundingDriftAbsorbedOnLastRow(t *testing.T) {
	// 3 × 333.33 rounds to 333.33 × 3 = 999.99 — 0.01 short of 1000.
	rs := newRows(1000, 1000, 1000)
	applyProrataCap(rs, 1000)
	if math.Abs(sumActual(rs)-1000) > 0.001 {
		t.Errorf("sum should be 1000.00 exactly, got %.4f", sumActual(rs))
	}
	// The first two should be equal (~333.33), the last carries the drift.
	if math.Abs(rs[0].actualPaid-rs[1].actualPaid) > 0.001 {
		t.Errorf("rows 0/1 should be identical, got %.4f vs %.4f", rs[0].actualPaid, rs[1].actualPaid)
	}
}

// payBaht must be preserved — the coversheet shows both "should get" and "actual paid".
func TestProrata_PreservesPayBaht(t *testing.T) {
	rs := newRows(10000, 20000)
	applyProrataCap(rs, 5000)
	if rs[0].payBaht != 10000 || rs[1].payBaht != 20000 {
		t.Errorf("payBaht mutated: %+v", rs)
	}
	if rs[0].actualPaid >= rs[0].payBaht || rs[1].actualPaid >= rs[1].payBaht {
		t.Errorf("actualPaid should be smaller than payBaht after scaling")
	}
}

// Edge: exactly at budget — no scaling.
func TestProrata_ExactlyAtBudget(t *testing.T) {
	rs := newRows(5000, 5000)
	if applyProrataCap(rs, 10000) {
		t.Errorf("exact-at-budget should not scale")
	}
}

// Edge: single row over budget — everything goes to that row.
func TestProrata_SingleRow(t *testing.T) {
	rs := newRows(10000)
	if !applyProrataCap(rs, 8000) {
		t.Fatalf("expected scaling")
	}
	if math.Abs(rs[0].actualPaid-8000) > 0.01 {
		t.Errorf("got %.2f want 8000", rs[0].actualPaid)
	}
}
