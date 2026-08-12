package service

import (
	"testing"

	"github.com/google/uuid"
)

/* -------------------------------------------------------------------------- */
/* RoundTwoOutstanding (12/08/2026)                                           */
/*                                                                            */
/* A course sitting in the payouts list's "ส่งออกแล้ว" bucket can still owe a  */
/* SECOND document once its term crosses the 30 กันยายน budget year — round 1 */
/* being exported must not read as the course being fully done.              */
/* -------------------------------------------------------------------------- */

func dashboardSummaryFor(f *tcFixture) []CourseSummary {
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

// A term that never crosses the budget year must never flag RoundTwoOutstanding
// — there is no round 2 to be outstanding.
func TestDashboardSummary_NonCrossingTermNeverFlagsRoundTwo(t *testing.T) {
	f := newTCFixture(t)
	for _, ym := range []string{"2569-06", "2569-07"} {
		f.addPeriod(ym)
	}
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP710", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("รอบเดียว แดชบอร์ด", "undergrad")
	f.assignTAOn(ta, courseID, regSec, "undergrad", []string{"2026-06-10"})
	f.exec(`UPDATE teaching_courses SET exported_at = now() WHERE id = $1`, courseID)

	c := summaryFor(dashboardSummaryFor(f), courseID)
	if c == nil {
		t.Fatal("course missing from dashboard summary")
	}
	if c.RoundTwoOutstanding {
		t.Error("a non-crossing term must never flag RoundTwoOutstanding")
	}
}

// The course has real ตุลาคม work but no export_batches slice has covered it
// yet — RoundTwoOutstanding must be true even though round 1 (exported_at) is
// already set. Once a round-2 export_batches row is recorded, it flips false.
func TestDashboardSummary_FlagsRoundTwoUntilItIsExported(t *testing.T) {
	f := newTCFixture(t)
	for _, ym := range []string{"2569-06", "2569-07", "2569-08", "2569-09", "2569-10"} {
		f.addPeriod(ym)
	}
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP720", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("ต่อเนื่อง แดชบอร์ด", "undergrad")
	f.assignTAOn(ta, courseID, regSec, "undergrad", []string{"2026-08-10", "2026-10-05"})
	f.exec(`UPDATE teaching_courses SET exported_at = now() WHERE id = $1`, courseID)

	c := summaryFor(dashboardSummaryFor(f), courseID)
	if c == nil {
		t.Fatal("course missing from dashboard summary")
	}
	if !c.RoundTwoOutstanding {
		t.Error("course has ตุลาคม work with no round-2 export yet — must flag RoundTwoOutstanding")
	}

	f.exec(`INSERT INTO export_batches
	            (id, teaching_course_id, file_path, file_name, ta_count, total_baht, generated_at, generated_by, months)
	        VALUES (gen_random_uuid(), $1, 'x', 'x.zip', 0, 0, now(), $2, $3)`,
		courseID, f.lectID, []string{"2026-10"})

	c = summaryFor(dashboardSummaryFor(f), courseID)
	if c == nil {
		t.Fatal("course missing from dashboard summary (2nd read)")
	}
	if c.RoundTwoOutstanding {
		t.Error("round 2 has now been exported — RoundTwoOutstanding must clear")
	}
}

// A crossing term whose course never worked past กันยายน has nothing owed in
// round 2 — RoundTwoOutstanding must stay false even though exported_at is
// set and the TERM itself crosses the boundary (some OTHER course might use
// round 2; this one does not).
func TestDashboardSummary_NoRoundTwoContentNeverFlags(t *testing.T) {
	f := newTCFixture(t)
	for _, ym := range []string{"2569-06", "2569-07", "2569-08", "2569-09", "2569-10"} {
		f.addPeriod(ym)
	}
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP730", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("จบก่อนตุลา แดชบอร์ด", "undergrad")
	f.assignTAOn(ta, courseID, regSec, "undergrad", []string{"2026-08-10", "2026-09-14"})
	f.exec(`UPDATE teaching_courses SET exported_at = now() WHERE id = $1`, courseID)

	c := summaryFor(dashboardSummaryFor(f), courseID)
	if c == nil {
		t.Fatal("course missing from dashboard summary")
	}
	if c.RoundTwoOutstanding {
		t.Error("course has no ตุลาคม work at all — must not flag RoundTwoOutstanding")
	}
}
