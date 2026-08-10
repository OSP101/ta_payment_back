// citizen_id.go is the one place in this codebase a TA citizen ID number
// exists outside of a request body — everywhere else (GetProfile, the users
// list, every other API response) it stays absent, per migration 0047's
// original PDPA policy. See migration 0076 for why this one field is an
// exception, and internal/pii for how it is encrypted.
package service

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/audit"
)

// citizenIDKeyVersion is bumped whenever PII_ENC_KEY is rotated to a new
// value, so old rows keep recording which key encrypted them.
const citizenIDKeyVersion = 1

// storeCitizenID encrypts and stores nationalID against userID, replacing
// whatever was stored before (a resubmitted profile supersedes the old
// number, same as every other field on the form). Runs inside the caller's
// transaction — UpsertProfile's own INSERT ... ON CONFLICT for ta_profiles
// must already have run in that same tx, since this is a plain UPDATE.
//
// last4 is derived from nationalID itself, never accepted as separate input,
// so the two can never disagree.
func (s *DocsService) storeCitizenID(ctx context.Context, tx pgx.Tx, userID uuid.UUID, nationalID string) error {
	if s.pii == nil {
		return errors.New("citizen id encryption is not configured (PII_ENC_KEY missing)")
	}
	sealed, err := s.pii.Seal(userID[:], []byte(nationalID))
	if err != nil {
		return err
	}
	last4 := nationalID
	if len(last4) > 4 {
		last4 = last4[len(last4)-4:]
	}
	_, err = tx.Exec(ctx, `
		UPDATE ta_profiles
		SET citizen_id_enc = $2, citizen_id_last4 = $3, citizen_id_key_version = $4
		WHERE user_id = $1`,
		userID, sealed, last4, citizenIDKeyVersion)
	return err
}

// RevealCitizenID decrypts and returns userID's stored citizen ID. This is
// the ONLY function in the codebase that produces the plaintext number back
// out of the database — the transfer-cover document generator (Phase 3) is
// its intended caller. Every call is audited: reading PII back out is itself
// an event worth a trail, distinct from the (unaudited) fact that it exists.
func (s *DocsService) RevealCitizenID(ctx context.Context, actor, userID uuid.UUID, reason string) (string, error) {
	if s.pii == nil {
		return "", errors.New("citizen id encryption is not configured (PII_ENC_KEY missing)")
	}
	var sealed []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT citizen_id_enc FROM ta_profiles WHERE user_id = $1`, userID,
	).Scan(&sealed); err != nil {
		return "", err
	}
	if len(sealed) == 0 {
		return "", ErrNotFound
	}
	plain, err := s.pii.Open(userID[:], sealed)
	if err != nil {
		return "", err
	}
	if err := s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "ta_profile.citizen_id.reveal", Entity: "ta_profile",
		EntityID: userID.String(), Note: reason,
	}); err != nil {
		return "", err
	}
	return string(plain), nil
}
