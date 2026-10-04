package service

import "testing"

func TestOrderDateOK(t *testing.T) {
	for s, want := range map[string]bool{
		"2026-10-05":     true,
		"24 มกราคม 2569": true,
		"2-10-05":        false, // half-typed year from the date picker
		"0002-10-05":     false,
		"2026-13-01":     false,
		"":               false,
		"5/10/2026":      false,
	} {
		if got := orderDateOK(s); got != want {
			t.Errorf("orderDateOK(%q) = %v, want %v", s, got, want)
		}
	}
}
