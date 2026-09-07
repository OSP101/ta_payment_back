package service

import (
	"fmt"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

// ปะหน้าจ่ายตรง is held to the office's own file, docs/ปะหน้าจ่ายตรง-CY.xls.
// The generated sheet had drifted from it on nearly every axis at once — the
// four heading lines printed left-aligned, unbolded and three points too small;
// the table ran a sixth column the template does not have; every rule was drawn
// in grey rather than black; the grand total had no double underline; and an
// unruled spacer row tore the grid open directly above the figure the finance
// office keys into ERP.
//
// These tests pin the shape, not the figures — the figures are asserted in
// export_transfer_cover_test.go.

func coverStyle(t *testing.T, wb *excelize.File, sheet, cell string) *excelize.Style {
	t.Helper()
	id, err := wb.GetCellStyle(sheet, cell)
	if err != nil {
		t.Fatal(err)
	}
	s, err := wb.GetStyle(id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// coverFixture builds a one-sheet cover with two payable TAs, so the data rows,
// the closing row and the total all have known addresses:
//
//	5 header · 6,7 data · 8 the closing ruled row · 9 total · 12–14 signature
func coverFixture(t *testing.T) (*excelize.File, string) {
	t.Helper()
	f := newTCFixture(t)
	courseA, regA, _ := f.insertCourse(tcCourseOpts{Code: "CP111", Curriculum: "CY", LectureHrs: 100})
	f.assignTA(f.newTA("หนึ่ง ทดสอบ", "undergrad"), courseA, regA, "undergrad", []int{1})
	f.assignTA(f.newTA("สอง ทดสอบ", "undergrad"), courseA, regA, "undergrad", []int{2})
	f.markExported(courseA)

	body, _, err := f.svc.BuildTransferCoverWorkbook(f.ctx, f.actor(), f.termID, nil, "undergrad")
	if err != nil {
		t.Fatalf("BuildTransferCoverWorkbook: %v", err)
	}
	wb := openWorkbookBytes(t, body)
	t.Cleanup(func() { wb.Close() })
	return wb, "CY ปกติ"
}

// The template is FIVE columns. The sixth (หมายเหตุ, carrying ใหม่/เก่า) was
// ours, not theirs — it survives on the preview screen, not on the paper.
func TestTransferCoverSheet_IsFiveColumnsWide(t *testing.T) {
	wb, sheet := coverFixture(t)

	if got, _ := wb.GetCellValue(sheet, "E5"); got != "หมายเลขพร้อมเพย์" {
		t.Errorf("E5 = %q, want the last template heading หมายเลขพร้อมเพย์", got)
	}
	for _, cell := range []string{"F5", "F6", "F7"} {
		if got, _ := wb.GetCellValue(sheet, cell); got != "" {
			t.Errorf("%s = %q — the sheet has grown a sixth column the office's "+
				"template does not have", cell, got)
		}
	}
	// The heading block and the signature block must span the same five.
	for _, want := range []string{"A1:E1", "A2:E2", "A3:E3", "A4:E4"} {
		if !hasMerge(t, wb, sheet, want) {
			t.Errorf("heading merge %s is missing — a heading centred over the "+
				"wrong width does not sit over the table", want)
		}
	}
}

// All four heading lines are centred and bold in the template, at 20 for the
// document's name and 18 beneath. Left-aligned 15pt text was the single loudest
// difference on the page.
func TestTransferCoverSheet_HeadingBlockMatchesTheTemplate(t *testing.T) {
	wb, sheet := coverFixture(t)
	for cell, size := range map[string]float64{"A1": 20, "A2": 18, "A3": 18, "A4": 18} {
		s := coverStyle(t, wb, sheet, cell)
		if s.Font == nil || s.Font.Size != size || !s.Font.Bold {
			t.Errorf("%s font = %+v, want TH Sarabun New %.0f bold", cell, s.Font, size)
		}
		if s.Alignment == nil || s.Alignment.Horizontal != "center" {
			t.Errorf("%s is not centred (%+v)", cell, s.Alignment)
		}
	}
	// The last heading line carries the rule that separates the block from the
	// table, and it has to run the whole width, not stop under column A.
	for _, cell := range []string{"A4", "C4", "E4"} {
		if got := borderOf(coverStyle(t, wb, sheet, cell), "bottom"); got != "thin" {
			t.Errorf("%s has no rule under the heading block (%q)", cell, got)
		}
	}
}

// Grey rules read as a different document beside the real one — the claim book
// made the same mistake and was corrected the same way.
func TestTransferCoverSheet_RulesAreBlack(t *testing.T) {
	wb, sheet := coverFixture(t)
	for _, cell := range []string{"A5", "E5", "A6", "D6", "E7"} {
		s := coverStyle(t, wb, sheet, cell)
		for _, b := range s.Border {
			if b.Color != "" && b.Color != "000000" && b.Color != "FF000000" {
				t.Errorf("%s draws its %s rule in %s, want black", cell, b.Type, b.Color)
			}
		}
	}
}

// The list runs into its total as one block: an empty RULED row, then the
// total. An unstyled gap there leaves the figure the office keys sitting under
// nothing.
func TestTransferCoverSheet_ListClosesIntoTheTotalWithoutAGap(t *testing.T) {
	wb, sheet := coverFixture(t)
	const closingRow, totalRow = 8, 9

	if got, _ := wb.GetCellValue(sheet, fmt.Sprintf("A%d", totalRow)); got != "รวม" {
		t.Fatalf("A%d = %q, want รวม — the block moved and this test is now "+
			"asserting the wrong rows", totalRow, got)
	}
	for _, col := range []string{"A", "B", "C", "D", "E"} {
		cell := fmt.Sprintf("%s%d", col, closingRow)
		if got := borderOf(coverStyle(t, wb, sheet, cell), "left"); got == "" {
			t.Errorf("%s is unruled — the table breaks open right above the total", cell)
		}
	}
}

// Accounting convention, and the template follows it: the grand total is closed
// with a double rule.
func TestTransferCoverSheet_GrandTotalIsDoubleUnderlined(t *testing.T) {
	wb, sheet := coverFixture(t)
	s := coverStyle(t, wb, sheet, "D9")
	if got := borderOf(s, "bottom"); got != "double" {
		t.Errorf("the total's bottom rule is %q, want double", got)
	}
	if f, _ := wb.GetCellFormula(sheet, "D9"); f == "" {
		t.Error("the total is not a live SUM")
	}
}

// hasMerge reports whether the sheet carries exactly this merged range.
func hasMerge(t *testing.T, wb *excelize.File, sheet, want string) bool {
	t.Helper()
	merges, err := wb.GetMergeCells(sheet)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range merges {
		if m.GetStartAxis()+":"+m.GetEndAxis() == want {
			return true
		}
	}
	return false
}

// THE HEADING MUST NAME THE MONTHS THAT ARE ACTUALLY ON THE SHEET.
//
// It used to print the term's own starts_on–ends_on span, so every slice of a
// term was headed with the whole term. Because งบแผ่นดิน closes 30 กันยายน, a
// ภาคต้น that teaches มิ.ย.–ต.ค. is ALWAYS issued in slices — the มิ.ย.–ก.ย.
// cover was headed "(เดือนมิถุนายน - ตุลาคม 2569)", telling the finance office
// it covered a month whose money is not on the sheet and whose appropriation
// had already closed.
func TestThaiSelectedMonthsLabel(t *testing.T) {
	for _, c := range []struct {
		name   string
		months []string
		want   string
	}{
		{"one month stands alone",
			[]string{"2026-06"}, "มิถุนายน 2569"},
		{"a run writes the year once",
			[]string{"2026-06", "2026-07", "2026-08", "2026-09"}, "มิถุนายน - กันยายน 2569"},
		{"unsorted input is still read in calendar order",
			[]string{"2026-09", "2026-06", "2026-08", "2026-07"}, "มิถุนายน - กันยายน 2569"},
		{"a run across the new year names both years",
			[]string{"2025-12", "2026-01"}, "ธันวาคม 2568 - มกราคม 2569"},
		// The one that matters most: a gap must not be collapsed into a range.
		{"a gap is spelled out, never collapsed to first-last",
			[]string{"2026-06", "2026-08"}, "มิถุนายน 2569, สิงหาคม 2569"},
		{"duplicates do not repeat",
			[]string{"2026-06", "2026-06", "2026-07"}, "มิถุนายน - กรกฎาคม 2569"},
		{"nothing selected yields nothing rather than a stray bracket",
			nil, ""},
	} {
		if got := thaiSelectedMonthsLabel(c.months); got != c.want {
			t.Errorf("%s: %v → %q, want %q", c.name, c.months, got, c.want)
		}
	}
}

// End to end: the fiscal slice the document was issued for is the slice named
// on its face — with the term carrying MORE months than the selection, which is
// the only shape in which this can go wrong.
func TestTransferCoverSheet_HeadingNamesTheSelectedMonthsOnly(t *testing.T) {
	f := newTCFixture(t)
	courseA, regA, _ := f.insertCourse(tcCourseOpts{Code: "CP111", Curriculum: "CY", LectureHrs: 100})
	f.assignTA(f.newTA("หนึ่ง ทดสอบ", "undergrad"), courseA, regA, "undergrad", []int{1})
	f.markExported(courseA)

	// A ภาคต้น that teaches มิ.ย.–ต.ค.: five periods, of which งบแผ่นดิน can
	// only ever pay the first four on one document.
	for _, mm := range []string{"06", "07", "08", "09", "10"} {
		if "2569-"+mm == f.ym {
			continue // already created by markExported
		}
		f.exec(`INSERT INTO submission_periods (id, term_id, year_month, due_date, label, starts_on, is_closed)
		        VALUES (gen_random_uuid(), $1, $2, '2026-12-31'::date, $3, '2026-06-01'::date, FALSE)`,
			f.termID, "2569-"+mm, "รอบ 2569-"+mm)
	}

	greg, err := gregorianYearMonth(f.ym)
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := f.svc.BuildTransferCoverWorkbook(f.ctx, f.actor(), f.termID, []string{greg}, "undergrad")
	if err != nil {
		t.Fatalf("BuildTransferCoverWorkbook: %v", err)
	}
	wb := openWorkbookBytes(t, body)
	defer wb.Close()

	got, _ := wb.GetCellValue("CY ปกติ", "A4")
	want := thaiSelectedMonthsLabel([]string{greg})
	if !strings.Contains(got, "(เดือน"+want+")") {
		t.Errorf("A4 = %q, want it to name exactly (เดือน%s) — the heading is not "+
			"the months on the sheet", got, want)
	}
	// The term now spans five months; a one-month document must claim none of
	// the other four.
	for _, other := range []string{"มิถุนายน", "กรกฎาคม", "สิงหาคม", "กันยายน", "ตุลาคม"} {
		if !strings.Contains(want, other) && strings.Contains(got, other) {
			t.Errorf("A4 = %q names %s, which is not in this document", got, other)
		}
	}
}

// Names print with their คำนำหน้า, as the office's template writes them
// ("นายอภิภัทร คําพุทธ") — but the sheet is still ordered by ชื่อ.
//
// Gluing the prefix on and sorting the result would group the whole document
// into นางสาว first, then นาย, before any given name was consulted: a list
// nobody can scan for a person, on the page the finance office reads name by
// name against a bank file.
func TestTransferCoverSheet_NamesCarryThePrefixButSortByGivenName(t *testing.T) {
	f := newTCFixture(t)
	courseA, regA, _ := f.insertCourse(tcCourseOpts{Code: "CP111", Curriculum: "CY", LectureHrs: 100})
	// Given names in order ก, ข, ค — with prefixes that would reverse two of
	// them if the prefix were part of the key (นางสาว sorts before นาย).
	for _, p := range []struct{ prefix, name string }{
		{"นาย", "กมล"},
		{"นางสาว", "ขวัญ"},
		{"นาย", "คมสัน"},
	} {
		ta := f.newTA(p.name, "undergrad")
		f.exec(`INSERT INTO ta_profiles (user_id, status, current_round, prefix)
		        VALUES ($1,'approved',1,$2)
		        ON CONFLICT (user_id) DO UPDATE SET prefix = $2`, ta, p.prefix)
		f.assignTA(ta, courseA, regA, "undergrad", []int{1})
	}
	f.markExported(courseA)

	body, _, err := f.svc.BuildTransferCoverWorkbook(f.ctx, f.actor(), f.termID, nil, "undergrad")
	if err != nil {
		t.Fatalf("BuildTransferCoverWorkbook: %v", err)
	}
	wb := openWorkbookBytes(t, body)
	defer wb.Close()

	var got []string
	for r := 6; r <= 8; r++ {
		v, _ := wb.GetCellValue("CY ปกติ", fmt.Sprintf("B%d", r))
		got = append(got, v)
	}
	want := []string{"นายกมล ทดสอบ", "นางสาวขวัญ ทดสอบ", "นายคมสัน ทดสอบ"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("B%d = %q, want %q (full column: %v)", 6+i, got[i], want[i], got)
		}
	}
}
