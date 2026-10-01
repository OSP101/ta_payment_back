package service

import (
	"strings"
	"testing"
)

// ตีกลับ on a month nobody has signed off: the grid offered the button there
// and every press was refused with "ยังไม่มีสถานะให้ตีกลับ", because the only
// thing send-back knew how to move was the month's status. On an open month the
// approved rows now go back to the TA, who can only edit draft/rejected rows.
func TestSendBack_UnsignedOpenMonthRejectsApprovedRows(t *testing.T) {
	f, month := reviewFixture(t)
	staff := f.insertUser("staff", "officer")
	pid := mustUUID(t, f.periodID(t, month))

	if err := f.Periods.MarkSentBack(f.ctx, staff, pid, f.TAID, f.CourseID, "pending", "ชั่วโมงไม่ตรงตาราง"); err != nil {
		t.Fatalf("MarkSentBack on an unsigned open month: %v", err)
	}
	var status, reason, byRole string
	var approvedAt *string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT status::text, COALESCE(reject_reason,''), COALESCE(rejected_by_role,''), approved_at::text
		 FROM work_logs WHERE assignment_id=$1`,
		f.AssignmentID).Scan(&status, &reason, &byRole, &approvedAt); err != nil {
		t.Fatal(err)
	}
	// The TA screen names the sender from this; it said "อาจารย์" for staff.
	if byRole != "staff" {
		t.Errorf("rejected_by_role = %q, want staff", byRole)
	}
	if status != "rejected" || reason != "ชั่วโมงไม่ตรงตาราง" {
		t.Fatalf("row = %s / %q, want rejected with the officer's reason", status, reason)
	}
	if approvedAt != nil {
		t.Errorf("approved_at kept on a rejected row: %s", *approvedAt)
	}
	// The send-back writes its reason on the month's status row (WP3), so the
	// timeline shows the current reason; the month stays pending.
	if got := f.statusOf(t); got != "pending" {
		t.Errorf("status after send-back = %q, want pending", got)
	}

	// Nothing approved is left, so a second press says so rather than succeeding.
	err := f.Periods.MarkSentBack(f.ctx, staff, pid, f.TAID, f.CourseID, "pending", "อีกครั้ง")
	if err == nil || !strings.Contains(err.Error(), "ไม่มีรายการที่อนุมัติแล้ว") {
		t.Fatalf("second send-back = %v, want a nothing-to-send-back refusal", err)
	}
}

// A closed month is final: a rejected row there is forfeited and can never be
// resent, so sending it back would cancel the TA's pay instead of fixing it.
func TestSendBack_UnsignedClosedMonthRefused(t *testing.T) {
	f, month := reviewFixture(t)
	staff := f.insertUser("staff", "officer")
	pid := mustUUID(t, f.periodID(t, month))
	f.exec(`UPDATE submission_periods SET is_closed = true WHERE id = $1`, pid)

	err := f.Periods.MarkSentBack(f.ctx, staff, pid, f.TAID, f.CourseID, "pending", "แก้ไข")
	if err == nil || !strings.Contains(err.Error(), "ปิดรับบันทึกเวลาแล้ว") {
		t.Fatalf("MarkSentBack on a closed month = %v, want a closed-period refusal", err)
	}
	var status string
	if err := f.Pool.QueryRow(f.ctx, `SELECT status::text FROM work_logs WHERE assignment_id=$1`,
		f.AssignmentID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "approved" {
		t.Fatalf("row moved to %s on a refused send-back", status)
	}
}

// A signed-off month now behaves like every other: it goes back to pending AND
// its rows go back to the TA as rejected, in one press (WP3, bug 3). Moving
// only the status left a month nobody could edit.
func TestSendBack_ReviewedMonthAlsoReopensRows(t *testing.T) {
	f, month := reviewFixture(t)
	staff := f.insertUser("staff", "officer")
	pid := mustUUID(t, f.periodID(t, month))
	if err := f.Periods.MarkStaffReviewed(f.ctx, staff, pid, f.TAID, f.CourseID, ""); err != nil {
		t.Fatalf("MarkStaffReviewed: %v", err)
	}
	if err := f.Periods.MarkSentBack(f.ctx, staff, pid, f.TAID, f.CourseID, "pending", "ตรวจใหม่"); err != nil {
		t.Fatalf("MarkSentBack: %v", err)
	}
	if got := f.statusOf(t); got != "pending" {
		t.Fatalf("status = %q, want pending", got)
	}
	var status string
	if err := f.Pool.QueryRow(f.ctx, `SELECT status::text FROM work_logs WHERE assignment_id=$1`,
		f.AssignmentID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" {
		t.Fatalf("reviewed-month send-back left rows %s, want rejected", status)
	}
}
