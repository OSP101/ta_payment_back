package service

import (
	"testing"

	"github.com/google/uuid"
)

// Live bug (11/08/2026, CP423434): a TA who once held BOTH a grad-special and
// a grad-regular assignment on the same course — the special one rejected and
// re-requested, leaving a dead assignment behind — got stuck forever at
// "ยังมี N รายการที่อาจารย์ยังไม่อนุมัติ" on their real, fully-approved regular
// hours. monthWorklogReadiness counted work_logs by (ta_id, course_id) alone,
// with no section/track filter, so the grad-special assignment's leftover
// 'submitted' rows (impossible to ever move — those TAs stopped logging
// entirely) were being added to the SAME TA's real-assignment readiness count.
func TestMarkStaffReviewed_NotBlockedByDeadGradSpecialSiblingAssignment(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "phd", Track: "regular"})
	pid := f.addSubmissionPeriod(currentMonthMM(), "2026-12-31", "", false)
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, "", false); err != nil {
		t.Fatal(err)
	}

	// A second, DEAD assignment for the same TA on the same course: a
	// grad-special (special-track) section whose work_logs were left
	// 'submitted' from before grad-special stopped logging entirely. This
	// mirrors the live data: two assignments for one TA on one course, one
	// real and approved, one dead and stuck.
	deadSection := uuid.New()
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track)
	        VALUES ($1, $2, '99', 'special')`, deadSection, f.CourseID)
	deadAssign := uuid.New()
	f.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
	        VALUES ($1, $2, $3, $4, 'phd')`, deadAssign, f.RequestID, deadSection, f.TAID)
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES (gen_random_uuid(), $1, $2::date, '09:00', '11:00', 2, 'review', 'submitted')`,
		deadAssign, day(10))

	if err := f.Periods.MarkStaffReviewed(f.ctx, f.StaffID, pid, f.TAID, f.CourseID, ""); err != nil {
		t.Fatalf("MarkStaffReviewed must not be blocked by a dead grad-special "+
			"sibling assignment's leftover 'submitted' row: %v", err)
	}
}
