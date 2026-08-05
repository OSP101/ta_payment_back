package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// Tests for the deferred-decision model (phase 1 of the 24/07/2026 plan).
//
// The behaviours pinned here are the ones the lecturers asked for by name:
//   - a request can be submitted before the TA has a timetable;
//   - the TA's own class always wins, and a clashing session is dropped whole;
//   - a TA who cannot cover any session releases that course's quota;
//   - putting a name on a request books the quota immediately.

// requestFixture extends the worklog fixture with the pieces a TA request
// needs: a TARequestService, and helpers to add timetable rows.
type requestFixture struct {
	*fixture
	Req *TARequestService
}

func newRequestFixture(t *testing.T, opts fixtureOpts) *requestFixture {
	// These tests create the request themselves; the fixture's own pre-wired
	// request would collide with it under the duplicate rule.
	opts.NoRequest = true
	// The whole point of this file is what happens BEFORE and AFTER the TA files
	// a timetable — resting the request, trimming clashing sessions, sweeping once
	// it arrives. Each test therefore starts with none and files its own.
	opts.NoOwnClassSchedule = true
	f := newFixture(t, opts)
	return &requestFixture{
		fixture: f,
		Req: &TARequestService{
			pool:   f.Pool,
			aud:    audit.New(f.Pool),
			budget: &BudgetService{pool: f.Pool},
			notify: f.Svc.notify,
		},
	}
}

// addTAClass gives the fixture's TA a class in their own timetable.
func (rf *requestFixture) addTAClass(dayOfWeek int, start, end string) {
	rf.exec(`INSERT INTO ta_class_schedules
	           (id, user_id, term_id, course_code, day_of_week, start_time, end_time, is_wba)
	         VALUES (gen_random_uuid(), $1, $2, 'OWN101', $3, $4::time, $5::time, FALSE)`,
		rf.TAID, rf.TermID, dayOfWeek, start, end)
}

// allSessionsLab turns the fixture section's lecture period into a second lab.
//
// It existed because between 31/07/2026 and 05/08/2026 only lab periods blocked,
// so "every session clashes" was otherwise unreachable. Lectures block again, so
// a whole-day class now clashes with both sessions on its own — the helper
// survives for the tests that want a lab-only section, not for that reason.
func (rf *requestFixture) allSessionsLab() {
	rf.exec(`UPDATE section_schedules SET kind = 'lab' WHERE section_id = $1`, rf.SectionID)
}

// createInput builds the payload for one TA covering the fixture's section.
func (rf *requestFixture) createInput() CreateTARequestInput {
	in := CreateTARequestInput{
		TeachingCourseID: rf.CourseID,
		ReimburseScope:   "both",
	}
	in.Assignments = append(in.Assignments, AssignmentInput{
		SectionIDs: []uuid.UUID{rf.SectionID},
		TAID:       rf.TAID,
		Level:      "undergrad",
		// The fixture course is 3(3-3-…), so 2h of each stays inside the
		// per-section contact-hour ceiling.
		Workload: WorkloadInput{AttendanceHrs: 2, LabHrs: 2},
	})
	return in
}

func (rf *requestFixture) assignmentState(reqID uuid.UUID) (state string, reason *string) {
	rf.t.Helper()
	if err := rf.Pool.QueryRow(rf.ctx, `
		SELECT state::text, state_reason FROM ta_request_assignments
		WHERE request_id = $1 LIMIT 1`, reqID).Scan(&state, &reason); err != nil {
		rf.t.Fatalf("read assignment state: %v", err)
	}
	return state, reason
}

func (rf *requestFixture) status(reqID uuid.UUID) string {
	rf.t.Helper()
	st, err := rf.Req.requestStatus(rf.ctx, reqID)
	if err != nil {
		rf.t.Fatalf("read status: %v", err)
	}
	return st
}

// ---------------------------------------------------------------------------
// Case 1 — lecturer submits before the TA has a timetable
// ---------------------------------------------------------------------------

func TestCreate_RestsWhenTAHasNoSchedule(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})

	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("submitting before the TA files a timetable must be allowed: %v", err)
	}
	if res.Status != "submitted" {
		t.Fatalf("status = %q, want \"submitted\" (waiting for the timetable)", res.Status)
	}
	// The lecturer must be told why nothing was decided.
	if len(res.Checks) == 0 || !strings.Contains(res.Checks[0].Message, "รอตารางเรียน") {
		t.Errorf("the result should explain that it is waiting for a timetable, got: %+v", res.Checks)
	}
}

