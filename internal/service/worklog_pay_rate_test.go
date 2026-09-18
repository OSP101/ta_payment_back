package service

import (
	"testing"

	"github.com/google/uuid"
)

// PayRateFor backs the "ส่งอนุมัติ" confirm dialog's per-month baht estimate —
// see the doc comment on PayRateEstimate. These pin the four (level, track)
// combinations it must tell apart, since each one bills differently.

func TestPayRateFor_UndergradRegular(t *testing.T) {
	f := newFixture(t, fixtureOpts{Rates: rateOverrides{UndergradRegular: 40}})
	out, err := f.Svc.PayRateFor(f.ctx, f.TAID, f.AssignmentID)
	if err != nil {
		t.Fatal(err)
	}
	if out.IsLumpsum {
		t.Error("undergrad ภาคปกติ must not be a lumpsum")
	}
	if out.RatePerHour != 40 {
		t.Errorf("rate = %v, want 40", out.RatePerHour)
	}
	if out.MonthlyCapBaht != 0 {
		t.Errorf("monthly cap = %v, want 0 — ภาคปกติ has no monthly ceiling", out.MonthlyCapBaht)
	}
}

func TestPayRateFor_UndergradSpecial_CarriesTheMonthlyCap(t *testing.T) {
	f := newFixture(t, fixtureOpts{Track: "special", Rates: rateOverrides{UndergradSpecial: 50}})
	out, err := f.Svc.PayRateFor(f.ctx, f.TAID, f.AssignmentID)
	if err != nil {
		t.Fatal(err)
	}
	if out.IsLumpsum {
		t.Error("undergrad ภาคพิเศษ must not be a lumpsum")
	}
	if out.RatePerHour != 50 {
		t.Errorf("rate = %v, want 50", out.RatePerHour)
	}
	// ประกาศ: "50 บาท/ชั่วโมง หรือ 2,000 บาท/เดือน" — the fixture's pay_rates row
	// leaves ug_special_monthly_cap at its schema default (see migration 0040).
	if out.MonthlyCapBaht != 2000 {
		t.Errorf("monthly cap = %v, want 2000", out.MonthlyCapBaht)
	}
}

func TestPayRateFor_GraduateRegular(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master", Rates: rateOverrides{GradRegularHourly: 55}})
	out, err := f.Svc.PayRateFor(f.ctx, f.TAID, f.AssignmentID)
	if err != nil {
		t.Fatal(err)
	}
	if out.IsLumpsum {
		t.Error("graduate ภาคปกติ must not be a lumpsum")
	}
	if out.RatePerHour != 55 {
		t.Errorf("rate = %v, want 55", out.RatePerHour)
	}
	if out.MonthlyCapBaht != 0 {
		t.Errorf("monthly cap = %v, want 0 — graduate ภาคปกติ has no monthly ceiling", out.MonthlyCapBaht)
	}
}

func TestPayRateFor_GraduateSpecial_IsAFlatLumpsumNotAnHourlyRate(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master", Track: "special"})
	out, err := f.Svc.PayRateFor(f.ctx, f.TAID, f.AssignmentID)
	if err != nil {
		t.Fatal(err)
	}
	if !out.IsLumpsum {
		t.Fatal("graduate ภาคพิเศษ must be reported as a lumpsum")
	}
	if out.RatePerHour != 0 {
		t.Errorf("rate = %v, want 0 — a lumpsum has no per-hour figure to multiply by", out.RatePerHour)
	}
	// The fixture seeds graduate_special_lumpsum=12000 and leaves
	// grad_special_term_cap at its schema default of 12000 (migration 0018) —
	// LEAST of the two, same as the real settlement (see budget.go).
	if out.LumpsumBaht != 12000 {
		t.Errorf("lumpsum = %v, want 12000", out.LumpsumBaht)
	}
}

func TestPayRateFor_RefusesACallerWhoDoesNotOwnTheAssignment(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	if _, err := f.Svc.PayRateFor(f.ctx, uuid.New(), f.AssignmentID); err == nil {
		t.Fatal("expected an error for a non-owning caller, got nil")
	}
}
