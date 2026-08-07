package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// VerifyUserPassword is the re-authentication in front of the document-bundle
// download and the staff worklog editor. Neither sits behind the login route's
// per-IP limiter, so without a limit of its own the gate is a password oracle
// that a hijacked session can grind at bcrypt speed.
//
// These tests pin the limit. The first is the one that matters: past the
// threshold the CORRECT password must be refused too, because a limiter that
// still opens for the right answer has not stopped the search — it has only
// slowed it, and the attacker's last guess is the one that counts.

// pwGateReset drops an actor's counter. Package-level state outlives a single
// test; fixtures use a fresh uuid each time, but a test that names its own actor
// must not leak into the next one.
func pwGateReset(actor uuid.UUID) { pwAttempts.Delete(actor) }

func TestPasswordGate_LocksOutAfterRepeatedWrongPasswords(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	t.Cleanup(func() { pwGateReset(f.StaffID) })

	// Sanity: the correct password works before anything has gone wrong. If this
	// fails the rest proves nothing.
	if err := VerifyUserPassword(f.ctx, f.Pool, f.StaffID, fixturePassword); err != nil {
		t.Fatalf("correct password must be accepted on a clean counter: %v", err)
	}

	for i := 1; i <= pwGateMaxFails; i++ {
		err := VerifyUserPassword(f.ctx, f.Pool, f.StaffID, "not-the-password")
		if err == nil {
			t.Fatalf("wrong password #%d was accepted", i)
		}
		if ue, ok := err.(*UserError); !ok || ue.Status != 401 {
			t.Fatalf("wrong password #%d: want a 401 UserError, got %#v", i, err)
		}
	}

	// The point of the whole change: the CORRECT password is now refused.
	err := VerifyUserPassword(f.ctx, f.Pool, f.StaffID, fixturePassword)
	if err == nil {
		t.Fatalf("after %d wrong passwords the gate still opened for the correct one — "+
			"an attacker grinding this gate would have gotten in on the guess after next",
			pwGateMaxFails)
	}
	ue, ok := err.(*UserError)
	if !ok || ue.Status != 429 {
		t.Fatalf("lockout must surface as 429 so the UI can say 'wait', got %#v", err)
	}
}

func TestPasswordGate_ForgetsFailuresAfterASuccess(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	t.Cleanup(func() { pwGateReset(f.StaffID) })

	// Only CONSECUTIVE misses count. An officer who mistypes once now and again
	// must never accumulate their way into a lockout.
	for round := 0; round < 3; round++ {
		for i := 0; i < pwGateMaxFails-1; i++ {
			if err := VerifyUserPassword(f.ctx, f.Pool, f.StaffID, "not-the-password"); err == nil {
				t.Fatal("wrong password was accepted")
			}
		}
		if err := VerifyUserPassword(f.ctx, f.Pool, f.StaffID, fixturePassword); err != nil {
			t.Fatalf("round %d: %d misses is under the limit, the correct password must still work: %v",
				round, pwGateMaxFails-1, err)
		}
	}
}

func TestPasswordGate_ReopensWhenTheLockoutExpires(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	t.Cleanup(func() { pwGateReset(f.StaffID) })

	for i := 0; i < pwGateMaxFails; i++ {
		if err := VerifyUserPassword(f.ctx, f.Pool, f.StaffID, "not-the-password"); err == nil {
			t.Fatal("wrong password was accepted")
		}
	}
	if err := VerifyUserPassword(f.ctx, f.Pool, f.StaffID, fixturePassword); err == nil {
		t.Fatal("expected the gate to be locked")
	}

	// Wind the expiry back rather than sleeping 15 minutes. There is no injected
	// clock in this package, and a test that sleeps for the real window is a test
	// nobody runs.
	v, ok := pwAttempts.Load(f.StaffID)
	if !ok {
		t.Fatal("no attempt entry recorded")
	}
	e := v.(*pwAttemptEntry)
	e.mu.Lock()
	e.until = time.Now().Add(-time.Second)
	e.mu.Unlock()

	if err := VerifyUserPassword(f.ctx, f.Pool, f.StaffID, fixturePassword); err != nil {
		t.Fatalf("the lockout must expire on its own — the admin unlock is a shortcut, "+
			"not the only way out, and out-of-hours there may be no admin to call: %v", err)
	}
}

func TestPasswordGate_LockoutIsPerActor(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	other := f.insertStaffWithPassword()
	t.Cleanup(func() { pwGateReset(f.StaffID); pwGateReset(other) })

	for i := 0; i < pwGateMaxFails; i++ {
		if err := VerifyUserPassword(f.ctx, f.Pool, f.StaffID, "not-the-password"); err == nil {
			t.Fatal("wrong password was accepted")
		}
	}
	if err := VerifyUserPassword(f.ctx, f.Pool, f.StaffID, fixturePassword); err == nil {
		t.Fatal("expected the first officer to be locked out")
	}

	// One officer under attack must not shut the download for the desk next door.
	if err := VerifyUserPassword(f.ctx, f.Pool, other, fixturePassword); err != nil {
		t.Fatalf("a second officer must be unaffected by the first one's lockout: %v", err)
	}
}

// --- the admin unlock ---
//
// The lockout expires on its own after pwGateLockout, so this endpoint is a
// shortcut, not the only way out. What it must not become is a way for the
// locked-out person to let themselves back in.