// The fixture's section meets Monday 09:00–12:00 (lecture) and 13:00–16:00
// (lab). A TA class covering only the afternoon costs them the lab and leaves
// the lecture session workable.
func TestReevaluate_TrimsOnlyTheClashingSession(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	rf.addTAClass(1, "13:00", "16:00") // clashes with the lab only
	if err := rf.Req.ReevaluateForTA(rf.ctx, rf.TAID, rf.TermID); err != nil {
		t.Fatalf("reevaluate: %v", err)
	}

	state, reason := rf.assignmentState(res.ID)
	if state != "trimmed" {
		t.Fatalf("state = %q, want \"trimmed\" — one of two sessions still works", state)
	}
	if reason == nil || !strings.Contains(*reason, "1 จาก 2") {
		t.Errorf("reason should say 1 of 2 sessions clash, got: %v", reason)
	}
	if st := rf.status(res.ID); st != "approved" {
		t.Errorf("request status = %q, want approved — the TA can still cover the lecture", st)
	}
}

// The DEFERRED path must reach the same verdict as the submit path — that is the
// whole point of routing both through applyClashOutcome. A timetable landing on
// the lecture costs the TA that lecture and nothing else.
//
// Until 05/08/2026 it cost them nothing, because lecture periods were exempt.
// See lecture_clash_exemption_test.go for both sides of that decision.
func TestReevaluate_LectureOnlyClashTrimsTheLecture(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	rf.addTAClass(1, "09:00", "12:00") // the lecture session, and only that
	if err := rf.Req.ReevaluateForTA(rf.ctx, rf.TAID, rf.TermID); err != nil {
		t.Fatalf("reevaluate: %v", err)
	}

	state, reason := rf.assignmentState(res.ID)
	if state != "trimmed" {
		t.Fatalf("state = %q, want \"trimmed\" — the lecture is lost, the lab is not (reason: %v)",
			state, reason)
	}
	if reason == nil || !strings.Contains(*reason, "1 จาก 2") {
		t.Errorf("reason should quantify what was lost, got: %v", reason)
	}
}

func TestReevaluate_DropsSectionWhenEverySessionClashes(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Covers both Monday sessions, and both are blocking.
	rf.allSessionsLab()
	rf.addTAClass(1, "08:00", "17:00")
	if err := rf.Req.ReevaluateForTA(rf.ctx, rf.TAID, rf.TermID); err != nil {
		t.Fatalf("reevaluate: %v", err)
	}

	state, reason := rf.assignmentState(res.ID)
	if state != "dropped" {
		t.Fatalf("state = %q, want \"dropped\"", state)
	}
	if reason == nil || !strings.Contains(*reason, "ทุกคาบ") {
		t.Errorf("reason should say every session clashes, got: %v", reason)
	}
	// Nobody is left who can teach, so the request cannot be approved.
	if st := rf.status(res.ID); st != "rejected" {
		t.Errorf("request status = %q, want rejected", st)
	}
}

// A timetable that does not overlap at all leaves everything intact.
func TestReevaluate_ApprovesWhenNoClash(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	rf.addTAClass(3, "09:00", "12:00") // Wednesday — the section meets Monday
	if err := rf.Req.ReevaluateForTA(rf.ctx, rf.TAID, rf.TermID); err != nil {
		t.Fatalf("reevaluate: %v", err)
	}

	if state, _ := rf.assignmentState(res.ID); state != "active" {
		t.Errorf("state = %q, want \"active\"", state)
	}
	if st := rf.status(res.ID); st != "approved" {
		t.Errorf("request status = %q, want approved", st)
	}
}

// WBA rows are a year-4 sentinel spanning no real teaching time and are
// excluded from conflict maths by rule C5.
func TestReevaluate_IgnoresWBARows(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	rf.exec(`INSERT INTO ta_class_schedules
	           (id, user_id, term_id, day_of_week, start_time, end_time, is_wba)
	         VALUES (gen_random_uuid(), $1, $2, 1, '00:00'::time, '00:00'::time, TRUE)`,
		rf.TAID, rf.TermID)
	if err := rf.Req.ReevaluateForTA(rf.ctx, rf.TAID, rf.TermID); err != nil {
		t.Fatalf("reevaluate: %v", err)
	}

	if state, _ := rf.assignmentState(res.ID); state != "active" {
		t.Errorf("a WBA row must not trim anything, state = %q", state)
	}
}

