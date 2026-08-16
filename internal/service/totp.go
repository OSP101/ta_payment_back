// totp.go implements TOTP-based two-factor authentication: enrolment
// (generate → verify → promote), login verification, recovery codes, and
// admin reset. See migrations/0091_two_factor_auth.up.sql for the storage
// shape and internal/pii for how the secret is encrypted at rest.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/pii"
)

// totpKeyVersion is bumped whenever TOTP_ENC_KEY is rotated to a new value,
// so old rows keep recording which key encrypted them — same convention as
// citizenIDKeyVersion.
const totpKeyVersion = 1

// Step parameters match totp.Validate's own Google-Authenticator-compatible
// defaults (30s period, ±1 period skew, 6 digits, SHA1) — every mainstream
// authenticator app assumes these.
const (
	totpPeriodSeconds = 30
	totpSkewSteps     = 1
)

// recoveryCodeCount is how many one-time codes are issued on enable/regenerate.
const recoveryCodeCount = 10

var (
	// ErrMFAAlreadyEnabled guards against overwriting a live secret — see
	// GenerateEnrollment's doc comment for why this matters (a stray repeat
	// POST to /me/2fa/setup must never invalidate a working authenticator).
	ErrMFAAlreadyEnabled = errors.New("mfa: already enabled")
	// ErrMFANotPending means Enable was called with no GenerateEnrollment
	// result to confirm.
	ErrMFANotPending = errors.New("mfa: no pending enrollment")
	// ErrMFANotEnabled means the account has no live secret to verify/disable.
	ErrMFANotEnabled = errors.New("mfa: not enabled")
	// ErrMFAInvalidCode is returned for a wrong TOTP code (enrolment verify
	// only — the login path never returns this: see LoginVerifyCode's own
	// doc comment on why a wrong code there returns bool, not an error).
	ErrMFAInvalidCode = errors.New("mfa: invalid code")
)

// MFAService owns TOTP enrolment/verification and recovery codes.
type MFAService struct {
	pool *pgxpool.Pool
	aud  *audit.Auditor
	// totp is a SEPARATE Cipher from DocsService's pii field — see
	// config.Config.TOTPEncKey's doc comment for why this must never share a
	// key with PII_ENC_KEY or TA_DOCS_ENC_KEY.
	totp *pii.Cipher
}

// Enrollment is what GenerateEnrollment hands back for the setup screen to
// render. Secret is included only so the page can offer "can't scan? type
// this instead" — the same secret is embedded in the QR/otpauth URL.
type Enrollment struct {
	Secret     string
	OTPAuthURL string
}

