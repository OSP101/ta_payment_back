package service

import (
	"testing"

	"ta-payment-back/internal/timeutil"
)

// remindableDueDate sits inside the default 3-day remind_days_before window
// (today + 2), so SweepReminders' in-window check passes regardless of the
// calendar day the suite runs on.
func remindableDueDate() string { return timeutil.Now().AddDate(0, 0, 2).Format("2006-01-02") }

// SweepReminders fires "โปรดบันทึกเวลาปฏิบัติงาน...ให้ครบ" at TAs with a
// pending submission_period_status row. A grad-special (master/phd,
// track=special) TA no longer logs work_logs at all — pay is computed
// automatically from the regular track's class schedule — so this reminder
// is both wrong for them and, since their status can never leave 'pending',
// would otherwise fire every 24 hours forever.
func TestSweepReminders_ExcludesGradSpecial(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master", Track: "special"})
	f.addSubmissionPeriod(currentMonthMM(), remindableDueDate(), "", false)

	n, err := f.Periods.SweepReminders(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("SweepReminders sent %d reminder(s) to a grad-special TA, want 0", n)
	}
}

// A grad-regular (master/phd, track=regular) TA still logs their own hours,
// so the reminder must still reach them.
func TestSweepReminders_StillRemindsGradRegular(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master", Track: "regular"})
	f.addSubmissionPeriod(currentMonthMM(), remindableDueDate(), "", false)

	n, err := f.Periods.SweepReminders(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("SweepReminders sent 0 reminders for a grad-regular TA with a pending period, want at least 1")
	}
}
