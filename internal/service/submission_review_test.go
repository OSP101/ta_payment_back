package service

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// mustUUID parses an id read back from SQL, failing the test rather than
// returning a zero value that would produce a confusing "not found" later.
func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return id
}

// Step 3 of the staff workflow — ตรวจสอบเบิกจ่ายค่าตอบแทน.
//
// The load-bearing claim is the gate: a month that staff have not signed off
// must not reach the payout export. Before this step existed, export accepted
// anything the lecturer had approved.

// reviewFixture builds a course with one approved month of work, a matching
// submission period, and a printed appointment order — the shape every test here
// needs.
//
// The order is part of the baseline because without it the course is not in the
// review queue at all, which is a separate rule with its own tests
// (TestReviewQueue_RequiresPrintedAppointmentOrder). Tests about what the queue
// DOES with a course should not have to re-establish that it is in the queue.
func reviewFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f, month := reviewFixtureWithoutOrder(t)
	f.addAppointmentOrder()
	return f, month
}

// reviewFixtureWithoutOrder is the same world with the TA NOT yet appointed —
// requested and approved in the app, but no printed order. Used by the tests that
// are about the gate itself rather than about what happens past it.
func reviewFixtureWithoutOrder(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1`, f.AssignmentID)
	f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "", false)
	return f, currentMonthMM()
}

func (f *fixture) periodID(t *testing.T, month string) (id string) {
	t.Helper()
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT id FROM submission_periods WHERE term_id=$1 AND RIGHT(year_month,2)=$2`,
		f.TermID, month).Scan(&id); err != nil {
		t.Fatalf("look up period: %v", err)
	}
	return id
}

func (f *fixture) statusOf(t *testing.T) string {
	t.Helper()
	var st string
	err := f.Pool.QueryRow(f.ctx,
		`SELECT status FROM submission_period_status WHERE ta_id=$1 AND teaching_course_id=$2`,
		f.TAID, f.CourseID).Scan(&st)
	if err != nil {
		return "(no row)"
	}
	return st
}

// The gate. An approved-but-unreviewed month must not export.
func TestExport_RefusesMonthNotStaffReviewed(t *testing.T) {
	f, _ := reviewFixture(t)
	staff := f.insertUser("staff", "officer")

	n, err := f.Periods.MarkCourseExported(f.ctx, staff, f.CourseID, nil)
	if err != nil {
		t.Fatalf("MarkCourseExported: %v", err)
	}
	if n != 0 {
		t.Fatalf("exported %d rows without staff review — the gate is open", n)
	}
	if got := f.statusOf(t); got == "exported" {
		t.Fatalf("status reached %q without staff review", got)
	}
}

// …and the same month exports once staff have signed it off.
func TestExport_AllowsMonthAfterStaffReview(t *testing.T) {
	f, month := reviewFixture(t)
	staff := f.insertUser("staff", "officer")
	pid := f.periodID(t, month)

	if err := f.Periods.MarkStaffReviewed(f.ctx, staff,
		mustUUID(t, pid), f.TAID, f.CourseID, "ตรวจแล้ว"); err != nil {
		t.Fatalf("MarkStaffReviewed: %v", err)
	}
	if got := f.statusOf(t); got != StatusStaffReviewed {
		t.Fatalf("status = %q, want %q", got, StatusStaffReviewed)
	}

	n, err := f.Periods.MarkCourseExported(f.ctx, staff, f.CourseID, nil)
	if err != nil {
		t.Fatalf("MarkCourseExported: %v", err)
	}
	if n == 0 {
		t.Fatal("a staff-reviewed month should export")
	}
	if got := f.statusOf(t); got != "exported" {
		t.Fatalf("status = %q, want exported", got)
	}
}

// Signing off on a month the TA can still edit would make the signature
// meaningless — and the export gate downstream trusts it.
func TestStaffReview_RefusesWhileRowsUnapproved(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2)) // stays draft
	f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "", false)
	staff := f.insertUser("staff", "officer")
	pid := f.periodID(t, currentMonthMM())

	err := f.Periods.MarkStaffReviewed(f.ctx, staff, mustUUID(t, pid), f.TAID, f.CourseID, "")
	if err == nil {
		t.Fatal("must refuse while rows are still unapproved")
	}
	if !strings.Contains(err.Error(), "อาจารย์ยังไม่อนุมัติ") {
		t.Errorf("message should explain what is missing, got: %v", err)
	}
}

