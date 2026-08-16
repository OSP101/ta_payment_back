package service

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// mfaMaxFails/mfaLockout back a per-account lockout for POST
// /auth/login/2fa, the second step of login.
//
// This is a THIRD, separate copy of the login_gate.go/password_gate.go
// shape — deliberately not reused, and NOT the same map as loginAttempts.
// The reason is more than the "single-process, bounded key space" argument
// those two already give each other: loginGateSucceed is called the instant
// bcrypt passes (UserService.Authenticate), BEFORE a 2FA challenge is even
// created. If a TOTP-guessing loop shared that same counter, every fresh
// POST /auth/login with a correct (attacker-held) password would reset it to
// zero, and an attacker who already has the password would face no
// meaningful account-level limit on guessing the second factor — only the
// per-challenge attempt cap in mfa_challenges, which they can trivially
// reset by requesting a new challenge. Keeping this gate on its own map,
// touched only by the /2fa step, means a leaked password alone still buys an
// attacker nothing once this gate closes.
const mfaMaxFails = 7
const mfaLockout = 15 * time.Minute

type mfaAttemptEntry struct {
	mu    sync.Mutex
	fails int
	until time.Time
}

// mfaAttempts counts consecutive failed second-factor attempts per USER ID —
// same keying rationale as loginAttempts: the caller has already proven they
// hold the password by the time this gate is consulted, so the key space is
// bounded by the real user count.
var mfaAttempts sync.Map // uuid.UUID -> *mfaAttemptEntry

// mfaGateCheck refuses a 2FA attempt outright while the account is locked
// out, before the submitted code is even compared.
func mfaGateCheck(userID uuid.UUID) error {
	v, ok := mfaAttempts.Load(userID)
	if !ok {
		return nil
	}
	e := v.(*mfaAttemptEntry)
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
	e.fails = 0
	e.until = time.Time{}
	return nil
}

// mfaGateFail records a wrong TOTP/recovery code and shuts the gate on the
// Nth one. Requesting a fresh login challenge does NOT reset this counter —
// only mfaGateSucceed does, on an actually-correct second factor.
func mfaGateFail(userID uuid.UUID) {
	v, _ := mfaAttempts.LoadOrStore(userID, &mfaAttemptEntry{})
	e := v.(*mfaAttemptEntry)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fails++
	if e.fails >= mfaMaxFails {
		e.until = time.Now().Add(mfaLockout)
	}
}

// mfaGateSucceed forgets the account's second-factor failures.
func mfaGateSucceed(userID uuid.UUID) {
	mfaAttempts.Delete(userID)
}
