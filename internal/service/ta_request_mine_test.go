package service

import "testing"

// A TA named on a request must see it before it is approved, and see that the
// thing holding it up is their own timetable (TA feedback 05/10/2026).
func TestListPendingForTA_ShowsWaitingRequestAndWhy(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	second := rf.insertUser("ta", "ta2")
	rf.addClassFor(second) // the second TA is ready; the fixture TA is not

	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.twoTAInput(second, WorkloadInput{AttendanceHrs: 2, LabHrs: 2}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	mine := resultFor(t, res, rf.TAID)
	if mine.Status != "submitted" {
		t.Fatalf("fixture TA without a timetable: status = %q, want submitted", mine.Status)
	}

	got, err := rf.Req.ListPendingForTA(rf.ctx, rf.TAID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != mine.ID {
		t.Fatalf("want exactly the TA's own waiting request %s, got %+v", mine.ID, got)
	}
	p := got[0]
	if !p.WaitingOnMe || p.WaitingOnOthers != 0 {
		t.Errorf("waiting_on_me=%v waiting_on_others=%d, want true/0", p.WaitingOnMe, p.WaitingOnOthers)
	}
	if len(p.Sections) == 0 || p.Code == "" || p.LecturerName == "" {
		t.Errorf("course details missing: %+v", p)
	}

	// The second TA was approved on the spot: nothing pending for them.
	if other, err := rf.Req.ListPendingForTA(rf.ctx, second); err != nil || len(other) != 0 {
		t.Errorf("approved TA should have no pending requests, got %+v (err %v)", other, err)
	}

	// Once the timetable is in, the request is decided and leaves the list.
	rf.addClassFor(rf.TAID)
	if err := rf.Req.ReevaluateForTA(rf.ctx, rf.TAID, rf.TermID); err != nil {
		t.Fatal(err)
	}
	if after, err := rf.Req.ListPendingForTA(rf.ctx, rf.TAID); err != nil || len(after) != 0 {
		t.Errorf("decided request still listed: %+v (err %v)", after, err)
	}
}
