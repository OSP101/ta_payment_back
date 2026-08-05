package service

import (
	"testing"
)

// The "+ เพิ่มรายการ" controls used to be drawn on every month, including ones
// the lecturer had signed off — and Upsert refuses a new row the moment
// ANYTHING on the assignment has been submitted or approved. The button and the
// rule now come from one predicate, so the screen cannot offer what the server
// will refuse.

// monthClosed reports whether the screen would hide "+ เพิ่ม" for a month.
func (f *fixture) monthClosed(t *testing.T, ym string) bool {
	t.Helper()
	teaching := &TeachingService{pool: f.Pool}
	list, err := teaching.ListAssignmentsForTA(f.ctx, f.TAID, &f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list {
		if a.ID != f.AssignmentID {
			continue
		}
		for _, m := range a.MonthsInReview {
			if m == ym {
				return true
			}
		}
		return false
	}
	t.Fatal("assignment not listed")
	return false
}

func TestMonthsInReview_TrackWhatUpsertActuallyAccepts(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	thisMonth := day(10)[:7]
	next := monthStart().AddDate(0, 1, 0)
	nextMonth := next.Format("2006-01")
	nextDay := next.AddDate(0, 0, 9).Format("2006-01-02")

	if f.monthClosed(t, thisMonth) {
		t.Fatal("a fresh assignment must accept rows")
	}
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if f.monthClosed(t, thisMonth) {
		t.Error("drafts alone must not close a month to new rows")
	}

	// Submitting is the moment the server stops accepting — for THAT month.
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	if !f.monthClosed(t, thisMonth) {
		t.Error("month still open in the payload, but Upsert now refuses it")
	}
	if _, err := f.Svc.Upsert(f.ctx, f.TAID, f.entry(day(12), "09:00", "11:00", 2)); err == nil {
		t.Fatal("Upsert accepted a row it was expected to refuse — the screen and the " +
			"rule have drifted apart")
	}

	// The whole point of scoping it: a later month is untouched. Submitting June
	// used to close October too, so a TA who picked up an extra session mid-term
	// could never log it.
	if f.monthClosed(t, nextMonth) {
		t.Error("a later month must stay open")
	}
	if _, err := f.Svc.Upsert(f.ctx, f.TAID, f.entry(nextDay, "09:00", "11:00", 2)); err != nil {
		t.Fatalf("a later month must still accept rows: %v", err)
	}

	// ...and this month stays closed once approved, which is the case the screen
	// was getting wrong: a signed-off month still showing "+ เพิ่มในเดือนนี้".
	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, "", false); err != nil {
		t.Fatal(err)
	}
	if !f.monthClosed(t, thisMonth) {
		t.Error("a month the lecturer has approved must not offer to add rows")
	}
}
