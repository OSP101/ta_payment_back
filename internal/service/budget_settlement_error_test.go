package service

import (
	"testing"

	"github.com/google/uuid"
)

// PAY-01: budget.Compute failing (DB hiccup, or here simply a course that does
// not exist) must not be read as "no cap configured". Before the fix,
// settleAs swallowed the error and left capRegular/capSpecial at their zero
// value, which settleTrack treats as "unlimited" — every month came back
// Paid=true, OverBudget=false, silently over-paying the course.
func TestSettleAs_ReturnsErrorWhenBudgetComputeFails(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := exportSvcFor(f)

	bogusCourseID := uuid.New() // never inserted into teaching_courses
	got, err := svc.settleAs(f.ctx, bogusCourseID, mergedSittingsCTE, SettleChronological)
	if err == nil {
		t.Fatalf("settleAs with an unreadable budget must return an error, got settlement=%+v", got)
	}
	if got != nil {
		t.Fatalf("settleAs must not return a partial CourseSettlement alongside the error, got %+v", got)
	}
}
