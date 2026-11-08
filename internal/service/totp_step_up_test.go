package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"ta-payment-back/internal/testutil"
)

// setTestTOTP installs a new authenticator in a disposable fixture, so tests
// can request independent codes without sleeping for the next 30-second step.
// Replay tests keep the returned code and do not reinstall between attempts.
func setTestTOTP(t *testing.T, s *MFAService, user uuid.UUID) string {
	t.Helper()
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "test", AccountName: "test@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := s.totp.Seal(user[:], []byte(key.Secret()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(context.Background(), `UPDATE users SET totp_secret_enc=$2,
 totp_enabled_at=NOW(), totp_last_step=NULL WHERE id=$1`, user, enc); err != nil {
		t.Fatal(err)
	}
	return genCode(t, key.Secret(), time.Now())
}

func TestTOTPStepUp_RejectsPasswordRecoveryAndReplay(t *testing.T) {
	pool := testutil.NewPool(t)
	s := newTestMFAService(t, pool)
	user := mkMFATestUser(t, pool)
	code := setTestTOTP(t, s, user)
	ctx := context.Background()
	recovery, err := s.RegenerateRecoveryCodes(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "account-password", recovery[0]} {
		if err := s.VerifyStepUp(ctx, user, value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	if err := s.VerifyStepUp(ctx, user, code); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyStepUp(ctx, user, code); err == nil {
		t.Fatal("accepted a replayed code")
	}
	// Rejecting a recovery code must not consume it: it remains usable for login.
	if ok, err := s.LoginVerifyCode(ctx, user, recovery[0]); err != nil || !ok {
		t.Fatalf("recovery: %v %v", ok, err)
	}
}

func TestTOTPStepUp_ConcurrentReplayHasOneWinner(t *testing.T) {
	pool := testutil.NewPool(t)
	s := newTestMFAService(t, pool)
	user := mkMFATestUser(t, pool)
	code := setTestTOTP(t, s, user)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if s.VerifyStepUp(context.Background(), user, code) == nil {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("accepted %d copies of one code", wins.Load())
	}
}

func TestTOTPStepUp_LockoutAndAccountState(t *testing.T) {
	pool := testutil.NewPool(t)
	s := newTestMFAService(t, pool)
	ctx := context.Background()
	user := mkMFATestUser(t, pool)
	if err := s.VerifyStepUp(ctx, user, "123456"); err == nil {
		t.Fatal("accepted unenrolled user")
	}
	code := setTestTOTP(t, s, user)
	for range pwGateMaxFails {
		_ = s.VerifyStepUp(ctx, user, "invalid")
	}
	var ue *UserError
	if err := s.VerifyStepUp(ctx, user, code); !errors.As(err, &ue) || ue.Status != 429 {
		t.Fatalf("lockout: %v", err)
	}
	pwGateClear(user)
	if _, err := pool.Exec(ctx, `UPDATE users SET is_active=FALSE WHERE id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyStepUp(ctx, user, code); err == nil {
		t.Fatal("accepted inactive user")
	}
}

func TestTOTPStepUp_RejectsExpiredCode(t *testing.T) {
	pool := testutil.NewPool(t)
	s := newTestMFAService(t, pool)
	user := mkMFATestUser(t, pool)
	current := setTestTOTP(t, s, user)
	var enc []byte
	if err := pool.QueryRow(context.Background(), `SELECT totp_secret_enc FROM users WHERE id=$1`, user).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	plain, err := s.totp.Open(user[:], enc)
	if err != nil {
		t.Fatal(err)
	}
	expired := genCode(t, string(plain), time.Now().Add(-3*time.Minute))
	if err := s.VerifyStepUp(context.Background(), user, expired); err == nil {
		t.Fatal("accepted expired code")
	}
	if err := s.VerifyStepUp(context.Background(), user, current); err != nil {
		t.Fatal(err)
	}
}

func TestNumericTOTP_RequiresSixASCIIDigits(t *testing.T) {
	for _, code := range []string{"123456", "000001"} {
		if !isNumericTOTP(code) {
			t.Fatalf("rejected %q", code)
		}
	}
	for _, code := range []string{"+12345", "-12345", "12345", "1234567", "１２３４５６", "12 456", "ABCDEF"} {
		if isNumericTOTP(code) {
			t.Fatalf("accepted %q", code)
		}
	}
}
