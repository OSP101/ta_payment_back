package service

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// Tests for two lecturer-facing gaps reported 10/08/2026:
//
//  1. the same TA could be submitted for the same course more than once —
//     autoDecide's own "duplicate" rule already caught this and auto-rejected
//     the request, but only AFTER writing rows and running the whole decision
//     pipeline, which from the lecturer's side read as "the system let me send
//     it" rather than a refusal;
//  2. there was no way to withdraw a request already sent, for a typo or a
//     TA the lecturer wants to swap out.

func taReqSvcFor(f *fixture) *TARequestService {
	return &TARequestService{pool: f.Pool, aud: audit.New(f.Pool)}
}

func (f *fixture) countRequests() int {
	f.t.Helper()
	var n int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM ta_requests WHERE teaching_course_id = $1 AND lecturer_id = $2`,
		f.CourseID, f.LecturerID).Scan(&n); err != nil {
		f.t.Fatalf("count requests: %v", err)
	}
	return n
}

func inputForFixtureTA(f *fixture) CreateTARequestInput {
	return CreateTARequestInput{
		TeachingCourseID: f.CourseID,
		ReimburseScope:   "both",
		Assignments: []AssignmentInput{{
			SectionIDs: []uuid.UUID{f.SectionID},
			TAID:       f.TAID,
			Level:      "undergrad",
			Workload:   WorkloadInput{AttendanceHrs: 2, LabHrs: 2},
		}},
	}
}

/* -------------------------------------------------------------------------- */
/* The hard pre-check                                                        */
/* -------------------------------------------------------------------------- */

// THE bug report: the fixture's default request is already 'approved' for
// (TAID, CourseID) — submitting the same pair again must be refused
// immediately, with NOTHING written, not silently accepted and rejected later.
func TestCreate_RefusesDuplicateActiveRequestImmediately(t *testing.T) {
	f := newFixture(t, fixtureOpts{}) // RequestStatus defaults to "approved"
	svc := taReqSvcFor(f)
	before := f.countRequests()
	if before != 1 {
		t.Fatalf("fixture bug: expected the pre-wired request, got %d", before)
	}

	_, err := svc.Create(f.ctx, f.LecturerID, inputForFixtureTA(f))
	if err == nil {
		t.Fatal("Create must refuse a TA who already has an active request on this course")
	}
	if !strings.Contains(err.Error(), "มีคำขอ TA ของวิชานี้ที่ยังดำเนินการอยู่แล้ว") {
		t.Errorf("error should name the reason, got: %v", err)
	}
	// The whole point: refused BEFORE anything is written, not created-then-
	// rejected. A second row (even a rejected one) means the guard ran too
	// late.
	if after := f.countRequests(); after != before {
		t.Errorf("request count changed from %d to %d — the duplicate attempt wrote a row instead of being refused up front", before, after)
	}
}

// A 'submitted' (still-deferred, not yet decided) request is just as active
// as an 'approved' one for this purpose — the TA is booked either way.
func TestCreate_RefusesDuplicateAgainstSubmittedRequest(t *testing.T) {
	f := newFixture(t, fixtureOpts{RequestStatus: "submitted"})
	svc := taReqSvcFor(f)

	_, err := svc.Create(f.ctx, f.LecturerID, inputForFixtureTA(f))
	if err == nil {
		t.Fatal("Create must refuse against a submitted (not yet decided) duplicate too")
	}
}

// The counterpart, pinning the comment in fixture_test.go: a REJECTED (or
// cancelled) prior request must NOT block a fresh, legitimate resubmission —
// otherwise a lecturer who fixed a rejection-causing mistake could never try
// again.
func TestCreate_AllowsResubmissionAfterRejectedOrCancelled(t *testing.T) {
	for _, status := range []string{"rejected", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{RequestStatus: status})
			svc := taReqSvcFor(f)

			res, err := svc.Create(f.ctx, f.LecturerID, inputForFixtureTA(f))
			if err != nil {
				t.Fatalf("Create should succeed after a %s prior request: %v", status, err)
			}
			if res.Status != "approved" {
				t.Errorf("status = %q, want approved (everything else about this fixture is clean)", res.Status)
			}
		})
	}
}

/* -------------------------------------------------------------------------- */
/* Cancel                                                                     */
/* -------------------------------------------------------------------------- */

func requestStatusAndReason(f *fixture, id uuid.UUID) (status string, reason *string) {
	f.t.Helper()
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT status::text, reject_reason FROM ta_requests WHERE id = $1`, id).Scan(&status, &reason); err != nil {
		f.t.Fatalf("read request: %v", err)
	}
	return status, reason
}

