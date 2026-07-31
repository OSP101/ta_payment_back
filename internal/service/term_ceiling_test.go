package service

import (
	"context"
	"math"
	"testing"
)

// The term-hour ceiling had two independent faults that together put every TA on
// a real course over their limit before they typed anything:
//
//  1. the ceiling was `weekly × months × 4`, i.e. every month is exactly four
//     weeks. Term 2569/1 runs 22 Jun – 18 Oct — 119 days, exactly 17 weeks — so
//     the ceiling came out at 96 hours for a timetable that produces 98; and
//  2. Generate never checked the ceiling at all, so nothing noticed.
//
// Fixing only (1) leaves the generator unguarded for the next schedule that
// overshoots; fixing only (2) makes the generator cut real hours to fit a wrong
// number. These tests hold both.

// termFixtureWeeks sets the fixture course's term to an exact number of weeks so
// the arithmetic under test is unambiguous.
func termFixtureWeeks(t *testing.T, weeks int) *fixture {
	t.Helper()
	f := newFixture(t, fixtureOpts{})
	// Inclusive day count: a 17-week term spans 119 days including both ends.
	f.exec(`UPDATE academic_terms SET starts_on = $1::date, ends_on = $1::date + ($2)::int
	         WHERE id = $3`,
		monthStart().Format("2006-01-02"), weeks*7-1, f.TermID)
	f.exec(`UPDATE teaching_courses SET starts_on = NULL, ends_on = NULL WHERE id = $1`, f.CourseID)
	return f
}

// The calendar, not months × 4.
func TestWeeksInTerm_CountsRealCalendarWeeks(t *testing.T) {
	f := termFixtureWeeks(t, 17)
	ctx := context.Background()

	got := WeeksInTerm(ctx, f.Pool, f.CourseID)
	if math.Abs(got-17.0) > 0.01 {
		t.Errorf("WeeksInTerm = %.2f, want 17 — the term is 119 days; months × 4 would say 16 "+
			"and shave a week off every TA's ceiling", got)
	}
}

// The number shown to the TA and the number the server refuses at must be the
// same number. They were two separate copies of the formula.
func TestTermCeiling_DisplayedMatchesEnforced(t *testing.T) {
	f := termFixtureWeeks(t, 17)
	ctx := context.Background()

	tsvc := &TeachingService{pool: f.Pool}
	list, err := tsvc.ListAssignmentsForTA(ctx, f.TAID, &f.CourseID)
	if err != nil {
		t.Fatalf("ListAssignmentsForTA: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("no assignment returned")
	}
	shown := list[0].TermHourCeiling

	enforced := f.Svc.weeksInTermForCourse(ctx, f.CourseID)
	// The fixture declares 20h of each of four categories; the enforced ceiling is
	// weekly total × weeks, and the displayed one must be computed the same way.
	if math.Abs(shown/enforced-list[0].WeeklyCapLecture-list[0].WeeklyCapLab-
		list[0].WeeklyCapReview-list[0].WeeklyCapOther) > 0.01 {
		t.Errorf("displayed ceiling %.2f is not weeklyTotal × %.2f weeks — "+
			"the TA is shown a limit the server does not enforce", shown, enforced)
	}
}

// THE SAFETY NET. Generate must stop at the ceiling instead of walking the
// calendar past it.
func TestGenerate_StopsAtTermCeiling(t *testing.T) {
	f := termFixtureWeeks(t, 17)
	ctx := context.Background()

	// A deliberately tiny allowance: 1 hour of LAB a week and nothing else, so
	// the ceiling (17h) bites long before the timetable runs out. Lab, not
	// lecture, on purpose: a lab session is worked whole (the fixture's is 3h),
	// so generation overshoots the weekly declaration and the ceiling has
	// something to stop. Lecture duty can no longer overshoot at all — it is
	// narrowed to the declared attendance window (see the เช็คชื่อ rule in
	// Generate), which would make this test vacuous.
	f.exec(`UPDATE ta_workload_forms
	           SET attendance_hrs = 0, lab_hrs = 1, check_work_hrs = 0, ug_other_hrs = 0
	         WHERE assignment_id = $1`, f.AssignmentID)

	res, err := f.Svc.Generate(ctx, f.TAID, f.AssignmentID)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	var total float64
	if err := f.Pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(hours),0) FROM work_logs WHERE assignment_id=$1`,
		f.AssignmentID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	ceiling := 1.0 * 17.0
	if total > ceiling+0.01 {
		t.Errorf("generated %.1f hours against a %.1f-hour ceiling — the generator "+
			"walked past the limit the TA is paid against", total, ceiling)
	}
	if !res.StoppedAtTermCeiling {
		t.Error("stopped_at_term_ceiling is false, so the TA is given fewer rows than " +
			"their timetable with no reason shown")
	}
	if math.Abs(res.TermHourCeiling-ceiling) > 0.01 {
		t.Errorf("reported ceiling = %.1f, want %.1f", res.TermHourCeiling, ceiling)
	}
}

// ...and a normal allowance must NOT be cut. This is the half that matters to the
// TA: the fix is supposed to raise the ceiling to the real calendar, not trim
// hours to fit the old approximation.
func TestGenerate_FullTimetableFitsUnderTheRealCeiling(t *testing.T) {
	f := termFixtureWeeks(t, 17)
	ctx := context.Background()

	// The fixture's section teaches 3h lecture + 3h lab weekly; declare exactly
	// that, which is what a lecturer filling the form honestly would write.
	f.exec(`UPDATE ta_workload_forms
	           SET attendance_hrs = 3, lab_hrs = 3, check_work_hrs = 0, ug_other_hrs = 0
	         WHERE assignment_id = $1`, f.AssignmentID)

	res, err := f.Svc.Generate(ctx, f.TAID, f.AssignmentID)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.StoppedAtTermCeiling {
		t.Errorf("generation hit the ceiling on a timetable the lecturer declared in full — "+
			"under months × 4 this is exactly the case that lost a week (ceiling %.1f)",
			res.TermHourCeiling)
	}
}
