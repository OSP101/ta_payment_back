package service

import "testing"

// The two calendars this codebase mixes. submission_periods.year_month carries
// a BUDDHIST academic year, work_date a Gregorian one, and every baht figure is
// keyed by the latter — so getting this converter wrong silently files a
// month's money under the wrong document.
func TestGregorianYearMonth(t *testing.T) {
	cases := []struct{ in, want string }{
		// ภาคต้น 2569 — the whole term sits in Gregorian 2026.
		{"2569-06", "2026-06"},
		{"2569-09", "2026-09"},
		{"2569-10", "2026-10"},
		// ภาคปลาย 2569 — พ.ย./ธ.ค. stay in 2026, but ม.ค.–มี.ค. of the SAME
		// academic year are already Gregorian 2027. This is the wrap
		// BulkCreateForTerm applies when stamping starts_on.
		{"2569-11", "2026-11"},
		{"2569-12", "2026-12"},
		{"2569-01", "2027-01"},
		{"2569-03", "2027-03"},
		// ภาคฤดูร้อน (เม.ย.–พ.ค.) also belongs to the following Gregorian year.
		{"2569-04", "2027-04"},
		{"2569-05", "2027-05"},
	}
	for _, c := range cases {
		got, err := gregorianYearMonth(c.in)
		if err != nil {
			t.Fatalf("gregorianYearMonth(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("gregorianYearMonth(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"2569", "2569-13", "2569-00", "abc-06", "2569-xx"} {
		if _, err := gregorianYearMonth(bad); err == nil {
			t.Errorf("gregorianYearMonth(%q) = nil error, want a rejection", bad)
		}
	}
}

// The Thai budget year runs 1 ต.ค. → 30 ก.ย. and is named for the year it ends
// in, so ตุลาคม is already the NEXT year's money.
func TestFiscalYearOf(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"2026-06", 2026},
		{"2026-09", 2026}, // last month of the closing year
		{"2026-10", 2027}, // first month of the new one
		{"2026-12", 2027},
		{"2027-01", 2027},
		{"2027-09", 2027},
	}
	for _, c := range cases {
		got, err := fiscalYearOf(c.in)
		if err != nil {
			t.Fatalf("fiscalYearOf(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("fiscalYearOf(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestFiscalSplit(t *testing.T) {
	months := func(gregs ...string) []TermMonth {
		out := make([]TermMonth, 0, len(gregs))
		for _, g := range gregs {
			out = append(out, TermMonth{YearMonth: g})
		}
		return out
	}

	// ภาคต้น: มิ.ย.–ต.ค. straddles 30 ก.ย., which is the whole reason this
	// feature exists.
	got, err := fiscalSplit(months("2026-06", "2026-07", "2026-08", "2026-09", "2026-10"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Crosses {
		t.Error("มิ.ย.–ต.ค. must be reported as crossing the budget year")
	}
	if len(got.Before) != 4 || got.Before[0] != "2026-06" || got.Before[3] != "2026-09" {
		t.Errorf("Before = %v, want มิ.ย.–ก.ย.", got.Before)
	}
	if len(got.After) != 1 || got.After[0] != "2026-10" {
		t.Errorf("After = %v, want ต.ค. only", got.After)
	}

	// ภาคปลาย: พ.ย.→มี.ค. spans two CALENDAR years but only one BUDGET year,
	// so it must not be split. A naive "is the month ≥ October" rule gets this
	// backwards, putting January before November.
	got, err = fiscalSplit(months("2026-11", "2026-12", "2027-01", "2027-02", "2027-03"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Crosses {
		t.Errorf("พ.ย.–มี.ค. sits in one budget year; got a split: before=%v after=%v", got.Before, got.After)
	}
	if len(got.Before) != 5 {
		t.Errorf("Before = %v, want all five months", got.Before)
	}
}

// An empty request means "the whole term" — the behaviour every caller had
// before month scoping existed, so an older client keeps producing the same
// document. A month the term does not have is refused rather than dropped:
// quietly exporting less than asked for is how money goes missing.
func TestNormalizeMonthSelection(t *testing.T) {
	all := []TermMonth{{YearMonth: "2026-06"}, {YearMonth: "2026-07"}, {YearMonth: "2026-10"}}

	got, err := normalizeMonthSelection(all, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("empty selection = %v, want every month", got)
	}

	// Out of order and duplicated — comes back in calendar order, once each.
	got, err = normalizeMonthSelection(all, []string{"2026-10", "2026-06", "2026-06"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "2026-06" || got[1] != "2026-10" {
		t.Errorf("got %v, want [2026-06 2026-10]", got)
	}

	if _, err := normalizeMonthSelection(all, []string{"2026-08"}); err == nil {
		t.Error("a month outside the term must be refused, not silently dropped")
	}
}