func TestCancel_ApprovedRequestWithNoWorkLogsSucceeds(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := taReqSvcFor(f)

	if err := svc.Cancel(f.ctx, f.LecturerID, f.RequestID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	status, reason := requestStatusAndReason(f, f.RequestID)
	if status != "cancelled" {
		t.Errorf("status = %q, want cancelled", status)
	}
	if reason == nil || *reason == "" {
		t.Error("reject_reason should record why (shown to the lecturer as the row's note)")
	}
}

func TestCancel_SubmittedRequestSucceeds(t *testing.T) {
	f := newFixture(t, fixtureOpts{RequestStatus: "submitted"})
	svc := taReqSvcFor(f)

	if err := svc.Cancel(f.ctx, f.LecturerID, f.RequestID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if status, _ := requestStatusAndReason(f, f.RequestID); status != "cancelled" {
		t.Errorf("status = %q, want cancelled", status)
	}
}

// Cancelling frees the quota it was holding — the whole reason "submitting
// books the slot" (reservedCourseCount) works is that every consumer reads
// status IN ('submitted','approved'); flipping to 'cancelled' must be enough
// on its own, with no other table needing a matching update.
func TestCancel_FreesTheReservedQuota(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := taReqSvcFor(f)

	// A second course this TA could be requested for, to observe the quota
	// count against. insertCourse reads NumStudents/TermStart/TermEnd off the
	// opts struct directly — it never defaults them itself (newFixture does
	// that once, up front), so a bare literal here would leave TermStart/
	// TermEnd empty and fail the date columns.
	otherOpts := fixtureOpts{Level: "undergrad", Track: "regular"}
	applyFixtureDefaults(&otherOpts)
	otherCourse := f.insertCourse(otherOpts)

	before, err := svc.reservedCourseCount(f.ctx, f.Pool, f.TAID, f.TermID, otherCourse)
	if err != nil {
		t.Fatal(err)
	}
	if before != 1 {
		t.Fatalf("fixture bug: expected the approved request to reserve 1 course, got %d", before)
	}

	if err := svc.Cancel(f.ctx, f.LecturerID, f.RequestID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	after, err := svc.reservedCourseCount(f.ctx, f.Pool, f.TAID, f.TermID, otherCourse)
	if err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Errorf("reserved count after cancel = %d, want 0 — the quota must be released", after)
	}
}

func TestCancel_RefusesWhenNotTheOwningLecturer(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := taReqSvcFor(f)
	otherLecturer := f.insertUser("lecturer", "other-lecturer")

	err := svc.Cancel(f.ctx, otherLecturer, f.RequestID)
	if err == nil {
		t.Fatal("Cancel must refuse a caller who does not own the request")
	}
	if status, _ := requestStatusAndReason(f, f.RequestID); status != "approved" {
		t.Errorf("status changed to %q on a refused cancel — must stay untouched", status)
	}
}

func TestCancel_RefusesAlreadyDecidedTerminalStatuses(t *testing.T) {
	for _, status := range []string{"rejected", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{RequestStatus: status})
			svc := taReqSvcFor(f)
			if err := svc.Cancel(f.ctx, f.LecturerID, f.RequestID); err == nil {
				t.Errorf("Cancel must refuse a request already %s", status)
			}
		})
	}
}

// The safety gate: once a TA has logged real hours, the request can no longer
// be pulled out from under them via self-service — those work_logs would be
// silently orphaned from every query that reads them through
// r.status = 'approved'.
func TestCancel_RefusesWhenWorkLogsExist(t *testing.T) {
	f := newFixture(t, fixtureOpts{}) // NoOwnClassSchedule defaults to false → TA has a timetable
	svc := taReqSvcFor(f)
	f.mustUpsert(f.entry(day(1), "09:00", "10:00", 1))

	err := svc.Cancel(f.ctx, f.LecturerID, f.RequestID)
	if err == nil {
		t.Fatal("Cancel must refuse once the TA has logged hours against the request")
	}
	if !strings.Contains(err.Error(), "บันทึกเวลา") {
		t.Errorf("error should explain why, got: %v", err)
	}
	if status, _ := requestStatusAndReason(f, f.RequestID); status != "approved" {
		t.Errorf("status changed to %q on a refused cancel — must stay untouched", status)
	}
}

func TestCancel_UnknownRequestIsAnError(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := taReqSvcFor(f)
	if err := svc.Cancel(f.ctx, f.LecturerID, uuid.New()); err == nil {
		t.Error("Cancel on a nonexistent id must error")
	}
}
