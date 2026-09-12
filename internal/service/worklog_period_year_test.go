package service

import (
	"testing"

	"github.com/google/uuid"
)

// QUAL-03: submission_periods.year_month joins used to match work_logs by
// to_char(work_date,'MM') alone, throwing away the year — safe only because
// every query happened to be scoped to one teaching_course_id, and GetTimeline
// specifically never asserted that the submission_period (periodID) and the
// teaching_course (tcID) it was called with actually belong to the SAME
// term. Two terms whose October periods share the digits "10" but differ in
// academic_year would have their work_logs cross-counted.
//
// This pins GetTimeline (submission_period.go), one of ~15 sites sharing the
// bug: a course's own October work must not be attributed to a DIFFERENT
// term's October period just because the month digits match.
func TestGetTimeline_DoesNotCrossCountAnotherTermsSameMonth(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	// f.AcademicYear (2569) is a semester-1 term, so October sits in calendar
	// 2026 — see period_lock.go's year_month mapping comment. This work_log
	// genuinely belongs to f.CourseID's own term.
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES (gen_random_uuid(), $1, '2026-10-15', '09:00', '11:00', 2, 'สอนปฏิบัติการ', 'approved')`,
		f.AssignmentID)

	// A second, unrelated term one academic year later, with its own October
	// period — same MM ("10"), different academic_year ("2570" vs "2569").
	otherTerm := uuid.New()
	f.exec(`INSERT INTO academic_terms (id, academic_year, semester, is_active) VALUES ($1, 2570, 1, FALSE)`,
		otherTerm)
	otherPeriod := uuid.New()
	f.exec(`INSERT INTO submission_periods (id, term_id, year_month, due_date, label, starts_on)
	        VALUES ($1, $2, '2570-10', '2027-10-05', 'รอบ 2570-10', '2027-10-01')`,
		otherPeriod, otherTerm)

	// Calling GetTimeline with THIS OTHER term's period but f.CourseID's own
	// (term 2569) course/TA must find none of f.CourseID's October work —
	// that work belongs to the 2569-10 period, not 2570-10.
	timeline, err := f.Periods.GetTimeline(f.ctx, f.LecturerID, otherPeriod, f.TAID, f.CourseID)
	if err != nil {
		t.Fatalf("GetTimeline: %v", err)
	}
	if timeline == nil {
		t.Fatal("GetTimeline returned nil")
	}
	if timeline.WorklogTotal != 0 {
		t.Errorf("WorklogTotal = %d, want 0 — the course's October 2569 work must not "+
			"be counted against a different term's October 2570 period just because "+
			"the month digits match", timeline.WorklogTotal)
	}
}
