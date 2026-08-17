package service

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/auth"
)

// ValidatePassword gained a fuller rule set (uppercase + lowercase + digit +
// special char + a known-weak blocklist) alongside the length check that
// already existed. These are pure-function cases — no DB needed.
func TestValidatePassword(t *testing.T) {
	cases := []struct {
		name string
		pw   string
		ok   bool
	}{
		{"too short", "Ab1!ab", false},
		{"no uppercase", "abcdefg1!", false},
		{"no lowercase", "ABCDEFG1!", false},
		{"no digit", "Abcdefgh!", false},
		{"no special char", "Abcdefgh1", false},
		{"blocklisted common password", "Password1!", false},
		{"valid strong password", "Xk9#mQz2Lp", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidatePassword(c.pw)
			if c.ok && err != nil {
				t.Fatalf("expected %q to pass, got error: %v", c.pw, err)
			}
			if !c.ok && err == nil {
				t.Fatalf("expected %q to be rejected, but it passed", c.pw)
			}
		})
	}
}

// insertUserWithTempPassword mirrors the state a brand-new account (or one
// just hit by an admin ResetPassword) is left in: a real password hash, and
// must_change_password=TRUE so the forced first-login flow applies.
func (f *fixture) insertUserWithTempPassword(tempPw string) uuid.UUID {
	id := f.insertUser("ta", "tapw")
	hash, err := auth.HashPassword(tempPw)
	if err != nil {
		f.t.Fatalf("hash temp password: %v", err)
	}
	f.exec(`UPDATE users SET password_hash=$1, must_change_password=TRUE WHERE id=$2`, hash, id)
	return id
}

// --- voluntary change (Account Settings) ---

func TestChangePassword_VoluntaryRequiresCurrentPassword(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	err := f.users().ChangePassword(f.ctx, f.StaffID, "", "Xk9#mQz2Lp")
	ue, ok := err.(*UserError)
	if !ok || ue.Status != 400 {
		t.Fatalf("want 400 when current_password is missing for a voluntary change, got %#v", err)
	}
}

func TestChangePassword_VoluntaryRejectsWrongCurrentPassword(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	err := f.users().ChangePassword(f.ctx, f.StaffID, "not-the-password", "Xk9#mQz2Lp")
	ue, ok := err.(*UserError)
	if !ok || ue.Status != 401 {
		t.Fatalf("want 401 for a wrong current password, got %#v", err)
	}
}

func TestChangePassword_VoluntarySuccessAppliesNewPassword(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	const newPw = "Xk9#mQz2Lp"
	if err := f.users().ChangePassword(f.ctx, f.StaffID, fixturePassword, newPw); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	t.Cleanup(func() { pwGateReset(f.StaffID) })
	if err := VerifyUserPassword(f.ctx, f.Pool, f.StaffID, newPw); err != nil {
		t.Fatalf("new password should verify: %v", err)
	}
	if err := VerifyUserPassword(f.ctx, f.Pool, f.StaffID, fixturePassword); err == nil {
		t.Fatal("old password must no longer work after the change")
	}
}

// --- reuse rule: applies to BOTH flows ---

func TestChangePassword_RejectsReuseOfCurrentPasswordInVoluntaryFlow(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	// fixturePassword itself doesn't satisfy the new character rules, but the
	// reuse check must fire before ValidatePassword's rules even run — a
	// same-password "change" is a duplicate error, not a weak-password one.
	err := f.users().ChangePassword(f.ctx, f.StaffID, fixturePassword, fixturePassword)
	ue, ok := err.(*UserError)
	if !ok || ue.Status != 400 || !strings.Contains(ue.Msg, "ซ้ำ") {
		t.Fatalf("want a 400 duplicate-password error, got %#v", err)
	}
}

func TestChangePassword_ForcedFlowRejectsReuseOfTempPassword(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	const tempPw = "TempPw9!Zx"
	id := f.insertUserWithTempPassword(tempPw)
	// The whole point of this rule: a TA who just logged in with the
	// system-generated temp password (handed to them in plaintext by staff —
	// see TempPasswordPanel) must not be allowed to keep it.
	err := f.users().ChangePassword(f.ctx, id, "", tempPw)
	ue, ok := err.(*UserError)
	if !ok || ue.Status != 400 || !strings.Contains(ue.Msg, "ซ้ำ") {
		t.Fatalf("must reject keeping the temp password, got %#v", err)
	}
}

// --- forced first-login change (must_change_password) ---

func TestChangePassword_ForcedFlowDoesNotRequireCurrentPassword(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	id := f.insertUserWithTempPassword("TempPw9!Zx")
	if err := f.users().ChangePassword(f.ctx, id, "", "Xk9#mQz2Lp"); err != nil {
		t.Fatalf("a forced first-login change should not require current_password: %v", err)
	}
}

func TestChangePassword_ForcedFlowSuccessClearsMustChangeFlag(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	id := f.insertUserWithTempPassword("TempPw9!Zx")
	if err := f.users().ChangePassword(f.ctx, id, "", "Xk9#mQz2Lp"); err != nil {
		t.Fatalf("expected success: %v", err)
	}
	u, err := f.users().Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if u.MustChangePassword {
		t.Fatal("must_change_password should be cleared after a successful change")
	}
}

// --- ValidatePassword's fuller rule still runs inside ChangePassword ---

func TestChangePassword_PropagatesValidatePasswordRule(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	// Passes length/upper/lower/digit but is missing a special character.
	err := f.users().ChangePassword(f.ctx, f.StaffID, fixturePassword, "Abcdefgh1")
	ue, ok := err.(*UserError)
	if !ok || ue.Status != 400 || !strings.Contains(ue.Msg, "อักขระพิเศษ") {
		t.Fatalf("expected the missing-special-char rule to surface, got %#v", err)
	}
}