// ---------------------------------------------------------------------------
// Case 2 — the timetable already exists when the lecturer submits
// ---------------------------------------------------------------------------

// Every session clashes, so nothing is salvageable. The request is still
// WRITTEN — as 'rejected', with the assignment dropped — rather than refused
// with an error. That is what the deferred path does, and a rejected row does
// not block a re-submission (the duplicate check counts only submitted and
// approved), so the lecturer loses nothing by it and gains a record of why.
func TestCreate_RejectsWhenEverySessionClashes(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	// A class covering the whole day collides with both the lecture and the lab.
	// The declared workload is attendance + lab teaching — both performed inside
	// the session — so nothing survives and dropping is right.
	rf.addTAClass(1, "08:00", "17:00") // timetable first

	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Status != "rejected" {
		t.Errorf("status = %q, want rejected — nobody on this request can teach", res.Status)
	}
	if state, _ := rf.assignmentState(res.ID); state != "dropped" {
		t.Errorf("assignment state = %q, want dropped", state)
	}
	if !strings.Contains(res.RejectReason, "ติดตารางเรียน") {
		t.Errorf("reject reason should say why, got: %q", res.RejectReason)
	}
}

// The counterpart to the drop test above: the same total collision, but the
// lecturer asked for grading — work done outside the session. The assignment
// must SURVIVE. Dropping it was the old behaviour and it threw away the only
// work the TA could actually have done.
func TestCreate_KeepsFullyClashingSectionWhenOffSlotWorkDeclared(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	rf.addTAClass(1, "08:00", "17:00") // covers every session

	in := rf.inputFor([]SectionWorkload{{
		SectionID: rf.SectionID,
		Workload:  WorkloadInput{CheckWorkHrs: 2}, // grading only — never in the room
	}}, "undergrad")

	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	state, reason := rf.assignmentState(res.ID)
	if state == "dropped" {
		t.Fatalf("the assignment must survive — grading happens outside the session (reason: %v)", reason)
	}
	if reason == nil || !strings.Contains(*reason, "นอกคาบ") {
		t.Errorf("the reason should tell the TA what they can still do, got: %v", reason)
	}
}

// The 3ก change. A lab clash costs the TA that lab, not the section: they keep
// the lecture duty and the grading, which is the arrangement the college
// actually uses (จิรายุ assists SC362004 sec 2-3 doing grading only, while one
// of its labs sits on a class of his).
func TestCreate_TrimsPartialClashInsteadOfRefusing(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	rf.addTAClass(1, "13:00", "16:00") // clashes with the lab session only

	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("a partial clash must not refuse the submission: %v", err)
	}
	if res.Status != "approved" {
		t.Errorf("status = %q, want approved — the lecture session is still workable", res.Status)
	}
	state, reason := rf.assignmentState(res.ID)
	if state != "trimmed" {
		t.Fatalf("assignment state = %q, want trimmed", state)
	}
	if reason == nil || !strings.Contains(*reason, "1 จาก 2") {
		t.Errorf("reason should quantify what was lost, got: %v", reason)
	}

	// The lecturer must SEE the trim at submit time, or they plan around hours
	// that will never be worked.
	var warned bool
	for _, c := range res.Checks {
		if c.Rule == "clash_trimmed" && c.Warning && strings.Contains(c.Message, "1 จาก 2") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no clash_trimmed warning returned to the lecturer; checks = %+v", res.Checks)
	}
}

