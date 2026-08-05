package service

import (
	"testing"

	"github.com/google/uuid"
)

// ApproveMany exists because looping the single-assignment endpoint is not the
// same thing. A TA who helps with two sections is two assignments; if the
// second call is refused the first has already committed, and the lecturer is
// shown an error for something that half happened.
//
// These tests pin the two properties the loop could not give: all-or-nothing,
// and a budget check that sees the whole batch.

// twoSectionFixture gives one TA two sections of one course, each with its own
// submitted hours (NOT co-taught — separate sittings, so the hours add up).
func twoSectionFixture(t *testing.T, o fixtureOpts) (*fixture, uuid.UUID) {
	t.Helper()
	f := newFixture(t, o)
	sibling := f.siblingAssignment("regular", nil)

	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	second := f.entry(day(11), "09:00", "11:00", 2)
	second.AssignmentID = sibling
	f.mustUpsert(second)

	f.exec(`UPDATE work_logs SET status='submitted', submitted_at=now()
	        WHERE assignment_id IN ($1, $2)`, f.AssignmentID, sibling)
	return f, sibling
}

func (f *fixture) worklogStatusOf(t *testing.T, assignmentID uuid.UUID) string {
	t.Helper()
	var s string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT DISTINCT status FROM work_logs WHERE assignment_id=$1`, assignmentID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestApproveMany_ApprovesEverySectionInOneGo(t *testing.T) {
	f, sibling := twoSectionFixture(t, fixtureOpts{})

	if err := f.Svc.ApproveMany(f.ctx, f.LecturerID,
		[]uuid.UUID{f.AssignmentID, sibling}, "", false); err != nil {
		t.Fatalf("ApproveMany: %v", err)
	}
	if got := f.worklogStatusOf(t, f.AssignmentID); got != "approved" {
		t.Errorf("section 1 = %q, want approved", got)
	}
	if got := f.worklogStatusOf(t, sibling); got != "approved" {
		t.Errorf("section 2 = %q, want approved", got)
	}

	// One audit row per assignment — the trail is still per-assignment even
	// though the decision was one act.
	var n int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM audit_logs WHERE action='worklog.approve'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("audit rows = %d, want 2", n)
	}
}

// The whole point. If anything refuses part-way, NOTHING may be left approved.
func TestApproveMany_LeavesNothingApprovedWhenOneSectionFails(t *testing.T) {
	f, sibling := twoSectionFixture(t, fixtureOpts{})

	// Make the audit write fail on the way through, which aborts the batch
	// after the first section's UPDATE has already run inside the transaction.
	f.exec(`ALTER TABLE audit_logs
	        ADD CONSTRAINT no_batch_audit CHECK (action <> 'worklog.approve')`)

	if err := f.Svc.ApproveMany(f.ctx, f.LecturerID,
		[]uuid.UUID{f.AssignmentID, sibling}, "", false); err == nil {
		t.Fatal("a batch that could not be completed must be refused")
	}
	if got := f.worklogStatusOf(t, f.AssignmentID); got != "submitted" {
		t.Errorf("section 1 = %q, want submitted — the first section stayed approved "+
			"while the batch failed, which is exactly what looping the single "+
			"endpoint did", got)
	}
	if got := f.worklogStatusOf(t, sibling); got != "submitted" {
		t.Errorf("section 2 = %q, want submitted", got)
	}
}

// The budget no longer refuses an approval — it decides which MONTHS get paid
// (04/08/2026). This used to assert the opposite: that a batch pushing the
// course over its cap was rejected whole. Work that has already happened cannot
// be un-happened by refusing to record it, so the batch goes through and the
// shortfall is settled at export by dropping whole months.
func TestApproveMany_ApprovesEvenWhenItExceedsTheCourseBudget(t *testing.T) {
	f, sibling := twoSectionFixture(t, fixtureOpts{
		Rates: rateOverrides{UGRegularDailyCap: 24},
	})
	// Price an hour so this batch cannot possibly fit the course budget.
	f.exec(`UPDATE pay_rates SET undergrad_regular = 999999`)

	if err := f.Svc.ApproveMany(f.ctx, f.LecturerID,
		[]uuid.UUID{f.AssignmentID, sibling}, "", false); err != nil {
		t.Fatalf("a batch over budget must still be approved: %v", err)
	}
	for _, id := range []uuid.UUID{f.AssignmentID, sibling} {
		if got := f.worklogStatusOf(t, id); got != "approved" {
			t.Errorf("section %v = %q, want approved", id, got)
		}
	}

	// ...and the overrun is visible where it is actually settled.
	st, err := exportSvcFor(f).SettleCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if !st.OverBudget || len(st.UnpaidMonths) == 0 {
		t.Errorf("settlement = %+v, want the overrun reported as unpaid months", st)
	}
}

// A section already finished must not sink the batch — one half of a co-taught
// pair is routinely approved on its own first.
func TestApproveMany_SkipsSectionsWithNothingLeftToApprove(t *testing.T) {
	f, sibling := twoSectionFixture(t, fixtureOpts{})
	f.exec(`UPDATE work_logs SET status='approved', approved_at=now(), approved_by=$1
	        WHERE assignment_id=$2`, f.LecturerID, f.AssignmentID)

	if err := f.Svc.ApproveMany(f.ctx, f.LecturerID,
		[]uuid.UUID{f.AssignmentID, sibling}, "", false); err != nil {
		t.Fatalf("a batch with one section already done must still approve the rest: %v", err)
	}
	if got := f.worklogStatusOf(t, sibling); got != "approved" {
		t.Errorf("section 2 = %q, want approved", got)
	}
}

// ...but a batch where there was nothing to do anywhere is an error, not a
// silent success that leaves the caller thinking work happened.
func TestApproveMany_RefusesWhenNothingIsWaiting(t *testing.T) {
	f, sibling := twoSectionFixture(t, fixtureOpts{})
	f.exec(`UPDATE work_logs SET status='approved', approved_at=now(), approved_by=$1
	        WHERE assignment_id IN ($2, $3)`, f.LecturerID, f.AssignmentID, sibling)

	if err := f.Svc.ApproveMany(f.ctx, f.LecturerID,
		[]uuid.UUID{f.AssignmentID, sibling}, "", false); err == nil {
		t.Fatal("an empty batch must report that there was nothing to approve")
	}
}

// A lecturer must not approve hours on someone else's course, batch or not.
func TestApproveMany_RefusesAssignmentsOutsideTheReviewersCourses(t *testing.T) {
	f, sibling := twoSectionFixture(t, fixtureOpts{})
	stranger := f.insertUser("lecturer", "outsider")

	if err := f.Svc.ApproveMany(f.ctx, stranger,
		[]uuid.UUID{f.AssignmentID, sibling}, "", false); err == nil {
		t.Fatal("a lecturer who teaches none of these courses must be refused")
	}
	if got := f.worklogStatusOf(t, f.AssignmentID); got != "submitted" {
		t.Errorf("section 1 = %q, want submitted", got)
	}
}