// GenerateEnrollment creates a new pending TOTP secret for userID and stores
// it encrypted in totp_pending_secret_enc — NOT in totp_secret_enc, which
// stays untouched. This two-column split exists because a user can reach
// /me/2fa/setup more than once (refreshing the page, opening it in a second
// tab, retrying after a typo): if setup wrote straight into the LIVE secret
// column, the second call would silently invalidate whatever the user's
// authenticator app already has enrolled from the first — a real,
// one-stray-POST self-lockout, not a hypothetical one. Nothing about the
// user's actual 2FA status changes until Enable confirms this pending value.
//
// Refuses outright when 2FA is already enabled: re-enrolling over a live
// secret is what Disable-then-GenerateEnrollment is for, not this.
func (s *MFAService) GenerateEnrollment(ctx context.Context, userID uuid.UUID, accountName string) (*Enrollment, error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      "TA Payment (KKU)",
		AccountName: accountName,
		Period:      totpPeriodSeconds,
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1,
	})
	if err != nil {
		return nil, err
	}
	secret := key.Secret()
	sealed, err := s.totp.Seal(userID[:], []byte(secret))
	if err != nil {
		return nil, err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE users
		SET totp_pending_secret_enc = $2, totp_key_version = $3
		WHERE id = $1 AND totp_enabled_at IS NULL`,
		userID, sealed, totpKeyVersion)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrMFAAlreadyEnabled
	}
	return &Enrollment{Secret: secret, OTPAuthURL: key.URL()}, nil
}

// Enable confirms a pending enrollment: the caller must present a code that
// validates against the PENDING secret (proving they actually scanned it),
// at which point it is promoted to the live secret, totp_enabled_at is set,
// and a fresh set of recovery codes is issued (replacing any from a previous
// enable/regenerate — see the DELETE below). Returns the plaintext codes;
// this is the only time they are ever visible.
func (s *MFAService) Enable(ctx context.Context, userID uuid.UUID, code string) ([]string, error) {
	var pendingEnc []byte
	var enabledAt *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT totp_pending_secret_enc, totp_enabled_at FROM users WHERE id = $1`,
		userID).Scan(&pendingEnc, &enabledAt); err != nil {
		return nil, err
	}
	if enabledAt != nil {
		return nil, ErrMFAAlreadyEnabled
	}
	if len(pendingEnc) == 0 {
		return nil, ErrMFANotPending
	}
	plain, err := s.totp.Open(userID[:], pendingEnc)
	if err != nil {
		return nil, err
	}
	step, ok := matchStep(string(plain), code, time.Now())
	if !ok {
		return nil, ErrMFAInvalidCode
	}

	codes, hashes, err := generateRecoveryCodes()
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		UPDATE users
		SET totp_secret_enc = totp_pending_secret_enc,
		    totp_pending_secret_enc = NULL,
		    totp_enabled_at = NOW(),
		    totp_last_step = $2
		WHERE id = $1`, userID, step); err != nil {
		return nil, err
	}
	// Regenerating on every Enable, not just the first one, so re-enrolling
	// after a Disable can't leave stale codes from a prior enrollment alive.
	if _, err := tx.Exec(ctx, `DELETE FROM mfa_recovery_codes WHERE user_id = $1`, userID); err != nil {
		return nil, err
	}
	for _, h := range hashes {
		if _, err := tx.Exec(ctx,
			`INSERT INTO mfa_recovery_codes (user_id, code_hash) VALUES ($1, $2)`,
			userID, h); err != nil {
			return nil, err
		}
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{
		ActorID: &userID, Action: "user.2fa_enabled", Entity: "user", EntityID: userID.String(),
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return codes, nil
}

// Disable turns 2FA off and discards recovery codes. The caller (handler
// layer) is responsible for re-authenticating the actor first via
// VerifyUserPassword (password_gate.go) before calling this — Disable itself
// only verifies the second factor, not the password, matching the "prove
// both factors to remove either" posture other account-security actions use.
func (s *MFAService) Disable(ctx context.Context, actor uuid.UUID, code string) error {
	var secretEnc []byte
	var enabledAt *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT totp_secret_enc, totp_enabled_at FROM users WHERE id = $1`,
		actor).Scan(&secretEnc, &enabledAt); err != nil {
		return err
	}
	if enabledAt == nil {
		return ErrMFANotEnabled
	}
	ok, err := s.verifyLiveCode(ctx, actor, secretEnc, code)
	if err != nil {
		return err
	}
	if !ok {
		return ErrMFAInvalidCode
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE users
		SET totp_secret_enc = NULL, totp_pending_secret_enc = NULL,
		    totp_enabled_at = NULL, totp_last_step = NULL, totp_key_version = NULL
		WHERE id = $1`, actor); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM mfa_recovery_codes WHERE user_id = $1`, actor); err != nil {
		return err
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{
		ActorID: &actor, Action: "user.2fa_disabled", Entity: "user", EntityID: actor.String(),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AdminReset clears actorID's target's 2FA entirely — the account-recovery
// path for a lost device, used when the target cannot produce a code
// themselves. Callers MUST restrict this to admins and re-verify the ADMIN's
// own password first (VerifyUserPassword) — see internal/handler/mfa.go and
// the plan's note on why this cannot be adminOrStaff: staff already hold
// unrestricted password reset, and adding unrestricted 2FA reset on top would
// turn any staff account into a one-click path to full admin takeover.
func (s *MFAService) AdminReset(ctx context.Context, actorID, targetID uuid.UUID) error {
	if actorID == targetID {
		// Same reasoning as ClearPasswordGateLockout's own self-unlock
		// refusal: an admin who still has account access must remove their
		// OWN 2FA the harder way — POST /me/2fa/disable, which requires a
		// valid code, not just a password — or this endpoint would be a
		// strictly weaker path to the same result for exactly the people it
		// is least appropriate for.
		return Forbidden("รีเซ็ต 2FA ของตนเองไม่ได้ ใช้เมนู \"ปิดใช้งาน 2FA\" ในบัญชีของตนเองแทน")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE users
		SET totp_secret_enc = NULL, totp_pending_secret_enc = NULL,
		    totp_enabled_at = NULL, totp_last_step = NULL, totp_key_version = NULL
		WHERE id = $1`, targetID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM mfa_recovery_codes WHERE user_id = $1`, targetID); err != nil {
		return err
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{
		ActorID: &actorID, Action: "user.2fa_reset", Entity: "user", EntityID: targetID.String(),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RegenerateRecoveryCodes replaces every existing code with a fresh set of
// recoveryCodeCount, atomically (old codes are dead the instant this
// commits, never partially valid). Caller re-authenticates first, same as
// Disable.
func (s *MFAService) RegenerateRecoveryCodes(ctx context.Context, userID uuid.UUID) ([]string, error) {
	var enabledAt *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT totp_enabled_at FROM users WHERE id = $1`, userID).Scan(&enabledAt); err != nil {
		return nil, err
	}
	if enabledAt == nil {
		return nil, ErrMFANotEnabled
	}
	codes, hashes, err := generateRecoveryCodes()
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM mfa_recovery_codes WHERE user_id = $1`, userID); err != nil {
		return nil, err
	}
	for _, h := range hashes {
		if _, err := tx.Exec(ctx,
			`INSERT INTO mfa_recovery_codes (user_id, code_hash) VALUES ($1, $2)`,
			userID, h); err != nil {
			return nil, err
		}
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{
		ActorID: &userID, Action: "user.2fa_recovery_regenerated", Entity: "user", EntityID: userID.String(),
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return codes, nil
}

// LoginVerifyCode checks code — a 6-digit TOTP or a recovery code — for
// userID as the second step of login. Deliberately returns (bool, error)
// rather than an error sentinel for "wrong code": the caller (login handler)
// must show the IDENTICAL message for a wrong code, an expired challenge, and
// a consumed challenge, so that none of the three becomes a distinguishing
// oracle for an attacker. Folding all of "wrong" into a plain false lets the
// handler pick one message for every rejection without threading error types
// through that decision.
func (s *MFAService) LoginVerifyCode(ctx context.Context, userID uuid.UUID, code string) (bool, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return false, nil
	}
	// A 6-digit numeric string is unambiguously a TOTP code — recovery codes
	// are always longer (see recoveryCodeAlphabet/recoveryCodeLength) — so
	// this dispatch never needs a "try both" fallback that would otherwise
	// burn a live recovery code on a mistyped TOTP attempt.
	if isNumericTOTP(code) {
		var secretEnc []byte
		var enabledAt *time.Time
		if err := s.pool.QueryRow(ctx,
			`SELECT totp_secret_enc, totp_enabled_at FROM users WHERE id = $1`,
			userID).Scan(&secretEnc, &enabledAt); err != nil {
			return false, err
		}
		if enabledAt == nil || len(secretEnc) == 0 {
			return false, nil
		}
		return s.verifyLiveCode(ctx, userID, secretEnc, code)
	}
	return s.verifyRecoveryCode(ctx, userID, code)
}

// verifyLiveCode decrypts secretEnc and checks code against it, persisting
// the replay guard atomically. The single UPDATE ... WHERE totp_last_step <
// $2 is what makes this race-safe: two concurrent requests presenting the
// SAME still-valid code can only have one of them win this statement, even
// though both would independently compute the same matching step.
func (s *MFAService) verifyLiveCode(ctx context.Context, userID uuid.UUID, secretEnc []byte, code string) (bool, error) {
	plain, err := s.totp.Open(userID[:], secretEnc)
	if err != nil {
		return false, err
	}
	step, ok := matchStep(string(plain), code, time.Now())
	if !ok {
		return false, nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE users SET totp_last_step = $2
		WHERE id = $1 AND (totp_last_step IS NULL OR totp_last_step < $2)`,
		userID, step)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *MFAService) verifyRecoveryCode(ctx context.Context, userID uuid.UUID, code string) (bool, error) {
	hash := hashRecoveryCode(code)
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		UPDATE mfa_recovery_codes SET used_at = NOW()
		WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL
		RETURNING id`, userID, hash).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := s.aud.Log(ctx, audit.Entry{
		ActorID: &userID, Action: "auth.2fa_recovery_used", Entity: "user", EntityID: userID.String(),
	}); err != nil {
		return false, err
	}
	return true, nil
}

// matchStep checks code against secretB32 across the ±totpSkewSteps window
// around now (matching the tolerance every mainstream authenticator app
// assumes), and returns the matching step counter so the caller can enforce
// the replay guard. Unlike totp.ValidateCustom (which only reports true/
// false), this needs to know WHICH step matched.
func matchStep(secretB32, code string, now time.Time) (step int64, ok bool) {
	cur := now.Unix() / totpPeriodSeconds
	for i := -int64(totpSkewSteps); i <= int64(totpSkewSteps); i++ {
		candidate := cur + i
		want, err := totp.GenerateCodeCustom(secretB32, time.Unix(candidate*totpPeriodSeconds, 0), totp.ValidateOpts{
			Period:    totpPeriodSeconds,
			Digits:    otp.DigitsSix,
			Algorithm: otp.AlgorithmSHA1,
		})
		if err != nil {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return candidate, true
		}
	}
	return 0, false
}

func isNumericTOTP(code string) bool {
	if len(code) != 6 {
		return false
	}
	_, err := strconv.Atoi(code)
	return err == nil
}

// recoveryCodeAlphabet excludes ambiguous characters (0/O, 1/I/L), same
// convention as generateTempPassword in user.go.
const recoveryCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
const recoveryCodeLength = 10 // rendered as two dash-separated groups of 5

func generateRecoveryCodes() (plain []string, hashes [][]byte, err error) {
	plain = make([]string, recoveryCodeCount)
	hashes = make([][]byte, recoveryCodeCount)
	for i := range plain {
		code, genErr := randomRecoveryCode()
		if genErr != nil {
			return nil, nil, genErr
		}
		plain[i] = code
		h := hashRecoveryCode(code)
		hashes[i] = h
	}
	return plain, hashes, nil
}

func randomRecoveryCode() (string, error) {
	b := make([]byte, recoveryCodeLength)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = recoveryCodeAlphabet[int(b[i])%len(recoveryCodeAlphabet)]
	}
	half := recoveryCodeLength / 2
	return fmt.Sprintf("%s-%s", string(b[:half]), string(b[half:])), nil
}

// hashRecoveryCode normalizes (strip separators/whitespace, uppercase) before
// hashing, so "abcd-efghi", "ABCDEFGHI ", and "abcdefghi" all look up the same
// stored row. SHA-256 rather than bcrypt — see migration 0091's own doc
// comment on mfa_recovery_codes for why: these are 128-bit random values with
// no dictionary to defend against, so a single indexed lookup on the hash is
// both correct and constant-time by construction, unlike bcrypt-comparing
// against every one of a user's remaining codes in turn.
func hashRecoveryCode(code string) []byte {
	norm := strings.ToUpper(strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, code))
	sum := sha256.Sum256([]byte(norm))
	return sum[:]
}
