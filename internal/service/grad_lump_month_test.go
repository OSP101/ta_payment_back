package service

import "testing"

// The lump is dated by weight, whole baht a month, remainder to the earliest
// month — the same shape as spreadOverMonths, so every monthly document prints
// whole amounts and they sum back to the lump exactly.
func TestPlaceLump_WholeBahtPerMonthSummingToTheLump(t *testing.T) {
	got := placeLump(4000, map[string]float64{"2026-06": 6, "2026-07": 18, "2026-08": 14, "2026-09": 16, "2026-10": 10})
	// 64 h: 375 / 1125 / 875 / 1000 / 625 = 4000 exactly.
	want := map[string]float64{"2026-06": 375, "2026-07": 1125, "2026-08": 875, "2026-09": 1000, "2026-10": 625}
	for ym, w := range want {
		if got[ym] != w {
			t.Errorf("%s = %.2f, want %.2f", ym, got[ym], w)
		}
	}
	// Awkward weights: 4000 over 3 equal months = 1333.33 → 1334 / 1333 / 1333.
	got = placeLump(4000, map[string]float64{"2026-06": 1, "2026-07": 1, "2026-08": 1})
	if got["2026-06"] != 1334 || got["2026-07"] != 1333 || got["2026-08"] != 1333 {
		t.Errorf("got %v, want 1334 / 1333 / 1333", got)
	}
	if placeLump(4000, map[string]float64{}) == nil || len(placeLump(4000, nil)) != 0 {
		t.Error("no weights must yield an empty (not nil-panicking) allocation")
	}
}