func (f *fixture) users() *UserService {
	return &UserService{pool: f.Pool, aud: audit.New(f.Pool)}
}

// lockOut burns through the limit so the actor is shut out.
func (f *fixture) lockOut(actor uuid.UUID) {
	f.t.Helper()
	for i := 0; i < pwGateMaxFails; i++ {
		if err := VerifyUserPassword(f.ctx, f.Pool, actor, "not-the-password"); err == nil {
			f.t.Fatal("wrong password was accepted")
		}
	}
	if err := VerifyUserPassword(f.ctx, f.Pool, actor, fixturePassword); err == nil {
		f.t.Fatal("setup: expected the actor to be locked out")
	}
}

func TestPasswordGateUnlock_AdminRestoresAccess(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	admin := f.insertUser("admin", "admin")
	t.Cleanup(func() { pwGateReset(f.StaffID) })

	f.lockOut(f.StaffID)

	wasLocked, err := f.users().ClearPasswordGateLockout(f.ctx, admin, f.StaffID)
	if err != nil {
		t.Fatalf("admin unlock failed: %v", err)
	}
	if !wasLocked {
		t.Fatal("was_locked must report true — the admin needs to know the click did something")
	}

	// The whole point: the officer can work again without waiting out the window.
	if err := VerifyUserPassword(f.ctx, f.Pool, f.StaffID, fixturePassword); err != nil {
		t.Fatalf("after an admin unlock the correct password must work: %v", err)
	}
}

func TestPasswordGateUnlock_ResetsTheCounterNotJustTheLockout(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	admin := f.insertUser("admin", "admin")
	t.Cleanup(func() { pwGateReset(f.StaffID) })

	f.lockOut(f.StaffID)
	if _, err := f.users().ClearPasswordGateLockout(f.ctx, admin, f.StaffID); err != nil {
		t.Fatalf("admin unlock failed: %v", err)
	}

	// An unlock that cleared `until` but left fails at 5 would re-lock on the
	// very next typo, and the officer would phone the admin again.
	if err := VerifyUserPassword(f.ctx, f.Pool, f.StaffID, "not-the-password"); err == nil {
		t.Fatal("wrong password was accepted")
	}
	if err := VerifyUserPassword(f.ctx, f.Pool, f.StaffID, fixturePassword); err != nil {
		t.Fatalf("one typo after an unlock must not re-lock the account: %v", err)
	}
}

func TestPasswordGateUnlock_RefusesSelfUnlock(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	admin := f.insertStaffWithPassword()
	f.exec(`INSERT INTO user_roles (user_id, role) VALUES ($1, 'admin'::role_code)`, admin)
	t.Cleanup(func() { pwGateReset(admin) })

	f.lockOut(admin)

	// An attacker on a hijacked admin session must not be able to grind the gate
	// five guesses at a time, unlock themselves, and go again.
	_, err := f.users().ClearPasswordGateLockout(f.ctx, admin, admin)
	if err == nil {
		t.Fatal("an admin unlocking themselves defeats the limit entirely for the " +
			"one account worth stealing — it must be refused")
	}
	ue, ok := err.(*UserError)
	if !ok || ue.Status != 403 {
		t.Fatalf("self-unlock must be a 403, got %#v", err)
	}
	if err := VerifyUserPassword(f.ctx, f.Pool, admin, fixturePassword); err == nil {
		t.Fatal("the refused self-unlock must leave the lockout in place")
	}
}

func TestPasswordGateUnlock_ReportsNotLockedAndUnknownUser(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	admin := f.insertUser("admin", "admin")

	// Unlocking someone who was never locked is a no-op, not an error — but it
	// must say so, or the admin cannot tell a fixed problem from a wrong guess
	// about who was stuck.
	wasLocked, err := f.users().ClearPasswordGateLockout(f.ctx, admin, f.StaffID)
	if err != nil {
		t.Fatalf("unlocking an unlocked user must not error: %v", err)
	}
	if wasLocked {
		t.Fatal("was_locked must be false for a user who was not locked out")
	}

	if _, err := f.users().ClearPasswordGateLockout(f.ctx, admin, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unlocking a user who does not exist must be ErrNotFound, got %v", err)
	}
}

func TestPasswordGateUnlock_IsAudited(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	admin := f.insertUser("admin", "admin")
	t.Cleanup(func() { pwGateReset(f.StaffID) })

	f.lockOut(f.StaffID)
	if _, err := f.users().ClearPasswordGateLockout(f.ctx, admin, f.StaffID); err != nil {
		t.Fatalf("admin unlock failed: %v", err)
	}

	// The unlock is not password-gated, so the audit row is the ONLY thing making
	// it accountable. If it is missing, the control has no witness.
	var n int
	var after string
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT COUNT(*), COALESCE(MAX(after::text), '')
		FROM audit_logs
		WHERE action = 'user.password_gate_unlock' AND actor_id = $1 AND entity_id = $2`,
		admin, f.StaffID.String()).Scan(&n, &after); err != nil {
		t.Fatalf("query audit_logs: %v", err)
	}
	if n != 1 {
		t.Fatalf("want exactly 1 audit row for the unlock, got %d", n)
	}
	if !strings.Contains(squash(after), `"was_locked":true`) {
		t.Fatalf("the audit row must record that the target really was locked, got %s", after)
	}
}
