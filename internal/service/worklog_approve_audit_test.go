package service

import (
	"testing"
)

// The lecturer's "ประวัติการอนุมัติ" panel is built entirely from audit_logs
// (ListApprovalHistory reads action IN ('worklog.approve','worklog.reject')),
// so an approval that leaves no audit row is an approval that never happened as
// far as the record is concerned. Auditor.Log swallows its error into a log
// line, which means the failure is silent by construction.
//
// Written while chasing a live observation: approvals driven through the UI
// changed work_logs.approved_at but left audit_logs.id_seq untouched, so no
// INSERT was even attempted. This pins the service contract so the question can
// be answered about the code rather than about one afternoon's dev stack.
func TestApprove_WritesAnAuditRow(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='submitted', submitted_at=now()
	        WHERE assignment_id=$1`, f.AssignmentID)

	ym := day(10)[:7]
	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, ym, false); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	var n int
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT COUNT(*) FROM audit_logs
		WHERE action = 'worklog.approve'
		  AND entity = 'assignment'
		  AND entity_id = $1::text
		  AND actor_id = $2
		  AND note = $3`, f.AssignmentID.String(), f.LecturerID, ym).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("audit rows = %d, want 1 — the approval history panel reads this table, "+
			"and Auditor.Log reports its failures to a log line nobody watches", n)
	}

	// The TA has to learn their month went through; the notification is the only
	// thing that tells them. The title carries the course code (03/08/2026), so
	// this matches the phrase rather than the whole string — a TA on three
	// courses needs to know WHICH one was approved.
	var notes int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM notifications
		 WHERE user_id=$1 AND title LIKE 'อนุมัติบันทึกเวลา%'`,
		f.TAID).Scan(&notes); err != nil {
		t.Fatal(err)
	}
	if notes == 0 {
		t.Error("approving wrote no notification — the TA is never told")
	}
}

// Writing the audit row AFTER the commit leaves a window: the hours are already
// approved, and if the record then fails there is no honest answer left — say
// "ok" and the trail has a hole, say "failed" and the lecturer retries into
// "ไม่มีรายการที่รออนุมัติ" for something that did happen.
//
// LogTx closes the window. This test forces the audit insert to fail and checks
// the approval went with it; against a post-commit version the hours stay
// approved while the caller is told it failed.
func TestApprove_RollsBackTheApprovalWhenTheAuditCannotBeWritten(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	id := f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='submitted', submitted_at=now()
	        WHERE assignment_id=$1`, f.AssignmentID)

	// Make exactly this audit write fail. The fixture owns a throwaway
	// database, so the constraint cannot leak into another test.
	f.exec(`ALTER TABLE audit_logs
	        ADD CONSTRAINT no_approve_audit CHECK (action <> 'worklog.approve')`)

	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, day(10)[:7], false); err == nil {
		t.Fatal("an approval whose audit row cannot be written must fail, not report success")
	}

	var status string
	if err := f.Pool.QueryRow(f.ctx, `SELECT status FROM work_logs WHERE id=$1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "submitted" {
		t.Errorf("status = %q, want \"submitted\" — the hours were approved and the "+
			"record of it was not, so the trail now disagrees with the money", status)
	}
}

// An approval that changes nothing must not leave a record saying it did. This
// is the branch that returns before the audit call, and it is the one that
// would explain a missing row without anything being broken.
func TestApprove_RefusesAndRecordsNothingWhenNoRowsAreWaiting(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	// Left as draft — nothing is submitted, so there is nothing to approve.

	ym := day(10)[:7]
	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, ym, false); err == nil {
		t.Fatal("approving a month with no submitted rows must be refused")
	}

	var n int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM audit_logs WHERE action='worklog.approve' AND entity_id=$1::text`,
		f.AssignmentID.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d audit row(s) written for an approval that was refused", n)
	}
}
