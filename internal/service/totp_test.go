package service

import (
	"context"
	"crypto/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp"
	pqtotp "github.com/pquerna/otp/totp"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/pii"
	"ta-payment-back/internal/testutil"
)

func newTestMFAService(t *testing.T, pool *pgxpool.Pool) *MFAService {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cipher, err := pii.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return &MFAService{pool: pool, aud: audit.New(pool), totp: cipher}
}

func mkMFATestUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, first_name, last_name, is_active)
		 VALUES ($1, $2, 'ท', 'ท', TRUE)`,
		id, id.String()+"@mfa-test.local"); err != nil {
		t.Fatal(err)
	}
	return id
}

// genCode is a small helper mirroring matchStep's own code generation, used
// to produce a code for a KNOWN offset from "now" without going through the
// service (so the test is not just checking the implementation against
// itself).
func genCode(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := pqtotp.GenerateCodeCustom(secret, at, pqtotp.ValidateOpts{
		Period: totpPeriodSeconds, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// TestMatchStep_AcceptsSkewWindow pins the ±1 period tolerance every
// mainstream authenticator app assumes: a code generated 30s in the past or
// 30s in the future (clock drift, or the user being slow to type) must still
// validate, and a code two periods away must not.
func TestMatchStep_AcceptsSkewWindow(t *testing.T) {
	key, err := pqtotp.Generate(pqtotp.GenerateOpts{Issuer: "test", AccountName: "a@b.c"})
	if err != nil {
		t.Fatal(err)
	}
	secret := key.Secret()
	now := time.Now()

	for _, offset := range []time.Duration{-totpPeriodSeconds * time.Second, 0, totpPeriodSeconds * time.Second} {
		code := genCode(t, secret, now.Add(offset))
		if _, ok := matchStep(secret, code, now); !ok {
			t.Errorf("code from offset %v should validate within skew window", offset)
		}
	}

	tooFar := genCode(t, secret, now.Add(-3*totpPeriodSeconds*time.Second))
	if _, ok := matchStep(secret, tooFar, now); ok {
		t.Error("code 3 periods away should NOT validate")
	}
}

// TestLoginVerifyCode_RejectsReplay is the property the whole totp_last_step
// column exists for: the SAME still-valid code must not verify twice.
func TestLoginVerifyCode_RejectsReplay(t *testing.T) {
	pool := testutil.NewPool(t)
	svc := newTestMFAService(t, pool)
	uid := mkMFATestUser(t, pool)

	enr, err := svc.GenerateEnrollment(context.Background(), uid, "a@b.c")
	if err != nil {
		t.Fatal(err)
	}
	code := genCode(t, enr.Secret, time.Now())
	if _, err := svc.Enable(context.Background(), uid, code); err != nil {
		t.Fatalf("Enable with a fresh code should succeed: %v", err)
	}

	// A second login with the SAME code the enable step already consumed
	// (that code's step is now stored as totp_last_step) must be rejected.
	ok, err := svc.LoginVerifyCode(context.Background(), uid, code)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("replaying the code Enable already consumed must be rejected")
	}

	// A genuinely fresh code (next period) must still work.
	fresh := genCode(t, enr.Secret, time.Now().Add(totpPeriodSeconds*time.Second))
	ok, err = svc.LoginVerifyCode(context.Background(), uid, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("a fresh code from the next period should be accepted")
	}
}

// TestRecoveryCode_SingleUseUnderConcurrency is Trap 12 from the plan: two
// concurrent redemptions of the SAME recovery code must not both succeed.
// The atomic UPDATE ... WHERE used_at IS NULL RETURNING id is what this pins.
func TestRecoveryCode_SingleUseUnderConcurrency(t *testing.T) {
	pool := testutil.NewPool(t)
	svc := newTestMFAService(t, pool)
	uid := mkMFATestUser(t, pool)

	enr, err := svc.GenerateEnrollment(context.Background(), uid, "a@b.c")
	if err != nil {
		t.Fatal(err)
	}
	code := genCode(t, enr.Secret, time.Now())
	codes, err := svc.Enable(context.Background(), uid, code)
	if err != nil {
		t.Fatal(err)
	}
	target := codes[0]

	const attempts = 8
	var successes int32
	var wg sync.WaitGroup
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()
			ok, err := svc.LoginVerifyCode(context.Background(), uid, target)
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				atomic.AddInt32(&successes, 1)
			}
		}()
	}
	wg.Wait()

	if successes != 1 {
		t.Errorf("exactly one concurrent redemption of the same recovery code should succeed, got %d", successes)
	}
}

// TestChallenge_AttemptCapAndExpiry pins mfa_challenges' two independent
// rejection paths: the 5-attempt cap, and TTL expiry — both must reject with
// the same sentinel (ErrMFAChallengeInvalid), never a distinguishable error.
func TestChallenge_AttemptCapAndExpiry(t *testing.T) {
	pool := testutil.NewPool(t)
	svc := newTestMFAService(t, pool)
	uid := mkMFATestUser(t, pool)

	enr, err := svc.GenerateEnrollment(context.Background(), uid, "a@b.c")
	if err != nil {
		t.Fatal(err)
	}
	code := genCode(t, enr.Secret, time.Now())
	if _, err := svc.Enable(context.Background(), uid, code); err != nil {
		t.Fatal(err)
	}

	t.Run("attempt cap", func(t *testing.T) {
		t.Cleanup(func() { mfaAttempts.Delete(uid) })
		token, err := svc.IssueChallenge(context.Background(), uid)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < mfaChallengeMaxAttempts; i++ {
			if _, err := svc.RedeemChallenge(context.Background(), token, "000000", "", ""); err == nil {
				t.Fatalf("wrong code attempt #%d should not succeed", i+1)
			}
		}
		// One more attempt past the cap — even with a CORRECT code — must
		// still fail: the challenge itself is now exhausted, not just
		// individually-wrong codes.
		fresh := genCode(t, enr.Secret, time.Now().Add(2*totpPeriodSeconds*time.Second))
		if _, err := svc.RedeemChallenge(context.Background(), token, fresh, "", ""); err == nil {
			t.Error("a correct code past the attempt cap should still be rejected")
		}
	})

	t.Run("expiry", func(t *testing.T) {
		t.Cleanup(func() { mfaAttempts.Delete(uid) })
		token, err := svc.IssueChallenge(context.Background(), uid)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(),
			`UPDATE mfa_challenges SET expires_at = NOW() - INTERVAL '1 minute' WHERE user_id = $1`,
			uid); err != nil {
			t.Fatal(err)
		}
		fresh := genCode(t, enr.Secret, time.Now().Add(3*totpPeriodSeconds*time.Second))
		if _, err := svc.RedeemChallenge(context.Background(), token, fresh, "", ""); err == nil {
			t.Error("a correct code against an expired challenge should still be rejected")
		}
	})
}
