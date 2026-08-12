package service

import "testing"

/* -------------------------------------------------------------------------- */
/* Level split (12/08/2026)                                                   */
/*                                                                            */
/* ปะหน้าจ่ายตรง became two separate FILES — ป.ตรี and บัณฑิตศึกษา — because    */
/* staff make claims for the two levels differently. Level is independent of  */
/* the month slice: a person's TA LEVEL comes from their own assignment       */
/* (ta_request_assignments.level), not from which document is being printed,  */
/* so the same course, the same TA, or even the same person twice over can    */
/* land on both files without being double-counted or dropped from either.    */
/* -------------------------------------------------------------------------- */

func sheetRowNames(sheets []transferCoverSheet) map[string]float64 {
	out := map[string]float64{}
	for _, sh := range sheets {
		for _, r := range sh.Rows {
			out[r.Name] += r.Baht
		}
	}
	return out
}

// Two different TAs — one undergrad, one graduate — on two different courses.
// Each file must contain ONLY its own level, and the two totals must sum to
// the combined money actually earned (no double count, no drop).
func TestBuildTransferCoverSheets_LevelSplitsFiles(t *testing.T) {
	f := newTCFixture(t)
	courseA, regA, _ := f.insertCourse(tcCourseOpts{Code: "CP410", Curriculum: "CY", LectureHrs: 100})
	courseB, regB, _ := f.insertCourse(tcCourseOpts{Code: "CP420", Curriculum: "CY", LectureHrs: 100})

	ug := f.newTA("สมชาย ปตรี", "undergrad")
	f.assignTA(ug, courseA, regA, "undergrad", []int{1}) // 1hr @ 50 = 50 baht

	grad := f.newTA("บัณฑิต ชั่วโมงจริง", "master")
	f.assignTA(grad, courseB, regB, "master", []int{2}) // 1hr @ graduate_regular_hourly(50) = 50 baht

	f.financeSend(courseA)
	f.financeSend(courseB)

	ugSheets, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID, nil, "undergrad")
	if err != nil {
		t.Fatalf("undergrad: %v", err)
	}
	gradSheets, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID, nil, "graduate")
	if err != nil {
		t.Fatalf("graduate: %v", err)
	}

	ugRows := sheetRowNames(ugSheets)
	gradRows := sheetRowNames(gradSheets)

	if got, want := ugRows["สมชาย ปตรี ทดสอบ"], 50.0; got != want {
		t.Errorf("undergrad file: สมชาย = %.2f, want %.2f", got, want)
	}
	if _, present := ugRows["บัณฑิต ชั่วโมงจริง ทดสอบ"]; present {
		t.Error("undergrad file must not contain the graduate TA at all")
	}
	if got, want := gradRows["บัณฑิต ชั่วโมงจริง ทดสอบ"], 50.0; got != want {
		t.Errorf("graduate file: บัณฑิต = %.2f, want %.2f", got, want)
	}
	if _, present := gradRows["สมชาย ปตรี ทดสอบ"]; present {
		t.Error("graduate file must not contain the undergrad TA at all")
	}

	sum := round2(sheetsTotal(ugSheets) + sheetsTotal(gradSheets))
	if sum != 100 {
		t.Errorf("undergrad + graduate = %.2f, want 100 (50 + 50, nothing lost or duplicated)", sum)
	}
}