// THE POINT OF 3ก. The same TA with the same timetable must get the same verdict
// whether they filed it before or after the lecturer submitted. Until now the
// order decided it: file first and a clashing lab cost the whole section; file
// afterwards and it cost only that lab.
func TestClashVerdict_IsTheSameWhicheverOrderTheTimetableArrives(t *testing.T) {
	// Timetable first, then submit.
	before := newRequestFixture(t, fixtureOpts{})
	before.addTAClass(1, "13:00", "16:00")
	resBefore, err := before.Req.Create(before.ctx, before.LecturerID, before.createInput())
	if err != nil {
		t.Fatalf("create (timetable first): %v", err)
	}
	stateBefore, _ := before.assignmentState(resBefore.ID)

	// Submit first, then timetable.
	after := newRequestFixture(t, fixtureOpts{})
	resAfter, err := after.Req.Create(after.ctx, after.LecturerID, after.createInput())
	if err != nil {
		t.Fatalf("create (submit first): %v", err)
	}
	after.addTAClass(1, "13:00", "16:00")
	if err := after.Req.ReevaluateForTA(after.ctx, after.TAID, after.TermID); err != nil {
		t.Fatalf("reevaluate: %v", err)
	}
	stateAfter, _ := after.assignmentState(resAfter.ID)

	if stateBefore != stateAfter {
		t.Errorf("state depends on when the timetable was filed: before=%q after=%q — "+
			"one timetable, one rule, one answer", stateBefore, stateAfter)
	}
	if st := after.status(resAfter.ID); st != resBefore.Status {
		t.Errorf("request status depends on filing order: before=%q after=%q",
			resBefore.Status, st)
	}
}

// A lecture sitting on the TA's own class is a real collision again.
//
// It was exempted on 31/07/2026 — taking attendance was held not to require the
// TA in the room for the hour — and un-exempted on 05/08/2026 by the college:
// a TA whose class covers every lecture of a group does not get that group's
// เช็คชื่อ duty. clashBlockingKind is the single place that decides it, so this
// path and checkOwnClassConflict move together; the earlier bug was that they
// did not, and the exemption was dead in production while a test asserted it.
//
// The submission is still WRITTEN rather than refused. Refusing here would make
// the answer depend on whether the timetable arrived before or after the
// lecturer pressed Send — see the order-independence test above.
func TestCreate_TrimsLectureOnlyClash(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	rf.addTAClass(1, "09:00", "12:00") // exactly the lecture session

	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("a clash must never refuse the submission outright: %v", err)
	}
	state, reason := rf.assignmentState(res.ID)
	if state != "trimmed" {
		t.Fatalf("assignment state = %q, want trimmed — the lecture is lost, the lab is not (reason: %v)", state, reason)
	}
	if reason == nil || !strings.Contains(*reason, "1 จาก 2") {
		t.Errorf("reason should quantify what was lost, got: %v", reason)
	}
}

func TestCreate_AllowsWhenScheduleExistsWithoutClash(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	rf.addTAClass(3, "09:00", "12:00") // Wednesday, no clash

	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("a non-clashing timetable must not block submission: %v", err)
	}
	// Everyone has a timetable, so the verdict is immediate.
	if res.Status != "approved" {
		t.Fatalf("status = %q, want approved (nothing left to wait for)", res.Status)
	}
}

// ---------------------------------------------------------------------------
// Quota — reserved at submission, released when a TA is dropped
// ---------------------------------------------------------------------------

func TestQuota_ReservedWhileRequestIsPending(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	if _, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput()); err != nil {
		t.Fatalf("create: %v", err)
	}

	// From the point of view of a DIFFERENT course, this TA is already booked
	// for one course even though nothing has been approved.
	other := uuid.New()
	n, err := rf.Req.reservedCourseCount(rf.ctx, rf.Pool, rf.TAID, rf.TermID, other)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("reserved courses = %d, want 1 — submitting books the slot", n)
	}
}

func TestQuota_ReleasedWhenAssignmentDropped(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	if _, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput()); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Every session must genuinely clash for the assignment to be dropped, and
	// since 31/07/2026 a lecture period no longer counts — a TA whose class sits
	// on it can still take attendance, so they keep the section and the quota
	// with it. Make both sessions labs so "cannot teach at all" is true.
	rf.allSessionsLab()
	rf.addTAClass(1, "08:00", "17:00")
	if err := rf.Req.ReevaluateForTA(rf.ctx, rf.TAID, rf.TermID); err != nil {
		t.Fatalf("reevaluate: %v", err)
	}

	other := uuid.New()
	n, err := rf.Req.reservedCourseCount(rf.ctx, rf.Pool, rf.TAID, rf.TermID, other)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("reserved courses = %d, want 0 — a TA who cannot teach the course frees the quota", n)
	}
}

