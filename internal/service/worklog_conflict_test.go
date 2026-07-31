package service

import (
	"strings"
	"testing"
)

// The overlap predicate decides whether a TA gets paid for an hour, so its
// edges are worth pinning without a database in the way.

func blocksFixture() []ownClassBlock {
	return []ownClassBlock{
		{Label: "CP101", Day: 1, StartMin: 9 * 60, EndMin: 12 * 60}, // Mon 09:00–12:00
		{Label: "", Day: 3, StartMin: 13 * 60, EndMin: 15*60 + 30},  // Wed 13:00–15:30, unlabelled
	}
}

func TestFindOwnClassClash_Overlaps(t *testing.T) {
	blocks := blocksFixture()
	cases := []struct {
		name            string
		day, start, end int
		want            bool
	}{
		{"identical slot", 1, 9 * 60, 12 * 60, true},
		{"contained inside", 1, 10 * 60, 11 * 60, true},
		{"straddles the start", 1, 8 * 60, 10 * 60, true},
		{"straddles the end", 1, 11 * 60, 13 * 60, true},
		{"encloses the class", 1, 8 * 60, 13 * 60, true},
		{"one minute of overlap", 1, 11*60 + 59, 13 * 60, true},

		{"ends exactly when class starts", 1, 7 * 60, 9 * 60, false},
		{"starts exactly when class ends", 1, 12 * 60, 14 * 60, false},
		{"same time, different weekday", 2, 9 * 60, 12 * 60, false},
		{"clear of the class", 1, 13 * 60, 15 * 60, false},

		{"second block matches", 3, 14 * 60, 16 * 60, true},
		{"weekday with no class", 5, 9 * 60, 12 * 60, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := findOwnClassClash(blocks, c.day, c.start, c.end) != nil
			if got != c.want {
				t.Errorf("clash = %v, want %v", got, c.want)
			}
		})
	}
}

// Back-to-back teaching is legal — the rule must not quietly steal the hour on
// either side of a class.
func TestFindOwnClassClash_TouchingEdgesAreFree(t *testing.T) {
	blocks := blocksFixture()
	if findOwnClassClash(blocks, 1, 8*60, 9*60) != nil {
		t.Error("a slot ending exactly at the class start must be allowed")
	}
	if findOwnClassClash(blocks, 1, 12*60, 13*60) != nil {
		t.Error("a slot starting exactly at the class end must be allowed")
	}
}

func TestFindOwnClassClash_EmptyTimetable(t *testing.T) {
	if findOwnClassClash(nil, 1, 9*60, 12*60) != nil {
		t.Error("a TA with no timetable can never clash")
	}
}

// The message is the whole point of the feature — a bare refusal leaves the TA
// unable to tell whether to move the entry or fix their timetable.
func TestOwnClassBlockDescribe(t *testing.T) {
	labelled := ownClassBlock{Label: "CP101", Day: 1, StartMin: 9 * 60, EndMin: 12 * 60}
	got := labelled.describe()
	for _, want := range []string{"CP101", "จันทร์", "09:00", "12:00"} {
		if !strings.Contains(got, want) {
			t.Errorf("describe() = %q, missing %q", got, want)
		}
	}

	// An unlabelled row must not render a dangling "()" prefix.
	bare := ownClassBlock{Day: 3, StartMin: 13 * 60, EndMin: 15*60 + 30}
	got = bare.describe()
	if strings.Contains(got, "(") || strings.Contains(got, ")") {
		t.Errorf("unlabelled block should render times only, got %q", got)
	}
	if !strings.Contains(got, "13:00") || !strings.Contains(got, "15:30") {
		t.Errorf("unlabelled block lost its times: %q", got)
	}
}

func TestHHMM(t *testing.T) {
	cases := map[int]string{0: "00:00", 9 * 60: "09:00", 13*60 + 5: "13:05", 23*60 + 59: "23:59"}
	for min, want := range cases {
		if got := hhmm(min); got != want {
			t.Errorf("hhmm(%d) = %q, want %q", min, got, want)
		}
	}
}
