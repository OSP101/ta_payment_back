package service

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"
)

// The college files a DIFFERENT หลักฐานการจ่ายเงิน for บัณฑิตศึกษา than for
// ป.ตรี, and the difference is a column layout, not wording — compare their own
// docs/15.CP362104.xlsx (one total column) with docs/14. CP363761-บัณฑิต.xls
// (one column per month). These tests pin the graduate shape, and pin that the
// undergrad document is left alone.

// gradLogHours writes an approved regular-track work log on an explicit date,
// so a test can put hours in two different calendar months — day() only ever
// produces dates inside the current one.
//
// Anchored at midnight rather than 09:00 so a block of up to 23 hours still
// lands on a valid clock: these documents are about monthly totals, and a
// realistic start time would cap the fixture at six.
func (f *tcFixture) gradLogHours(assignID uuid.UUID, date, activity string, hours int) {
	f.t.Helper()
	if hours < 1 || hours > 23 {
		f.t.Fatalf("gradLogHours: %d hours does not fit in one day", hours)
	}
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES (gen_random_uuid(), $1, $2::date, '00:00', $3, $4, $5, 'approved')`,
		assignID, date, fmt.Sprintf("%02d:00", hours), hours, activity)
}

// A course with no graduate TA must produce no graduate workbook at all — not
// an empty one, and not an error. Most courses are undergrad-only and their
// export pack must be unchanged.
func TestBuildGradEvidenceWorkbook_NilForUndergradOnlyCourse(t *testing.T) {
	f := newTCFixture(t)
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP111111", Curriculum: "CY", LectureHrs: 3})
	ta := f.newTA("ปริญญาตรี", "undergrad")
	f.assignTA(ta, courseID, regSec, "undergrad", []int{10, 11})

	book, err := f.svc.BuildGradEvidenceWorkbook(f.ctx, courseID, nil)
	if err != nil {
		t.Fatalf("BuildGradEvidenceWorkbook: %v", err)
	}
	if book != nil {
		t.Fatal("an undergrad-only course must produce no graduate evidence workbook")
	}
}

// The headline difference from the undergrad form: hours are broken out per
// month, and the total is their sum rather than a single figure typed once.
func TestBuildGradEvidenceWorkbook_RegularSplitsHoursByMonth(t *testing.T) {
	f := newTCFixture(t)
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP363761", Curriculum: "CY", LectureHrs: 3})
	ta := f.newTA("บัณฑิตปกติ", "phd")
	assign := f.assignTA(ta, courseID, regSec, "phd", nil)
	f.gradLogHours(assign, "2026-06-10", "lecture", 6)
	f.gradLogHours(assign, "2026-07-10", "lecture", 4)
	f.gradLogHours(assign, "2026-07-20", "review", 2)

	book, err := f.svc.BuildGradEvidenceWorkbook(f.ctx, courseID, []string{"2026-06", "2026-07"})
	if err != nil {
		t.Fatalf("BuildGradEvidenceWorkbook: %v", err)
	}
	if book == nil {
		t.Fatal("a course with a grad-regular TA must produce the graduate workbook")
	}
	x := openWorkbookBytes(t, book)
	sheet := sheetGradEvidenceRegular

	// Row 9 carries one Thai month label per column, E onward.
	for cell, want := range map[string]string{"E9": "มิถุนายน 2569", "F9": "กรกฎาคม 2569"} {
		got, err := x.GetCellValue(sheet, cell)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s = %q, want %q — the month columns are what makes this the graduate form", cell, got, want)
		}
	}
	// Row 10 holds this TA's hours in the month they were worked. 6 in June and
	// 4+2 in July: a single 12-hour total column would lose exactly this. The
	// values read back through the sheet's one-decimal hour format.
	// TrimSpace: the sheet's one-decimal hour format reserves alignment
	// padding that newer excelize versions include in GetCellValue's
	// returned string (Excel itself renders it as blank space regardless —
	// see export_combined_book_test.go's BillsLabAndLectureIntoSeparateColumns
	// for the same thing on a different sheet). Not a data bug, just this
	// test asserting the actual number instead of a formatting library's
	// internal rendering choice.
	for cell, want := range map[string]string{"E10": "6.0", "F10": "6.0"} {
		got, err := x.GetCellValue(sheet, cell)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(got) != want {
			t.Errorf("%s = %q, want %q hours", cell, got, want)
		}
	}
	// G is รวมชั่วโมง, H the hourly rate, I the money — the hourly chain the
	// เหมาจ่าย sheet deliberately does not have.
	if got, _ := x.GetCellFormula(sheet, "G10"); got != "SUM(E10:F10)" {
		t.Errorf("total-hours formula = %q, want SUM(E10:F10)", got)
	}
	if got, _ := x.GetCellValue(sheet, "H10"); strings.TrimSpace(got) != "50" {
		t.Errorf("rate = %q, want 50 (graduate_regular_hourly)", got)
	}
	if got, _ := x.GetCellFormula(sheet, "I10"); got != "G10*H10" {
		t.Errorf("amount formula = %q, want G10*H10", got)
	}
	if got, _ := x.GetCellValue(sheet, "D10"); got != "ป.เอก" {
		t.Errorf("level = %q, want ป.เอก", got)
	}
	// The เหมาจ่าย sheet must not exist when nobody is on that track.
	if idx, _ := x.GetSheetIndex(sheetGradEvidenceSpecial); idx >= 0 {
		t.Error("a course with no grad-special TA must not get an empty พิเศษ sheet")
	}
}

// cellAmount reads a money cell back as a number.
//
// The comma format matters here: Sscanf("%f") on "1,250" stops at the comma and
// yields 1, so a naive read silently under-counts every figure over a thousand
// — which is most real ones, and would make a slice-sum assertion pass on
// numbers that do not actually add up.
func cellAmount(t *testing.T, x *excelize.File, sheet, cell string) float64 {
	t.Helper()
	got, err := x.GetCellValue(sheet, cell)
	if err != nil {
		t.Fatal(err)
	}
	got = strings.ReplaceAll(strings.TrimSpace(got), ",", "")
	if got == "" {
		return 0
	}
	v, err := strconv.ParseFloat(got, 64)
	if err != nil {
		t.Fatalf("cell %s = %q, not a number: %v", cell, got, err)
	}
	return v
}

// sumSpecialRow adds up the printed month columns of the เหมาจ่าย sheet's
// first claimant, i.e. what that document actually claims.
func sumSpecialRow(t *testing.T, book []byte, nMonths int) float64 {
	t.Helper()
	x := openWorkbookBytes(t, book)
	var sum float64
	for i := 0; i < nMonths; i++ {
		col, _ := excelColumnName(5 + i)
		sum += cellAmount(t, x, sheetGradEvidenceSpecial, fmt.Sprintf("%s10", col))
	}
	return sum
}

// ภาคต้น teaches มิ.ย.–ต.ค. but งบแผ่นดิน closes 30 กันยายน, so the term is
// claimed on TWO documents: มิ.ย.–ก.ย. against the closing year, ตุลาคม against
// the new one. เหมาจ่าย is 4,000 per course per TERM — across both documents
// together, not on each.
//
// The lump was being divided by the SELECTED months, so the four-month document
// carried the whole 4,000 and October's carried another 4,000: 8,000 against a
// 4,000 cap, on two forms that each look right on their own. This is the
// invariant that catches it — the slices must add up to the whole, never more.
func TestBuildGradEvidenceWorkbook_LumpSplitsAcrossFiscalYearsWithoutDoubleBilling(t *testing.T) {
	f := newTCFixture(t)
	courseID, _, specSec := f.insertCourse(tcCourseOpts{Code: "CP423436", Curriculum: "CY", LectureHrs: 3})
	ta := f.newTA("บัณฑิตพิเศษ", "master")
	f.assignTA(ta, courseID, specSec, "master", nil) // เหมาจ่าย logs nothing

	// The five months ภาคต้น is taught over, as periods staff would have created.
	for _, mm := range []string{"06", "07", "08", "09", "10"} {
		f.exec(`INSERT INTO submission_periods (id, term_id, year_month, due_date, label, starts_on, is_closed)
		        VALUES (gen_random_uuid(), $1, $2, '2026-12-31'::date, $3, '2026-06-01'::date, FALSE)
		        ON CONFLICT DO NOTHING`, f.termID, "2569-"+mm, "รอบ "+mm)
	}

	oldYear := []string{"2026-06", "2026-07", "2026-08", "2026-09"}
	newYear := []string{"2026-10"}

	oldBook, err := f.svc.BuildGradEvidenceWorkbook(f.ctx, courseID, oldYear)
	if err != nil {
		t.Fatalf("old-year export: %v", err)
	}
	newBook, err := f.svc.BuildGradEvidenceWorkbook(f.ctx, courseID, newYear)
	if err != nil {
		t.Fatalf("new-year export: %v", err)
	}
	oldSum := sumSpecialRow(t, oldBook, len(oldYear))
	newSum := sumSpecialRow(t, newBook, len(newYear))

	// graduate_special_lumpsum is 1000 in this fixture's rate table.
	const lump = 1000.0
	if oldSum >= lump {
		t.Errorf("the มิ.ย.–ก.ย. document claims %.2f, which is the whole %.0f lump or more — "+
			"ตุลาคม is claimed separately and would take the total over the per-term cap",
			oldSum, lump)
	}
	if newSum <= 0 {
		t.Errorf("the ตุลาคม document claims %.2f; the month carries real lump", newSum)
	}
	if got := oldSum + newSum; got != lump {
		t.Errorf("the two fiscal-year documents together claim %.2f, want exactly %.0f — "+
			"the term's lump must be split between them, not duplicated or lost", got, lump)
	}

	// And a whole-term export still claims exactly the lump, once.
	whole, err := f.svc.BuildGradEvidenceWorkbook(f.ctx, courseID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := sumSpecialRow(t, whole, 5); got != lump {
		t.Errorf("whole-term export claims %.2f, want %.0f", got, lump)
	}
}

// Formatting the office checked against their own file: the (ตัวอักษร)
// amount-in-words band is filled silver and boxed in thin rules. The graduate
// form does NOT use the medium side rules the undergrad one does, so the two
// writers' styling cannot be shared.
func TestBuildGradEvidenceWorkbook_AmountInWordsBandMatchesTheCollegesFill(t *testing.T) {
	f := newTCFixture(t)
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP363766", Curriculum: "CY", LectureHrs: 3})
	ta := f.newTA("บัณฑิตปกติ", "phd")
	assign := f.assignTA(ta, courseID, regSec, "phd", nil)
	f.gradLogHours(assign, "2026-06-10", "lecture", 6)

	book, err := f.svc.BuildGradEvidenceWorkbook(f.ctx, courseID, []string{"2026-06"})
	if err != nil {
		t.Fatalf("BuildGradEvidenceWorkbook: %v", err)
	}
	x := openWorkbookBytes(t, book)
	sheet := sheetGradEvidenceRegular

	// One claimant → total on row 12, amount-in-words on row 13.
	styleID, err := x.GetCellStyle(sheet, "D13")
	if err != nil {
		t.Fatal(err)
	}
	fill, err := x.GetStyle(styleID)
	if err != nil {
		t.Fatal(err)
	}
	if len(fill.Fill.Color) == 0 || !strings.Contains(strings.ToUpper(fill.Fill.Color[0]), "C0C0C0") {
		t.Errorf("(ตัวอักษร) band fill = %v, want the college's silver C0C0C0 — "+
			"an unfilled band is what the office flagged", fill.Fill.Color)
	}
	// ...and the total band above it is boxed thin, not in the undergrad
	// form's medium side rules.
	totalStyle, err := x.GetCellStyle(sheet, "F12")
	if err != nil {
		t.Fatal(err)
	}
	st, err := x.GetStyle(totalStyle)
	if err != nil {
		t.Fatal(err)
	}
	for _, bd := range st.Border {
		if bd.Style == 2 { // medium
			t.Errorf("total band uses a medium %s rule; the graduate form is thin throughout", bd.Type)
		}
	}
}

// Live bug (12/08/2026, CP423434): one graduate TA held BOTH regular-track
// assignments (sec 1, 2) and a special-track one (sec 3) on the same course.
// The builder treated "is grad-special" as a property of the PERSON, so it
// printed only their เหมาจ่าย row and threw away 108 approved regular-track
// hours — ฿5,400 of real work missing from the claim document, with nothing
// on the page to suggest anything was gone.
//
// Track is a property of the ASSIGNMENT. One TA can appear on both sheets, and
// that is precisely why this workbook has two.
func TestBuildGradEvidenceWorkbook_TAOnBothTracksAppearsOnBothSheets(t *testing.T) {
	f := newTCFixture(t)
	courseID, regSec, specSec := f.insertCourse(tcCourseOpts{Code: "CP423434", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("วรพจน์", "phd")

	regAssign := f.assignTA(ta, courseID, regSec, "phd", nil)
	f.gradLogHours(regAssign, "2026-06-10", "lecture", 6)
	f.gradLogHours(regAssign, "2026-07-10", "lecture", 8)
	// ...and the same person on the special-programme section.
	f.assignTA(ta, courseID, specSec, "phd", nil)

	book, err := f.svc.BuildGradEvidenceWorkbook(f.ctx, courseID, []string{"2026-06", "2026-07"})
	if err != nil {
		t.Fatalf("BuildGradEvidenceWorkbook: %v", err)
	}
	x := openWorkbookBytes(t, book)

	if idx, _ := x.GetSheetIndex(sheetGradEvidenceRegular); idx < 0 {
		t.Fatal("the ปกติ sheet is missing — this TA's regular-track hours were dropped " +
			"because they also hold a special-track assignment")
	}
	if got, _ := x.GetCellValue(sheetGradEvidenceRegular, "E10"); strings.TrimSpace(got) != "6.0" {
		t.Errorf("June hours = %q, want 6.0", got)
	}
	if got, _ := x.GetCellValue(sheetGradEvidenceRegular, "F10"); strings.TrimSpace(got) != "8.0" {
		t.Errorf("July hours = %q, want 8.0", got)
	}
	// The เหมาจ่าย row is still theirs too — the two claims coexist.
	if idx, _ := x.GetSheetIndex(sheetGradEvidenceSpecial); idx < 0 {
		t.Fatal("the พิเศษ sheet is missing — the special-track lump was dropped")
	}
	if got, _ := x.GetCellValue(sheetGradEvidenceSpecial, "B10"); got == "" {
		t.Error("the พิเศษ sheet has no claimant row")
	}
}

// Second half of the same live bug: the hours printed must be the hours the
// PAYOUT settles, not the raw work_log durations.
//
// A TA assisting two sections that meet at the same hour has one sitting
// written against each — the payout merges them and pays once, but summing the
// rows counts them twice. On CP423434 that put 196 hours on the claim form
// against the 108 the system pays, so the document billed ฿4,400 more than the
// transfer behind it.
func TestBuildGradEvidenceWorkbook_HoursMatchTheSettlementNotRawLogs(t *testing.T) {
	f := newTCFixture(t)
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP423435", Curriculum: "CY", LectureHrs: 100})

	// A second regular-track section of the same course, and the same TA on
	// both — the sec 1 / sec 2 pair that meets in one room at one hour.
	// sec_no '3' because insertCourse already took '1' and '2'.
	sec2 := uuid.New()
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track, curriculum)
	        VALUES ($1, $2, '3', 'regular', 'CY')`, sec2, courseID)

	ta := f.newTA("วรพจน์", "phd")
	a1 := f.assignTA(ta, courseID, regSec, "phd", nil)
	a2 := f.assignTA(ta, courseID, sec2, "phd", nil)
	// The SAME sitting written against both assignments.
	f.gradLogHours(a1, "2026-06-10", "lecture", 4)
	f.gradLogHours(a2, "2026-06-10", "lecture", 4)

	book, err := f.svc.BuildGradEvidenceWorkbook(f.ctx, courseID, []string{"2026-06"})
	if err != nil {
		t.Fatalf("BuildGradEvidenceWorkbook: %v", err)
	}
	x := openWorkbookBytes(t, book)
	got, _ := x.GetCellValue(sheetGradEvidenceRegular, "E10")
	if strings.TrimSpace(got) != "4.0" {
		t.Errorf("June hours = %q, want 4.0 — one sitting taught once is paid once. "+
			"8.0 means the two co-taught sections were added together, billing the "+
			"finance office for hours the payout will not transfer", got)
	}
}

