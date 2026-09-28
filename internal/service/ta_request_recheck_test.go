package service

import (
	"testing"

	"github.com/google/uuid"
)

// The timetable page autosaves after every edit. A request waiting on the TA's
// timetable is decided on the FIRST save — often a single class — and the
// classes added a moment later used to be ignored, because only 'submitted'
// requests were re-evaluated. The real case: the lecture clash was saved first
// (1 of 2 sessions, approved), the lab clash second, and the request stayed
// approved with "ยังลงเวลาในคาบปฏิบัติการได้" although nothing was workable.

func (rf *requestFixture) assignmentID(reqID uuid.UUID) uuid.UUID {
	rf.t.Helper()
	var id uuid.UUID
	if err := rf.Pool.QueryRow(rf.ctx,
		`SELECT id FROM ta_request_assignments WHERE request_id = $1 LIMIT 1`, reqID).Scan(&id); err != nil {
		rf.t.Fatalf("assignment lookup: %v", err)
	}
	return id
}

// saveTimetable stands in for one autosave: add a class, then run the hook the
// real save runs.
func (rf *requestFixture) saveTimetable(day int, start, end string) {
	rf.t.Helper()
	rf.addTAClass(day, start, end)
	if err := rf.Req.ReevaluateForTA(rf.ctx, rf.TAID, rf.TermID); err != nil {
		rf.t.Fatalf("reevaluate: %v", err)
	}
}

func TestTimetableSavedInStepsReachesTheSameVerdict(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput()) // เช็คชื่อ + สอนปฏิบัติการ
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	rf.saveTimetable(1, "09:00", "12:00") // first autosave: the lecture only
	if st := rf.status(res.ID); st != "approved" {
		t.Fatalf("after the first save: status = %q, want approved (the lab is still free)", st)
	}
	if state, _ := rf.assignmentState(res.ID); state != "trimmed" {
		t.Fatalf("after the first save: state = %q, want trimmed", state)
	}

	rf.saveTimetable(1, "13:00", "16:00") // second autosave: the lab too
	state, reason := rf.assignmentState(res.ID)
	if state != "dropped" {
		t.Fatalf("every session now clashes and nothing off-slot was declared: state = %q (reason %v)", state, reason)
	}
	if st := rf.status(res.ID); st != "rejected" {
		t.Errorf("nobody on the request can work: status = %q, want rejected", st)
	}
}

// Autosave fires on every edit; a save that changes nothing must not rewrite
// the verdict (or re-notify).
func TestRecheckLeavesAnUnchangedVerdictAlone(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rf.saveTimetable(1, "09:00", "12:00")

	var first string
	asg := rf.assignmentID(res.ID)
	if err := rf.Pool.QueryRow(rf.ctx,
		`SELECT state_decided_at::text FROM ta_request_assignments WHERE id = $1`, asg).Scan(&first); err != nil {
		t.Fatalf("read: %v", err)
	}
	rf.saveTimetable(3, "09:00", "12:00") // Wednesday: touches nothing in this section
	var second string
	if err := rf.Pool.QueryRow(rf.ctx,
		`SELECT state_decided_at::text FROM ta_request_assignments WHERE id = $1`, asg).Scan(&second); err != nil {
		t.Fatalf("read: %v", err)
	}
	if first != second {
		t.Errorf("an unrelated save rewrote the verdict: %s -> %s", first, second)
	}
}

// Logged hours hang off the assignment and exports skip dropped ones, so a
// recheck must not drop an assignment the TA has already logged against.
func TestRecheckKeepsAnAssignmentWithLoggedHours(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rf.saveTimetable(1, "09:00", "12:00")
	rf.exec(`INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity)
	         VALUES ($1, CURRENT_DATE, '13:00', '15:00', 2, 'lab')`, rf.assignmentID(res.ID))

	rf.saveTimetable(1, "13:00", "16:00")
	if state, _ := rf.assignmentState(res.ID); state == "dropped" {
		t.Fatal("an assignment with logged hours must not be dropped by a recheck")
	}
	if st := rf.status(res.ID); st != "approved" {
		t.Errorf("status = %q, want approved — the assignment is still on the request", st)
	}
}
