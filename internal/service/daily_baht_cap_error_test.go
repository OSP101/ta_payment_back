package service

import (
	"context"
	"testing"
)

// PAY-02: enforceDailyBahtCap must distinguish "no pay_rates row" (nothing to
// enforce) from "could not read pay_rates" (connection reset, timeout,
// cancelled context). Before the fix, both cases returned nil, which the
// caller reads as "under the cap" — a single DB hiccup could let a work_logs
// row past the ฿300/day ceiling permanently, with nothing rechecking it later.
func TestEnforceDailyBahtCap_ReturnsErrorWhenReadFails(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	cancelled, cancel := context.WithCancel(f.ctx)
	cancel()

	err := f.Svc.enforceDailyBahtCap(cancelled, f.TAID, f.entry(day(10), "09:00", "11:00", 2))
	if err == nil {
		t.Fatal("enforceDailyBahtCap must return an error when pay_rates cannot be read, got nil")
	}
}
