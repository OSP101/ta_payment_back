package service

import (
	"testing"

	"github.com/google/uuid"
)

/* -------------------------------------------------------------------------- */
/* Per-round standing on the payouts list (12/08/2026, reshaped 13/08/2026)   */
/*                                                                            */
/* A course sitting in the payouts list's "ส่งออกแล้ว" bucket can still owe a  */
/* SECOND document once its term crosses the 30 กันยายน budget year — round 1 */
/* being exported must not read as the course being fully done.               */
/*                                                                            */
/* The first version of this answered with one boolean, RoundTwoOutstanding,  */
/* which could not separate "round 2 is finished" from "this course has no    */
/* round-2 work at all". CourseSummary.Rounds now carries billable+exported   */
/* per round, so these tests assert the pair rather than the collapsed flag.  */
/* -------------------------------------------------------------------------- */

func dashboardFor(f *tcFixture) *PayoutDashboard {
	f.t.Helper()
	svc := &ExportBatchService{pool: f.pool, aud: f.svc.aud}
	out, err := svc.DashboardSummary(f.ctx, &BudgetService{pool: f.pool}, f.svc, f.termID)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func summaryFor(rows []CourseSummary, courseID uuid.UUID) *CourseSummary {
	for i := range rows {
		if rows[i].TeachingCourseID == courseID {
			return &rows[i]
		}
	}
	return nil
}

// roundOf picks one round's standing out of a course summary, failing loudly
// rather than returning a zero value that would silently pass an assertion.
func roundOf(t *testing.T, c *CourseSummary, round int) CourseRoundStatus {
	t.Helper()
	for _, r := range c.Rounds {
		if r.Round == round {
			return r
		}
	}
	t.Fatalf("course has no round %d; rounds = %+v", round, c.Rounds)
	return CourseRoundStatus{}
}

// A term that never crosses the budget year has ONE document, so it must carry
// no per-round breakdown at all — the screen keys off Rounds being empty to
// stay silent about "รอบ" for the ordinary term.
func TestDashboardSummary_NonCrossingTermHasNoRounds(t *testing.T) {
	f := newTCFixture(t)
	for _, ym := range []string{"2569-06", "2569-07"} {
		f.addPeriod(ym)
	}
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP710", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("รอบเดียว แดชบอร์ด", "undergrad")
	f.assignTAOn(ta, courseID, regSec, "undergrad", []string{"2026-06-10"})
	f.exec(`UPDATE teaching_courses SET exported_at = now() WHERE id = $1`, courseID)

	dash := dashboardFor(f)
	if dash.Split.Crosses {
		t.Error("มิ.ย.–ก.ค. sits inside one budget year — Crosses must be false")
	}
	c := summaryFor(dash.Courses, courseID)
	if c == nil {
		t.Fatal("course missing from dashboard summary")
	}
	if len(c.Rounds) != 0 {
		t.Errorf("non-crossing term must carry no rounds; got %+v", c.Rounds)
	}
}

// The course has real ตุลาคม work but no export_batches slice has covered it
// yet — round 2 must read billable-but-not-exported even though round 1 is
// already done. Once a round-2 batch is recorded, round 2 flips to exported
// while round 1 stays exported.
func TestDashboardSummary_RoundTwoStaysOwedUntilItIsExported(t *testing.T) {
	f := newTCFixture(t)
	for _, ym := range []string{"2569-06", "2569-07", "2569-08", "2569-09", "2569-10"} {
		f.addPeriod(ym)
	}
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP720", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("ต่อเนื่อง แดชบอร์ด", "undergrad")
	f.assignTAOn(ta, courseID, regSec, "undergrad", []string{"2026-08-10", "2026-10-05"})
	f.exec(`UPDATE teaching_courses SET exported_at = now() WHERE id = $1`, courseID)
	// Round 1 was exported as its own slice, the way a crossing term is meant
	// to be claimed.
	f.recordExportBatch(courseID, f.lectID, []string{"2026-06", "2026-07", "2026-08", "2026-09"})

	dash := dashboardFor(f)
	if !dash.Split.Crosses {
		t.Fatal("มิ.ย.–ต.ค. crosses 30 ก.ย. — Crosses must be true")
	}
	c := summaryFor(dash.Courses, courseID)
	if c == nil {
		t.Fatal("course missing from dashboard summary")
	}
	if r1 := roundOf(t, c, 1); !r1.Billable || !r1.Exported {
		t.Errorf("round 1 = %+v, want billable and exported", r1)
	}
	if r2 := roundOf(t, c, 2); !r2.Billable || r2.Exported {
		t.Errorf("round 2 = %+v, want billable but NOT exported", r2)
	}

	f.recordExportBatch(courseID, f.lectID, []string{"2026-10"})

	c = summaryFor(dashboardFor(f).Courses, courseID)
	if c == nil {
		t.Fatal("course missing from dashboard summary (2nd read)")
	}
	if r2 := roundOf(t, c, 2); !r2.Exported {
		t.Errorf("round 2 has now been exported; got %+v", r2)
	}
	if r1 := roundOf(t, c, 1); !r1.Exported {
		t.Errorf("recording round 2 must not un-export round 1; got %+v", r1)
	}
}

// A crossing term whose course never worked past กันยายน owes nothing in round
// 2. This is the state the old single boolean could not distinguish from
// "round 2 done": both read false, so the screen could not tell a finished
// course from one with a second file still due.
func TestDashboardSummary_RoundTwoNotBillableWhenNoOctoberWork(t *testing.T) {
	f := newTCFixture(t)
	for _, ym := range []string{"2569-06", "2569-07", "2569-08", "2569-09", "2569-10"} {
		f.addPeriod(ym)
	}
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP730", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("จบก่อนตุลา แดชบอร์ด", "undergrad")
	f.assignTAOn(ta, courseID, regSec, "undergrad", []string{"2026-08-10", "2026-09-14"})
	f.exec(`UPDATE teaching_courses SET exported_at = now() WHERE id = $1`, courseID)

	c := summaryFor(dashboardFor(f).Courses, courseID)
	if c == nil {
		t.Fatal("course missing from dashboard summary")
	}
	if r1 := roundOf(t, c, 1); !r1.Billable {
		t.Errorf("round 1 = %+v, want billable — the work is in ส.ค./ก.ย.", r1)
	}
	if r2 := roundOf(t, c, 2); r2.Billable {
		t.Errorf("round 2 = %+v, want NOT billable — no ตุลาคม work exists", r2)
	}
}

// The regression that motivated migration 0083: a WHOLE-TERM export must clear
// round 2, because it really did claim ตุลาคม.
//
// Before the fix, the handler stored the absent ?months= as SQL NULL, and
// `NULL && '{2026-10}'` is NULL rather than true — so the course stayed flagged
// as owing round 2 with no action available that could ever clear it. If this
// test fails, either ResolveCourseMonths stopped resolving or something began
// writing NULL months again.
func TestDashboardSummary_WholeTermExportClearsBothRounds(t *testing.T) {
	f := newTCFixture(t)
	all := []string{"2569-06", "2569-07", "2569-08", "2569-09", "2569-10"}
	for _, ym := range all {
		f.addPeriod(ym)
	}
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP740", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("ทั้งเทอม แดชบอร์ด", "undergrad")
	f.assignTAOn(ta, courseID, regSec, "undergrad", []string{"2026-08-10", "2026-10-05"})

	// What the handler now records when staff never touch the month picker:
	// the RESOLVED whole-term list, not nil.
	months, err := f.svc.ResolveCourseMonths(f.ctx, courseID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(months) != len(all) {
		t.Fatalf("ResolveCourseMonths(nil) = %v, want all %d months of the term", months, len(all))
	}
	f.recordExportBatch(courseID, f.lectID, months)

	c := summaryFor(dashboardFor(f).Courses, courseID)
	if c == nil {
		t.Fatal("course missing from dashboard summary")
	}
	if r2 := roundOf(t, c, 2); !r2.Exported {
		t.Errorf("a whole-term ZIP covered ตุลาคม, so round 2 is exported; got %+v", r2)
	}
	if r1 := roundOf(t, c, 1); !r1.Exported {
		t.Errorf("the same ZIP covered มิ.ย.–ก.ย.; got %+v", r1)
	}
}
