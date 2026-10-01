package service

import "testing"

// A TA who types a class by mistake after approval gets their section trimmed
// or dropped; deleting that class must put it back. It used to stay cut for
// good: the recheck skipped dropped rows, never restored trimmed ones, and the
// in-class hours it had zeroed were lost — a dead end that needed staff.

func (rf *requestFixture) removeTAClasses(start string) {
	rf.exec(`DELETE FROM ta_class_schedules WHERE user_id = $1 AND start_time = $2::time`, rf.TAID, start)
}

// labOnlyInput is createInput for a section allSessionsLab turned into two
// labs: a lecture duty would be refused there.
func (rf *requestFixture) labOnlyInput() CreateTARequestInput {
	in := rf.createInput()
	in.Assignments[0].Workload = WorkloadInput{LabHrs: 2}
	return in
}

func (rf *requestFixture) attendanceHrs(reqID interface{}) float64 {
	var h float64
	if err := rf.Pool.QueryRow(rf.ctx, `
		SELECT COALESCE(w.attendance_hrs, 0) FROM ta_workload_forms w
		JOIN ta_request_assignments a ON a.id = w.assignment_id
		WHERE a.request_id = $1`, reqID).Scan(&h); err != nil {
		rf.t.Fatalf("attendance: %v", err)
	}
	return h
}

func TestClashRestore_TrimmedComesBackWithItsHours(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	rf.addTAClass(3, "09:00", "12:00") // Wednesday: no clash, request approves
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if st := rf.status(res.ID); st != "approved" {
		t.Fatalf("status = %q, want approved", st)
	}
	before := rf.attendanceHrs(res.ID)

	rf.addTAClass(1, "09:00", "12:00") // the mistake: on the Monday lecture
	if err := rf.Req.ReevaluateForTA(rf.ctx, rf.TAID, rf.TermID); err != nil {
		t.Fatal(err)
	}
	if state, _ := rf.assignmentState(res.ID); state != "trimmed" {
		t.Fatalf("after the clash: state = %q, want trimmed", state)
	}

	rf.removeTAClasses("09:00")
	rf.addTAClass(3, "09:00", "12:00") // the legitimate class stays
	if err := rf.Req.ReevaluateForTA(rf.ctx, rf.TAID, rf.TermID); err != nil {
		t.Fatal(err)
	}
	if state, reason := rf.assignmentState(res.ID); state != "active" {
		t.Fatalf("after fixing the timetable: state = %q (%v), want active", state, reason)
	}
	if after := rf.attendanceHrs(res.ID); after != before {
		t.Errorf("attendance hours = %v, want the declared %v back", after, before)
	}
}

func TestClashRestore_DroppedAndRejectedRequestComesBack(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	rf.allSessionsLab()
	rf.addTAClass(3, "09:00", "12:00")
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.labOnlyInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if st := rf.status(res.ID); st != "approved" {
		t.Fatalf("status = %q, want approved", st)
	}

	rf.addTAClass(1, "08:00", "17:00") // covers every Monday session
	if err := rf.Req.ReevaluateForTA(rf.ctx, rf.TAID, rf.TermID); err != nil {
		t.Fatal(err)
	}
	if state, _ := rf.assignmentState(res.ID); state != "dropped" {
		t.Fatalf("after the clash: state = %q, want dropped", state)
	}
	if st := rf.status(res.ID); st != "rejected" {
		t.Fatalf("after the clash: request = %q, want rejected (nobody left)", st)
	}

	rf.removeTAClasses("08:00")
	if err := rf.Req.ReevaluateForTA(rf.ctx, rf.TAID, rf.TermID); err != nil {
		t.Fatal(err)
	}
	if state, reason := rf.assignmentState(res.ID); state != "active" {
		t.Fatalf("after fixing the timetable: state = %q (%v), want active", state, reason)
	}
	if st := rf.status(res.ID); st != "approved" {
		t.Errorf("request = %q, want approved again", st)
	}
	// Good news must not arrive under the "ทับซ้อน" headline.
	var restored int
	if err := rf.Pool.QueryRow(rf.ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id = $1 AND title LIKE 'คืนสิทธิ์ผู้ช่วยสอน%' AND channel = 'in_app'`,
		rf.TAID).Scan(&restored); err != nil {
		t.Fatal(err)
	}
	if restored == 0 {
		t.Error("the TA got no คืนสิทธิ์ผู้ช่วยสอน notice")
	}
}

func TestClashRestore_DroppedStaysWhenTheCapIsFull(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	rf.allSessionsLab()
	rf.addTAClass(3, "09:00", "12:00")
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.labOnlyInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rf.addTAClass(1, "08:00", "17:00")
	if err := rf.Req.ReevaluateForTA(rf.ctx, rf.TAID, rf.TermID); err != nil {
		t.Fatal(err)
	}
	// While dropped, the TA took another course and the cap is 1.
	rf.secondCourseAssignment(fixtureOpts{})
	rf.setCourseCap(1)

	rf.removeTAClasses("08:00")
	if err := rf.Req.ReevaluateForTA(rf.ctx, rf.TAID, rf.TermID); err != nil {
		t.Fatal(err)
	}
	if state, _ := rf.assignmentState(res.ID); state != "dropped" {
		t.Errorf("state = %q, want dropped — restoring would exceed the course cap", state)
	}
}
