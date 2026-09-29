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
	var status, reason string
	var approvedAt *string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT status::text, COALESCE(reject_reason,''), approved_at::text FROM work_logs WHERE assignment_id=$1`,
		f.AssignmentID).Scan(&status, &reason, &approvedAt); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" || reason != "ชั่วโมงไม่ตรงตาราง" {
		t.Fatalf("row = %s / %q, want rejected with the officer's reason", status, reason)
	}
	if approvedAt != nil {
		t.Errorf("approved_at kept on a rejected row: %s", *approvedAt)
	}
	if got := f.statusOf(t); got != "(no row)" {
		t.Errorf("send-back minted a status row: %q", got)
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

// The signed-off path is unchanged: a staff_reviewed month goes back to
// pending and its rows stay as they are.
func TestSendBack_ReviewedMonthStillMovesStatus(t *testing.T) {
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
	if status != "approved" {
		t.Fatalf("reviewed-month send-back moved rows to %s", status)
	}
}
