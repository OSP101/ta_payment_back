package service

import (
	"strings"
	"testing"
)

// No back-dated entry, by anyone (03/08/2026). The closed-month rule used to
// exempt staff, so "the deadline" meant one thing to a TA and another to the
// office — and a TA who missed it could still get the hours in by asking. These
// tests pin the rule as one rule.

// closedPeriodFor closes the fixture's month.
func (f *fixture) closePeriodFor(t *testing.T, mm string) {
	t.Helper()
	f.exec(`INSERT INTO submission_periods (id, term_id, year_month, label, starts_on, due_date, is_closed)
	        SELECT gen_random_uuid(), $1, t.academic_year::text || '-' || $2,
	               'เดือนทดสอบ', CURRENT_DATE - 60, CURRENT_DATE - 30, TRUE
	        FROM academic_terms t WHERE t.id = $1
	        ON CONFLICT DO NOTHING`, f.TermID, mm)
}

func TestStaffUpsert_RefusesAClosedMonth(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.closePeriodFor(t, day(10)[5:7])

	// A brand-new row in the closed month.
	w := f.entry(day(12), "09:00", "11:00", 2)
	_, err := f.Svc.StaffUpsert(f.ctx, f.LecturerID, true, w)
	if err == nil {
		t.Fatal("staff must not be able to add hours to a month that has closed")
	}
	if !strings.Contains(err.Error(), "ไม่ประสงค์ลงเวลา") {
		t.Errorf("the refusal should state the rule, got: %v", err)
	}
}

func TestStaffDelete_RefusesAClosedMonth(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	id := f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.closePeriodFor(t, day(10)[5:7])

	if err := f.Svc.StaffDelete(f.ctx, f.LecturerID, true, id); err == nil {
		t.Fatal("staff must not be able to delete hours from a month that has closed")
	}
	var still int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM work_logs WHERE id=$1`, id).Scan(&still); err != nil {
		t.Fatal(err)
	}
	if still != 1 {
		t.Error("the row was deleted anyway")
	}
}

// The month is only closed for WRITES. A lecturer must still be able to approve
// what a TA submitted before the deadline — otherwise meeting the deadline
// would not be enough either, and the rule would punish the wrong people.
func TestApprove_StillWorksAfterTheMonthCloses(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	f.closePeriodFor(t, day(10)[5:7])

	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, "", false); err != nil {
		t.Fatalf("a lecturer must still approve work submitted in time: %v", err)
	}
	if got := f.worklogStatusOf(t, f.AssignmentID); got != "approved" {
		t.Errorf("status = %q, want approved", got)
	}
}

// Staff keep their powers while the month is open — this is a deadline, not a
// removal of the correction path.
func TestStaffUpsert_StillWorksWhileTheMonthIsOpen(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	w := f.entry(day(12), "09:00", "11:00", 2)
	if _, err := f.Svc.StaffUpsert(f.ctx, f.LecturerID, true, w); err != nil {
		t.Fatalf("staff must still be able to correct an open month: %v", err)
	}
}
