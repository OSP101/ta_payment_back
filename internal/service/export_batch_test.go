package service

import (
	"testing"

	"ta-payment-back/internal/audit"
)

// The payout card names the lecturer because they are who an officer phones
// when a course is stuck — so it names them the way the office addresses them,
// with the ตำแหน่งทางวิชาการ, matching the claim documents the same card links to.
//
// The second half is the part that has bitten this codebase twice: the title
// must not become the sort key. Gluing it on and ordering by the result files
// every รศ. above every ผศ., which is rank order — not a list anyone can look a
// name up in.
func TestDashboardSummary_LecturersCarryTheirTitleButSortByGivenName(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "", false)

	// A second lecturer on the same course, named so that the two orderings
	// disagree: sorted by given name อนงค์ comes first, sorted with the title
	// glued on "รศ. ดร.อนงค์" comes after "ผศ. ดร.ฮารีส".
	second := f.insertUser("lecturer", "lecturer2")
	f.exec(`UPDATE users SET title='รศ. ดร.', first_name='อนงค์', last_name='ก' WHERE id=$1`, second)
	f.exec(`UPDATE users SET title='ผศ. ดร.', first_name='ฮารีส', last_name='ฮ' WHERE id=$1`, f.LecturerID)
	f.exec(`INSERT INTO teaching_lecturers (teaching_course_id, lecturer_id, is_primary)
	        VALUES ($1, $2, FALSE) ON CONFLICT DO NOTHING`, f.CourseID, second)
	// is_primary sorts first, so clear it — otherwise the primary flag decides
	// the order and the given-name tie-break never runs.
	f.exec(`UPDATE teaching_lecturers SET is_primary = FALSE WHERE teaching_course_id = $1`, f.CourseID)

	exp := &ExportBatchService{pool: f.Pool, aud: audit.New(f.Pool)}
	all, err := exp.DashboardSummary(f.ctx, &BudgetService{pool: f.Pool}, exportSvcFor(f), f.TermID)
	if err != nil {
		t.Fatalf("DashboardSummary: %v", err)
	}
	var got string
	for _, s := range all.Courses {
		if s.TeachingCourseID == f.CourseID {
			got = s.LecturerNames
		}
	}
	const want = "รศ. ดร.อนงค์ ก, ผศ. ดร.ฮารีส ฮ"
	if got != want {
		t.Errorf("lecturer_names = %q, want %q — the title prints, and the order "+
			"stays on the given name", got, want)
	}
}
