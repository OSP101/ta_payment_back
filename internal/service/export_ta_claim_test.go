package service

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"
)

// A TA's own copy must hold their block and nobody else's — the course sheet
// lists every classmate's hours and pay, which is not theirs to download.
func TestTAClaimSheet_HoldsOnlyTheirOwnBlock(t *testing.T) {
	me, other := uuid.New(), uuid.New()
	day := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	people := []claimant{
		{TAID: other, Name: "คนอื่น ทดสอบ", LevelTH: "ป.ตรี", Rate: 40, PaidBaht: 80, FullBaht: 80,
			Rows: []claimSheetRow{{Date: day, Group: "ปกติ Sec1", Range: "13.00 - 15.00", Note: "เช็คชื่อ"}}},
		{TAID: me, Name: "ฉันเอง ทดสอบ", LevelTH: "ป.ตรี", Rate: 40, PaidBaht: 160, FullBaht: 160,
			Rows: []claimSheetRow{{Date: day, Group: "ปกติ Sec1", Range: "13.00 - 17.00", Note: "สอนปฏิบัติ"}}},
	}
	mine := onlyClaimant(people, me)
	if len(mine) != 1 || mine[0].TAID != me {
		t.Fatalf("onlyClaimant = %+v, want just the caller", mine)
	}
	if got := onlyClaimant(people, uuid.New()); got != nil {
		t.Errorf("a TA with no block got %d blocks, want none", len(got))
	}

	f := excelize.NewFile()
	t.Cleanup(func() { f.Close() })
	st, err := newClaimStyles(f)
	if err != nil {
		t.Fatal(err)
	}
	d := &combinedBookData{CourseCode: "CP362104", AcademicYear: 2569, Semester: 1, RateUGRegular: 40}
	if err := writeClaimSheet(f, st, sheetClaimRegular, "ภาคปกติ", d, mine); err != nil {
		t.Fatal(err)
	}
	starts := blockStarts(t, f, sheetClaimRegular, people)
	if len(starts) != 1 {
		t.Fatalf("found %d blocks, want only the caller's", len(starts))
	}
	if got, _ := f.GetCellValue(sheetClaimRegular, cellAt("B", starts[0])); got != "ฉันเอง ทดสอบ" {
		t.Errorf("block names %q, want the caller", got)
	}
	// Numbered 1 on their own sheet, not by their place in the course list.
	if got, _ := f.GetCellValue(sheetClaimRegular, cellAt("A", starts[0])); got != "1" {
		t.Errorf("ลำดับ = %q, want 1", got)
	}
}
