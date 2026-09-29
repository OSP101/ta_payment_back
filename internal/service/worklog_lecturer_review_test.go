package service

import (
	"encoding/json"
	"strings"
	"testing"
)

// The lecturer corrects a co-taught sitting on the review screen. It is one
// real session written against both sections, so the correction must reach
// both copies — otherwise sec 1 and sec 2 disagree about when the lab ran.
func TestLecturerAdjust_CoTaughtSittingMovesTogether(t *testing.T) {
	f, sibling := pendingCoTaughtFixture(t)
	var logID string
	if err := f.Pool.QueryRow(f.ctx, `SELECT id FROM work_logs WHERE assignment_id=$1`, f.AssignmentID).Scan(&logID); err != nil {
		t.Fatal(err)
	}

	err := f.Svc.LecturerAdjust(f.ctx, f.LecturerID, false, LecturerAdjustInput{
		LogID: mustUUID(t, logID), StartTime: "09:00", EndTime: "10:00", Hours: 1,
		Reason: "ปฏิบัติการเลิก 10 โมง ตามตารางจริง",
	})
	if err != nil {
		t.Fatalf("LecturerAdjust: %v", err)
	}
	for _, aid := range []any{f.AssignmentID, sibling} {
		var end, status string
		var hours float64
		if err := f.Pool.QueryRow(f.ctx,
			`SELECT LEFT(end_time::text,5), hours, status::text FROM work_logs WHERE assignment_id=$1`, aid).
			Scan(&end, &hours, &status); err != nil {
			t.Fatal(err)
		}
		if end != "10:00" || hours != 1 {
			t.Errorf("assignment %v: %s %.1f ชม., want 10:00 1.0 — every copy of the sitting must move", aid, end, hours)
		}
		if status != "submitted" {
			t.Errorf("assignment %v: status %s, want still submitted — approving is a separate act", aid, status)
		}
	}

	// The TA reads the correction and its reason on their own assignment.
	changes, err := f.Svc.ListChanges(f.ctx, f.TAID, f.AssignmentID, false)
	if err != nil {
		t.Fatalf("ListChanges as TA: %v", err)
	}
	if len(changes) != 1 || changes[0].Action != "edit" || !strings.Contains(changes[0].Reason, "ตามตารางจริง") {
		t.Fatalf("changes = %+v, want one edit with the reason", changes)
	}
	var before, after changeSnapshot
	if err := json.Unmarshal(changes[0].Before, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(changes[0].After, &after); err != nil {
		t.Fatal(err)
	}
	if before.EndTime != "11:00" || before.Hours != 2 || after.EndTime != "10:00" || after.Hours != 1 {
		t.Errorf("before/after = %+v → %+v, want 11:00 2 ชม. → 10:00 1 ชม.", before, after)
	}
}

// Cutting removes the sitting from the claim — every copy — while keeping its
// last state and the reason where both the lecturer and the TA can find it.
func TestLecturerCut_RemovesEveryCopyAndKeepsHistory(t *testing.T) {
	f, sibling := pendingCoTaughtFixture(t)
	var logID string
	if err := f.Pool.QueryRow(f.ctx, `SELECT id FROM work_logs WHERE assignment_id=$1`, sibling).Scan(&logID); err != nil {
		t.Fatal(err)
	}
	if err := f.Svc.LecturerCut(f.ctx, f.LecturerID, false, mustUUID(t, logID), "วันนี้ไม่มีการเรียนการสอน"); err != nil {
		t.Fatalf("LecturerCut: %v", err)
	}
	var left int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM work_logs WHERE assignment_id IN ($1,$2)`, f.AssignmentID, sibling).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d copies left in work_logs, want 0", left)
	}
	for _, aid := range []any{f.AssignmentID, sibling} {
		var n int
		if err := f.Pool.QueryRow(f.ctx,
			`SELECT COUNT(*) FROM work_log_lecturer_changes WHERE assignment_id=$1 AND action='cut' AND reason LIKE '%ไม่มีการเรียน%'`, aid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("assignment %v: %d cut records, want 1", aid, n)
		}
	}
}

func TestLecturerReview_Guards(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	id := f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))

	// A draft has not been sent: nothing to review yet.
	if err := f.Svc.LecturerCut(f.ctx, f.LecturerID, false, id, "ไม่มีการเรียนการสอนวันนี้"); err == nil {
		t.Error("cutting a draft must be refused")
	}
	f.exec(`UPDATE work_logs SET status='submitted', submitted_at=now() WHERE id=$1`, id)

	if err := f.Svc.LecturerCut(f.ctx, f.LecturerID, false, id, "สั้น"); err == nil {
		t.Error("a reason shorter than the minimum must be refused")
	}
	other := f.insertUser("lecturer", "other-lecturer")
	if err := f.Svc.LecturerCut(f.ctx, other, false, id, "ไม่มีการเรียนการสอนวันนี้"); err == nil {
		t.Error("a lecturer of another course must be refused")
	}

	// The period closed while the row waited for review: the lecturer still
	// reviews it, so correcting it must still work.
	f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "", true)
	if err := f.Svc.LecturerAdjust(f.ctx, f.LecturerID, false, LecturerAdjustInput{
		LogID: id, StartTime: "09:00", EndTime: "10:00", Hours: 1, Reason: "ตรวจงานจริงแค่ 1 ชั่วโมง",
	}); err != nil {
		t.Errorf("adjust in a closed period, row sent in time: %v", err)
	}

	f.exec(`UPDATE work_logs SET status='approved' WHERE id=$1`, id)
	if err := f.Svc.LecturerCut(f.ctx, f.LecturerID, false, id, "ไม่มีการเรียนการสอนวันนี้"); err == nil {
		t.Error("an approved row is a signed record and must not be cut on this path")
	}
}
