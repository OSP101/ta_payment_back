package service

import (
	"strings"
	"testing"
)

// A term staff have not opened takes no TA request: no ช่วงรับคำขอ at all, or
// one that has not started yet. 2569/2 on 03/10/2026 took requests and hours
// before staff set it up, and the hours then had nowhere to be exported.
func TestCreate_RefusedUntilStaffOpenTheRequestWindow(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	rf.exec(`DELETE FROM ta_request_windows WHERE term_id = $1`, rf.TermID)

	if _, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput()); err == nil ||
		!strings.Contains(err.Error(), "ยังไม่ได้กำหนดช่วงรับคำขอ") {
		t.Fatalf("no window: err = %v, want the not-set-up refusal", err)
	}

	rf.exec(`INSERT INTO ta_request_windows (id, term_id, opens_at, closes_at, is_open)
	         VALUES (gen_random_uuid(), $1, NOW() + INTERVAL '2 days', NOW() + INTERVAL '30 days', TRUE)`, rf.TermID)
	if _, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput()); err == nil ||
		!strings.Contains(err.Error(), "ช่วงรับคำขอของภาคเรียนนี้เริ่ม") {
		t.Fatalf("future window: err = %v, want the not-yet-open refusal", err)
	}
	var n int
	if err := rf.Pool.QueryRow(rf.ctx,
		`SELECT COUNT(*) FROM ta_requests WHERE teaching_course_id = $1`, rf.CourseID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d requests written, want none — a refusal must leave no row behind", n)
	}
}

// Past the closing date a request is still accepted, marked late — the gate is
// only about the window having OPENED.
func TestCreate_AfterTheWindowClosesIsLateNotRefused(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	rf.exec(`UPDATE ta_request_windows SET opens_at = NOW() - INTERVAL '30 days', closes_at = NOW() - INTERVAL '1 day'
	         WHERE term_id = $1`, rf.TermID)
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("a late request must still be accepted: %v", err)
	}
	var late bool
	if err := rf.Pool.QueryRow(rf.ctx, `SELECT is_late FROM ta_requests WHERE id = $1`, res.ID).Scan(&late); err != nil {
		t.Fatal(err)
	}
	if !late {
		t.Error("is_late = false, want true")
	}
}

// A work log in a month with no submission period is refused, for the TA and
// for staff keying it in on their behalf: it could never be reviewed or
// exported.
func TestWorklog_RefusedInAMonthWithNoPeriod(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.clearPeriods()

	if _, err := f.upsert(f.entry(day(10), "09:00", "11:00", 2)); err == nil ||
		!strings.Contains(err.Error(), "ยังไม่ได้เปิดรอบลงเวลา") {
		t.Fatalf("TA write: err = %v, want the no-period refusal", err)
	}
	if _, err := f.Svc.StaffUpsert(f.ctx, f.StaffID, true, f.entry(day(10), "09:00", "11:00", 2), nil); err == nil ||
		!strings.Contains(err.Error(), "ยังไม่ได้เปิดรอบลงเวลา") {
		t.Fatalf("staff write: err = %v, want the no-period refusal", err)
	}
}

// Drafts already sitting in a month whose period is later removed are not sent:
// Submit holds them back and says why.
func TestSubmit_HoldsBackRowsInAMonthWithNoPeriod(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.clearPeriods()

	err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID)
	if err == nil || !strings.Contains(err.Error(), "ยังไม่ได้เปิดรอบลงเวลา") {
		t.Fatalf("err = %v, want the no-period refusal", err)
	}
	var submitted int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM work_logs WHERE assignment_id = $1 AND status = 'submitted'`, f.AssignmentID).Scan(&submitted); err != nil {
		t.Fatal(err)
	}
	if submitted != 0 {
		t.Errorf("%d rows submitted, want 0", submitted)
	}
}
