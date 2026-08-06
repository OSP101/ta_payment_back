package service

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

// The combined workbook replaced a folder-per-TA zip. What makes it usable is
// that the office can print ONE file in the order they file it — so the things
// worth pinning are the ones that break silently: which column an hour bills
// into, that a block's totals point at that block's own rows, and that the
// evidence sheet links to the right block rather than to the first one.

func TestClaimHours(t *testing.T) {
	cases := []struct {
		rng  string
		want float64
	}{
		{"13.00 - 17.00", 4},
		{"14.00 - 15.00", 1},
		{"19.00 - 21.00", 2},
		{"13.30 - 15.00", 1.5},
		{"", 0},
		{"ไม่ใช่เวลา", 0},
	}
	for _, c := range cases {
		if got := claimHours(c.rng); got != c.want {
			t.Errorf("claimHours(%q) = %v, want %v", c.rng, got, c.want)
		}
	}
}

// The two hours columns are the whole money calculation. Lab duty bills into
// ปฏิบัติการ, everything else into บรรยาย — put a lab hour in the wrong column
// and the claim is still arithmetically consistent, just wrong.
func TestIsLabNote(t *testing.T) {
	for _, n := range []string{"สอนปฏิบัติ", "สอนปฏิบัติ (ชดเชย)", "เตรียมเอกสารปฏิบัติ"} {
		if !isLabNote(n) {
			t.Errorf("%q must bill as ปฏิบัติการ", n)
		}
	}
	for _, n := range []string{"เช็คชื่อ", "เช็คชื่อ (ชดเชย)", "ตรวจงาน", "งานอื่นๆ", ""} {
		if isLabNote(n) {
			t.Errorf("%q must bill as บรรยาย", n)
		}
	}
}

func TestThaiMonthRange(t *testing.T) {
	d := func(y int, m time.Month) time.Time { return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC) }
	if got := thaiMonthRange(d(2026, 6), d(2026, 10)); got != "มิถุนายน 2569 - ตุลาคม 2569" {
		t.Errorf("range = %q", got)
	}
	// A single month must not print "X - X".
	if got := thaiMonthRange(d(2026, 6), d(2026, 6)); got != "มิถุนายน 2569" {
		t.Errorf("single month = %q, want no range", got)
	}
}

