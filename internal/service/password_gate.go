package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/auth"
)

// pwGateMaxFails is how many consecutive wrong passwords one actor may present
// to this gate before it stops answering at all.
//
// Stricter than the login route's 10/minute (see handler.Mount) on purpose: the
// person here is ALREADY logged in, so they are re-typing a password they know.
// Five misses is generous for a typo and useless for guessing.
const pwGateMaxFails = 5

// pwGateLockout is how long the gate stays shut once the limit is hit. Long
// enough that sustained guessing is pointless, short enough that an officer who
// fumbled their password is not locked out of the download for the afternoon.
//
// An admin can cut the wait short (ClearPasswordGateLockout), but this window is
// still the recovery path that has to work on its own: there is no self-service
// unlock, and at 21:00 on a Sunday there may be no admin to call.
const pwGateLockout = 15 * time.Minute

type pwAttemptEntry struct {
	mu    sync.Mutex
	fails int
	// until is the lockout expiry; zero means "not locked out".
	until time.Time
}

// pwAttempts counts consecutive gate failures per actor.
//
// In-process on purpose, mirroring the zipTokens map in docs.go: this system
// runs as one backend process against one database, so a counter here needs no
// migration and no write on every wrong guess. The cost is that a restart
// forgets every counter — acceptable, because the attacker this defends against
// holds a hijacked session on an office machine and cannot make the server
// restart, and because bcrypt already caps the guess rate at roughly ten per
// second. If this ever runs as more than one process, the counter has to move
// to Postgres or the limit becomes per-replica and means very little.
//
// Keyed by actor rather than IP: the whole premise is a session stolen at one
// desk, so the IP is constant and worthless as a key. The login route's per-IP
// limiter covers the other direction.
//
// Entries are created only on failure and deleted on success, so the map is
// bounded by the number of staff who have recently mistyped — a handful.
var pwAttempts sync.Map // uuid.UUID -> *pwAttemptEntry

// pwGateCheck refuses an attempt outright while the actor is locked out, before
// any query or bcrypt compare runs.
func pwGateCheck(actor uuid.UUID) error {
	v, ok := pwAttempts.Load(actor)
	if !ok {
		return nil
	}
	e := v.(*pwAttemptEntry)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.until.IsZero() {
		return nil
	}
	if remain := time.Until(e.until); remain > 0 {
		// Round up: "รออีก 0 นาที" reads like a bug.
		mins := int(remain/time.Minute) + 1
		return &UserError{Status: 429, Msg: fmt.Sprintf(
			"ยืนยันรหัสผ่านไม่ถูกต้องหลายครั้งเกินไป กรุณารออีก %d นาทีแล้วลองใหม่", mins)}
	}
	// The lockout has expired — the next window starts clean.
	e.fails = 0
	e.until = time.Time{}
	return nil
}

// pwGateFail records a wrong password and shuts the gate on the Nth one.
func pwGateFail(actor uuid.UUID) {
	v, _ := pwAttempts.LoadOrStore(actor, &pwAttemptEntry{})
	e := v.(*pwAttemptEntry)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fails++
	if e.fails >= pwGateMaxFails {
		e.until = time.Now().Add(pwGateLockout)
	}
}

// pwGateSucceed forgets the actor's failures. Only CONSECUTIVE misses count:
// an officer who mistypes twice a week should never accumulate their way into a
// lockout.
func pwGateSucceed(actor uuid.UUID) {
	pwAttempts.Delete(actor)
}

// pwGateLocked reports whether the actor is currently shut out, and until when.
// Read-only: it does not clear an expired lockout, because a caller asking "is
// this person locked?" should not have the side effect of unlocking them.
func pwGateLocked(actor uuid.UUID) (bool, time.Time) {
	v, ok := pwAttempts.Load(actor)
	if !ok {
		return false, time.Time{}
	}
	e := v.(*pwAttemptEntry)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.until.IsZero() || !time.Now().Before(e.until) {
		return false, time.Time{}
	}
	return true, e.until
}

// pwGateClear discards the actor's counter outright — the admin unlock.
func pwGateClear(actor uuid.UUID) {
	pwAttempts.Delete(actor)
}

