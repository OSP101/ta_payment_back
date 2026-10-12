package service

import (
	"errors"
	"testing"
)

// Theme and language are saved on the account (migration 0153) and come
// back through Get, which is what /me returns. An empty field leaves the
// saved value alone, so the theme switch can't reset the language.
func TestSetUIPreferences(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := f.users()

	u, err := svc.Get(f.ctx, f.StaffID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if u.UITheme != "system" || u.UILocale != "th" {
		t.Fatalf("defaults: want system/th, got %s/%s", u.UITheme, u.UILocale)
	}

	if err := svc.SetUIPreferences(f.ctx, f.StaffID, "dark", ""); err != nil {
		t.Fatalf("set theme: %v", err)
	}
	if err := svc.SetUIPreferences(f.ctx, f.StaffID, "", "en"); err != nil {
		t.Fatalf("set locale: %v", err)
	}
	u, _ = svc.Get(f.ctx, f.StaffID)
	if u.UITheme != "dark" || u.UILocale != "en" {
		t.Fatalf("after two partial saves: want dark/en, got %s/%s", u.UITheme, u.UILocale)
	}

	for _, bad := range [][2]string{{"purple", ""}, {"", "fr"}, {"DARK", ""}} {
		if err := svc.SetUIPreferences(f.ctx, f.StaffID, bad[0], bad[1]); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%v: want ErrInvalidInput, got %v", bad, err)
		}
	}
	u, _ = svc.Get(f.ctx, f.StaffID)
	if u.UITheme != "dark" || u.UILocale != "en" {
		t.Fatalf("a refused save must change nothing, got %s/%s", u.UITheme, u.UILocale)
	}
}
