package service

import (
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A staff correction rewrites hours a lecturer already approved, so it is gated
// four ways: the officer's password, a reason long enough to be a sentence, rows
// that actually belong to this TA on this course, and a record that survives the
// people involved.
//
// These tests pin each gate. They matter more than most: the endpoint's whole
// purpose is to be the accountable path, and a gate that quietly stops working
// leaves an editor that looks accountable and is not.

const goodReason = "ตารางสอนเปลี่ยนกลางเทอม ต้องแก้เวลาให้ตรง"

// jsonb renders with a space after the colon and in no fixed key order, so
// assertions squash whitespace rather than matching a literal rendering.
var wsRe = regexp.MustCompile(`\s+`)

func squash(s string) string { return wsRe.ReplaceAllString(s, "") }

func batchFixture(t *testing.T) (*fixture, uuid.UUID) {
	t.Helper()
	f := newFixture(t, fixtureOpts{})
	id := f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	return f, id
}

func (f *fixture) batchInput(logID uuid.UUID, reason, password string) EditBatchInput {
	w := f.entry(day(10), "09:00", "10:00", 1)
	w.ID = logID
	return EditBatchInput{
		TeachingCourseID: f.CourseID,
		TAID:             f.TAID,
		YearMonth:        day(10)[:7],
		Reason:           reason,
		Password:         password,
		Changes:          []EditChange{{WorkLogID: logID, Action: "update", After: w}},
	}
}

func TestEditBatch_RefusesWithoutPassword(t *testing.T) {
	f, id := batchFixture(t)

	if _, err := f.Svc.ApplyStaffEditBatch(f.ctx, f.StaffID, f.batchInput(id, goodReason, "")); err == nil {
		t.Fatal("an edit with no password must be refused — a stolen session must not " +
			"be able to rewrite approved hours")
	}
	if _, err := f.Svc.ApplyStaffEditBatch(f.ctx, f.StaffID, f.batchInput(id, goodReason, "wrong-password")); err == nil {
		t.Fatal("a wrong password must be refused")
	}

	// ...and nothing may have changed on the way to being refused.
	var hours float64
	if err := f.Pool.QueryRow(f.ctx, `SELECT hours FROM work_logs WHERE id=$1`, id).Scan(&hours); err != nil {
		t.Fatal(err)
	}
	if hours != 2 {
		t.Errorf("hours = %.1f, want 2 — a refused batch must not have applied anything", hours)
	}
}

func TestEditBatch_RefusesEmptyOrShortReason(t *testing.T) {
	f, id := batchFixture(t)

	for _, reason := range []string{"", "แก้", "ผิด"} {
		if _, err := f.Svc.ApplyStaffEditBatch(f.ctx, f.StaffID,
			f.batchInput(id, reason, fixturePassword)); err == nil {
			t.Errorf("reason %q was accepted — it is sent verbatim to the lecturer and the "+
				"TA, and a one-word reason explains nothing to either", reason)
		}
		// Refused BEFORE anything was written, not after. The table also has a
		// CHECK on reason length, so dropping the Go check still produces an
		// error — but by then the hours have been rewritten and the batch that
		// would have explained them fails to insert, leaving a silent edit.
		var hours float64
		if err := f.Pool.QueryRow(f.ctx, `SELECT hours FROM work_logs WHERE id=$1`, id).Scan(&hours); err != nil {
			t.Fatal(err)
		}
		if hours != 2 {
			t.Fatalf("reason %q: hours = %.1f, want 2 — the rows were changed and only then "+
				"was the batch refused, so the edit happened with nothing recording why", reason, hours)
		}
	}
}

// The path carries a course and a TA; the row ids do not have to agree with
// them unless something checks. Without this the endpoint edits any row in the
// database that the caller can name.
func TestEditBatch_RefusesRowsFromAnotherCourse(t *testing.T) {
	f, _ := batchFixture(t)
	other := f.secondCourseAssignment(fixtureOpts{})
	foreign := f.entry(day(11), "09:00", "11:00", 2)
	foreign.AssignmentID = other
	foreignID := f.mustUpsert(foreign)

	// The change itself is perfectly valid — same assignment as the row it
	// targets — so ownership is the ONLY thing that can refuse it. Built the
	// other way round, the test passes because the edit was malformed and would
	// keep passing with the ownership check deleted.
	after := f.entry(day(11), "09:00", "10:00", 1)
	after.ID = foreignID
	after.AssignmentID = other
	in := f.batchInput(foreignID, goodReason, fixturePassword)
	in.Changes = []EditChange{{WorkLogID: foreignID, Action: "update", After: after}}

	if _, err := f.Svc.ApplyStaffEditBatch(f.ctx, f.StaffID, in); err == nil {
		t.Fatal("a row belonging to another course must be refused — otherwise the course " +
			"id in the path is decoration and this is an arbitrary-row editor")
	}
	var hours float64
	if err := f.Pool.QueryRow(f.ctx, `SELECT hours FROM work_logs WHERE id=$1`, foreignID).Scan(&hours); err != nil {
		t.Fatal(err)
	}
	if hours != 2 {
		t.Errorf("the foreign row was edited anyway (hours = %.1f)", hours)
	}
}

// The happy path, and the record it must leave.
func TestEditBatch_AppliesAndRecordsWhatChanged(t *testing.T) {
	f, id := batchFixture(t)

	res, err := f.Svc.ApplyStaffEditBatch(f.ctx, f.StaffID, f.batchInput(id, goodReason, fixturePassword))
	if err != nil {
		t.Fatalf("ApplyStaffEditBatch: %v", err)
	}
	if res.Applied != 1 {
		t.Errorf("applied = %d, want 1", res.Applied)
	}

	var hours float64
	if err := f.Pool.QueryRow(f.ctx, `SELECT hours FROM work_logs WHERE id=$1`, id).Scan(&hours); err != nil {
		t.Fatal(err)
	}
	if hours != 1 {
		t.Errorf("hours = %.1f, want 1 — the edit did not reach the row", hours)
	}

	var reason string
	var changes string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT reason, changes::text FROM worklog_edit_batches WHERE id=$1`,
		res.BatchID).Scan(&reason, &changes); err != nil {
		t.Fatalf("the batch was not recorded: %v", err)
	}
	if reason != goodReason {
		t.Errorf("reason = %q, want %q", reason, goodReason)
	}
	// The BEFORE value is the whole point of recording: after the write there is
	// no other copy of what the row used to say.
	if !strings.Contains(squash(changes), `"hours":2`) {
		t.Errorf("the recorded change does not carry the previous hours: %s", changes)
	}
}

func TestEditBatch_DeleteIsRecordedToo(t *testing.T) {
	f, id := batchFixture(t)

	in := f.batchInput(id, goodReason, fixturePassword)
	in.Changes = []EditChange{{WorkLogID: id, Action: "delete"}}
	res, err := f.Svc.ApplyStaffEditBatch(f.ctx, f.StaffID, in)
	if err != nil {
		t.Fatalf("ApplyStaffEditBatch: %v", err)
	}

	var n int
	if err := f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM work_logs WHERE id=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("the row was not deleted")
	}
	var changes string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT changes::text FROM worklog_edit_batches WHERE id=$1`, res.BatchID).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(squash(changes), `"action":"delete"`) || !strings.Contains(squash(changes), `"hours":2`) {
		t.Errorf("a deleted row must be recoverable from the record, got: %s", changes)
	}
}

