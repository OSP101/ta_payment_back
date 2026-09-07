package service

import (
	"testing"

	"github.com/xuri/excelize/v2"
)

// The หลักฐานการจ่ายเงินอื่น ๆ sheet is compared side by side against the
// college's own file (docs/15.CP362104.xlsx) whenever it is touched, because it
// is the page the finance office reads the total off. Four of its details were
// carried by that file and not by ours, and none of them shows up in a
// comparison of the figures — only on paper:
//
//	· the total and the amount in words are ONE ruled band across the table's
//	  full width, not two loose lines below it;
//	· the amount in words sits on a silver ground;
//	· both signature blocks are centred under their own rule;
//	· หมายเหตุ is left empty for the office to write in.
//
// The figures are asserted elsewhere. This file asserts how the sheet looks.

// evidenceStyle reads back the effective style of one cell.
func evidenceStyle(t *testing.T, f *excelize.File, cell string) *excelize.Style {
	t.Helper()
	id, err := f.GetCellStyle(sheetEvidenceRegular, cell)
	if err != nil {
		t.Fatal(err)
	}
	s, err := f.GetStyle(id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// borderOf returns the style of one side ("left"/"right"/"top"/"bottom"), or ""
// when that side carries no rule.
func borderOf(s *excelize.Style, side string) string {
	for _, b := range s.Border {
		if b.Type == side {
			return borderStyleName(b.Style)
		}
	}
	return ""
}

// borderStyleName maps excelize's numeric border styles back to the names the
// writers use, for the weights these documents draw with.
func borderStyleName(n int) string {
	switch n {
	case 1:
		return "thin"
	case 2:
		return "medium"
	case 6:
		return "double"
	case 7:
		return "hair"
	}
	return "other"
}

// The fixture's own geometry: two people, so the table's last name is row 11,
// row 12 closes it, and the band follows.
const (
	evSumRow   = 13
	evWordsRow = 14
	evSigRow   = 17
)

func TestEvidenceSheet_TotalAndWordsFormOneRuledBand(t *testing.T) {
	f, _, _ := combinedFixture(t)
	cols := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J"}

	for _, r := range []int{evSumRow, evWordsRow} {
		for _, col := range cols {
			cell := cellAt(col, r)
			s := evidenceStyle(t, f, cell)
			if got := borderOf(s, "top"); got != "thin" {
				t.Errorf("%s has no rule above it (%q) — the band must run the "+
					"full width of the table, not stop under one column", cell, got)
			}
			if got := borderOf(s, "bottom"); got != "thin" {
				t.Errorf("%s has no rule below it (%q)", cell, got)
			}
		}
		// The band is closed at both ends, or it reads as an open row rather
		// than as part of the table.
		if got := borderOf(evidenceStyle(t, f, cellAt("A", r)), "left"); got != "thin" {
			t.Errorf("row %d is open on the left (%q)", r, got)
		}
		if got := borderOf(evidenceStyle(t, f, cellAt("J", r)), "right"); got != "thin" {
			t.Errorf("row %d is open on the right (%q)", r, got)
		}
	}
}

// The figure the office keys into ERP keeps the heavier sides the college's
// file gives it — laying the band down must not flatten them to thin.
func TestEvidenceSheet_TotalKeepsItsMediumSideRules(t *testing.T) {
	f, _, _ := combinedFixture(t)
	s := evidenceStyle(t, f, cellAt("G", evSumRow))
	for _, side := range []string{"left", "right"} {
		if got := borderOf(s, side); got != "medium" {
			t.Errorf("the total's %s rule is %q, want medium", side, got)
		}
	}
}

func TestEvidenceSheet_AmountInWordsSitsOnTheSilverBand(t *testing.T) {
	f, _, _ := combinedFixture(t)
	for _, col := range []string{"C", "D", "E", "F", "G", "H"} {
		cell := cellAt(col, evWordsRow)
		s := evidenceStyle(t, f, cell)
		if s.Fill.Type != "pattern" || s.Fill.Pattern != 1 ||
			len(s.Fill.Color) == 0 || s.Fill.Color[0] != fillSilver {
			t.Errorf("%s is not on the silver ground (type=%q pattern=%d color=%v)",
				cell, s.Fill.Type, s.Fill.Pattern, s.Fill.Color)
		}
	}
}

// THE ONE THE OFFICE REPORTED. A signature line left-aligned in its merged
// block puts the name hard against the column edge while the rule above it runs
// the whole width, so the name reads as belonging to the neighbouring column.
func TestEvidenceSheet_BothSignatureBlocksAreCentred(t *testing.T) {
	f, d, _ := combinedFixture(t)
	blocks := map[string]string{"B": "อาจารย์ผู้สอน", "G": "ผู้รับรอง"}
	for col, who := range blocks {
		// The rule, the name and the position line — every line of the block.
		for r := evSigRow; r <= evSigRow+2; r++ {
			cell := cellAt(col, r)
			if v, _ := f.GetCellValue(sheetEvidenceRegular, cell); v == "" {
				t.Fatalf("%s (%s) is empty — the block moved, and this test is "+
					"now asserting the alignment of a blank cell", cell, who)
			}
			if got := evidenceStyle(t, f, cell).Alignment.Horizontal; got != "center" {
				t.Errorf("%s (%s) is aligned %q, want center", cell, who, got)
			}
		}
	}
	// Guard the premise: the lecturer really is the left-hand signer, so the
	// block being centred is the one the office was looking at.
	if v, _ := f.GetCellValue(sheetEvidenceRegular, cellAt("B", evSigRow+1)); v != "("+d.LecturerName+")" {
		t.Errorf("the left block names %q, not the lecturer", v)
	}
}

// หมายเหตุ is the office's own column: they write against a row by hand when a
// claim needs a note. It used to be pre-filled with the course code on every
// row — already printed in the รหัสวิชา line above the table — which left them
// nowhere to write. The college's file leaves it empty, and so do we.
func TestEvidenceSheet_NoteColumnIsLeftForTheOffice(t *testing.T) {
	f, d, people := combinedFixture(t)
	for i := range people {
		cell := cellAt("J", 10+i)
		v, err := f.GetCellValue(sheetEvidenceRegular, cell)
		if err != nil {
			t.Fatal(err)
		}
		if v != "" {
			t.Errorf("%s is pre-filled with %q, want empty", cell, v)
		}
	}
	// The column is still ruled and still headed — empty, not dropped.
	if v, _ := f.GetCellValue(sheetEvidenceRegular, "J8"); v != "หมายเหตุ" {
		t.Errorf("the หมายเหตุ heading is %q", v)
	}
	if got := borderOf(evidenceStyle(t, f, cellAt("J", 10)), "right"); got != "thin" {
		t.Errorf("the หมายเหตุ column lost its rule (%q)", got)
	}
	// And the code itself has not gone missing from the sheet.
	if v, _ := f.GetCellValue(sheetEvidenceRegular, "B6"); v != "รหัสวิชา "+d.CourseCode {
		t.Errorf("B6 = %q, the course code no longer heads the sheet", v)
	}
}
