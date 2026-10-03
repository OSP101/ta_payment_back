package service

import (
	"testing"

	"github.com/google/uuid"
)

// A submission naming several TAs is judged one TA at a time (03/10/2026,
// migration 0149): "ใครผ่านก็คนนั้นผ่านเลย ไม่ต้องรอคนอื่น". Each TA becomes
// their own request, tied to the others by batch_id.

// addClassFor gives any TA a class that does not touch the fixture section
// (Tuesday; the section meets on Monday), so they count as having a timetable.
func (rf *requestFixture) addClassFor(ta uuid.UUID) {
	rf.exec(`INSERT INTO ta_class_schedules
	           (id, user_id, term_id, course_code, day_of_week, start_time, end_time, is_wba)
	         VALUES (gen_random_uuid(), $1, $2, 'OWN102', 2, '09:00'::time, '12:00'::time, FALSE)`,
		ta, rf.TermID)
}

func (rf *requestFixture) twoTAInput(second uuid.UUID, secondWorkload WorkloadInput) CreateTARequestInput {
	in := rf.createInput()
	in.Assignments = append(in.Assignments, AssignmentInput{
		SectionIDs: []uuid.UUID{rf.SectionID},
		TAID:       second,
		Level:      "undergrad",
		Workload:   secondWorkload,
	})
	return in
}

func resultFor(t *testing.T, res *CreateResult, ta uuid.UUID) CreateResult {
	t.Helper()
	for _, r := range res.Requests {
		if r.TAID != nil && *r.TAID == ta {
			return r
		}
	}
	t.Fatalf("no result for TA %s in %+v", ta, res.Requests)
	return CreateResult{}
}

func TestCreate_OneRequestPerTA_WaitsOnlyForTheirOwnTimetable(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	second := rf.insertUser("ta", "ta2")
	rf.addClassFor(rf.TAID) // the first TA is ready; the second has no timetable

	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.twoTAInput(second, WorkloadInput{AttendanceHrs: 2, LabHrs: 2}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(res.Requests) != 2 {
		t.Fatalf("want one request per TA, got %d", len(res.Requests))
	}
	first, other := resultFor(t, res, rf.TAID), resultFor(t, res, second)
	if first.ID == other.ID {
		t.Fatal("both TAs landed on the same request")
	}
	if first.BatchID == nil || other.BatchID == nil || *first.BatchID != *other.BatchID {
		t.Errorf("the two requests should share one batch_id: %v vs %v", first.BatchID, other.BatchID)
	}
	if first.Status != "approved" {
		t.Errorf("TA with a timetable: status = %q, want approved without waiting for the other (%+v)", first.Status, first.Checks)
	}
	if other.Status != "submitted" {
		t.Errorf("TA without a timetable: status = %q, want submitted", other.Status)
	}
	var tas int
	if err := rf.Pool.QueryRow(rf.ctx,
		`SELECT COUNT(DISTINCT ta_id) FROM ta_request_assignments WHERE request_id = $1`, first.ID).Scan(&tas); err != nil {
		t.Fatal(err)
	}
	if tas != 1 {
		t.Errorf("request holds %d TAs, want 1", tas)
	}

	// The second TA files a timetable: only their own request is decided.
	rf.addClassFor(second)
	if err := rf.Req.ReevaluateForTA(rf.ctx, second, rf.TermID); err != nil {
		t.Fatalf("reevaluate: %v", err)
	}
	if st := rf.status(other.ID); st != "approved" {
		t.Errorf("second TA after filing a timetable: status = %q, want approved", st)
	}
	if st := rf.status(first.ID); st != "approved" {
		t.Errorf("first TA's request changed to %q", st)
	}
}

func TestCreate_OneTAFailing_DoesNotRejectTheOthers(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	second := rf.insertUser("ta", "ta2")
	rf.addClassFor(rf.TAID)
	rf.addClassFor(second)

	// No hours declared for the second TA fails the blocking workload rule.
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.twoTAInput(second, WorkloadInput{}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	first, other := resultFor(t, res, rf.TAID), resultFor(t, res, second)
	if first.Status != "approved" {
		t.Errorf("passing TA: status = %q, want approved (%+v)", first.Status, first.Checks)
	}
	if other.Status != "rejected" || other.RejectReason == "" {
		t.Errorf("failing TA: status = %q reason = %q, want rejected with a reason", other.Status, other.RejectReason)
	}
	// Each request's counts are its own TA only.
	var ug int
	if err := rf.Pool.QueryRow(rf.ctx,
		`SELECT COALESCE(SUM(undergrad_count),0) FROM ta_request_counts WHERE request_id = $1`, first.ID).Scan(&ug); err != nil {
		t.Fatal(err)
	}
	if ug != 1 {
		t.Errorf("counts on the passing TA's request = %d, want 1", ug)
	}
}
