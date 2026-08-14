package service

import (
	"testing"

	"github.com/google/uuid"
)

// UserService.Authenticate is what POST /auth/login calls. These tests pin
// the per-account lockout it layers under the route's own per-IP limiter
// (handler.Mount's loginLimiter) — see login_gate.go's doc comment for why
// that IP limiter alone is not enough against credential stuffing spread
// across many IPs.

// loginGateReset drops an account's counter. Package-level state outlives a
// single test; fixtures use a fresh uuid each time, but a test that names its
// own actor must not leak into the next one.
func loginGateReset(userID uuid.UUID) { loginAttempts.Delete(userID) }

func TestLogin_LocksOutAfterRepeatedWrongPasswords(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	t.Cleanup(func() { loginGateReset(f.StaffID) })

	email, err := f.users().GetEmail(f.ctx, f.StaffID)
	if err != nil {
		t.Fatalf("get fixture email: %v", err)
	}

	// Sanity: the correct password works before anything has gone wrong. If
	// this fails the rest proves nothing.
	if _, err := f.users().Authenticate(f.ctx, email, fixturePassword, "127.0.0.1", "test"); err != nil {
		t.Fatalf("correct password must be accepted on a clean counter: %v", err)
	}

	for i := 1; i <= loginMaxFails; i++ {
		_, err := f.users().Authenticate(f.ctx, email, "not-the-password", "127.0.0.1", "test")
		if err == nil {
			t.Fatalf("wrong password #%d was accepted", i)
		}
		if ue, ok := err.(*UserError); !ok || ue.Status != 401 {
			t.Fatalf("wrong password #%d: want a 401 UserError, got %#v", i, err)
		}
	}

	// The point of the whole test: the CORRECT password is now refused too —
	// a limiter that still opens for the right answer has only slowed a
	// brute-force search, not stopped it.
	_, err = f.users().Authenticate(f.ctx, email, fixturePassword, "127.0.0.1", "test")
	if err == nil {
		t.Fatalf("after %d wrong passwords the gate still opened for the correct one — "+
			"an attacker grinding this account would have gotten in on the guess after next",
			loginMaxFails)
	}
	ue, ok := err.(*UserError)
	if !ok || ue.Status != 429 {
		t.Fatalf("lockout must surface as 429 so the UI can say 'wait', got %#v", err)
	}
}

func TestLogin_OnlyConsecutiveFailuresCount(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	t.Cleanup(func() { loginGateReset(f.StaffID) })

	email, err := f.users().GetEmail(f.ctx, f.StaffID)
	if err != nil {
		t.Fatalf("get fixture email: %v", err)
	}

	for round := 0; round < 3; round++ {
		for i := 0; i < loginMaxFails-1; i++ {
			if _, err := f.users().Authenticate(f.ctx, email, "not-the-password", "127.0.0.1", "test"); err == nil {
				t.Fatalf("round %d: wrong password #%d was accepted", round, i)
			}
		}
		// One under the threshold each round — a real login clears it before
		// it ever accumulates into a lockout.
		if _, err := f.users().Authenticate(f.ctx, email, fixturePassword, "127.0.0.1", "test"); err != nil {
			t.Fatalf("round %d: correct password after %d (not %d) failures should still work: %v",
				round, loginMaxFails-1, loginMaxFails, err)
		}
	}
}

// A nonexistent email must never lock — see login_gate.go's doc comment on
// loginAttempts for why the map is keyed on the resolved user id rather than
// the raw email string. Repeated attempts against an address with no account
// behind it should just keep answering the ordinary "wrong credentials" 401,
// never a 429.
func TestLogin_UnknownEmailNeverLocksOut(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	for i := 1; i <= loginMaxFails+3; i++ {
		_, err := f.users().Authenticate(f.ctx, "no-such-account@example.test", "whatever", "127.0.0.1", "test")
		ue, ok := err.(*UserError)
		if !ok {
			t.Fatalf("attempt #%d: want a UserError, got %#v", i, err)
		}
		if ue.Status != 401 {
			t.Fatalf("attempt #%d: unknown email must stay a plain 401, got %d", i, ue.Status)
		}
	}
}

func TestLogin_LockoutIsPerAccount(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	other := f.insertStaffWithPassword()
	t.Cleanup(func() { loginGateReset(f.StaffID); loginGateReset(other) })

	email, err := f.users().GetEmail(f.ctx, f.StaffID)
	if err != nil {
		t.Fatalf("get fixture email: %v", err)
	}
	otherEmail, err := f.users().GetEmail(f.ctx, other)
	if err != nil {
		t.Fatalf("get other fixture email: %v", err)
	}

	for i := 0; i < loginMaxFails; i++ {
		if _, err := f.users().Authenticate(f.ctx, email, "not-the-password", "127.0.0.1", "test"); err == nil {
			t.Fatal("wrong password was accepted")
		}
	}
	if _, err := f.users().Authenticate(f.ctx, email, fixturePassword, "127.0.0.1", "test"); err == nil {
		t.Fatal("locked account should still be refused")
	}

	// A DIFFERENT account, sharing nothing but the same fixture password and
	// caller IP, must be unaffected.
	if _, err := f.users().Authenticate(f.ctx, otherEmail, fixturePassword, "127.0.0.1", "test"); err != nil {
		t.Fatalf("a different account must not be caught by another account's lockout: %v", err)
	}
}
