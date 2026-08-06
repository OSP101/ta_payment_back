package service

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"
)

// The workbook is what lands in the management meeting — its sheets must carry
// the same figures the page shows, Thai programme names identical to the chart
// legend, and (since 06/08/2026) a cover sheet with the KPI block and two
// embedded charts.
func TestAnalyticsWorkbookMirrorsTheStruct(t *testing.T) {
	a := &TermAnalytics{
		TermLabel:       "2569/1",
		BudgetUsed:      40008.36,
		BudgetAllocated: 112500,
		ApprovedHours:   1523,
		TotalTAs:        6,
		CoursesWithTA:   7,
		CoursesOpen:     127,
		Monthly:         []MonthSpend{{YearMonth: "2026-06", Baht: 2360}, {YearMonth: "2026-07", Baht: 6760}},
		Curricula: []CurriculumStat{
			{Curriculum: "IT", CoursesOpen: 33, CoursesWithTA: 7, TAs: 6, SpentBaht: 40008.36, CapBaht: 112500},
			{Curriculum: "OTHER", CoursesOpen: 3},
			{Curriculum: ""}, // unknown — must render as ยังไม่ระบุ, not crash
		},
		Courses: []CourseSpendStat{{
			TeachingCourseID: uuid.New(), Code: "SC362102", NameTH: "SOFTWARE ENGINEERING",
			Curriculum: "IT", TAs: 4, ApprovedHours: 542, SpentBaht: 11888.5, CapBaht: 12000, OverBudget: true,
		}},
	}
	body, err := AnalyticsWorkbook(a)
	if err != nil {
		t.Fatalf("AnalyticsWorkbook: %v", err)
	}
	f, err := excelize.OpenReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("output is not a readable xlsx: %v", err)
	}
	defer f.Close()

	wantSheets := []string{"สรุป", "รายเดือน", "รายหลักสูตร", "รายวิชา"}
	got := f.GetSheetList()
	if len(got) != len(wantSheets) {
		t.Fatalf("sheets = %v, want %v", got, wantSheets)
	}
	for i, s := range wantSheets {
		if got[i] != s {
			t.Fatalf("sheet %d = %q, want %q", i, got[i], s)
		}
	}

	cell := func(sheet, ref string) string {
		v, err := f.GetCellValue(sheet, ref)
		if err != nil {
			t.Fatalf("GetCellValue(%s!%s): %v", sheet, ref, err)
		}
		return v
	}

	// Cover: title, term in the subtitle, the headline KPI.
	if cell("สรุป", "A1") != "รายงานการใช้งบประมาณผู้ช่วยสอน (TA)" {
		t.Fatalf("cover title wrong: %q", cell("สรุป", "A1"))
	}
	if got := cell("สรุป", "A3"); got == "" || !bytes.Contains([]byte(got), []byte("2569/1")) {
		t.Fatalf("cover subtitle should carry the term, got %q", got)
	}
	// Formatted value — proves both the figure and the #,##0.00 format.
	if cell("สรุป", "C5") != "40,008.36" {
		t.Fatalf("cover KPI เบิกจ่ายแล้ว = %q, want 40,008.36", cell("สรุป", "C5"))
	}

	// Monthly: Thai month labels (BE) and a cumulative column that actually
	// accumulates.
	if cell("รายเดือน", "A2") != "มิ.ย. 69" || cell("รายเดือน", "A3") != "ก.ค. 69" {
		t.Fatalf("month labels wrong: %q / %q", cell("รายเดือน", "A2"), cell("รายเดือน", "A3"))
	}
	if cell("รายเดือน", "C3") != "9,120.00" {
		t.Fatalf("cumulative = %q, want 9,120.00 (2360+6760)", cell("รายเดือน", "C3"))
	}

	// Curriculum: display names must not drift off the frontend legend.
	if cell("รายหลักสูตร", "A2") != "เทคโนโลยีสารสนเทศ" {
		t.Fatalf("curriculum display name drifted: %q", cell("รายหลักสูตร", "A2"))
	}
	if cell("รายหลักสูตร", "A3") != "คณะอื่น ๆ" || cell("รายหลักสูตร", "A4") != "ยังไม่ระบุ" {
		t.Fatalf("OTHER/unknown names wrong: %q / %q", cell("รายหลักสูตร", "A3"), cell("รายหลักสูตร", "A4"))
	}

	// Courses: the over-budget row carries the status text; % is stored as a
	// fraction so the 0.0% format renders it, not a pre-multiplied number.
	if cell("รายวิชา", "A2") != "SC362102" {
		t.Fatalf("course row wrong: %q", cell("รายวิชา", "A2"))
	}
	if got := cell("รายวิชา", "I2"); got != "เกินเพดาน" {
		t.Fatalf("status = %q, want เกินเพดาน", got)
	}
	if got := cell("รายวิชา", "H2"); got != "99.1%" {
		t.Fatalf("pct should format as 99.1%% (fraction 11888.5/12000 under 0.0%%), got %q", got)
	}

	// The cover must actually embed charts — this is what "ไม่มีกราฟ" was
	// about. excelize has no chart-listing API, so count the chart parts in
	// the zip package itself.
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("open xlsx as zip: %v", err)
	}
	charts := 0
	for _, zf := range zr.File {
		if strings.HasPrefix(zf.Name, "xl/charts/chart") {
			charts++
		}
	}
	if charts < 2 {
		t.Fatalf("cover should embed at least 2 charts, found %d chart parts", charts)
	}
}
