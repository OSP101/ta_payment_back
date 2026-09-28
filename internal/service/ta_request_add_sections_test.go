package service

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A lecturer who asked for a TA on Sec 1 later wants them on Sec 2 as well.
// Cancel-and-resubmit is refused once the TA has logged hours, so the new
// section is appended to the existing request instead.

// approvedWithTA returns an approved request with the TA on the fixture's
// section, and the TA's timetable filed (on a day nothing meets).
func (rf *requestFixture) approvedWithTA() uuid.UUID {
	rf.t.Helper()
	rf.addTAClass(5, "09:00", "12:00") // Friday: clashes with nothing
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		rf.t.Fatalf("create: %v", err)
	}
	if res.Status != "approved" {
		rf.t.Fatalf("fixture request: status = %q, want approved", res.Status)
	}
	return res.ID
}

func (rf *requestFixture) addSecs(reqID uuid.UUID, secs ...SectionWorkload) (*CreateResult, error) {
	return rf.Req.AddSections(rf.ctx, rf.LecturerID, reqID, AddSectionsInput{TAID: rf.TAID, Sections: secs})
}

func ugSec(id uuid.UUID, attendance, lab float64) SectionWorkload {
	return SectionWorkload{SectionID: id, Workload: WorkloadInput{AttendanceHrs: attendance, LabHrs: lab}}
}

func (rf *requestFixture) countAssignments(reqID uuid.UUID) int {
	rf.t.Helper()
	var n int
	if err := rf.Pool.QueryRow(rf.ctx,
		`SELECT COUNT(*) FROM ta_request_assignments WHERE request_id = $1 AND state <> 'dropped'`, reqID).Scan(&n); err != nil {
		rf.t.Fatalf("count: %v", err)
	}
	return n
}

// The case that asked for it: the TA has already logged hours on Sec 1.
func TestAddSections_AppendsToTheApprovedRequestKeepingLoggedHours(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	reqID := rf.approvedWithTA()
	sec1 := rf.assignmentID(reqID)
	rf.exec(`INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity)
	         VALUES ($1, CURRENT_DATE, '09:00', '11:00', 2, 'attendance')`, sec1)

	sec2 := rf.addSection("02", 3) // Wednesday
	if _, err := rf.addSecs(reqID, ugSec(sec2, 2, 2)); err != nil {
		t.Fatalf("add sections: %v", err)
	}
	if n := rf.countAssignments(reqID); n != 2 {
		t.Fatalf("assignments on the request = %d, want 2", n)
	}
	if st := rf.status(reqID); st != "approved" {
		t.Errorf("status = %q, want approved", st)
	}
	var logs int
	if err := rf.Pool.QueryRow(rf.ctx,
		`SELECT COUNT(*) FROM work_logs WHERE assignment_id = $1`, sec1).Scan(&logs); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if logs != 1 {
		t.Errorf("Sec 1's logged hours must be untouched, got %d rows", logs)
	}
}

func TestAddSections_RefusesASectionAlreadyHeld(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	reqID := rf.approvedWithTA()
	if _, err := rf.addSecs(reqID, ugSec(rf.SectionID, 1, 0)); err == nil {
		t.Fatal("adding the section the TA already covers must be refused")
	}
}

func TestAddSections_RefusesATANotOnTheRequest(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	reqID := rf.approvedWithTA()
	sec2 := rf.addSection("02", 3)
	_, err := rf.Req.AddSections(rf.ctx, rf.LecturerID, reqID,
		AddSectionsInput{TAID: rf.LecturerID, Sections: []SectionWorkload{ugSec(sec2, 1, 0)}})
	if err == nil || !strings.Contains(err.Error(), "ไม่ได้อยู่ในคำขอนี้") {
		t.Fatalf("want a refusal for a TA who is not on the request, got %v", err)
	}
}

// A new section taught in the same sitting as an old one is one piece of work:
// both rows must land in one co-taught group so the hours are billed once.
func TestAddSections_RegroupsCotaughtSections(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	reqID := rf.approvedWithTA()
	sec2 := rf.addSection("02", 1) // same day and times as Sec 1
	if _, err := rf.addSecs(reqID, ugSec(sec2, 1, 0)); err != nil {
		t.Fatalf("add sections: %v", err)
	}
	var distinct, nulls int
	if err := rf.Pool.QueryRow(rf.ctx, `
		SELECT COUNT(DISTINCT cotaught_group), COUNT(*) FILTER (WHERE cotaught_group IS NULL)
		FROM ta_request_assignments WHERE request_id = $1`, reqID).Scan(&distinct, &nulls); err != nil {
		t.Fatalf("groups: %v", err)
	}
	if distinct != 1 || nulls != 0 {
		t.Errorf("want both rows in one co-taught group, got %d groups and %d NULL", distinct, nulls)
	}
}

// The new section is judged by the same clash rule as a fresh request. Every
// session of it sits on the TA's class and only in-class work was declared, so
// it is dropped — and the request stays approved on Sec 1.
func TestAddSections_JudgesTheNewSectionAgainstTheTimetable(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	reqID := rf.approvedWithTA()
	sec2 := rf.addSection("02", 2)     // Tuesday
	rf.addTAClass(2, "08:00", "17:00") // covers all of Sec 2
	res, err := rf.addSecs(reqID, ugSec(sec2, 2, 2))
	if err != nil {
		t.Fatalf("add sections: %v", err)
	}
	var state string
	if err := rf.Pool.QueryRow(rf.ctx, `
		SELECT state::text FROM ta_request_assignments WHERE request_id = $1 AND section_id = $2`,
		reqID, sec2).Scan(&state); err != nil {
		t.Fatalf("state: %v", err)
	}
	if state != "dropped" {
		t.Errorf("Sec 2 state = %q, want dropped", state)
	}
	if st := rf.status(reqID); st != "approved" {
		t.Errorf("status = %q, want approved — Sec 1 is still workable", st)
	}
	if len(res.Checks) == 0 {
		t.Error("the lecturer must be told the new section was dropped")
	}
}
