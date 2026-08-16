package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/audit"
)

// mfaChallengeTTL/mfaChallengeMaxAttempts bound the "you got the password
// right, now prove the second factor" ticket issued by step 1 of login.
const (
	mfaChallengeTTL          = 5 * time.Minute
	mfaChallengeMaxAttempts  = 5
	mfaChallengeTokenSizeRaw = 32 // bytes of randomness before base64 encoding
)

// ErrMFAChallengeInvalid covers every reason a challenge redemption can fail
// short of an actual system error: unknown token, expired, already consumed,
// attempts exhausted, or a wrong code. These are DELIBERATELY collapsed into
// one sentinel and one Thai message at the handler layer — see LoginVerifyCode's
// doc comment on why a wrong code, an expired challenge and a consumed
// challenge must be indistinguishable to the caller, or the difference itself
// becomes a probe an attacker can use to learn which case they hit.
var ErrMFAChallengeInvalid = errors.New("mfa: challenge invalid or code incorrect")

// IssueChallenge creates a login challenge for userID (step 1 of login has
// already verified the password by the time this is called) and returns the
// opaque token to hand to the client. Only its SHA-256 is stored — the
// plaintext token exists nowhere but the client's copy and this return value,
// same posture as a session cookie.
func (s *MFAService) IssueChallenge(ctx context.Context, userID uuid.UUID) (string, error) {
	raw := make([]byte, mfaChallengeTokenSizeRaw)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO mfa_challenges (user_id, token_hash, expires_at)
		VALUES ($1, $2, NOW() + make_interval(secs => $3))`,
		userID, hash[:], mfaChallengeTTL.Seconds()); err != nil {
		return "", err
	}
	return token, nil
}

// RedeemChallenge verifies code against the challenge tokenPlain identifies,
// and on success returns the userID it was issued for. This is POST
// /auth/login/2fa's whole implementation; the handler only has to turn the
// return values into an HTTP response and, on success, do exactly what a
// password-only login already does (CreateAndSupersede + Issue + cookie).
//
// The attempt cap and the code check happen in that order, both against the
// per-user mfa gate (mfa_gate.go), NOT the per-challenge attempts column
// alone: a caller who exhausts one challenge's 5 attempts and requests a
// fresh one from POST /auth/login does NOT get a fresh mfa-gate budget — see
// mfa_gate.go's own doc comment for why that matters against an attacker who
// already holds the password.
func (s *MFAService) RedeemChallenge(ctx context.Context, tokenPlain, code, ip, userAgent string) (uuid.UUID, error) {
	hash := sha256.Sum256([]byte(tokenPlain))

	// Atomically: find the challenge (live, unconsumed, under its attempt
	// cap) AND bump its attempt counter, in one statement. This is what
	// makes the cap race-safe — two concurrent redemptions of the same
	// challenge cannot both read attempts=4 and both proceed past a
	// check-then-write cap.
	var userID uuid.UUID
	err := s.pool.QueryRow(ctx, `
		UPDATE mfa_challenges
		SET attempts = attempts + 1
		WHERE token_hash = $1 AND attempts < $2
		  AND consumed_at IS NULL AND expires_at > NOW()
		RETURNING user_id`,
		hash[:], mfaChallengeMaxAttempts).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrMFAChallengeInvalid
	}
	if err != nil {
		return uuid.Nil, err
	}

	if gateErr := mfaGateCheck(userID); gateErr != nil {
		_ = s.aud.Log(ctx, audit.Entry{
			ActorID: &userID, Action: "auth.2fa_locked", Entity: "user",
			EntityID: userID.String(), IP: ip, UserAgent: userAgent,
		})
		return uuid.Nil, gateErr
	}

	valid, err := s.LoginVerifyCode(ctx, userID, code)
	if err != nil {
		return uuid.Nil, err
	}
	if !valid {
		mfaGateFail(userID)
		_ = s.aud.Log(ctx, audit.Entry{
			ActorID: &userID, Action: "auth.2fa_failed", Entity: "user",
			EntityID: userID.String(), IP: ip, UserAgent: userAgent,
		})
		return uuid.Nil, ErrMFAChallengeInvalid
	}

	if _, err := s.pool.Exec(ctx,
		`UPDATE mfa_challenges SET consumed_at = NOW() WHERE token_hash = $1`,
		hash[:]); err != nil {
		return uuid.Nil, err
	}
	mfaGateSucceed(userID)
	_ = s.aud.Log(ctx, audit.Entry{
		ActorID: &userID, Action: "auth.2fa_ok", Entity: "user",
		EntityID: userID.String(), IP: ip, UserAgent: userAgent,
	})
	return userID, nil
}

// CleanupChallenges deletes challenge rows a day past their expiry — the TTL
// itself is only 5 minutes, so a day of slack is already generous for a
// "why did my login get rejected" support question to still find the row,
// same reasoning as SessionService.Cleanup's own week of slack. Called from
// the scheduler's daily sweep.
func (s *MFAService) CleanupChallenges(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM mfa_challenges WHERE expires_at < NOW() - INTERVAL '1 day'`)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
