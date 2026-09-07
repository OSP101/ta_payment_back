package service

import (
	"strings"
	"testing"
)

// ปะหน้าจ่ายตรง is keyed into the university's ERP to move money, against ONE
// appropriation. A term teaching across 30 September spends from two, so a
// single sheet covering both halves cannot be keyed correctly under either.
//
// The mistake is invisible on the document — it shows names and amounts, and
// nothing on it says which budget year they belong to — so this is refused
// rather than warned about.

func fiscalMonths() []TermMonth {
	return []TermMonth{
		{YearMonth: "2026-06", Label: "มิถุนายน 2569"},
		{YearMonth: "2026-07", Label: "กรกฎาคม 2569"},
		{YearMonth: "2026-08", Label: "สิงหาคม 2569"},
		{YearMonth: "2026-09", Label: "กันยายน 2569"},
		{YearMonth: "2026-10", Label: "ตุลาคม 2569"},
	}
}

// The closing year's own months are one document and must pass.
func TestFiscalGuard_MonthsInsideOneBudgetYearAreAllowed(t *testing.T) {
	all := fiscalMonths()
	if err := assertOneFiscalYear(all, []string{"2026-06", "2026-07", "2026-08", "2026-09"}); err != nil {
		t.Errorf("the closing year's four months were refused: %v", err)
	}
	if err := assertOneFiscalYear(all, []string{"2026-10"}); err != nil {
		t.Errorf("the new year's month was refused on its own: %v", err)
	}
}

// THE ONE THAT PROTECTS THE TRANSFER. September and October sit either side of
// 30 September.
func TestFiscalGuard_StraddlingSelectionIsRefused(t *testing.T) {
	all := fiscalMonths()
	err := assertOneFiscalYear(all, []string{"2026-09", "2026-10"})
	if err == nil {
		t.Fatal("one document was allowed to cover two budget years — finance " +
			"would key half of it against the wrong appropriation")
	}
	// The officer has to know how to split it, so both halves must be named.
	for _, want := range []string{"กันยายน 2569", "ตุลาคม 2569"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
}

// Months are listed in calendar order. Sorting the Thai LABELS instead of the
// keys yields กรกฎาคม, กันยายน, มิถุนายน, สิงหาคม — alphabetical, and a jumble to
// anyone checking the list against a calendar.
func TestFiscalGuard_NamesMonthsInCalendarOrder(t *testing.T) {
	all := fiscalMonths()
	err := assertOneFiscalYear(all, []string{"2026-06", "2026-07", "2026-08", "2026-09", "2026-10"})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	msg := err.Error()
	prev := -1
	for _, m := range []string{"มิถุนายน 2569", "กรกฎาคม 2569", "สิงหาคม 2569", "กันยายน 2569"} {
		at := strings.Index(msg, m)
		if at < 0 {
			t.Fatalf("%s missing from: %s", m, msg)
		}
		if at < prev {
			t.Errorf("%s appears out of calendar order in: %s", m, msg)
		}
		prev = at
	}
}

// "ทั้งภาคเรียน" on a crossing term is the easiest way to get this wrong, and
// the one a tired officer is most likely to press.
func TestFiscalGuard_WholeTermIsRefusedWhenTheTermCrosses(t *testing.T) {
	all := fiscalMonths()
	var every []string
	for _, m := range all {
		every = append(every, m.YearMonth)
	}
	if err := assertOneFiscalYear(all, every); err == nil {
		t.Error("selecting the whole of a crossing term produced one document")
	}
}

// A term that does not cross the boundary must not be inconvenienced by any of
// this — ภาคปลาย runs พ.ย.→มี.ค., two calendar years inside one budget year.
func TestFiscalGuard_SecondSemesterSpansTwoCalendarYearsButOneBudgetYear(t *testing.T) {
	all := []TermMonth{
		{YearMonth: "2026-11", Label: "พฤศจิกายน 2569"},
		{YearMonth: "2026-12", Label: "ธันวาคม 2569"},
		{YearMonth: "2027-01", Label: "มกราคม 2570"},
		{YearMonth: "2027-02", Label: "กุมภาพันธ์ 2570"},
		{YearMonth: "2027-03", Label: "มีนาคม 2570"},
	}
	var every []string
	for _, m := range all {
		every = append(every, m.YearMonth)
	}
	if err := assertOneFiscalYear(all, every); err != nil {
		t.Errorf("ภาคปลาย was refused, but พ.ย.–มี.ค. is one budget year: %v", err)
	}
}
