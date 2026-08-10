package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/pii"
	"ta-payment-back/internal/testutil"
)

// citizenIDActor returns a real users.id — audit_logs.actor_id is a foreign
// key, so a reveal that succeeds (and therefore audits) needs a real actor.
func citizenIDActor(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, first_name, last_name, is_active) VALUES ($1,$2,'จนท','ทดสอบ',TRUE)`,
		id, "citizen-actor-"+id.String()+"@example.test"); err != nil {
		t.Fatalf("insert actor: %v", err)
	}
	return id
}

// testPIICipher gives every test in this package a real (random-keyed)
// cipher, so DocsService.pii is never nil in tests the way it would be if a
// fixture forgot to set PII_ENC_KEY in production — that misconfiguration is
// covered separately by TestStoreCitizenID_RefusesWithoutCipher.
func testPIICipher(t *testing.T) *pii.Cipher {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	c, err := pii.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func citizenIDFixture(t *testing.T) (svc *DocsService, ctx context.Context, userID uuid.UUID) {
	t.Helper()
	pool := testutil.NewPool(t)
	svc = &DocsService{pool: pool, aud: audit.New(pool), store: newMemStore(), pii: testPIICipher(t)}
	ctx = context.Background()
	userID = uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active) VALUES ($1,$2,'ทดสอบ','ทีเอ',TRUE)`,
		userID, "citizen-"+userID.String()+"@example.test"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO ta_profiles (user_id, status, current_round) VALUES ($1,'pending',1)`, userID); err != nil {
		t.Fatalf("insert ta_profiles: %v", err)
	}
	return svc, ctx, userID
}

func TestStoreCitizenID_EncryptsAndDerivesLast4(t *testing.T) {
	svc, ctx, userID := citizenIDFixture(t)

	tx, err := svc.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.storeCitizenID(ctx, tx, userID, "1234567890123"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var enc []byte
	var last4 string
	var keyVersion int
	if err := svc.pool.QueryRow(ctx,
		`SELECT citizen_id_enc, citizen_id_last4, citizen_id_key_version FROM ta_profiles WHERE user_id=$1`,
		userID).Scan(&enc, &last4, &keyVersion); err != nil {
		t.Fatal(err)
	}
	if len(enc) == 0 {
		t.Error("citizen_id_enc is empty")
	}
	// The stored bytes must not contain the plaintext anywhere in them.
	if bytes.Contains(enc, []byte("1234567890123")) {
		t.Error("citizen_id_enc contains the plaintext national id verbatim")
	}
	if last4 != "0123" {
		t.Errorf("citizen_id_last4 = %q, want 0123", last4)
	}
	if keyVersion != citizenIDKeyVersion {
		t.Errorf("citizen_id_key_version = %d, want %d", keyVersion, citizenIDKeyVersion)
	}
}

func TestStoreCitizenID_RefusesWithoutCipher(t *testing.T) {
	svc, ctx, userID := citizenIDFixture(t)
	svc.pii = nil // simulate PII_ENC_KEY missing

	tx, err := svc.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := svc.storeCitizenID(ctx, tx, userID, "1234567890123"); err == nil {
		t.Error("expected an error when the PII cipher is not configured")
	}
}

func TestRevealCitizenID_RoundtripsAndAudits(t *testing.T) {
	svc, ctx, userID := citizenIDFixture(t)
	tx, err := svc.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.storeCitizenID(ctx, tx, userID, "1234567890123"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	actor := citizenIDActor(t, svc.pool)
	got, err := svc.RevealCitizenID(ctx, actor, userID, "test reveal")
	if err != nil {
		t.Fatal(err)
	}
	if got != "1234567890123" {
		t.Errorf("RevealCitizenID = %q, want 1234567890123", got)
	}

	var action, entityID string
	if err := svc.pool.QueryRow(ctx,
		`SELECT action, entity_id FROM audit_logs WHERE action = 'ta_profile.citizen_id.reveal' ORDER BY at DESC LIMIT 1`,
	).Scan(&action, &entityID); err != nil {
		t.Fatalf("expected an audit row for the reveal: %v", err)
	}
	if entityID != userID.String() {
		t.Errorf("audit entity_id = %q, want %q", entityID, userID.String())
	}
}

func TestRevealCitizenID_NoRowIsNotFound(t *testing.T) {
	svc, ctx, userID := citizenIDFixture(t)
	if _, err := svc.RevealCitizenID(ctx, uuid.New(), userID, "test"); err == nil {
		t.Error("expected an error when no citizen id has been stored yet")
	}
}

// A citizen ID sealed for one user must never open for another — this is
// what binding the ciphertext to the user id as AAD (see internal/pii) is
// for. Simulates the failure mode by pointing RevealCitizenID's WHERE clause
// at a row whose ciphertext was sealed under a different user's id.
func TestRevealCitizenID_RejectsCiphertextSealedForAnotherUser(t *testing.T) {
	svc, ctx, userA := citizenIDFixture(t)
	userB := uuid.New()
	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active) VALUES ($1,$2,'ทดสอบ','ทีเอ2',TRUE)`,
		userB, "citizen-b-"+userB.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO ta_profiles (user_id, status, current_round) VALUES ($1,'pending',1)`, userB); err != nil {
		t.Fatal(err)
	}

	sealed, err := svc.pii.Seal(userA[:], []byte("1234567890123"))
	if err != nil {
		t.Fatal(err)
	}
	// Plant userA's sealed value on userB's row.
	if _, err := svc.pool.Exec(ctx,
		`UPDATE ta_profiles SET citizen_id_enc=$2 WHERE user_id=$1`, userB, sealed); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.RevealCitizenID(ctx, uuid.New(), userB, "test"); err == nil {
		t.Error("expected decryption to fail for a ciphertext sealed under a different user's AAD")
	}
}
