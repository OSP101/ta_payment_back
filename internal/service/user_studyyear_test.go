package service

import "testing"

// deriveStudyYear: 2-digit Buddhist admission prefix vs current academic year.
func TestDeriveStudyYear(t *testing.T) {
	cases := []struct {
		id        string
		cur, want int
	}{
		{"673380001-2", 2569, 3}, // admitted 2567 → year 3 in academic year 2569
		{"663380001-2", 2569, 4}, // 2566 → year 4
		{"693380001-2", 2569, 1}, // 2569 → year 1
		{"593380001-2", 2569, 0}, // 2559 → year 11 → out of range → fallback 0
		{"703380001-2", 2569, 0}, // 2570 → year 0 → out of range → 0
		{"", 2569, 0},            // no id
		{"6", 2569, 0},           // too short
		{"673380001-2", 0, 0},    // no active academic year
	}
	for _, c := range cases {
		if got := deriveStudyYear(c.id, c.cur); got != c.want {
			t.Errorf("deriveStudyYear(%q, %d) = %d, want %d", c.id, c.cur, got, c.want)
		}
	}
}