// ClearPasswordGateLockout is the admin's unlock for an officer who has locked
// themselves out of the document download or the worklog editor. Reports whether
// the target was actually locked, so the admin gets confirmation that the click
// did something rather than a bare ok.
//
// Admin-only, and admin-only in the reversal sense this codebase already uses
// elsewhere: staff move work forward, only admin undoes. See the route in
// handler.Mount.
//
// SELF-UNLOCK IS REFUSED. Without that rule the limit would evaporate for the
// most valuable account in the system: an attacker on a hijacked ADMIN session
// could grind the gate five guesses at a time, unlock themselves, and repeat
// forever. Refusing costs a genuinely locked-out admin nothing they did not
// already have — the 15-minute expiry is still there, and it is the same wait
// every other role had before this endpoint existed.
//
// Deliberately NOT behind VerifyUserPassword itself. Gating the unlock on the
// very gate it unlocks is a trap: an admin locked out at the gate could not
// reach the tool that clears it. The audit row is what makes this accountable
// instead — who unlocked whom, and when.
func (s *UserService) ClearPasswordGateLockout(ctx context.Context, actor, target uuid.UUID) (bool, error) {
	if actor == target {
		return false, Forbidden("ปลดล็อกบัญชีของตนเองไม่ได้ กรุณาให้ผู้ดูแลระบบท่านอื่นดำเนินการ หรือรอจนครบกำหนด")
	}
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT TRUE FROM users WHERE id = $1 AND deleted_at IS NULL`, target).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}

	wasLocked, until := pwGateLocked(target)

	// Audit BEFORE clearing. The lockout lives in memory and the audit row lives
	// in Postgres, so the two cannot commit together; given that, the safe order
	// is the one where a failed write leaves the account still locked. An unlock
	// with no record of it is the outcome worth preventing.
	entry := audit.Entry{
		ActorID: &actor, Action: "user.password_gate_unlock", Entity: "user",
		EntityID: target.String(),
		After:    map[string]any{"was_locked": wasLocked},
	}
	if wasLocked {
		entry.After.(map[string]any)["locked_until"] = until.Format(time.RFC3339)
	}
	if err := s.aud.Log(ctx, entry); err != nil {
		return false, err
	}

	pwGateClear(target)
	return wasLocked, nil
}

// VerifyUserPassword re-authenticates the person already holding a session,
// immediately before an act that a stolen session must not be able to perform.
//
// Two callers today: releasing a document bundle full of national IDs, and
// rewriting a TA's approved hours. Both are cases where "you are logged in" is
// not enough — the first hands over PII, the second moves money and overrides a
// lecturer's approval.
//
// Extracted from DocsService on 31/07/2026 when the worklog editor needed the
// same gate. A second copy of a password check is exactly the kind of thing that
// drifts: one side gets the is_active test and the other quietly does not.
//
// Attempt-limited per actor since 07/08/2026. Neither caller sits behind the
// login route's rate limiter, so before that this gate was an unmetered oracle:
// a hijacked session on an unattended machine could grind the officer's password
// here without ever touching /auth/login. Defence in depth — bcrypt already
// makes each guess slow — but an unlimited oracle should not exist at all.
//
// Timing is dominated by bcrypt, so no dummy compare is needed; an account
// deactivated mid-session simply has no row to match.
func VerifyUserPassword(ctx context.Context, pool *pgxpool.Pool, actor uuid.UUID, password string) error {
	if err := pwGateCheck(actor); err != nil {
		return err
	}
	if password == "" {
		// Not counted as a guess: an empty field is a client that did not send
		// the password, not an attempt at one, and counting it would let a UI
		// slip lock an officer out.
		return &UserError{Status: 401, Msg: "ต้องกรอกรหัสผ่านเพื่อยืนยันตัวตน"}
	}
	var hash string
	err := pool.QueryRow(ctx,
		`SELECT password_hash FROM users WHERE id = $1 AND is_active AND deleted_at IS NULL`,
		actor,
	).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		// Also not counted: there is no password to find here, so no amount of
		// trying reveals one.
		return &UserError{Status: 401, Msg: "บัญชีนี้ไม่สามารถใช้งานได้"}
	}
	if err != nil {
		return err
	}
	if !auth.CheckPassword(hash, password) {
		pwGateFail(actor)
		return &UserError{Status: 401, Msg: "รหัสผ่านไม่ถูกต้อง"}
	}
	pwGateSucceed(actor)
	return nil
}
