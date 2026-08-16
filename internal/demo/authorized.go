package demo

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotAuthorized is returned by Claim (and surfaced by
// Manager.TierForSlotIndex) when the email in question has no row in
// demo_authorized_testers. Unlike ErrFull this is not a capacity problem
// that resolves itself later — it stays true until staff adds the email via
// AddAuthorizedTester.
var ErrNotAuthorized = errors.New("demo: email not authorized for testing")

// AuthorizedTester is one row of demo_authorized_testers — who staff has
// allowed into the sandbox, and at what Tier. Exposed as JSON directly to
// the /staff/settings management tab (see internal/handler's demo-testers
// endpoints), so field names double as the API shape.
type AuthorizedTester struct {
	Email     string    `json:"email"`
	Tier      Tier      `json:"tier"`
	Note      string    `json:"note"`
	AddedBy   string    `json:"added_by"`
	CreatedAt time.Time `json:"created_at"`
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// LookupTier resolves email's current tier fresh from the database — no
// caching layer sits in front of this anywhere, so a change staff makes via
// RemoveAuthorizedTester/AddAuthorizedTester is visible to the very next
// call. Called both by Claim (deciding whether to hand out a slot at all)
// and by Manager.TierForSlotIndex (deciding whether a login may proceed).
func LookupTier(ctx context.Context, pool *pgxpool.Pool, email string) (Tier, bool, error) {
	email = normalizeEmail(email)
	if email == "" {
		return "", false, nil
	}
	var tier string
	err := pool.QueryRow(ctx,
		`SELECT tier FROM demo_authorized_testers WHERE email = $1`, email,
	).Scan(&tier)
	if err != nil {
		if isNoRows(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return Tier(tier), true, nil
}

// ListAuthorizedTesters returns every allowlisted tester, newest first — the
// /staff/settings management tab's whole data source.
func ListAuthorizedTesters(ctx context.Context, pool *pgxpool.Pool) ([]AuthorizedTester, error) {
	rows, err := pool.Query(ctx,
		`SELECT email, tier, note, added_by, created_at
		 FROM demo_authorized_testers ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AuthorizedTester{}
	for rows.Next() {
		var t AuthorizedTester
		var tier string
		if err := rows.Scan(&t.Email, &tier, &t.Note, &t.AddedBy, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.Tier = Tier(tier)
		out = append(out, t)
	}
	return out, rows.Err()
}

// AddAuthorizedTester grants email access at tier, or changes an existing
// grant's tier/note in place (ON CONFLICT) — re-inviting someone at a
// different tier is "add them again" from the settings UI's point of view,
// not a separate edit flow. created_at is preserved across that update so
// the list's "added" timestamp reflects when the person was first invited,
// not when their tier was last changed.
func AddAuthorizedTester(ctx context.Context, pool *pgxpool.Pool, email string, tier Tier, note, addedBy string) error {
	email = normalizeEmail(email)
	if email == "" {
		return errors.New("demo: email is required")
	}
	if !validTiers[tier] {
		return errors.New("demo: invalid tier")
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO demo_authorized_testers (email, tier, note, added_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (email) DO UPDATE
			SET tier = $2, note = $3, added_by = $4
	`, email, string(tier), note, normalizeEmail(addedBy))
	return err
}

// RemoveAuthorizedTester revokes email's access outright. Anyone currently
// sitting in a demo session under that email loses the ability to log into
// ANY account on their very next request — see Manager.TierForSlotIndex,
// which looks this up fresh rather than trusting a token's own claims.
func RemoveAuthorizedTester(ctx context.Context, pool *pgxpool.Pool, email string) error {
	email = normalizeEmail(email)
	_, err := pool.Exec(ctx, `DELETE FROM demo_authorized_testers WHERE email = $1`, email)
	return err
}
