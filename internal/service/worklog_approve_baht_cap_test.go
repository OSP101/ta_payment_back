package service

import (
	"strings"
	"testing"
)

// UAT DEF-005: the ฿300/day cap was checked only when a row was written.
// A cap lowered (or a rate raised) between logging and approval let an
// over-cap day through the last gate. Approval now re-prices each day.
func TestApprove_RechecksDailyBahtCap(t *testing.T) {
	f := newFixture(t, fixtureOpts{Rates: rateOverrides{UndergradRegular: 40}})
	f.mustUpsert(f.entry(day(10), "09:00", "14:00", 5)) // 200 ฿, fine at 300
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}

	f.exec(`UPDATE pay_rates SET daily_pay_cap_baht = 150`)
	err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, "", false)
	if err == nil || !strings.Contains(err.Error(), "ค่าตอบแทนรวมต่อวันเกิน 150 บาท") {
		t.Fatalf("approve over the lowered cap: err = %v, want refusal", err)
	}
	if !strings.Contains(err.Error(), thaiLongDateISO(day(10))) {
		t.Errorf("message %q should name the day in Thai (%s), not as an ISO date", err, thaiLongDateISO(day(10)))
	}
	if got := f.worklogStatusOf(t, f.AssignmentID); got != "submitted" {
		t.Errorf("row became %q, want it left submitted", got)
	}

	f.exec(`UPDATE pay_rates SET daily_pay_cap_baht = 300`)
	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, "", false); err != nil {
		t.Errorf("approve within the cap: %v", err)
	}
}

// The re-check prices only the days being approved now. An already approved
// day that a later rate rise would put over the cap must not block approving
// a different day — nobody can edit the old one any more.
func TestApprove_BahtRecheckIgnoresDaysAlreadyApproved(t *testing.T) {
	f := newFixture(t, fixtureOpts{Rates: rateOverrides{UndergradRegular: 40}})
	f.exec(`INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES ($1, $2, '08:00', '15:00', 7, 'review', 'approved')`, f.AssignmentID, day(3)) // 280 ฿ then
	f.mustUpsert(f.entry(day(10), "09:00", "10:00", 1))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE pay_rates SET undergrad_regular = 50`) // day 3 is now 350 ฿ > 300

	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, "", false); err != nil {
		t.Fatalf("approving day 10 must not be refused over already-approved day 3: %v", err)
	}
}
