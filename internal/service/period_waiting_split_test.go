package service

import (
	"testing"

	"github.com/google/uuid"
)

// The TA home page told a TA their LECTURER had not approved ten rows the TA
// had never sent. One count — "unapproved" — stood for three very different
// states, and the page picked the wrong one to name. These tests pin the split.

func waitingSplitFor(t *testing.T, f *fixture) (unapproved, waitingTA, waitingLecturer int) {
	t.Helper()
	svc := &SubmissionPeriodService{pool: f.Pool}
	rows, err := svc.PendingByTA(f.ctx, f.TAID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.TeachingCourseID == f.CourseID {
			unapproved += r.WorklogUnapproved
			waitingTA += r.WorklogWaitingTA
			waitingLecturer += r.WorklogWaitingLecturer
		}
	}
	return
}

// seedPeriod gives the fixture's term a period covering the month it logs into,
// so ListForTA has a row to report against.
func (f *fixture) seedPeriod(t *testing.T, mm string) {
	t.Helper()
	f.exec(`INSERT INTO submission_periods (id, term_id, year_month, label, starts_on, due_date, is_closed)
	        SELECT gen_random_uuid(), $1, t.academic_year::text || '-' || $2,
	               'เดือนทดสอบ', CURRENT_DATE - 40, CURRENT_DATE + 10, FALSE
	        FROM academic_terms t WHERE t.id = $1
	        ON CONFLICT DO NOTHING`, f.TermID, mm)
}

func TestPeriodStatus_UnsentRowsAreTheTAsMoveNotTheLecturers(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.seedPeriod(t, day(10)[5:7])
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.mustUpsert(f.entry(day(11), "09:00", "11:00", 2))

	unapproved, ta, lect := waitingSplitFor(t, f)
	if unapproved != 2 {
		t.Fatalf("unapproved = %d, want 2", unapproved)
	}
	if ta != 2 || lect != 0 {
		t.Errorf("waiting TA=%d lecturer=%d, want 2/0 — a draft the TA never sent "+
			"is not something the lecturer is holding up", ta, lect)
	}
}

func TestPeriodStatus_SubmittedRowsAreTheLecturersMove(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.seedPeriod(t, day(10)[5:7])
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}

	_, ta, lect := waitingSplitFor(t, f)
	if ta != 0 || lect != 1 {
		t.Errorf("waiting TA=%d lecturer=%d, want 0/1 once sent", ta, lect)
	}
}

// A bounced row is back with the TA, not with the lecturer — they are the one
// who has to fix and resend it.
func TestPeriodStatus_RejectedRowsReturnToTheTA(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.seedPeriod(t, day(10)[5:7])
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	if err := f.Svc.Reject(f.ctx, f.LecturerID, f.AssignmentID, "ชั่วโมงไม่ตรง", "", false); err != nil {
		t.Fatal(err)
	}

	_, ta, lect := waitingSplitFor(t, f)
	if ta != 1 || lect != 0 {
		t.Errorf("waiting TA=%d lecturer=%d, want 1/0 — a rejected row is the TA's to fix", ta, lect)
	}
}

// Approved rows are nobody's move.
func TestPeriodStatus_ApprovedRowsWaitOnNobody(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.seedPeriod(t, day(10)[5:7])
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, "", false); err != nil {
		t.Fatal(err)
	}

	unapproved, ta, lect := waitingSplitFor(t, f)
	if unapproved != 0 || ta != 0 || lect != 0 {
		t.Errorf("after approval: unapproved=%d TA=%d lecturer=%d, want all zero",
			unapproved, ta, lect)
	}
}

var _ = uuid.Nil
