package demo

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"ta-payment-back/internal/config"
	"ta-payment-back/internal/testutil"
)

func TestSlotClaim_SignVerify(t *testing.T) {
	key := []byte("k1")
	now := time.Now()
	raw := signClaim(key, slotClaim{Slot: 3, Email: "Tester@Example.ac.th"}, now)

	cl, err := verifyClaim(key, raw, now)
	if err != nil || cl.Slot != 3 || cl.Email != "tester@example.ac.th" {
		t.Fatalf("round trip: %+v %v", cl, err)
	}
	if _, err := verifyClaim([]byte("other-key"), raw, now); err == nil {
		t.Error("a claim signed with another key must not verify")
	}
	if _, err := verifyClaim(key, raw, now.Add(claimTTL+time.Minute)); err == nil {
		t.Error("an expired claim must not verify")
	}
	// Rewrite the slot number in the payload but keep the original signature:
	// the signature must no longer match.
	p, sig, _ := strings.Cut(raw, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(p)
	tampered := strings.Replace(string(payload), "3|", "0|", 1)
	if _, err := verifyClaim(key, base64.RawURLEncoding.EncodeToString([]byte(tampered))+"."+sig, now); err == nil {
		t.Error("a claim whose slot was edited must not verify")
	}
	for _, junk := range []string{"", "no-dot", "a.b", "..."} {
		if _, err := verifyClaim(key, junk, now); err == nil {
			t.Errorf("junk %q verified", junk)
		}
	}
}

// The escalation this closes: a TA-tier tester POSTing to ANOTHER tester's slot
// login and inheriting that slot owner's (staff) tier.
func TestTierForClaim_TierComesFromTheCallersClaimNotTheSlotOwner(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	for _, q := range []string{
		`CREATE TABLE demo_workspaces (slot_index INT PRIMARY KEY, schema_name TEXT, owner_email TEXT)`,
		`CREATE TABLE demo_authorized_testers (email TEXT PRIMARY KEY, tier TEXT NOT NULL, note TEXT NOT NULL DEFAULT '', added_by TEXT NOT NULL DEFAULT 'test')`,
		`INSERT INTO demo_authorized_testers (email, tier) VALUES ('staff@test.ac.th','staff'), ('ta@test.ac.th','ta')`,
		`INSERT INTO demo_workspaces VALUES (0,'demo_slot_0','staff@test.ac.th'), (1,'demo_slot_1','ta@test.ac.th')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	m := &Manager{mainPool: pool, cfg: config.Config{DemoJWTSecret: "test-secret"}}
	taClaim := m.SignSlotClaim(1, "ta@test.ac.th")

	if tier, err := m.TierForClaim(ctx, 1, taClaim); err != nil || tier != TierTA {
		t.Fatalf("own slot: tier=%q err=%v, want ta", tier, err)
	}
	if _, err := m.TierForClaim(ctx, 0, taClaim); !errors.Is(err, errBadClaim) {
		t.Fatalf("TA's claim on the staff tester's slot must be refused, got %v", err)
	}
	if _, err := m.TierForClaim(ctx, 0, ""); !errors.Is(err, errBadClaim) {
		t.Fatalf("a login with no claim must be refused, got %v", err)
	}
	// A validly signed claim from a previous owner stops working once the slot
	// has been reclaimed by someone else.
	stale := m.SignSlotClaim(0, "ta@test.ac.th")
	if _, err := m.TierForClaim(ctx, 0, stale); !errors.Is(err, errBadClaim) {
		t.Fatalf("a claim for an email that no longer owns the slot must be refused, got %v", err)
	}
	// Revocation still takes effect on the next login.
	if _, err := pool.Exec(ctx, `DELETE FROM demo_authorized_testers WHERE email='ta@test.ac.th'`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.TierForClaim(ctx, 1, taClaim); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a revoked tester must be refused, got %v", err)
	}
}
