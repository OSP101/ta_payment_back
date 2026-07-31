package service

import (
	"strings"
	"testing"

	"ta-payment-back/internal/audit"
)

// The merged payout screen (31/07/2026) groups months nobody at the desk can
// move under "waiting on someone else". That group needs two things the old
// split screens never had:
//
//  1. a truthful answer to WHO is holding the month — the queue reported one
//     "open rows" number covering both the TA's unsent drafts and the lecturer's
//     unapproved submissions, so the screen blamed the lecturer either way; and
//  2. one action — nudging the lecturer — which must refuse when the lecturer's
//     queue is in fact empty, or it teaches officers to chase the wrong person.

func remindSvc(f *fixture) *SubmissionPeriodService {
	return &SubmissionPeriodService{pool: f.Pool, aud: audit.New(f.Pool), notify: f.Svc.notify}
}

// A draft is with the TA. Nothing to remind a lecturer about.
func TestRemindLecturer_RefusesWhenNothingIsWithTheLecturer(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2)) // stays 'draft'

	err := remindSvc(f).RemindLecturerUnapproved(f.ctx, f.LecturerID, f.CourseID)
	if err == nil {
		t.Fatal("a course whose rows are all unsent drafts must not offer a lecturer reminder — " +
			"the work is with the TA, and nudging the lecturer sends the officer after the wrong person")
	}
	if !strings.Contains(err.Error(), "ไม่มีรายการที่รออาจารย์") {
		t.Errorf("message should say the lecturer's queue is empty, got: %v", err)
	}
}

func TestRemindLecturer_SendsWhenRowsAwaitApproval(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("submit: %v", err)
	}

	if err := remindSvc(f).RemindLecturerUnapproved(f.ctx, f.LecturerID, f.CourseID); err != nil {
		t.Fatalf("rows are sitting in the lecturer's queue, the reminder must go out: %v", err)
	}

	var n int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id = $1`, f.LecturerID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("no notification reached the lecturer — the button would look like it worked and do nothing")
	}
}

// Once a day, per course. Without this an officer clearing a backlog can send
// the same lecturer a dozen identical notifications in a minute.
func TestRemindLecturer_ThrottledToOncePerDay(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("submit: %v", err)
	}
	svc := remindSvc(f)
	if err := svc.RemindLecturerUnapproved(f.ctx, f.LecturerID, f.CourseID); err != nil {
		t.Fatalf("first reminder: %v", err)
	}

	err := svc.RemindLecturerUnapproved(f.ctx, f.LecturerID, f.CourseID)
	if err == nil {
		t.Fatal("a second reminder the same day must be refused")
	}
	if !strings.Contains(err.Error(), "24 ชั่วโมง") {
		t.Errorf("message should say when it can be sent again, got: %v", err)
	}
}

// The queue's split of who-is-waiting. One number could not distinguish them,
// and the screen that reads it has to name a person.
func TestReviewQueue_SeparatesWaitingOnTAFromWaitingOnLecturer(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	// The queue is gated on a printed appointment order — without it the course
	// is not payable and never appears (see AppointedSQL).
	f.addAppointmentOrder()
	f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "", false)
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2)) // draft — with the TA
	f.mustUpsert(f.entry(day(11), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Both are now 'submitted'; put one back in the TA's hands.
	f.exec(`UPDATE work_logs SET status='draft'
	         WHERE assignment_id=$1 AND work_date=$2::date`, f.AssignmentID, day(10))

	rows, err := f.Periods.ListReviewQueue(f.ctx, f.TermID)
	if err != nil {
		t.Fatalf("ListReviewQueue: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.TeachingCourseID != f.CourseID {
			continue
		}
		found = true
		if r.WaitingTA != 1 || r.WaitingLecturer != 1 {
			t.Errorf("waiting_ta=%d waiting_lecturer=%d, want 1 and 1 — "+
				"open_rows=%d alone cannot tell the officer who to chase",
				r.WaitingTA, r.WaitingLecturer, r.OpenRows)
		}
		if r.OpenRows != r.WaitingTA+r.WaitingLecturer {
			t.Errorf("open_rows (%d) must stay the sum of the two halves (%d + %d)",
				r.OpenRows, r.WaitingTA, r.WaitingLecturer)
		}
	}
	if !found {
		t.Fatal("the fixture course is missing from the review queue")
	}
}

// The grid cell needs a reason to be clicked, not just the ability. RowCount and
// ManualCount are what turn "40.0 ชม." into "40.0 ชม., 2 of them typed by hand" —
// the officer can then skip the months that are entirely generated instead of
// opening all five of everyone's.
func TestReviewQueue_ReportsWhatIsInsideTheMonth(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.addAppointmentOrder()
	f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "", false)
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2)) // TA-typed → source 'manual'
	f.mustUpsert(f.entry(day(11), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1`, f.AssignmentID)
	// One of them came from the generator.
	f.exec(`UPDATE work_logs SET source='auto'
	         WHERE assignment_id=$1 AND work_date=$2::date`, f.AssignmentID, day(11))

	rows, err := f.Periods.ListReviewQueue(f.ctx, f.TermID)
	if err != nil {
		t.Fatalf("ListReviewQueue: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.TeachingCourseID != f.CourseID {
			continue
		}
		found = true
		if r.RowCount != 2 {
			t.Errorf("row_count = %d, want 2", r.RowCount)
		}
		if r.ManualCount != 1 {
			t.Errorf("manual_count = %d, want 1 — a cell that cannot distinguish a "+
				"hand-typed claim from a generated copy gives the officer no way to "+
				"choose which month to read", r.ManualCount)
		}
	}
	if !found {
		t.Fatal("the fixture course is missing from the review queue")
	}
}