// A batch that changed nothing must not leave a record claiming it did. The
// reason and the evidence describe an act that did not happen.
func TestEditBatch_NoRecordWhenEveryRowIsRefused(t *testing.T) {
	f, id := batchFixture(t)

	in := f.batchInput(id, goodReason, fixturePassword)
	// 30 hours in a day breaks the daily cap, so StaffUpsert refuses it.
	bad := f.entry(day(10), "09:00", "10:00", 30)
	bad.ID = id
	in.Changes = []EditChange{{WorkLogID: id, Action: "update", After: bad}}

	if _, err := f.Svc.ApplyStaffEditBatch(f.ctx, f.StaffID, in); err == nil {
		t.Fatal("a batch where every row was refused must fail, not report success")
	}
	var n int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM worklog_edit_batches WHERE ta_id=$1`, f.TAID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d batch row(s) recorded for a batch that changed nothing", n)
	}
}

// The Sec column is read next to the money, and the track is what decides the
// rate — ป.ตรี ภาคปกติ is 40฿/h, ภาคพิเศษ 50฿/h. "sec 2" on its own does not
// tell a reviewer what an hour on that row is worth.
func TestMonthDetail_CarriesSectionTrack(t *testing.T) {
	f := newFixture(t, fixtureOpts{Track: "special"})
	f.addAppointmentOrder()
	f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "", false)
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1`, f.AssignmentID)

	rows, err := f.Periods.ListReviewQueue(f.ctx, f.TermID)
	if err != nil {
		t.Fatalf("ListReviewQueue: %v", err)
	}
	var periodID uuid.UUID
	for _, r := range rows {
		if r.TeachingCourseID == f.CourseID {
			periodID = r.PeriodID
		}
	}
	if periodID == uuid.Nil {
		t.Fatal("the fixture course is missing from the review queue")
	}

	detail, err := f.Periods.MonthDetailForReview(f.ctx, periodID, f.CourseID, f.TAID)
	if err != nil {
		t.Fatalf("MonthDetailForReview: %v", err)
	}
	if len(detail.Days) == 0 {
		t.Fatal("no days returned")
	}
	if detail.Days[0].Track != "special" {
		t.Errorf("day track = %q, want %q — without it the reviewer cannot tell a "+
			"40฿ hour from a 50฿ one", detail.Days[0].Track, "special")
	}
}