func TestStaffReview_RefusesEmptyMonth(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "", false)
	staff := f.insertUser("staff", "officer")
	pid := f.periodID(t, currentMonthMM())

	if err := f.Periods.MarkStaffReviewed(f.ctx, staff, mustUUID(t, pid), f.TAID, f.CourseID, ""); err == nil {
		t.Fatal("a month with no approved work has nothing to review")
	}
}

// Double sign-off must not silently overwrite the first reviewer's name.
func TestStaffReview_RefusesSecondTime(t *testing.T) {
	f, month := reviewFixture(t)
	staff := f.insertUser("staff", "officer")
	pid := mustUUID(t, f.periodID(t, month))

	if err := f.Periods.MarkStaffReviewed(f.ctx, staff, pid, f.TAID, f.CourseID, ""); err != nil {
		t.Fatalf("first review: %v", err)
	}
	err := f.Periods.MarkStaffReviewed(f.ctx, staff, pid, f.TAID, f.CourseID, "")
	if err == nil {
		t.Fatal("reviewing an already-reviewed month must be refused")
	}
}

// The queue is what staff actually work from, so it must show the month before
// review and reflect the new state after.
func TestReviewQueue_ShowsPendingThenReviewed(t *testing.T) {
	f, month := reviewFixture(t)
	staff := f.insertUser("staff", "officer")

	rows, err := f.Periods.ListReviewQueue(f.ctx, f.TermID)
	if err != nil {
		t.Fatalf("ListReviewQueue: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 queued month, got %d", len(rows))
	}
	r := rows[0]
	if r.Status != "pending" {
		t.Errorf("status = %q, want pending", r.Status)
	}
	if r.ApprovedHours != 2 {
		t.Errorf("approved hours = %v, want 2", r.ApprovedHours)
	}
	if r.OpenRows != 0 {
		t.Errorf("open rows = %d, want 0", r.OpenRows)
	}
	// 2h undergrad regular at the fixture's default 50฿/h.
	if r.ApprovedBaht != 100 {
		t.Errorf("approved baht = %v, want 100", r.ApprovedBaht)
	}

	if err := f.Periods.MarkStaffReviewed(f.ctx, staff,
		mustUUID(t, f.periodID(t, month)), f.TAID, f.CourseID, ""); err != nil {
		t.Fatalf("MarkStaffReviewed: %v", err)
	}
	rows, err = f.Periods.ListReviewQueue(f.ctx, f.TermID)
	if err != nil {
		t.Fatalf("ListReviewQueue after review: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != StatusStaffReviewed {
		t.Fatalf("queue should still show the row as reviewed, got %+v", rows)
	}
}

// A month with unapproved rows must be visible but flagged, not hidden —
// staff need to know it exists in order to chase it.
func TestReviewQueue_FlagsOpenRows(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.mustUpsert(f.entry(day(11), "09:00", "10:00", 1))
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1 AND work_date=$2::date`,
		f.AssignmentID, day(10))
	f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "", false)
	f.addAppointmentOrder() // the queue is gated on it — see AppointedSQL

	rows, err := f.Periods.ListReviewQueue(f.ctx, f.TermID)
	if err != nil {
		t.Fatalf("ListReviewQueue: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].OpenRows != 1 {
		t.Errorf("open rows = %d, want 1", rows[0].OpenRows)
	}
}

// The export screen names what it is skipping rather than quietly offering
// fewer downloads.
func TestUnreviewedCourseNames(t *testing.T) {
	f, month := reviewFixture(t)
	staff := f.insertUser("staff", "officer")

	names, err := f.Periods.UnreviewedCourseNames(f.ctx, f.CourseID)
	if err != nil {
		t.Fatalf("UnreviewedCourseNames: %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("expected the unreviewed month to be named, got %v", names)
	}

	if err := f.Periods.MarkStaffReviewed(f.ctx, staff,
		mustUUID(t, f.periodID(t, month)), f.TAID, f.CourseID, ""); err != nil {
		t.Fatalf("MarkStaffReviewed: %v", err)
	}
	names, err = f.Periods.UnreviewedCourseNames(f.ctx, f.CourseID)
	if err != nil {
		t.Fatalf("UnreviewedCourseNames after review: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("nothing should remain unreviewed, got %v", names)
	}
}

// The send-back rule requires ranks to strictly decrease, so inserting the new
// state must not let an exported month bounce "forward" onto it.
func TestStatusRank_StaffReviewSitsBeforeExport(t *testing.T) {
	order := []string{"pending", StatusStaffReviewed, "exported", "finance_sent"}
	for i := 1; i < len(order); i++ {
		if statusRank[order[i-1]] >= statusRank[order[i]] {
			t.Fatalf("%s must rank below %s", order[i-1], order[i])
		}
	}
}
