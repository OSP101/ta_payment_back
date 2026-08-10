package service

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

func newSeniorityFixture(t *testing.T) (svc *UserService, ctx context.Context, taID uuid.UUID, termA, termB uuid.UUID) {
	t.Helper()
	pool := testutil.NewPool(t)
	svc = &UserService{pool: pool, aud: audit.New(pool)}
	ctx = context.Background()

	taID = uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active)
		 VALUES ($1, $2, 'ทดสอบ', 'ทีเอ', TRUE)`,
		taID, "senior-ta-"+taID.String()+"@example.test"); err != nil {
		t.Fatalf("insert ta: %v", err)
	}

	termA = uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO academic_terms (id, academic_year, semester) VALUES ($1, 2568, 1)`, termA); err != nil {
		t.Fatalf("insert termA: %v", err)
	}
	termB = uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO academic_terms (id, academic_year, semester) VALUES ($1, 2569, 1)`, termB); err != nil {
		t.Fatalf("insert termB: %v", err)
	}
	return svc, ctx, taID, termA, termB
}

func TestTASeniority_NullFirstTermDefaultsToReturning(t *testing.T) {
	svc, ctx, taID, termA, _ := newSeniorityFixture(t)
	got, err := svc.TASeniority(ctx, taID, termA)
	if err != nil {
		t.Fatal(err)
	}
	if got != "returning" {
		t.Errorf("got %q, want returning (unknown history should never default to new)", got)
	}
}

func TestTASeniority_FirstTermMatchesIsNew(t *testing.T) {
	svc, ctx, taID, termA, termB := newSeniorityFixture(t)
	if err := svc.RecordTAFirstTermIfUnset(ctx, taID, termA); err != nil {
		t.Fatal(err)
	}

	if got, err := svc.TASeniority(ctx, taID, termA); err != nil || got != "new" {
		t.Errorf("term of first appointment: got %q, %v — want new, <nil>", got, err)
	}
	if got, err := svc.TASeniority(ctx, taID, termB); err != nil || got != "returning" {
		t.Errorf("a later term: got %q, %v — want returning, <nil>", got, err)
	}
}

// The whole reason to store the first term rather than a boolean: printing
// TERM A's document again after term B has run must still say "new" for term
// A, not flip to "returning" just because more time has passed.
func TestTASeniority_ReprintingAnOldTermStaysNew(t *testing.T) {
	svc, ctx, taID, termA, termB := newSeniorityFixture(t)
	if err := svc.RecordTAFirstTermIfUnset(ctx, taID, termA); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordTAFirstTermIfUnset(ctx, taID, termB); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.TASeniority(ctx, taID, termA); err != nil || got != "new" {
		t.Errorf("reprinting term A: got %q, %v — want new, <nil>", got, err)
	}
}

func TestTASeniority_RecordIsWriteOnce(t *testing.T) {
	svc, ctx, taID, termA, termB := newSeniorityFixture(t)
	if err := svc.RecordTAFirstTermIfUnset(ctx, taID, termA); err != nil {
		t.Fatal(err)
	}
	// A second appointment (term B) must not move the stamp — if it did,
	// every returning TA would look new again on their next term.
	if err := svc.RecordTAFirstTermIfUnset(ctx, taID, termB); err != nil {
		t.Fatal(err)
	}
	var stored uuid.UUID
	if err := svc.pool.QueryRow(ctx, `SELECT ta_first_term_id FROM users WHERE id = $1`, taID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != termA {
		t.Errorf("ta_first_term_id = %v, want %v (the FIRST term, unchanged)", stored, termA)
	}
}

func TestTASeniority_OverrideWinsOverFirstTerm(t *testing.T) {
	svc, ctx, taID, termA, _ := newSeniorityFixture(t)
	if err := svc.RecordTAFirstTermIfUnset(ctx, taID, termA); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.pool.Exec(ctx,
		`UPDATE users SET ta_seniority_override = 'returning' WHERE id = $1`, taID); err != nil {
		t.Fatal(err)
	}
	// Without the override this would read "new" (termA is the first-term
	// stamp) — the override must still win.
	if got, err := svc.TASeniority(ctx, taID, termA); err != nil || got != "returning" {
		t.Errorf("got %q, %v — want returning, <nil> (override must win)", got, err)
	}
}
