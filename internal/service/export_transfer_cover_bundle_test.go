package service

import (
	"strings"
	"testing"
)

/* -------------------------------------------------------------------------- */
/* Combined download (12/08/2026)                                             */
/*                                                                            */
/* Staff asked for ONE button instead of two separate downloads crowding the  */
/* header. The two documents underneath are still built, gated, and ledgered */
/* completely independently — merging the button must not merge the gates.   */
/* -------------------------------------------------------------------------- */

// Both levels ready → one zip, two files inside, neither missing.
func TestBuildTransferCoverBundle_BothLevelsReadyProducesTwoFiles(t *testing.T) {
	f := newTCFixture(t)
	courseA, regA, _ := f.insertCourse(tcCourseOpts{Code: "CP610", Curriculum: "CY", LectureHrs: 100})
	ug := f.newTA("บันเดิล ปตรี", "undergrad")
	f.assignTA(ug, courseA, regA, "undergrad", []int{1})
	f.financeSend(courseA)

	courseB, regB, _ := f.insertCourse(tcCourseOpts{Code: "CP620", Curriculum: "CY", LectureHrs: 100})
	grad := f.newTA("บันเดิล บัณฑิต", "master")
	f.assignTA(grad, courseB, regB, "master", []int{2})
	f.financeSend(courseB)

	body, warnings, err := f.svc.BuildTransferCoverBundle(f.ctx, f.actor(), f.termID, nil)
	if err != nil {
		t.Fatalf("both levels ready: %v", err)
	}
	names := zipEntryNames(t, body)
	if len(names) != 2 {
		t.Fatalf("zip entries = %v, want 2 (one per level)", names)
	}
	var sawUG, sawGrad bool
	for _, n := range names {
		if strings.Contains(n, "ปตรี") {
			sawUG = true
		}
		if strings.Contains(n, "บัณฑิต") {
			sawGrad = true
		}
	}
	if !sawUG || !sawGrad {
		t.Errorf("zip entries %v must contain one ปตรี file and one บัณฑิต file", names)
	}
	_ = warnings
}

// Only the undergrad course is finance_sent — the graduate course is not.
// The bundle must still succeed, shipping ONLY the ready level, with a
// warning explaining the other is missing — never a hard failure, since that
// would recreate exactly the coupling the level split was built to remove.
func TestBuildTransferCoverBundle_OneLevelBlockedShipsTheOtherWithWarning(t *testing.T) {
	f := newTCFixture(t)
	courseA, regA, _ := f.insertCourse(tcCourseOpts{Code: "CP630", Curriculum: "CY", LectureHrs: 100})
	ug := f.newTA("พร้อม ปตรี", "undergrad")
	f.assignTA(ug, courseA, regA, "undergrad", []int{1})
	f.financeSend(courseA)

	courseB, regB, _ := f.insertCourse(tcCourseOpts{Code: "CP640", Curriculum: "CY", LectureHrs: 100})
	grad := f.newTA("ค้าง บัณฑิต", "master")
	f.assignTA(grad, courseB, regB, "master", []int{2})
	// courseB deliberately left NOT finance_sent.

	body, warnings, err := f.svc.BuildTransferCoverBundle(f.ctx, f.actor(), f.termID, nil)
	if err != nil {
		t.Fatalf("one level ready must not error: %v", err)
	}
	names := zipEntryNames(t, body)
	if len(names) != 1 {
		t.Fatalf("zip entries = %v, want exactly 1 (only the ready level)", names)
	}
	if !strings.Contains(names[0], "ปตรี") {
		t.Errorf("the one shipped entry = %q, want the ready ปตรี file", names[0])
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "บัณฑิต") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a warning naming the missing graduate file; got %v", warnings)
	}
}

// Both levels blocked → nothing to ship, so the bundle must fail outright
// rather than return an empty zip.
func TestBuildTransferCoverBundle_BothLevelsBlockedFails(t *testing.T) {
	f := newTCFixture(t)
	f.addPeriod(f.ym) // the gate needs a submission period to compare work_logs against
	courseA, regA, _ := f.insertCourse(tcCourseOpts{Code: "CP650", Curriculum: "CY", LectureHrs: 100})
	ug := f.newTA("ค้าง ปตรี", "undergrad")
	f.assignTA(ug, courseA, regA, "undergrad", []int{1})
	// Not finance_sent.

	courseB, regB, _ := f.insertCourse(tcCourseOpts{Code: "CP660", Curriculum: "CY", LectureHrs: 100})
	grad := f.newTA("ค้าง บัณฑิต2", "master")
	f.assignTA(grad, courseB, regB, "master", []int{2})
	// Not finance_sent.

	if _, _, err := f.svc.BuildTransferCoverBundle(f.ctx, f.actor(), f.termID, nil); err == nil {
		t.Error("both levels blocked must fail — there is nothing to ship")
	}
}
