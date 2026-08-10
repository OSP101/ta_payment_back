package service

import (
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

/* -------------------------------------------------------------------------- */
/* Fiscal-year slicing of the per-course claim ZIP (ใบเบิก), 10/08/2026        */
/*                                                                            */
/* The claim workbook used to be built by claimLogsAllMonths — literally      */
/* "claimLogs without the month filter" — so a second export issued after     */
/* ตุลาคม repeated มิ.ย.–ก.ย. in full and the finance office was billed for    */
/* them twice, once against each budget year.                                 */
/* -------------------------------------------------------------------------- */

// twoMonthClaim sets a course up with approved work in two consecutive months
// and a submission period for each, then returns their Gregorian keys.
func twoMonthClaim(t *testing.T, f *fixture) (first, second string) {
	t.Helper()
	m1 := monthStart()
	m2 := m1.AddDate(0, 1, 0)
	f.addSubmissionPeriod(m1.Format("01"), "2026-12-31", "", false)
	f.addSubmissionPeriod(m2.Format("01"), "2026-12-31", "", false)
	f.mustUpsert(f.entry(m1.AddDate(0, 0, 9).Format("2006-01-02"), "09:00", "11:00", 2))
	f.mustUpsert(f.entry(m2.AddDate(0, 0, 9).Format("2006-01-02"), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1`, f.AssignmentID)
	return m1.Format("2006-01"), m2.Format("2006-01")
}

func claimTotal(comp *exportComputation) float64 {
	var t float64
	for _, r := range comp.records {
		t += r.actualPaid
	}
	return round2(t)
}

// THE invariant, for the document that actually reaches the finance office:
// slicing a term's claim into fiscal documents partitions the money, never
// recomputes it. Every คาบ lands in exactly one slice, so the slices sum to the
// undivided total. Failing this means either double-billing across two
// appropriations or a month nobody ever claims.
func TestBuildExportRows_MonthSlicesSumToTheWholeTerm(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	m1, m2 := twoMonthClaim(t, f)
	svc := exportSvcFor(f)

	whole, err := svc.buildExportRows(f.ctx, f.CourseID, nil)
	if err != nil {
		t.Fatal(err)
	}
	sliceA, err := svc.buildExportRows(f.ctx, f.CourseID, []string{m1})
	if err != nil {
		t.Fatal(err)
	}
	sliceB, err := svc.buildExportRows(f.ctx, f.CourseID, []string{m2})
	if err != nil {
		t.Fatal(err)
	}

	total := claimTotal(whole)
	if total <= 0 {
		t.Fatalf("fixture bug: whole-term claim is %.2f, expected real money", total)
	}
	if sum := round2(claimTotal(sliceA) + claimTotal(sliceB)); sum != total {
		t.Errorf("slices sum to %.2f but the whole term is %.2f — the finance office is being billed twice or a month is lost (%s=%.2f, %s=%.2f)",
			sum, total, m1, claimTotal(sliceA), m2, claimTotal(sliceB))
	}
	// Each slice really is only its own month: one 2-hour คาบ each, so they
	// must be equal halves rather than one slice quietly carrying both.
	if a, b := claimTotal(sliceA), claimTotal(sliceB); a != b {
		t.Errorf("%s = %.2f but %s = %.2f — each month holds one identical คาบ", m1, a, m2, b)
	}
	// And the hours follow the money.
	if whole.records[0].hoursTotal <= sliceA.records[0].hoursTotal {
		t.Errorf("whole-term hours %.1f are not more than one month's %.1f",
			whole.records[0].hoursTotal, sliceA.records[0].hoursTotal)
	}
}

// The printed claim rows must be sliced too, not just the totals — a workbook
// whose money says กันยายน while its daily table lists ตุลาคม is worse than one
// that is simply wrong, because it looks reconcilable.
func TestClaimLogs_RestrictedToTheSelectedMonths(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	m1, _ := twoMonthClaim(t, f)
	svc := exportSvcFor(f)

	all, err := svc.claimLogsAllMonths(f.ctx, f.TAID, f.CourseID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("unscoped claim logs = %d rows, want both months", len(all))
	}
	scoped, err := svc.claimLogsAllMonths(f.ctx, f.TAID, f.CourseID, []string{m1})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 {
		t.Fatalf("claim logs for %s = %d rows, want just that month's", m1, len(scoped))
	}
	if got := scoped[0].Date.Format("2006-01"); got != m1 {
		t.Errorf("claim log month = %s, want %s", got, m1)
	}
}

// The gate must judge only the months being claimed, or the มิ.ย.–ก.ย. document
// — which has to be filed before 30 กันยายน — cannot be produced until ตุลาคม
// has been taught and reviewed, i.e. only after its budget year has closed.
func TestCourseExportBlockers_ScopedToSelectedMonths(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	payoutReady(f)
	m1 := monthStart()
	m2 := m1.AddDate(0, 1, 0)
	p1 := f.addSubmissionPeriod(m1.Format("01"), "2026-12-31", "", false)
	f.addSubmissionPeriod(m2.Format("01"), "2026-12-31", "", false)
	f.mustUpsert(f.entry(m1.AddDate(0, 0, 9).Format("2006-01-02"), "09:00", "11:00", 2))
	f.mustUpsert(f.entry(m2.AddDate(0, 0, 9).Format("2006-01-02"), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1`, f.AssignmentID)
	// Only the first month has been signed off by staff.
	f.exec(`INSERT INTO submission_period_status (id, submission_period_id, ta_id, teaching_course_id, status)
	        VALUES (gen_random_uuid(), $1, $2, $3, 'staff_reviewed')`, p1, f.TAID, f.CourseID)

	svc := exportSvcFor(f)
	scoped, err := svc.CourseExportBlockers(f.ctx, f.CourseID, []string{m1.Format("2006-01")})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 0 {
		t.Errorf("%s is signed off, so its claim must be issuable; got %+v", m1.Format("2006-01"), scoped)
	}
	whole, err := svc.CourseExportBlockers(f.ctx, f.CourseID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(whole) == 0 {
		t.Error("the second month is unreviewed, so the undivided claim must still be blocked")
	}
}

// Exporting มิ.ย.–ก.ย. in September must not freeze ตุลาคม: that month is still
// being taught and belongs to the next budget year's document, so its worklogs
// have to stay editable.
func TestMarkCourseExported_LocksOnlySelectedMonths(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	m1 := monthStart()
	m2 := m1.AddDate(0, 1, 0)
	p1 := f.addSubmissionPeriod(m1.Format("01"), "2026-12-31", "", false)
	p2 := f.addSubmissionPeriod(m2.Format("01"), "2026-12-31", "", false)
	f.mustUpsert(f.entry(m1.AddDate(0, 0, 9).Format("2006-01-02"), "09:00", "11:00", 2))
	f.mustUpsert(f.entry(m2.AddDate(0, 0, 9).Format("2006-01-02"), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1`, f.AssignmentID)
	// Both months are staff-signed and therefore lockable; only one is exported.
	for _, p := range []uuid.UUID{p1, p2} {
		f.exec(`INSERT INTO submission_period_status (id, submission_period_id, ta_id, teaching_course_id, status)
		        VALUES (gen_random_uuid(), $1, $2, $3, 'staff_reviewed')`, p, f.TAID, f.CourseID)
	}

	n, err := f.Periods.MarkCourseExported(f.ctx, f.StaffID, f.CourseID, []string{m1.Format("2006-01")})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("locked %d cells, want 1 — only the exported month may freeze", n)
	}
	status := func(p uuid.UUID) string {
		var s string
		if err := f.Pool.QueryRow(f.ctx,
			`SELECT status FROM submission_period_status
			 WHERE submission_period_id=$1 AND ta_id=$2 AND teaching_course_id=$3`,
			p, f.TAID, f.CourseID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if got := status(p1); got != "exported" {
		t.Errorf("exported month status = %q, want exported", got)
	}
	if got := status(p2); got != StatusStaffReviewed {
		t.Errorf("unexported month status = %q, want it left editable at %q", got, StatusStaffReviewed)
	}
}

// Coverage is what stops staff billing a month twice or never billing it.
func TestCourseExportCoverage_TracksClaimedMonths(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	m1, _ := twoMonthClaim(t, f)
	svc := exportSvcFor(f)

	cov, err := svc.CourseExportCoverage(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cov.Months) != 2 {
		t.Fatalf("coverage months = %d, want the term's 2", len(cov.Months))
	}
	for _, m := range cov.Months {
		if m.Issued {
			t.Errorf("%s reported as claimed before any export", m.YearMonth)
		}
	}

	batches := &ExportBatchService{pool: f.Pool, aud: audit.New(f.Pool)}
	if _, err := batches.Record(f.ctx, f.StaffID, ExportBatch{
		TeachingCourseID: f.CourseID, FilePath: "x.zip", FileName: "x.zip",
		TACount: 1, TotalBaht: 100, Months: []string{m1},
	}); err != nil {
		t.Fatal(err)
	}
	cov, err = svc.CourseExportCoverage(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range cov.Months {
		want := m.YearMonth == m1
		if m.Issued != want {
			t.Errorf("%s issued = %v, want %v after claiming only %s", m.YearMonth, m.Issued, want, m1)
		}
	}
}
