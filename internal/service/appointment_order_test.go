package service

import "testing"

func TestEnsureBuddhistEra(t *testing.T) {
	cases := []struct{ in, want string }{
		{"14 มกราคม 2569", "14 มกราคม พ.ศ. 2569"},
		{"14 มกราคม พ.ศ. 2569", "14 มกราคม พ.ศ. 2569"}, // already has era
		{"14  มกราคม  2569", "14 มกราคม พ.ศ. 2569"},    // extra spaces collapse
		{"2569", "2569"}, // year alone — leave as typed
		{"14 มกราคม", "14 มกราคม"},             // no year
		{"14 January 25xx", "14 January 25xx"}, // non-numeric tail
		{"", ""},
	}
	for _, c := range cases {
		if got := ensureBuddhistEra(c.in); got != c.want {
			t.Errorf("ensureBuddhistEra(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
