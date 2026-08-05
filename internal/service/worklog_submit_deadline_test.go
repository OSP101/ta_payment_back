package service

import (
	"testing"
)

// A TA who misses a deadline used to be left pressing a live "ส่งอนุมัติ 10
// รายการ" button: the screen counted every draft, the server skipped the ones
// in closed months, and the two never agreed. These tests pin the agreement —
// the count the button shows and the rows Submit moves come from one predicate
// (unsubmittableMonthSQL), so a button offering to send something the server
// will refuse cannot be built again.

// closeMonth marks the term's period for a work_date's month as closed.
func (f *fixture) closeMonth(t *testing.T, mm string) {
	t.Helper()
	f.exec(`INSERT INTO submission_periods
	            (id, term_id, year_month, label, starts_on, due_date, is_closed)
	        SELECT gen_random_uuid(), $1, t.academic_year::text || '-' || $2,
	               'เดือนทดสอบ', CURRENT_DATE - 60, CURRENT_DATE - 30, TRUE
	        FROM academic_terms t WHERE t.id = $1
	        ON CONFLICT DO NOTHING`, f.TermID, mm)
}

// submittableCountOf reads the number the TA screen puts on the button.
func (f *fixture) submittableCountOf(t *testing.T) (unsent, submittable int) {
	t.Helper()
	teaching := &TeachingService{pool: f.Pool}
	list, err := teaching.ListAssignmentsForTA(f.ctx, f.TAID, &f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list {
		if a.ID == f.AssignmentID {
			return a.UnsentCount, a.SubmittableCount
		}
	}
	t.Fatal("assignment not found in the TA's list")
	return 0, 0
}

func TestSubmit_CountsAndActionAgreeWhenAMonthIsClosed(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	// Two drafts in the month the fixture logs into.
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.mustUpsert(f.entry(day(11), "09:00", "11:00", 2))

	unsent, submittable := f.submittableCountOf(t)
	if unsent != 2 || submittable != 2 {
		t.Fatalf("open month: unsent=%d submittable=%d, want 2/2", unsent, submittable)
	}

	f.closeMonth(t, day(10)[5:7])

	unsent, submittable = f.submittableCountOf(t)
	if unsent != 2 {
		t.Errorf("unsent = %d, want 2 — the rows still exist and the TA must see them", unsent)
	}
	if submittable != 0 {
		t.Errorf("submittable = %d, want 0 — the button must not offer to send rows "+
			"the server will skip", submittable)
	}

	// ...and the server agrees: nothing to send, said in words the TA can act on.
	err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID)
	if err == nil {
		t.Fatal("submitting a wholly-closed batch must be refused")
	}
	if got := f.worklogStatusOf(t, f.AssignmentID); got != "draft" {
		t.Errorf("rows became %q — a closed month must stay untouched", got)
	}
}

// The mixed case is the one that must not regress into all-or-nothing: a stale
// draft in a closed month cannot be allowed to wedge the months still open.
func TestSubmit_SendsTheOpenMonthAndLeavesTheClosedOne(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2)) // month gets closed below
	// A row in the FOLLOWING month, which stays open.
	next := monthStart().AddDate(0, 1, 9).Format("2006-01-02")
	f.mustUpsert(f.entry(next, "09:00", "11:00", 2))

	f.closeMonth(t, day(10)[5:7])

	unsent, submittable := f.submittableCountOf(t)
	if unsent != 2 || submittable != 1 {
		t.Fatalf("unsent=%d submittable=%d, want 2 unsent of which 1 sendable", unsent, submittable)
	}

	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("the open month must still go: %v", err)
	}

	var submitted, drafts int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FILTER (WHERE status='submitted'), COUNT(*) FILTER (WHERE status='draft')
		 FROM work_logs WHERE assignment_id=$1`, f.AssignmentID).Scan(&submitted, &drafts); err != nil {
		t.Fatal(err)
	}
	if submitted != 1 || drafts != 1 {
		t.Errorf("after submit: submitted=%d draft=%d, want 1/1 — the closed month's row "+
			"must stay behind without blocking the open one", submitted, drafts)
	}

	// The button now offers nothing, and still names what is stuck.
	unsent, submittable = f.submittableCountOf(t)
	if unsent != 1 || submittable != 0 {
		t.Errorf("after submit: unsent=%d submittable=%d, want 1/0", unsent, submittable)
	}
}
