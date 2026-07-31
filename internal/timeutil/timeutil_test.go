package timeutil

import (
	"testing"
	"time"
)

func TestBangkokIsUTCPlus7(t *testing.T) {
	// A fixed instant, not "now" — the assertion must not drift with the clock.
	ref := time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC)
	_, offset := ref.In(Bangkok).Zone()
	if offset != 7*60*60 {
		t.Fatalf("Bangkok offset = %ds, want %ds", offset, 7*60*60)
	}
}

// Thailand abolished daylight saving long before this system's date range, so
// the offset must be identical in every month. A tz database that resolved to
// some other zone would show a summer/winter split here.
func TestBangkokHasNoDaylightSaving(t *testing.T) {
	for _, m := range []time.Month{time.January, time.April, time.July, time.October} {
		ref := time.Date(2026, m, 15, 12, 0, 0, 0, time.UTC)
		if _, off := ref.In(Bangkok).Zone(); off != 7*60*60 {
			t.Errorf("%s offset = %ds, want %ds", m, off, 7*60*60)
		}
	}
}

// The whole point of the package: an instant that is still "yesterday" in UTC
// must report today's Bangkok calendar date. This is the seven-hour window at
// every month boundary that the money rules were previously getting wrong.
func TestBangkokDateAheadOfUTCAcrossMonthBoundary(t *testing.T) {
	// 2026-07-31 20:00 UTC == 2026-08-01 03:00 Bangkok.
	ref := time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC)

	if y, m, d := ref.Date(); y != 2026 || m != time.July || d != 31 {
		t.Fatalf("precondition: UTC date = %d-%02d-%02d, want 2026-07-31", y, m, d)
	}
	y, m, d := ref.In(Bangkok).Date()
	if y != 2026 || m != time.August || d != 1 {
		t.Fatalf("Bangkok date = %d-%02d-%02d, want 2026-08-01", y, m, d)
	}
}

func TestNowIsInBangkok(t *testing.T) {
	if got := Now().Location(); got != Bangkok {
		t.Fatalf("Now() location = %v, want %v", got, Bangkok)
	}
}

func TestTodayIsMidnightBangkok(t *testing.T) {
	today := Today()
	if h, m, s := today.Clock(); h != 0 || m != 0 || s != 0 {
		t.Errorf("Today() clock = %02d:%02d:%02d, want 00:00:00", h, m, s)
	}
	if today.Location() != Bangkok {
		t.Errorf("Today() location = %v, want Bangkok", today.Location())
	}
	// Same calendar day as Now().
	ny, nm, nd := Now().Date()
	ty, tm, td := today.Date()
	if ny != ty || nm != tm || nd != td {
		t.Errorf("Today() = %d-%02d-%02d, Now() = %d-%02d-%02d — must be the same day",
			ty, tm, td, ny, nm, nd)
	}
}

// ParseDate must anchor a bare calendar date to Bangkok, not UTC. Parsing as
// UTC and then comparing against Now() as instants is off by seven hours.
func TestParseDateAnchorsToBangkok(t *testing.T) {
	got, err := ParseDate("2026-08-01")
	if err != nil {
		t.Fatalf("ParseDate: %v", err)
	}
	want := time.Date(2026, 8, 1, 0, 0, 0, 0, Bangkok)
	if !got.Equal(want) {
		t.Fatalf("ParseDate = %v, want %v", got, want)
	}
	// And it is genuinely a different instant than the naive UTC reading.
	naive, _ := time.Parse("2006-01-02", "2026-08-01")
	if got.Equal(naive) {
		t.Errorf("ParseDate must not equal the UTC-anchored parse of the same string")
	}
}

func TestParseDateRejectsGarbage(t *testing.T) {
	if _, err := ParseDate("not-a-date"); err == nil {
		t.Fatal("ParseDate must reject a malformed date")
	}
}
