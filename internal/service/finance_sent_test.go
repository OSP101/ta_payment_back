package service

import (
	"strings"
	"testing"
)

// assertPayoutReady is what MarkFinanceSent relies on to refuse the
// finance handoff before the TA's reimbursement paperwork exists. After
// migration 0047 dropped the national_id/account_no/bank_name columns
// (PDPA), readiness can only be read off an approved creditor_form
// document — see payoutReady in export_gate_test.go, which mirrors the
// same check for the export gate.

func TestAssertPayoutReady_RejectsWhenProfileNotApproved(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	// Profile exists but staff have not approved it yet.
	f.exec(`INSERT INTO ta_profiles (user_id, prefix, status, current_round)
	        VALUES ($1, 'นาย', 'pending', 1)`, f.TAID)

	err := f.Periods.assertPayoutReady(f.ctx, f.TAID)
	if err == nil {
		t.Fatal("an unapproved profile must not pass payout readiness")
	}
	if !strings.Contains(err.Error(), "ยังไม่ผ่านการอนุมัติ") {
		t.Errorf("error should say the profile is unapproved, got: %v", err)
	}
}

func TestAssertPayoutReady_RejectsWhenNoApprovedCreditorForm(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	// Profile is approved, but no creditor-form document was ever uploaded —
	// the PDF that now carries the national-ID/bank data staff used to read
	// straight off the dropped columns.
	f.exec(`INSERT INTO ta_profiles (user_id, prefix, status, completed_at, current_round)
	        VALUES ($1, 'นาย', 'approved', now(), 1)`, f.TAID)

	err := f.Periods.assertPayoutReady(f.ctx, f.TAID)
	if err == nil {
		t.Fatal("a profile with no approved creditor-form document must not pass payout readiness")
	}
	if !strings.Contains(err.Error(), "แบบฟอร์มเจ้าหนี้") {
		t.Errorf("error should name the missing creditor form, got: %v", err)
	}
}

func TestMarkFinanceSent_SucceedsWhenPayoutReady(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	payoutReady(f)
	pid := f.addSubmissionPeriod(currentMonthMM(), "2026-12-31", "", false)
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, "", false); err != nil {
		t.Fatal(err)
	}
	if err := f.Periods.MarkStaffReviewed(f.ctx, f.StaffID, pid, f.TAID, f.CourseID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Periods.MarkCourseExported(f.ctx, f.StaffID, f.CourseID, nil); err != nil {
		t.Fatal(err)
	}

	if err := f.Periods.MarkFinanceSent(f.ctx, f.StaffID, pid, f.TAID, f.CourseID, ""); err != nil {
		t.Fatalf("MarkFinanceSent must succeed once the profile is approved and the creditor form is on file: %v", err)
	}

	var status string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT status::text FROM submission_period_status
		 WHERE submission_period_id=$1 AND ta_id=$2 AND teaching_course_id=$3`,
		pid, f.TAID, f.CourseID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "finance_sent" {
		t.Errorf("status = %q, want finance_sent", status)
	}
}
