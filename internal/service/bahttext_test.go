package service

import "testing"

func TestBahtText(t *testing.T) {
	cases := []struct {
		amount float64
		want   string
	}{
		{0, "ศูนย์บาทถ้วน"},
		{1, "หนึ่งบาทถ้วน"},
		{10, "สิบบาทถ้วน"},
		{11, "สิบเอ็ดบาทถ้วน"},
		{20, "ยี่สิบบาทถ้วน"},
		{21, "ยี่สิบเอ็ดบาทถ้วน"},
		{100, "หนึ่งร้อยบาทถ้วน"},
		{121, "หนึ่งร้อยยี่สิบเอ็ดบาทถ้วน"},
		{1000, "หนึ่งพันบาทถ้วน"},
		{100000, "หนึ่งแสนบาทถ้วน"},
		// 100,001 is a SINGLE group (it's under 1,000,000, so no "ล้าน" at all),
		// and 100001 read as one group is well over 10 — เอ็ด applies. Contrast
		// with 100,000,001 below, which splits into two separate chunks.
		{100001, "หนึ่งแสนเอ็ดบาทถ้วน"},
		{1000000, "หนึ่งล้านบาทถ้วน"},
		// Each chunk reads as if it were spoken on its own before "ล้าน": the
		// millions chunk here is literally "1", which alone is "หนึ่ง" (not
		// "เอ็ด" — that needs the group itself to be 10 or more). The trailing
		// units chunk is also just "1" in isolation, so it stays "หนึ่ง" too.
		{1000001, "หนึ่งล้านหนึ่งบาทถ้วน"},
		// Here the millions chunk is 21 — which, read alone, is "ยี่สิบเอ็ด" —
		// so the whole chunk carries "เอ็ด" into "ยี่สิบเอ็ดล้าน".
		{21000000, "ยี่สิบเอ็ดล้านบาทถ้วน"},
		{1000011, "หนึ่งล้านสิบเอ็ดบาทถ้วน"},
		{100000001, "หนึ่งร้อยล้านหนึ่งบาทถ้วน"},
		// satang
		{0.50, "ศูนย์บาทห้าสิบสตางค์"},
		{1.01, "หนึ่งบาทหนึ่งสตางค์"},
		{40008.36, "สี่หมื่นแปดบาทสามสิบหกสตางค์"},
		// negative — should not occur for a real payout, but must not silently
		// drop the sign
		{-5, "ลบห้าบาทถ้วน"},
	}
	for _, c := range cases {
		if got := BahtText(c.amount); got != c.want {
			t.Errorf("BahtText(%v) = %q, want %q", c.amount, got, c.want)
		}
	}
}

// Floating-point representation error must not leak into the satang digits:
// 2.9 stored as float64 can read back as 2.8999999999999995.
func TestBahtTextFloatRounding(t *testing.T) {
	if got, want := BahtText(2.90), "สองบาทเก้าสิบสตางค์"; got != want {
		t.Errorf("BahtText(2.90) = %q, want %q", got, want)
	}
	if got, want := BahtText(0.10), "ศูนย์บาทสิบสตางค์"; got != want {
		t.Errorf("BahtText(0.10) = %q, want %q", got, want)
	}
	// 0.29*100 lands on 28.999999999999996 in float64 — truncating (or
	// flooring) instead of rounding would read this back as 28 satang.
	if got, want := BahtText(0.29), "ศูนย์บาทยี่สิบเก้าสตางค์"; got != want {
		t.Errorf("BahtText(0.29) = %q, want %q", got, want)
	}
}