func TestCandidates_ReportsPendingReservation(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	if _, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput()); err != nil {
		t.Fatalf("create: %v", err)
	}

	// A second course in the same term asks who is available.
	secondCourse := uuid.New()
	rf.exec(`INSERT INTO teaching_courses
	           (id, term_id, code, name_th, level, credits, num_students, starts_on, ends_on)
	         VALUES ($1, $2, 'CP999999', 'อีกวิชา', 'undergrad', 3, 40,
	                 (SELECT starts_on FROM teaching_courses WHERE id=$3),
	                 (SELECT ends_on   FROM teaching_courses WHERE id=$3))`,
		secondCourse, rf.TermID, rf.CourseID)

	cands, err := rf.Req.Candidates(rf.ctx, secondCourse)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	var found bool
	for _, c := range cands {
		if c.ID != rf.TAID {
			continue
		}
		found = true
		if c.ApprovedCourseCount != 1 {
			t.Errorf("count = %d, want 1 — a pending request already books the slot", c.ApprovedCourseCount)
		}
		if c.AtQuota {
			t.Errorf("1 of 3 courses must not read as at-quota")
		}
	}
	if !found {
		t.Fatal("the TA should appear among the candidates")
	}
}

// ---------------------------------------------------------------------------
// Sweep — the safety net behind the save-time trigger
// ---------------------------------------------------------------------------

func TestSweep_LeavesRequestsStillWaiting(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	n, err := rf.Req.SweepPendingRequests(rf.ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Errorf("sweep decided %d requests, want 0 — the TA still has no timetable", n)
	}
	if st := rf.status(res.ID); st != "submitted" {
		t.Errorf("status = %q, want it to keep waiting", st)
	}
}

func TestSweep_FinalisesOnceTimetableExists(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Write the timetable directly, bypassing the save-time trigger — this is
	// exactly the "trigger was missed" scenario the sweep exists for.
	rf.addTAClass(3, "09:00", "12:00")

	n, err := rf.Req.SweepPendingRequests(rf.ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("sweep decided %d requests, want 1", n)
	}
	if st := rf.status(res.ID); st != "approved" {
		t.Errorf("status = %q, want approved", st)
	}
}

// Re-running the sweep must not re-decide or double-notify.
func TestSweep_IsIdempotent(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	if _, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput()); err != nil {
		t.Fatalf("create: %v", err)
	}
	rf.addTAClass(3, "09:00", "12:00")

	if _, err := rf.Req.SweepPendingRequests(rf.ctx); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	n, err := rf.Req.SweepPendingRequests(rf.ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if n != 0 {
		t.Errorf("second sweep decided %d requests, want 0", n)
	}
}

// The message a trimmed TA reads has to name WHICH session they lost and to
// WHAT. A bare count ("1 จาก 2 คาบ") is the same sentence whether they lost the
// lecture hour or the lab hour, and those leave them able to do different jobs:
// losing a lecture slot costs the attendance-taking for that hour, losing a lab
// slot means they cannot run the lab at all.
//
// clashLines is pure, so the wording is pinned here without a database.
func TestClashLines_NamesSessionTypeAndOwnCourse(t *testing.T) {
	lines := clashLines([]clashDetail{
		{SessionKind: "lab", Day: 2, Start: "13:00", End: "15:00",
			OwnCode: "322201", OwnName: "Data Structures", OwnKind: "lecture"},
	})
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %v", len(lines), lines)
	}
	got := lines[0]
	for _, want := range []string{
		"ปฏิบัติการ",  // the session type they lost
		"อังคาร",      // when
		"13:00–15:00", // which hour
		"322201",      // their own course
		"Data Structures",
		"(บรรยาย)", // and what kind of class of theirs takes the slot
	} {
		if !strings.Contains(got, want) {
			t.Errorf("line missing %q\ngot: %s", want, got)
		}
	}
}