// The same TA must also get their ภาระงาน form — it backs the regular-track
// hours above, and disappeared for exactly the same reason.
func TestBuildGradWorkloadForms_TAOnBothTracksStillGetsAForm(t *testing.T) {
	f := newTCFixture(t)
	courseID, regSec, specSec := f.insertCourse(tcCourseOpts{Code: "CP423434", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("วรพจน์", "phd")
	regAssign := f.assignTA(ta, courseID, regSec, "phd", nil)
	f.gradLogHours(regAssign, "2026-06-10", "lecture", 6)
	f.assignTA(ta, courseID, specSec, "phd", nil)

	forms, err := f.svc.BuildGradWorkloadForms(f.ctx, courseID, nil)
	if err != nil {
		t.Fatalf("BuildGradWorkloadForms: %v", err)
	}
	if len(forms) != 1 {
		t.Fatalf("got %d forms, want 1 — a TA with real regular-track hours needs the "+
			"form even though they also hold a special-track assignment", len(forms))
	}
}

// The other half of the split: on a MIXED course the graduate book carries
// only graduate TAs, exactly as the combined book carries only undergrads.
// Together these two tests pin that every TA lands on exactly one document —
// billed once, on the form their level is actually claimed with.
func TestBuildGradEvidenceWorkbook_ExcludesUndergradOnAMixedCourse(t *testing.T) {
	f := newTCFixture(t)
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP363764", Curriculum: "CY", LectureHrs: 3})

	ug := f.newTA("ปริญญาตรี", "undergrad")
	ugAssign := f.assignTA(ug, courseID, regSec, "undergrad", nil)
	f.gradLogHours(ugAssign, "2026-06-10", "lecture", 9)

	grad := f.newTA("บัณฑิตปกติ", "phd")
	gradAssign := f.assignTA(grad, courseID, regSec, "phd", nil)
	f.gradLogHours(gradAssign, "2026-06-11", "lecture", 6)

	book, err := f.svc.BuildGradEvidenceWorkbook(f.ctx, courseID, []string{"2026-06"})
	if err != nil {
		t.Fatalf("BuildGradEvidenceWorkbook: %v", err)
	}
	if book == nil {
		t.Fatal("a mixed course must still produce the graduate workbook")
	}
	x := openWorkbookBytes(t, book)
	sheet := sheetGradEvidenceRegular

	if got, _ := x.GetCellValue(sheet, "B10"); !strings.Contains(got, "บัณฑิตปกติ") {
		t.Errorf("first row = %q, want the graduate TA", got)
	}
	// Exactly one row: the undergrad's 9 hours belong on -เบิกจ่าย.xlsx, and
	// a second row here would bill them twice across the two documents.
	if got, _ := x.GetCellValue(sheet, "B11"); got != "" {
		t.Errorf("a second claimant %q appeared on the graduate book — "+
			"the undergrad TA belongs on the undergrad form only", got)
	}
	if got, _ := x.GetCellValue(sheet, "E10"); strings.TrimSpace(got) != "6.0" {
		t.Errorf("graduate hours = %q, want 6.0 (not merged with the undergrad's 9)", got)
	}
}

// The เหมาจ่าย sheet carries BAHT per month, not hours, and those months must
// add up to the flat term lump — the whole point of the 2026 change is that
// this TA logs nothing and the system still arrives at the right figure.
func TestBuildGradEvidenceWorkbook_SpecialMonthsSumToTheLump(t *testing.T) {
	f := newTCFixture(t)
	courseID, _, specSec := f.insertCourse(tcCourseOpts{Code: "CP363762", Curriculum: "CY", LectureHrs: 3})
	ta := f.newTA("บัณฑิตพิเศษ", "phd")
	// No work logs at all — grad-special stopped logging entirely.
	f.assignTA(ta, courseID, specSec, "phd", nil)

	// The WHOLE term — the fixture's course runs มิ.ย.–ต.ค., five months. The
	// lump is a per-term figure, so only a whole-term export claims all of it;
	// partial slices are the fiscal-split test's business.
	months := []string{"2026-06", "2026-07", "2026-08", "2026-09", "2026-10"}
	book, err := f.svc.BuildGradEvidenceWorkbook(f.ctx, courseID, months)
	if err != nil {
		t.Fatalf("BuildGradEvidenceWorkbook: %v", err)
	}
	if book == nil {
		t.Fatal("a grad-special TA with no work logs must still appear — their pay does not depend on logging")
	}
	x := openWorkbookBytes(t, book)
	sheet := sheetGradEvidenceSpecial

	var sum float64
	for i := range months {
		col, _ := excelColumnName(5 + i)
		sum += cellAmount(t, x, sheet, fmt.Sprintf("%s10", col))
	}
	// graduate_special_lumpsum is 1000 in this fixture's rate table, under the
	// 12000 term cap. EXACT, not approximate: 1000 split five ways rounds to
	// 200 each, but an uneven split (1000/3 → 333.33 three times) totals 999.99
	// unless the last month absorbs the remainder, and this document is
	// reconciled against the real transfer.
	if sum != 1000 {
		t.Errorf("month columns sum to %.2f, want exactly 1000 — a split that does not "+
			"add back up bills a different figure than the payout transfers", sum)
	}
	// เหมาจ่าย has no hour or rate column: column E onward is money straight
	// through to รับจริง, which is the months' own sum.
	if got, _ := x.GetCellFormula(sheet, "J10"); got != "SUM(E10:I10)" {
		t.Errorf("รับจริง formula = %q, want SUM(E10:I10) — the months ARE the money here", got)
	}
	if idx, _ := x.GetSheetIndex(sheetGradEvidenceRegular); idx >= 0 {
		t.Error("a course with no grad-regular TA must not get an empty ปกติ sheet")
	}
}

// A month the caller excluded must not appear as a column NOR be folded into
// the total. งบแผ่นดิน closes 30 กันยายน mid-term, so October has to be
// excludable without quietly riding along in the sum.
func TestBuildGradEvidenceWorkbook_HonoursTheMonthSelection(t *testing.T) {
	f := newTCFixture(t)
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP363763", Curriculum: "CY", LectureHrs: 3})
	ta := f.newTA("บัณฑิตปกติ", "master")
	assign := f.assignTA(ta, courseID, regSec, "master", nil)
	f.gradLogHours(assign, "2026-09-10", "lecture", 5)
	f.gradLogHours(assign, "2026-10-10", "lecture", 7) // the next budget year

	book, err := f.svc.BuildGradEvidenceWorkbook(f.ctx, courseID, []string{"2026-09"})
	if err != nil {
		t.Fatalf("BuildGradEvidenceWorkbook: %v", err)
	}
	x := openWorkbookBytes(t, book)
	sheet := sheetGradEvidenceRegular

	if got, _ := x.GetCellValue(sheet, "E9"); got != "กันยายน 2569" {
		t.Errorf("first month column = %q, want กันยายน 2569", got)
	}
	// One month selected means exactly one month column, so รวมจำนวนชั่วโมง
	// lands in F and sums E alone. October's 7 hours are in neither.
	if got, _ := x.GetCellValue(sheet, "F9"); got != "ชั่วโมง" {
		t.Errorf("column F should be รวมจำนวน/ชั่วโมง for a one-month selection, got %q — "+
			"a second month column means October rode along into a budget year it does not belong to", got)
	}
	if got, _ := x.GetCellFormula(sheet, "F10"); got != "SUM(E10:E10)" {
		t.Errorf("total-hours formula = %q, want SUM(E10:E10)", got)
	}
	if got, _ := x.GetCellValue(sheet, "E10"); strings.TrimSpace(got) != "5.0" {
		t.Errorf("September hours = %q, want 5.0 — October's 7 must not be folded in", got)
	}
}

// excelColumnName is the test's own 1-based column-number → letter helper, so
// the assertions can walk a variable-width month block.
func excelColumnName(n int) (string, error) {
	name := ""
	for n > 0 {
		n--
		name = string(rune('A'+n%26)) + name
		n /= 26
	}
	return name, nil
}
