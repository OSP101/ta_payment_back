package service

import (
	"testing"

	"github.com/google/uuid"
)

// legacyFromBatch folds a fresh two-TA submission back into the shape it had
// before 0149: one request, both TAs on it, no batch_id, with the given verdict.
func (rf *requestFixture) legacyFromBatch(res *CreateResult, status string) uuid.UUID {
	keep, other := res.Requests[0].ID, res.Requests[1].ID
	rf.exec(`UPDATE ta_request_assignments SET request_id = $1 WHERE request_id = $2`, keep, other)
	rf.exec(`DELETE FROM ta_request_counts WHERE request_id = $1`, other)
	rf.exec(`DELETE FROM ta_requests WHERE id = $1`, other)
	rf.exec(`UPDATE ta_requests SET batch_id = NULL, status = $2::ta_request_status,
	           reject_reason = CASE WHEN $2 = 'rejected' THEN 'ta2 Test ยังไม่ได้ระบุภาระงาน' END,
	           decided_at = CASE WHEN $2 = 'rejected' THEN NOW() END, decided_by = NULL
	         WHERE id = $1`, keep, status)
	return keep
}

func (rf *requestFixture) requestOf(ta uuid.UUID) (id uuid.UUID, status string) {
	rf.t.Helper()
	if err := rf.Pool.QueryRow(rf.ctx, `
		SELECT r.id, r.status::text FROM ta_requests r
		JOIN ta_request_assignments a ON a.request_id = r.id
		WHERE a.ta_id = $1 AND r.teaching_course_id = $2 LIMIT 1`, ta, rf.CourseID).Scan(&id, &status); err != nil {
		rf.t.Fatalf("request of TA: %v", err)
	}
	return id, status
}

// The case reported on 03/10/2026: an old request where one TA passed every
// check stayed rejected because of the other, so the passing TA never saw the
// course. The sweep splits it and approves the one who passes.
func TestSplitLegacy_ApprovesTheTAWhoPasses(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	second := rf.insertUser("ta", "ta2")
	rf.addClassFor(rf.TAID)
	rf.addClassFor(second)
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.twoTAInput(second, WorkloadInput{}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	legacy := rf.legacyFromBatch(res, "rejected")

	if _, err := rf.Req.SweepPendingRequests(rf.ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	firstReq, firstSt := rf.requestOf(rf.TAID)
	secondReq, secondSt := rf.requestOf(second)
	if firstReq == secondReq {
		t.Fatal("the legacy request was not split")
	}
	if firstSt != "approved" {
		t.Errorf("passing TA: status = %q, want approved", firstSt)
	}
	if secondSt != "rejected" {
		t.Errorf("failing TA: status = %q, want still rejected", secondSt)
	}
	var batched int
	if err := rf.Pool.QueryRow(rf.ctx,
		`SELECT COUNT(*) FROM ta_requests WHERE batch_id = $1`, legacy).Scan(&batched); err != nil {
		t.Fatal(err)
	}
	if batched != 2 {
		t.Errorf("requests tied to the original = %d, want 2", batched)
	}

	// Idempotent: a second sweep changes nothing.
	if _, err := rf.Req.SweepPendingRequests(rf.ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	var total int
	if err := rf.Pool.QueryRow(rf.ctx,
		`SELECT COUNT(*) FROM ta_requests WHERE teaching_course_id = $1`, rf.CourseID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("requests on the course after a second sweep = %d, want 2", total)
	}
}

// An old request waiting on one TA's timetable: the TA who has one is decided
// now instead of waiting.
func TestSplitLegacy_StopsWaitingOnTheOtherTA(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	second := rf.insertUser("ta", "ta2")
	rf.addClassFor(rf.TAID) // second has no timetable
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.twoTAInput(second, WorkloadInput{AttendanceHrs: 2, LabHrs: 2}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Approved-then-folded would not be legacy-shaped; rest both as before.
	rf.exec(`UPDATE ta_requests SET status = 'submitted', decided_at = NULL WHERE id = ANY($1)`,
		[]uuid.UUID{res.Requests[0].ID, res.Requests[1].ID})
	rf.legacyFromBatch(res, "submitted")

	if _, err := rf.Req.SweepPendingRequests(rf.ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, st := rf.requestOf(rf.TAID); st != "approved" {
		t.Errorf("TA with a timetable: status = %q, want approved", st)
	}
	if _, st := rf.requestOf(second); st != "submitted" {
		t.Errorf("TA without a timetable: status = %q, want still submitted", st)
	}
}

// A request a lecturer cancelled is their decision, not the system's verdict.
func TestSplitLegacy_LeavesCancelledAlone(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	second := rf.insertUser("ta", "ta2")
	rf.addClassFor(rf.TAID)
	rf.addClassFor(second)
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.twoTAInput(second, WorkloadInput{AttendanceHrs: 2, LabHrs: 2}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	legacy := rf.legacyFromBatch(res, "rejected")
	rf.exec(`UPDATE ta_requests SET status = 'cancelled', decided_by = $2 WHERE id = $1`, legacy, rf.LecturerID)

	if n, err := rf.Req.SplitLegacyRequests(rf.ctx); err != nil || n != 0 {
		t.Fatalf("split = %d, %v; want 0 for a cancelled request", n, err)
	}
}
