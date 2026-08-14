package service

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// loginMaxFails/loginLockout back a per-account lockout for POST
// /auth/login, layered under that route's own per-IP limiter (see
// handler.Mount's loginLimiter). The IP limiter alone stops the SYSTEM being
// hammered but not any ONE account being brute-forced — ten attempts a
// minute from ten different IPs each stay under a 10/min-per-IP limit while
// still grinding through a single person's password, which is exactly what
// distributed credential stuffing does to route around it.
//
// A little more forgiving than the re-authentication gate in
// password_gate.go (7 fails vs 5, same 15-minute window): the caller here
// has not yet proven they hold the account, so a slightly higher bar before
// treating it as an attack rather than a mistyped password is appropriate.
//
// This is a SEPARATE copy of that gate's shape rather than a shared
// abstraction on purpose — see pwAttempts's own doc comment in
// password_gate.go for the reasoning that already applies here too
// (single-process, bounded key space, bcrypt already caps the raw guess
// rate). The two gates protect different things — an already-authenticated
// actor re-proving identity, vs. an anonymous caller trying to become one —
// and duplicating ~30 lines costs less than coupling them.
const loginMaxFails = 7
const loginLockout = 15 * time.Minute

type loginAttemptEntry struct {
	mu    sync.Mutex
	fails int
	until time.Time
}

// loginAttempts counts consecutive failed logins per USER ID, never per the
// email string the caller typed. FindByEmail has already resolved a real
// account by the time this is consulted (see UserService.Authenticate), so
// the key space is bounded by the real user count — the same property
// pwAttempts relies on. Keying on the raw email instead would let anyone grow
// this map without limit just by submitting new addresses; the cost of not
// doing that is that a nonexistent account can never show as "locked", which
// costs nothing since there is no real account behind it to protect.
var loginAttempts sync.Map // uuid.UUID -> *loginAttemptEntry

// loginGateCheck refuses an attempt outright while the account is locked out,
// before any bcrypt compare runs.
func loginGateCheck(userID uuid.UUID) error {
	v, ok := loginAttempts.Load(userID)
	if !ok {
		return nil
	}
	e := v.(*loginAttemptEntry)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.until.IsZero() {
		return nil
	}
	if remain := time.Until(e.until); remain > 0 {
		mins := int(remain/time.Minute) + 1
		return &UserError{Status: 429, Msg: fmt.Sprintf(
			"เข้าสู่ระบบผิดพลาดหลายครั้งเกินไป กรุณารออีก %d นาทีแล้วลองใหม่", mins)}
	}
	// Lockout expired — the next window starts clean.
	e.fails = 0
	e.until = time.Time{}
	return nil
}

// loginGateFail records a wrong password and shuts the gate on the Nth one.
func loginGateFail(userID uuid.UUID) {
	v, _ := loginAttempts.LoadOrStore(userID, &loginAttemptEntry{})
	e := v.(*loginAttemptEntry)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fails++
	if e.fails >= loginMaxFails {
		e.until = time.Now().Add(loginLockout)
	}
}

// loginGateSucceed forgets the account's failures. Only CONSECUTIVE misses
// count: someone who mistypes their password twice a week should never
// accumulate their way into a lockout.
func loginGateSucceed(userID uuid.UUID) {
	loginAttempts.Delete(userID)
}
