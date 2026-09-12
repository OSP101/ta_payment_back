package service

import (
	"testing"
)

// ListApprovalHistory used to filter WHERE al.actor_id = $1 (the viewer's own
// actions only), so a lecturer had no way to see that staff/admin had
// approved or rejected a month on their behalf — the queue just emptied out
// with no visible reason, which is exactly what this session's live testing
// surfaced: an admin approved a TA's stuck hours, and the lecturer's own
// "ประวัติการอนุมัติ" panel stayed empty. This test pins the fix: the
// lecturer must see staff's action too, with the staff member's name.
func TestListApprovalHistory_ShowsActionsTakenByStaffNotJustTheViewer(t *testing.T) {
	f := newFixture(t, fixtureOpts{
		Workload: workloadHours{Attendance: 3, Lab: 3, CheckWork: 3},
	})
	if _, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// StaffID approves, NOT the lecturer.
	if err := f.Svc.Approve(f.ctx, f.StaffID, f.AssignmentID, "", true); err != nil {
		t.Fatalf("Approve (by staff): %v", err)
	}

	// The lecturer, who did nothing themselves, must still see the entry.
	entries, err := f.Svc.ListApprovalHistory(f.ctx, f.LecturerID, f.CourseID)
	if err != nil {
		t.Fatalf("ListApprovalHistory (lecturer view): %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 history entry visible to the lecturer, got %d", len(entries))
	}
	e := entries[0]
	if e.Action != "worklog.approve" {
		t.Errorf("action = %q, want worklog.approve", e.Action)
	}
	if e.ActorName == "" {
		t.Error("actor_name is empty — the lecturer can't tell WHO approved it")
	}
	// The history panel used to be "purely informational" — a TA name and
	// nothing else, no way to see what was actually approved. Rows is the
	// snapshot that makes "ดูรายละเอียด" possible.
	if len(e.Rows) == 0 {
		t.Error("rows is empty — the lecturer has no way to see what was actually approved")
	}
}

// The single-month Approve path (used by the per-month "อนุมัติ" button, as
// opposed to ApproveMany's "อนุมัติทุกเดือน" batch) didn't capture a row
// snapshot at all until this session — only yearMonth in the note. This
// pins that it now matches ApproveMany/Reject's Before.rows shape exactly.
func TestApprove_SingleMonthCapturesTheSameRowSnapshotAsBatchApproveAndReject(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.mustUpsert(f.entry(day(11), "09:00", "10:00", 1))
	f.exec(`UPDATE work_logs SET status='submitted', submitted_at=now() WHERE assignment_id=$1`, f.AssignmentID)

	ym := day(10)[:7]
	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, ym, false); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	entries, err := f.Svc.ListApprovalHistory(f.ctx, f.LecturerID, f.CourseID)
	if err != nil {
		t.Fatalf("ListApprovalHistory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if len(entries[0].Rows) != 2 {
		t.Fatalf("want 2 rows in the snapshot (both dates approved), got %d: %+v",
			len(entries[0].Rows), entries[0].Rows)
	}
	var total float64
	for _, r := range entries[0].Rows {
		total += r.Hours
		// StartTime/EndTime round out the row so the frontend can feed it
		// straight into MonthTable (the pending queue's own detail view,
		// reused here rather than a second design) without a blank column.
		if r.StartTime == "" || r.EndTime == "" {
			t.Errorf("row %+v missing start_time/end_time", r)
		}
	}
	if total != 3 {
		t.Errorf("snapshot hours sum = %.1f, want 3 (2+1)", total)
	}
}
