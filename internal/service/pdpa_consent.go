// pdpa_consent.go records that a TA has seen and accepted the PDPA notice
// shown before the profile form (citizen ID, bank/PromptPay details) is
// filled in for the first time — see migration 0092 and
// docs/SECURITY.md's "PDPA consent" section.
package service

import (
	"context"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// pdpaConsentVersion identifies the notice text a consent row was accepted
// against. Bump this if the notice materially changes — HasPdpaConsent only
// ever checks the current version, so every user is transparently asked to
// re-consent, with no extra migration needed.
const pdpaConsentVersion = 1

// RecordPdpaConsent stores that userID accepted the current PDPA notice, along
// with where and when. Idempotent: accepting the same version twice (a
// double-click, a retried request) is a no-op, not an error, per the
// UNIQUE(user_id, version) constraint in migration 0092.
func (s *DocsService) RecordPdpaConsent(ctx context.Context, userID uuid.UUID, ip, userAgent string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO pdpa_consents (user_id, version, ip, user_agent)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, version) DO NOTHING`,
		userID, pdpaConsentVersion, ip, userAgent); err != nil {
		return err
	}
	return s.aud.Log(ctx, audit.Entry{
		ActorID: &userID, Action: "user.pdpa_consent", Entity: "user",
		EntityID: userID.String(), IP: ip, UserAgent: userAgent,
	})
}

// HasPdpaConsent reports whether userID has accepted the current PDPA notice
// version. UpsertProfile refuses to run without it — see that function.
func (s *DocsService) HasPdpaConsent(ctx context.Context, userID uuid.UUID) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pdpa_consents WHERE user_id = $1 AND version = $2)`,
		userID, pdpaConsentVersion,
	).Scan(&ok)
	return ok, err
}