// Blank kind / unnamed class must still read as Thai. Both fallbacks used to
// stutter: an unknown session kind produced "คาบคาบสอน", and an unnamed class
// produced "ตรงกับ วิชาที่คุณเรียน ที่คุณเรียน".
func TestClashLines_FallsBackWhenFieldsBlank(t *testing.T) {
	got := clashLines([]clashDetail{
		{SessionKind: "", Day: 1, Start: "09:00", End: "11:00"},
	})[0]
	for _, bad := range []string{"คาบคาบ", "ตรงกับ  ", "ที่คุณเรียน ที่คุณเรียน"} {
		if strings.Contains(got, bad) {
			t.Errorf("fallback stutters (%q): %q", bad, got)
		}
	}
	for _, want := range []string{"คาบสอน", "จันทร์", "09:00–11:00", "ตรงกับคาบเรียนของคุณ"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

// End-to-end for the trimmed message: the SQL that finds the collisions, the
// wording, and the closing line about what is still possible.
//
// The fixture's section runs a lecture 09:00–12:00 and a lab 13:00–16:00 on
// Monday. The TA's own class covers only the lab hours, so the message must
// say the LAB is the part they lost and that the lecture is still theirs —
// the distinction the old count-only sentence could not express.
func TestApplyClashOutcome_ReasonNamesLostAndRemainingKinds(t *testing.T) {
	// newFixture already gives the section a Monday lecture 09:00–12:00 and a
	// Monday lab 13:00–16:00. The TA's own class covers only the lab hours.
	f := newFixture(t, fixtureOpts{RequestStatus: "submitted"})
	f.addOwnClass("322201 Data Structures", 1, "13:00", "16:00")

	tx, err := f.Pool.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(f.ctx)
	svc := &TARequestService{pool: f.Pool}
	if _, err := svc.applyClashOutcome(f.ctx, tx, f.RequestID); err != nil {
		t.Fatalf("applyClashOutcome: %v", err)
	}

	var state, reason string
	if err := tx.QueryRow(f.ctx,
		`SELECT state::text, COALESCE(state_reason,'') FROM ta_request_assignments WHERE id = $1`,
		f.AssignmentID).Scan(&state, &reason); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if state != "trimmed" {
		t.Fatalf("state = %q, want trimmed (only one of two sessions clashes)", state)
	}
	for _, want := range []string{
		"1 จาก 2 คาบ",            // how much
		"คาบปฏิบัติการ",          // WHICH session type was lost
		"13:00–16:00",            // when
		"322201 Data Structures", // the TA's own class that took the slot
		"ยังลงเวลาในคาบบรรยายได้", // and what they can still do
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("state_reason missing %q\ngot:\n%s", want, reason)
		}
	}
	// The lost kind must not also be advertised as still available.
	if strings.Contains(reason, "ยังลงเวลาในคาบปฏิบัติการได้") {
		t.Errorf("reason offers the very session that was trimmed:\n%s", reason)
	}
}

// Migration 0048 backfills the same sentence in SQL, because a migration cannot
// call Go. Two implementations of one format is exactly the kind of duplication
// that drifts silently — a tweak to clashLines() would leave already-decided
// rows reading differently from new ones, in the same list, with nothing to
// flag it.
//
// So: decide a request with the Go path, wipe the text, re-derive it with the
// migration's SQL, and require the two to be byte-identical.
func TestMigration0048_MatchesGoWording(t *testing.T) {
	f := newFixture(t, fixtureOpts{RequestStatus: "submitted"})
	f.addOwnClass("322201 Data Structures", 1, "13:00", "16:00")

	// 1. Go path.
	tx, err := f.Pool.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	svc := &TARequestService{pool: f.Pool}
	if _, err := svc.applyClashOutcome(f.ctx, tx, f.RequestID); err != nil {
		tx.Rollback(f.ctx)
		t.Fatalf("applyClashOutcome: %v", err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var fromGo string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT state_reason FROM ta_request_assignments WHERE id = $1`,
		f.AssignmentID).Scan(&fromGo); err != nil {
		t.Fatalf("read Go reason: %v", err)
	}

	// 2. Blank it, then let the migration rebuild it.
	f.exec(`UPDATE ta_request_assignments SET state_reason = 'PLACEHOLDER' WHERE id = $1`,
		f.AssignmentID)
	sqlBytes, err := os.ReadFile(
		filepath.Join(repoRoot(t), "migrations", "0048_rewrite_clash_reasons.up.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := f.Pool.Exec(f.ctx, string(sqlBytes)); err != nil {
		t.Fatalf("run migration 0048: %v", err)
	}
	var fromSQL string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT state_reason FROM ta_request_assignments WHERE id = $1`,
		f.AssignmentID).Scan(&fromSQL); err != nil {
		t.Fatalf("read SQL reason: %v", err)
	}

	if fromSQL == "PLACEHOLDER" {
		t.Fatal("migration did not touch the row it was written to backfill")
	}
	if fromGo != fromSQL {
		t.Errorf("migration wording drifted from the Go wording\nGo:\n%s\n\nSQL:\n%s", fromGo, fromSQL)
	}
}
