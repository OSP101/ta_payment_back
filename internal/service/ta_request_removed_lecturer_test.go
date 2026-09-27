package service

import (
	"strings"
	"testing"
)

// A lecturer taken off a course could still cancel the approved request they
// had filed there, dropping the TAs of a course no longer theirs (found in the
// UAT follow-up security review).
func TestCancel_RefusedOnceTheFilerNoLongerTeachesTheCourse(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.exec(`UPDATE ta_requests SET lecturer_id = $2 WHERE id = $1`, f.RequestID, f.LecturerID)
	f.exec(`DELETE FROM teaching_lecturers WHERE teaching_course_id = $1 AND lecturer_id = $2`, f.CourseID, f.LecturerID)

	svc := &TARequestService{pool: f.Pool}
	err := svc.Cancel(f.ctx, f.LecturerID, f.RequestID)
	if err == nil || !strings.Contains(err.Error(), "ไม่ได้เป็นอาจารย์ผู้สอนรายวิชานี้แล้ว") {
		t.Fatalf("cancel by a removed lecturer: err = %v, want refusal", err)
	}
}
