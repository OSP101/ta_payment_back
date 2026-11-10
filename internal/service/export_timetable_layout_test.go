package service

import (
	"fmt"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

func TestTimetableCourseLinesFitLecturerColumns(t *testing.T) {
	f, err := excelize.OpenFile("../../assets/templates/ta_claim_form.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const sheet = "ตารางสอน"
	style, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Family: "TH Sarabun New", Size: 12},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := "SC363002 STRATEGIC MANAGEMENT OF INFORMATION TECHNOLOGY"
	for _, col := range []string{"I", "R"} {
		for row := 23; row <= 25; row++ {
			if err := setTimetableCourseLine(f, sheet, col, row, text, 12, style); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A shorter line in the second lecturer's column must not shrink the row.
	longText := strings.Repeat("ชื่อวิชาภาษาไทย INFORMATION TECHNOLOGY ", 6)
	if err := setTimetableCourseLine(f, sheet, "I", 24, longText, 12, style); err != nil {
		t.Fatal(err)
	}
	height, _ := f.GetRowHeight(sheet, 24)
	if height <= 24 {
		t.Fatalf("long course row height = %v", height)
	}
	if err := setTimetableCourseLine(f, sheet, "R", 24, text, 12, style); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.GetRowHeight(sheet, 24); got != height {
		t.Fatalf("second lecturer shrank row: %v < %v", got, height)
	}
	buf, err := f.WriteToBuffer()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := excelize.OpenReader(buf)
	if err != nil {
		t.Fatal(err)
	}
	defer saved.Close()
	merges, err := saved.GetMergeCells(sheet)
	if err != nil {
		t.Fatal(err)
	}
	for col, end := range map[string]string{"I": "Q", "R": "Z"} {
		for row := 23; row <= 25; row++ {
			start, finish := fmt.Sprintf("%s%d", col, row), fmt.Sprintf("%s%d", end, row)
			found := false
			for _, m := range merges {
				if m.GetStartAxis() == start && m.GetEndAxis() == finish {
					found = true
				}
			}
			if !found {
				t.Errorf("missing merge %s:%s", start, finish)
			}
			want := text
			if col == "I" && row == 24 {
				want = longText
			}
			if got, _ := saved.GetCellValue(sheet, start); got != want {
				t.Errorf("%s text changed: %q", start, got)
			}
		}
	}
}