// The SAME person can hold an undergrad assignment on one course and a
// graduate assignment on another (a senior TA moonlighting, or simply two
// distinct rows in ta_request_assignments with different levels). They must
// appear on BOTH files — never merged into one, never silently dropped from
// either — each with only that assignment's own money.
func TestBuildTransferCoverSheets_SamePersonBothLevelAssignments(t *testing.T) {
	f := newTCFixture(t)
	courseA, regA, _ := f.insertCourse(tcCourseOpts{Code: "CP430", Curriculum: "CY", LectureHrs: 100})
	courseB, regB, _ := f.insertCourse(tcCourseOpts{Code: "CP440", Curriculum: "CY", LectureHrs: 100})

	person := f.newTA("สองสถานะ คนเดียว", "undergrad")
	f.assignTA(person, courseA, regA, "undergrad", []int{1, 2}) // 2hr @ 50 = 100 baht
	f.assignTA(person, courseB, regB, "master", []int{3})       // 1hr @ 50 = 50 baht

	f.financeSend(courseA)
	f.financeSend(courseB)

	ugSheets, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID, nil, "undergrad")
	if err != nil {
		t.Fatalf("undergrad: %v", err)
	}
	gradSheets, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID, nil, "graduate")
	if err != nil {
		t.Fatalf("graduate: %v", err)
	}

	ugRows := sheetRowNames(ugSheets)
	gradRows := sheetRowNames(gradSheets)
	if got, want := ugRows["สองสถานะ คนเดียว ทดสอบ"], 100.0; got != want {
		t.Errorf("undergrad row for the shared person = %.2f, want %.2f (only their undergrad assignment)", got, want)
	}
	if got, want := gradRows["สองสถานะ คนเดียว ทดสอบ"], 50.0; got != want {
		t.Errorf("graduate row for the shared person = %.2f, want %.2f (only their graduate assignment)", got, want)
	}
}

// The gate must be independent per level: a graduate course still mid-review
// must not hold the undergrad file's export shut, and the reverse.
func TestTermExportBlockers_LevelIsolated(t *testing.T) {
	f := newTCFixture(t)
	courseA, regA, _ := f.insertCourse(tcCourseOpts{Code: "CP450", Curriculum: "CY", LectureHrs: 100})
	courseB, regB, _ := f.insertCourse(tcCourseOpts{Code: "CP460", Curriculum: "CY", LectureHrs: 100})

	ug := f.newTA("รอไฟล์ ปตรี", "undergrad")
	f.assignTA(ug, courseA, regA, "undergrad", []int{1})
	f.financeSend(courseA) // undergrad course IS finance_sent

	grad := f.newTA("ค้างไฟล์ บัณฑิต", "master")
	f.assignTA(grad, courseB, regB, "master", []int{2})
	// courseB deliberately left NOT finance_sent.

	ugBlockers, err := f.svc.TermExportBlockers(f.ctx, f.termID, nil, "undergrad")
	if err != nil {
		t.Fatal(err)
	}
	if len(ugBlockers) != 0 {
		t.Errorf("undergrad file must be issuable even though the graduate course is unfinished; got blockers: %+v", ugBlockers)
	}

	gradBlockers, err := f.svc.TermExportBlockers(f.ctx, f.termID, nil, "graduate")
	if err != nil {
		t.Fatal(err)
	}
	if len(gradBlockers) == 0 {
		t.Error("graduate file must still be blocked — its own course has not reached finance_sent")
	}

	if _, _, err := f.svc.BuildTransferCoverWorkbook(f.ctx, f.actor(), f.termID, nil, "undergrad"); err != nil {
		t.Errorf("undergrad workbook should build despite the graduate course being stuck: %v", err)
	}
}