// combinedFixture writes a two-person sheet with no database behind it, so the
// layout can be asserted directly.
func combinedFixture(t *testing.T) (*excelize.File, *combinedBookData, []claimant) {
	t.Helper()
	d := &combinedBookData{
		CourseCode: "CP362104", AcademicYear: 2569, Semester: 1,
		MonthRange: "มิถุนายน 2569 - ตุลาคม 2569", LecturerName: "วรัญญา วรรณศรี",
		RateUGRegular: 40, RateUGSpecial: 50, RateGradRegular: 50,
		Certifier: CertifierChoice{
			Name: "ผศ. ดร.ณกร วัฒนกิจ", TitleLine: "รองคณบดีฝ่ายวิชาการ รักษาการแทน",
			ActingFor: "หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์", Resolved: true,
		},
	}
	day := func(n int) time.Time { return time.Date(2026, 6, n, 0, 0, 0, 0, time.UTC) }
	// Person 1 teaches 5 hours (฿200 at the rate) but the budget funds only
	// ฿150 — the over-budget case the ขอเบิกจ่ายเพียง line exists for. Person 2
	// is funded in full, so their ขอเบิกจ่ายเพียง stays blank.
	people := []claimant{
		{Name: "หนึ่ง ทดสอบ", LevelTH: "ป.ตรี", Rate: 40, PaidBaht: 150, FullBaht: 200, Rows: []claimSheetRow{
			{Date: day(1), Group: "ปกติ Sec1", Range: "13.00 - 17.00", Note: "สอนปฏิบัติ"},
			{Date: day(2), Group: "ปกติ Sec1", Range: "14.00 - 15.00", Note: "เช็คชื่อ"},
		}},
		{Name: "สอง ทดสอบ", LevelTH: "ป.ตรี", Rate: 40, PaidBaht: 80, FullBaht: 80, Rows: []claimSheetRow{
			{Date: day(3), Group: "ปกติ Sec1", Range: "19.00 - 21.00", Note: "ตรวจงาน"},
		}},
	}
	f := excelize.NewFile()
	t.Cleanup(func() { f.Close() })
	st, err := newClaimStyles(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeClaimSheet(f, st, sheetClaimRegular, "ภาคปกติ", d, people); err != nil {
		t.Fatal(err)
	}
	if err := writeEvidenceSheet(f, st, sheetEvidenceRegular, sheetClaimRegular, "ภาคปกติ", d, people); err != nil {
		t.Fatal(err)
	}
	return f, d, people
}

// Two people, two blocks, stacked — the point of the whole change.
func TestCombinedSheet_StacksOneBlockPerPerson(t *testing.T) {
	f, _, people := combinedFixture(t)

	rows, err := f.GetRows(sheetClaimRegular)
	if err != nil {
		t.Fatal(err)
	}
	var starts []int
	for i, r := range rows {
		if len(r) > 0 && strings.HasPrefix(r[0], "แบบใบเบิก") {
			starts = append(starts, i+1)
		}
	}
	if len(starts) != 2 {
		t.Fatalf("found %d blocks, want one per person", len(starts))
	}
	if starts[0] != 1 {
		t.Errorf("first block starts at row %d, want 1", starts[0])
	}
	for i, p := range people {
		got, err := f.GetCellValue(sheetClaimRegular, cellAt("B", starts[i]+8))
		if err != nil {
			t.Fatal(err)
		}
		if got != p.Name {
			t.Errorf("block %d names %q, want %q — the blocks are in the wrong order "+
				"or the name row moved", i+1, got, p.Name)
		}
	}
}

// A block's SUM must cover ITS OWN rows. Get this wrong and the second person's
// claim silently totals the first person's hours.
func TestCombinedSheet_TotalsCoverOnlyTheirOwnBlock(t *testing.T) {
	f, _, _ := combinedFixture(t)
	rows, _ := f.GetRows(sheetClaimRegular)
	var starts []int
	for i, r := range rows {
		if len(r) > 0 && strings.HasPrefix(r[0], "แบบใบเบิก") {
			starts = append(starts, i+1)
		}
	}
	if len(starts) < 2 {
		t.Fatal("need two blocks")
	}

	// Block 2's first data row is below block 1's last, so its SUM must not
	// reach back above its own header.
	first2 := starts[1] + 8
	for r := starts[1]; r < starts[1]+60; r++ {
		v, err := f.GetCellFormula(sheetClaimRegular, cellAt("H", r))
		// excelize stores formulas without the leading "="; accept either so the
		// test pins the RANGE, not the storage detail.
		v = strings.TrimPrefix(v, "=")
		if err != nil || !strings.HasPrefix(v, "SUM(") {
			continue
		}
		var from, to int
		if _, err := fmtSscanSum(v, &from, &to); err != nil {
			t.Fatalf("unexpected total formula %q", v)
		}
		if from < first2 {
			t.Errorf("block 2 totals from row %d, which is inside block 1 (its own data "+
				"starts at %d)", from, first2)
		}
		return
	}
	t.Fatal("block 2 has no total row")
}

// The evidence sheet must point at each person's own block. Every row linking to
// the first block is the failure that looks perfectly plausible on screen.
func TestCombinedSheet_EvidenceLinksToEachPersonsBlock(t *testing.T) {
	f, _, people := combinedFixture(t)

	seen := map[string]bool{}
	for i := range people {
		r := 10 + i
		name, err := f.GetCellFormula(sheetEvidenceRegular, cellAt("B", r))
		if err != nil {
			t.Fatal(err)
		}
		hours, _ := f.GetCellFormula(sheetEvidenceRegular, cellAt("D", r))
		if !strings.Contains(name, sheetClaimRegular) || !strings.Contains(hours, sheetClaimRegular) {
			t.Errorf("row %d does not link to the claim sheet: name=%q hours=%q", r, name, hours)
		}
		if seen[name] {
			t.Errorf("row %d reuses the reference %q — every person must link to their "+
				"OWN block", r, name)
		}
		seen[name] = true
	}
}

// Hours land in the column their activity bills from.
func TestCombinedSheet_BillsLabAndLectureIntoSeparateColumns(t *testing.T) {
	f, _, _ := combinedFixture(t)
	// Fixture row 1 of block 1 is a 4-hour สอนปฏิบัติ, row 2 a 1-hour เช็คชื่อ.
	// Values read back formatted: the hours columns wear the college's comma
	// format, so 4 displays as "4.00".
	lab, _ := f.GetCellValue(sheetClaimRegular, "I9")
	labWrong, _ := f.GetCellValue(sheetClaimRegular, "H9")
	lect, _ := f.GetCellValue(sheetClaimRegular, "H10")
	lectWrong, _ := f.GetCellValue(sheetClaimRegular, "I10")
	if lab != "4.00" || labWrong != "" {
		t.Errorf("lab hour: I9=%q H9=%q, want 4.00 in ปฏิบัติการ only", lab, labWrong)
	}
	if lect != "1.00" || lectWrong != "" {
		t.Errorf("lecture hour: H10=%q I10=%q, want 1.00 in บรรยาย only", lect, lectWrong)
	}
}

// The certifier signs every block, in the acting form when they are standing in.
func TestCombinedSheet_CertifierSignsEveryBlock(t *testing.T) {
	f, d, _ := combinedFixture(t)
	rows, _ := f.GetRows(sheetClaimRegular)
	found := 0
	for _, r := range rows {
		for _, cell := range r {
			if strings.Contains(cell, d.Certifier.ActingFor) && !strings.Contains(cell, "ตำแหน่ง") {
				found++
			}
		}
	}
	if found < 2 {
		t.Errorf("the acting seat appears %d times, want once per block — a signature "+
			"block missing its authority line is an unsigned claim", found)
	}
}

// Office instruction (ส.ค. 2569): the sheet lists every hour taught and totals
// it in รวมเป็นเงินทั้งสิ้น. ขอเบิกจ่ายเพียง (and the evidence sheet's รับจริง)
// prints the funded amount ONLY when the budget stopped short — a fully funded
// block leaves the line blank, so a figure there always means "งบไม่พอ".
func TestCombinedSheet_FundedAmountPrintsOnlyWhenBudgetStopsShort(t *testing.T) {
	f, _, _ := combinedFixture(t)
	raw := excelize.Options{RawCellValue: true}

	// Block 1 (underfunded, 150 of 200): top=1 → grand total row 32,
	// ขอเบิกจ่ายเพียง row 33.
	got, err := f.GetCellValue(sheetClaimRegular, "C33", raw)
	if err != nil {
		t.Fatal(err)
	}
	if got != "150" {
		t.Errorf("ขอเบิกจ่ายเพียง C33 = %q, want the funded 150, not the full 200", got)
	}
	// รวมเป็นเงินทั้งสิ้น stays the formula over the block's own full hours.
	if formula, _ := f.GetCellFormula(sheetClaimRegular, "C32"); formula == "" {
		t.Error("C32 must keep the full-amount formula — the budget must not touch it")
	}
	// Block 2 (funded in full): top=41 → ขอเบิกจ่ายเพียง row 73 stays blank.
	if got, _ := f.GetCellValue(sheetClaimRegular, "C73", raw); got != "" {
		t.Errorf("fully funded block prints ขอเบิกจ่ายเพียง %q, want blank", got)
	}

	// Evidence sheet: รับจริง is a literal only for the underfunded person…
	if got, _ := f.GetCellValue(sheetEvidenceRegular, "G10", raw); got != "150" {
		t.Errorf("evidence รับจริง G10 = %q, want the funded 150", got)
	}
	// …and stays the =F link for the fully funded one.
	if formula, _ := f.GetCellFormula(sheetEvidenceRegular, "G11"); formula == "" {
		t.Error("G11 must keep the =F11 link — funded in full means received = claimed")
	}
	// จำนวนเงิน keeps the full D×E formula for everyone.
	if formula, _ := f.GetCellFormula(sheetEvidenceRegular, "F10"); formula == "" {
		t.Error("F10 must keep the D×E full-amount formula")
	}
}

// A track nobody worked produces no sheet, rather than a blank one to leaf past.
func TestCombinedSheet_SkipsAnEmptyTrack(t *testing.T) {
	f, _, _ := combinedFixture(t)
	if idx, _ := f.GetSheetIndex(sheetClaimSpecial); idx >= 0 {
		t.Error("the special sheet must not exist when nobody has special-track hours")
	}
}

func cellAt(col string, row int) string { return fmt.Sprintf("%s%d", col, row) }

// fmtSscanSum pulls the row range out of "=SUM(H9:H24)".
func fmtSscanSum(formula string, from, to *int) (int, error) {
	inner := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(formula, "="), "SUM("), ")")
	parts := strings.Split(inner, ":")
	if len(parts) != 2 {
		return 0, errBadSum
	}
	f, err := parseTrailingInt(parts[0])
	if err != nil {
		return 0, err
	}
	t, err := parseTrailingInt(parts[1])
	if err != nil {
		return 0, err
	}
	*from, *to = f, t
	return 2, nil
}

var errBadSum = errors.New("not a SUM range")

func parseTrailingInt(ref string) (int, error) {
	i := 0
	for i < len(ref) && (ref[i] < '0' || ref[i] > '9') {
		i++
	}
	n := 0
	if i == len(ref) {
		return 0, errBadSum
	}
	for ; i < len(ref); i++ {
		n = n*10 + int(ref[i]-'0')
	}
	return n, nil
}
