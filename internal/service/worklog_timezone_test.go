package service

import (
	"testing"
	"time"

	"ta-payment-back/internal/timeutil"
)

// Regression guard for the month-boundary timezone bug.
//
// The back-date rule in validateWorkLogEntry compares the work date's
// (year, month) against todayRef's. Upsert used to pass time.Now(), whose
// calendar date follows the host's zone. On a UTC host, every month boundary
// opened a seven-hour window (00:00–07:00 Bangkok) in which todayRef still
// reported the previous month, so a TA could write into a month the system
// was supposed to have frozen — hours that then flow straight into a payout.
//
// Upsert now passes timeutil.Now(). These tests pin the semantics at the
// validator so the guarantee survives future edits to either side.

// nowUTCButAugustInBangkok is 2026-07-31 20:00 UTC == 2026-08-01 03:00 Bangkok:
// inside the window where the two zones disagree about the month.
func nowUTCButAugustInBangkok() time.Time {
	return time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC)
}

func backdateProbe(date string) WorkLog {
	return WorkLog{
		WorkDate: date, Activity: "review",
		StartTime: "09:00", EndTime: "11:00", Hours: 2,
	}
}

func TestValidate_BackdateUsesBangkokMonth(t *testing.T) {
	start, end := hardeningBounds() // 2026-06-01 .. 2026-10-31
	ref := nowUTCButAugustInBangkok().In(timeutil.Bangkok)

	// It is already August in Bangkok, so July is a closed month.
	err := validateWorkLogEntry(backdateProbe("2026-07-20"), hardeningGate(),
		start, end, examWindow{}, examWindow{}, holidaySet{}, makeupIndex{}, ref)
	if err == nil {
		t.Fatal("a July date must be rejected once Bangkok has rolled into August")
	}

	// August itself stays writable at 03:00 on the 1st.
	if err := validateWorkLogEntry(backdateProbe("2026-08-01"), hardeningGate(),
		start, end, examWindow{}, examWindow{}, holidaySet{}, makeupIndex{}, ref); err != nil {
		t.Fatalf("the current Bangkok month must be writable, got: %v", err)
	}
}

// The same instant read in UTC accepts the July row. This test documents the
// defect rather than the fix: if it ever starts failing, the two zones have
// stopped disagreeing and the guard above has lost its meaning.
func TestValidate_BackdateWouldLeakUnderUTC(t *testing.T) {
	start, end := hardeningBounds()
	ref := nowUTCButAugustInBangkok() // left in UTC on purpose

	if err := validateWorkLogEntry(backdateProbe("2026-07-20"), hardeningGate(),
		start, end, examWindow{}, examWindow{}, holidaySet{}, makeupIndex{}, ref); err != nil {
		t.Fatalf("precondition: UTC reading still thinks it is July, so this "+
			"write should pass — the divergence is the bug being guarded. got: %v", err)
	}
}

// Belt and braces: the guarantee must hold no matter what TZ the developer's
// machine or the CI runner is set to. timeutil.Now() is zone-independent by
// construction, so its calendar day always matches the Bangkok reading of the
// same instant.
func TestTimeutilNowMatchesBangkokCalendarDay(t *testing.T) {
	for _, host := range []string{"UTC", "America/Los_Angeles", "Asia/Bangkok", "Pacific/Kiritimati"} {
		loc, err := time.LoadLocation(host)
		if err != nil {
			t.Skipf("zone %s unavailable: %v", host, err)
		}
		instant := time.Now()
		wantY, wantM, wantD := instant.In(timeutil.Bangkok).Date()
		gotY, gotM, gotD := instant.In(loc).In(timeutil.Bangkok).Date()
		if wantY != gotY || wantM != gotM || wantD != gotD {
			t.Errorf("host zone %s changed the Bangkok calendar day: got %d-%02d-%02d want %d-%02d-%02d",
				host, gotY, gotM, gotD, wantY, wantM, wantD)
		}
	}
}