// Coverage (issued vs outstanding) must be tracked per level — issuing the
// undergrad file must not make the graduate screen believe its own months are
// already covered, and vice versa.
func TestTransferCoverCoverage_LevelIsolated(t *testing.T) {
	f := newTCFixture(t)
	f.addPeriod("2569-09")
	courseA, regA, _ := f.insertCourse(tcCourseOpts{Code: "CP470", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("ออกแล้ว ปตรี", "undergrad")
	f.assignTA(ta, courseA, regA, "undergrad", []int{1})
	f.financeSend(courseA)

	if _, _, err := f.svc.BuildTransferCoverWorkbook(f.ctx, f.actor(), f.termID, nil, "undergrad"); err != nil {
		t.Fatalf("issue undergrad file: %v", err)
	}

	ugCov, err := f.svc.TransferCoverCoverage(f.ctx, f.termID, "undergrad")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ugCov.Months {
		if !m.Issued {
			t.Errorf("undergrad coverage: %s should read issued after the undergrad file was built", m.YearMonth)
		}
	}

	gradCov, err := f.svc.TransferCoverCoverage(f.ctx, f.termID, "graduate")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range gradCov.Months {
		if m.Issued {
			t.Errorf("graduate coverage: %s must NOT read issued — only the undergrad file was ever built", m.YearMonth)
		}
	}
}

// ListTransferCoverExports scoped by level must still include a pre-split row
// (level IS NULL) in EVERY level's history — that generation genuinely did
// cover both files at once, back before the split existed.
func TestListTransferCoverExports_PreSplitRowCountsForBothLevels(t *testing.T) {
	f := newTCFixture(t)
	f.exec(`INSERT INTO transfer_cover_exports (id, term_id, generated_by, total_baht, sheet_count, document, months, level)
	        VALUES (gen_random_uuid(), $1, NULL, 500, 1, '{}'::jsonb, NULL, NULL)`, f.termID)

	ug, err := f.svc.ListTransferCoverExports(f.ctx, f.termID, "undergrad")
	if err != nil {
		t.Fatal(err)
	}
	if len(ug) != 1 {
		t.Errorf("undergrad history = %d rows, want 1 (the pre-split generation covered it too)", len(ug))
	}
	grad, err := f.svc.ListTransferCoverExports(f.ctx, f.termID, "graduate")
	if err != nil {
		t.Fatal(err)
	}
	if len(grad) != 1 {
		t.Errorf("graduate history = %d rows, want 1 (the pre-split generation covered it too)", len(grad))
	}
}

// A course whose only graduate TAs are grad-special (เหมาจ่าย, no work_logs at
// all) must not be blocked from the graduate file — there is nothing for any
// stage to review — but the build should say so via a warning rather than
// silence, since staff might otherwise assume the usual review happened.
func TestBuildTransferCoverWorkbook_GradSpecialOnlyWarnsButDoesNotBlock(t *testing.T) {
	f := newTCFixture(t)
	courseID, _, specSec := f.insertCourse(tcCourseOpts{Code: "CP480", Curriculum: "CY", LectureHrs: 100})
	f.newTA("เหมาจ่าย อย่างเดียว", "master")
	grad := f.newTA("เหมาจ่าย อย่างเดียว2", "master")
	f.assignTA(grad, courseID, specSec, "master", nil) // no work_logs at all

	blockers, err := f.svc.TermExportBlockers(f.ctx, f.termID, nil, "graduate")
	if err != nil {
		t.Fatal(err)
	}
	if len(blockers) != 0 {
		t.Fatalf("fixture bug: grad-special has no work_logs, so it can never appear as a blocker; got %+v", blockers)
	}

	body, warnings, err := f.svc.BuildTransferCoverWorkbook(f.ctx, f.actor(), f.termID, nil, "graduate")
	if err != nil {
		t.Fatalf("graduate workbook should build with only a warning, not an error: %v", err)
	}
	if len(body) == 0 {
		t.Error("expected a real workbook body")
	}
	found := false
	for _, w := range warnings {
		if w != "" && (w == "ไฟล์บัณฑิตศึกษามี TA เหมาจ่ายที่ไม่มีขั้นตอนตรวจสอบก่อนส่งออก (ไม่ต้องลงเวลา) กรุณาตรวจยอดเงินก่อนส่งการเงิน") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the เหมาจ่าย no-review warning; got warnings: %v", warnings)
	}
}

// Invalid level values are rejected outright rather than silently treated as
// "both" or "undergrad" — a caller must say which file it wants.
func TestBuildTransferCoverSheets_RejectsInvalidLevel(t *testing.T) {
	f := newTCFixture(t)
	if _, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID, nil, ""); err == nil {
		t.Error("empty level must be rejected")
	}
	if _, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID, nil, "both"); err == nil {
		t.Error("unknown level must be rejected")
	}
}
