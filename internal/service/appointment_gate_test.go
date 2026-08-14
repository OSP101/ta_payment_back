package service

import (
	"testing"

	"ta-payment-back/internal/audit"
)

// The printed appointment order (คำสั่งแต่งตั้ง) is what makes a TA's work
// official, so it gates the last two staff steps: nothing reaches payout review
// or export until the order exists.
//
// These tests approach it from both sides on purpose. Letting unappointed work
// through is the original defect — staff sign off money that the finance office
// will not pay. But over-blocking is just as bad and less visible: a course that
// can never satisfy the rule is work that silently disappears, and one of the
// tests below exists only because the first implementation had exactly that
// deadlock.

// A course whose TA has no printed order must not appear in the payout queue.
func TestReviewQueue_RequiresPrintedAppointmentOrder(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1`, f.AssignmentID)
	f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "", false)
	svc := f.Periods
	rows, err := svc.ListReviewQueue(f.ctx, f.TermID)
	if err != nil {
		t.Fatalf("ListReviewQueue: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("queue has %d rows before the appointment order was printed; "+
			"staff would sign off work that is not payable yet", len(rows))
	}

	// ...and the officer must be able to tell this apart from "no work at all".
	awaiting, err := svc.CountAwaitingAppointment(f.ctx, f.TermID)
	if err != nil {
		t.Fatalf("CountAwaitingAppointment: %v", err)
	}
	if awaiting != 1 {
		t.Errorf("awaiting_appointment = %d, want 1 — an empty queue with no "+
			"explanation is indistinguishable from a finished one", awaiting)
	}

	// Printing the order releases it, and the two numbers swap.
	f.addAppointmentOrder()
	rows, err = svc.ListReviewQueue(f.ctx, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("queue has %d rows after the order was printed, want 1", len(rows))
	}
	if awaiting, err = svc.CountAwaitingAppointment(f.ctx, f.TermID); err != nil {
		t.Fatal(err)
	} else if awaiting != 0 {
		t.Errorf("awaiting_appointment = %d after printing, want 0 — the two counts "+
			"must partition the same work", awaiting)
	}
}

// The order names (course, TA) pairs, not courses. A TA added to a course after
// the order was printed is not appointed, even though a colleague on the same
// course is — and paying them would be paying against a document that does not
// name them.
func TestReviewQueue_GateIsPerTANotPerCourse(t *testing.T) {
	f, _ := reviewFixture(t) // this TA IS on an order
	// A second TA on the same course, with approved work, but not on any order.
	other := f.secondTAOnSameCourse()

	rows, err := f.Periods.ListReviewQueue(f.ctx, f.TermID)
	if err != nil {
		t.Fatalf("ListReviewQueue: %v", err)
	}
	for _, r := range rows {
		if r.TAID == other {
			t.Fatalf("%s is not named on any appointment order but is in the payout queue "+
				"— the gate collapsed to course level", other)
		}
	}
	var found bool
	for _, r := range rows {
		if r.TAID == f.TAID {
			found = true
		}
	}
	if !found {
		t.Error("the appointed TA was dropped along with the unappointed one")
	}
}

// The exports dashboard needs BOTH conditions, and needs to say which one is
// missing — the two have different remedies on different screens.
func TestExportSummary_EligibilityNeedsOrderAndCompletedReview(t *testing.T) {
	f, month := reviewFixtureWithoutOrder(t)
	exp := &ExportBatchService{pool: f.Pool, aud: audit.New(f.Pool)}
	budget := &BudgetService{pool: f.Pool}

	row := func() CourseSummary {
		t.Helper()
		all, err := exp.DashboardSummary(f.ctx, budget, exportSvcFor(f), f.TermID)
		if err != nil {
			t.Fatalf("DashboardSummary: %v", err)
		}
		for _, s := range all.Courses {
			if s.TeachingCourseID == f.CourseID {
				return s
			}
		}
		t.Fatal("course missing from the summary")
		return CourseSummary{}
	}

	// Neither condition met.
	s := row()
	if s.HasAppointmentOrder {
		t.Error("has_appointment_order is true with no order printed")
	}
	if s.ExportEligible {
		t.Error("export_eligible is true with nothing done")
	}

	// Order printed, review still outstanding.
	f.addAppointmentOrder()
	s = row()
	if !s.HasAppointmentOrder {
		t.Error("has_appointment_order is false after printing the order")
	}
	if s.ReviewComplete {
		t.Error("review_complete is true while a month is still waiting on step 3")
	}
	if s.ExportEligible {
		t.Error("export_eligible is true on the order alone — the amounts are not final yet")
	}

	// Review signed off: now it qualifies.
	staff := f.insertUser("staff", "officer")
	pid := mustUUID(t, f.periodID(t, month))
	if err := f.Periods.MarkStaffReviewed(f.ctx, staff, pid, f.TAID, f.CourseID, ""); err != nil {
		t.Fatalf("MarkStaffReviewed: %v", err)
	}
	s = row()
	if !s.ReviewComplete {
		t.Errorf("review_complete is false after sign-off (unreviewed months: %v)", s.UnreviewedMonths)
	}
	if !s.ExportEligible {
		t.Error("export_eligible is false with the order printed and review complete")
	}
}

// A course nobody has worked on is not "complete" — it is empty. Treating an
// absence of work as a finished review would put every untouched course in the
// term on the export screen, which is the list this filtering exists to shorten.
func TestExportSummary_NoWorkIsNotReviewComplete(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "", false)
	f.addAppointmentOrder()

	exp := &ExportBatchService{pool: f.Pool, aud: audit.New(f.Pool)}
	all, err := exp.DashboardSummary(f.ctx, &BudgetService{pool: f.Pool}, exportSvcFor(f), f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range all.Courses {
		if s.TeachingCourseID != f.CourseID {
			continue
		}
		if s.ReviewComplete || s.ExportEligible {
			t.Errorf("a course with no work logged reports review_complete=%v export_eligible=%v",
				s.ReviewComplete, s.ExportEligible)
		}
		return
	}
	t.Fatal("course missing from the summary")
}

// THE DEADLOCK. An unappointed TA's approved month cannot be reviewed — it is not
// in the queue. If it still counted as an unreviewed month, the course could
// never reach ReviewComplete, so it would never become exportable no matter what
// staff did: the screen would demand a sign-off that no screen offers.
func TestExportSummary_UnappointedWorkDoesNotBlockExportForever(t *testing.T) {
	f, month := reviewFixture(t) // appointed TA, approved work

	// A colleague with approved work who is NOT on the order.
	f.secondTAOnSameCourse()

	// Everything a diligent officer CAN do: sign off the one row the queue offers.
	staff := f.insertUser("staff", "officer")
	pid := mustUUID(t, f.periodID(t, month))
	if err := f.Periods.MarkStaffReviewed(f.ctx, staff, pid, f.TAID, f.CourseID, ""); err != nil {
		t.Fatalf("MarkStaffReviewed: %v", err)
	}

	exp := &ExportBatchService{pool: f.Pool, aud: audit.New(f.Pool)}
	all, err := exp.DashboardSummary(f.ctx, &BudgetService{pool: f.Pool}, exportSvcFor(f), f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range all.Courses {
		if s.TeachingCourseID != f.CourseID {
			continue
		}
		if !s.ExportEligible {
			t.Errorf("course is stuck: export_eligible=false with unreviewed months %v, "+
				"but those months belong to a TA the review queue refuses to show",
				s.UnreviewedMonths)
		}
		return
	}
	t.Fatal("course missing from the summary")
}
