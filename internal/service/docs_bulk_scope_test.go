package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/auth"
	"ta-payment-back/internal/testutil"
)

// The bulk download must contain the TAs the officer approved on the screen in
// front of them — not every approved TA in the database.
//
// It used to resolve "all approved profiles", so a file pulled after approving
// one person also carried hand-offs from weeks earlier: people who were never on
// that screen, whose PII the officer then held a copy of without knowing. These
// tests pin the scope, and pin that an empty request cannot fall back to
// everyone — the fallback IS the bug.

const bulkTestPassword = "officer-pw-12345"

// scopeFixture builds an officer with a real password hash and `n` approved TAs,
// each with the three required approved documents. Returns their ids in order.
func scopeFixture(t *testing.T, n int) (*DocsService, uuid.UUID, []uuid.UUID) {
	t.Helper()
	pool := testutil.NewPool(t)
	svc := &DocsService{pool: pool, aud: audit.New(pool)}
	ctx := context.Background()

	hash, err := auth.HashPassword(bulkTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	officer := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active, password_hash)
		 VALUES ($1, $2, 'เจ้าหน้าที่', 'ทดสอบ', TRUE, $3)`,
		officer, "scope-officer-"+officer.String()+"@example.test", hash); err != nil {
		t.Fatalf("insert officer: %v", err)
	}

	tas := make([]uuid.UUID, 0, n)
	for i := 0; i < n; i++ {
		ta := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, email, first_name, last_name, is_active, student_id)
			 VALUES ($1, $2, 'ทีเอ', 'ทดสอบ', TRUE, $3)`,
			ta, "scope-ta-"+ta.String()+"@example.test",
			// Distinct student ids so the bundle's ORDER BY is deterministic.
			"65302000"+string(rune('0'+i))+"-0"); err != nil {
			t.Fatalf("insert ta: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO ta_profiles (user_id, prefix, status, completed_at, current_round, verified_at)
			 VALUES ($1, 'นาย', 'approved', NOW(), 1, NOW())`, ta); err != nil {
			t.Fatalf("insert profile: %v", err)
		}
		for _, kind := range requiredDocKinds {
			id := uuid.New()
			if _, err := pool.Exec(ctx, `
				INSERT INTO ta_documents
				  (id, user_id, kind, filename, mime, size_bytes, storage_key, status, round)
				VALUES ($1,$2,$3,$4,'application/pdf',1,$5,'approved',1)`,
				id, ta, kind, kind+".pdf", "key/"+id.String()); err != nil {
				t.Fatalf("insert doc: %v", err)
			}
		}
		tas = append(tas, ta)
	}
	return svc, officer, tas
}

// docOwners resolves whose documents a minted token actually points at, which is
// the only thing that determines what ends up in the file.
func docOwners(t *testing.T, svc *DocsService, token string, actor uuid.UUID) map[uuid.UUID]bool {
	t.Helper()
	docIDs, err := svc.ConsumeZipToken(token, actor, uuid.Nil)
	if err != nil {
		t.Fatalf("ConsumeZipToken: %v", err)
	}
	rows, err := svc.pool.Query(context.Background(),
		`SELECT DISTINCT user_id FROM ta_documents WHERE id = ANY($1)`, docIDs)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out[id] = true
	}
	return out
}

// The bug, directly: three approved TAs in the database, one on the screen.
func TestMintAllApproved_OnlyIncludesTheRequestedTAs(t *testing.T) {
	svc, officer, tas := scopeFixture(t, 3)
	onScreen := tas[1]

	token, count, err := svc.MintAllApprovedZipToken(
		context.Background(), officer, bulkTestPassword, []uuid.UUID{onScreen})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if count != 1 {
		t.Errorf("ta_count = %d, want 1 — the number shown to the officer must match the file", count)
	}

	owners := docOwners(t, svc, token, officer)
	if !owners[onScreen] {
		t.Error("the requested TA is missing from the bundle")
	}
	for _, other := range []uuid.UUID{tas[0], tas[2]} {
		if owners[other] {
			t.Errorf("%s was approved earlier and was NOT on the screen, but is in the file", other)
		}
	}
}

// Several people on the screen must all arrive — narrowing must not become
// "only the first".
func TestMintAllApproved_IncludesEveryRequestedTA(t *testing.T) {
	svc, officer, tas := scopeFixture(t, 3)
	want := []uuid.UUID{tas[0], tas[2]}

	token, count, err := svc.MintAllApprovedZipToken(
		context.Background(), officer, bulkTestPassword, want)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if count != len(want) {
		t.Errorf("ta_count = %d, want %d", count, len(want))
	}
	owners := docOwners(t, svc, token, officer)
	for _, ta := range want {
		if !owners[ta] {
			t.Errorf("%s was requested but is not in the file", ta)
		}
	}
	if owners[tas[1]] {
		t.Error("an unrequested TA leaked in")
	}
}

// An empty list must be refused, NOT treated as "everyone". This is the exact
// fallback that produced the bug, so it gets its own test.
//
// Asserting only "an error came back" would prove nothing: `user_id = ANY('{}')`
// matches no rows, so the no-documents error fires either way and the assertion
// passes with the guard deleted (verified by mutation). What the guard uniquely
// does is refuse BEFORE the password is consulted — an empty selection is a
// client mistake, not a failed authentication — so the discriminator is that a
// wrong password still yields the selection error rather than a 401.
func TestMintAllApproved_EmptyListIsRefusedBeforeAuthenticating(t *testing.T) {
	svc, officer, _ := scopeFixture(t, 3)

	_, _, err := svc.MintAllApprovedZipToken(
		context.Background(), officer, "wrong-password-entirely", nil)
	if err == nil {
		t.Fatal("an empty request must be refused, not resolved to every approved TA")
	}
	var ue *UserError
	if !errors.As(err, &ue) {
		t.Fatalf("want a UserError, got %#v", err)
	}
	if ue.Status == 401 {
		t.Errorf("empty selection reached the password check (status %d) — the guard is gone, "+
			"and with it the only thing stopping an empty list from meaning everyone", ue.Status)
	}
	if !strings.Contains(ue.Msg, "ยังไม่มีใคร") {
		t.Errorf("refusal = %q; it should say nobody was selected, not that nothing is approved — "+
			"the officer's screen had approvals on it", ue.Msg)
	}
}

// The list narrows; it does not authorise. A TA who is not approved cannot be
// pulled into the file by naming them.
func TestMintAllApproved_RequestedButUnapprovedIsExcluded(t *testing.T) {
	svc, officer, tas := scopeFixture(t, 2)
	ctx := context.Background()

	// tas[1] gets sent back after the officer's screen was drawn.
	if _, err := svc.pool.Exec(ctx,
		`UPDATE ta_profiles SET status='needs_fix' WHERE user_id=$1`, tas[1]); err != nil {
		t.Fatal(err)
	}

	token, count, err := svc.MintAllApprovedZipToken(ctx, officer, bulkTestPassword, tas)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if count != 1 {
		t.Errorf("ta_count = %d, want 1 — only the still-approved TA belongs in the file", count)
	}
	if owners := docOwners(t, svc, token, officer); owners[tas[1]] {
		t.Error("a TA whose profile is not approved was included because the client named them")
	}
}

// Requesting only unqualified TAs must fail rather than produce an empty file.
func TestMintAllApproved_NoneQualifyIsAnError(t *testing.T) {
	svc, officer, tas := scopeFixture(t, 1)
	ctx := context.Background()

	if _, err := svc.pool.Exec(ctx,
		`UPDATE ta_profiles SET status='needs_fix' WHERE user_id=$1`, tas[0]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.MintAllApprovedZipToken(ctx, officer, bulkTestPassword, tas); err == nil {
		t.Fatal("a request resolving to no documents must be an error, not an empty bundle")
	}
}

// The password gate must still hold, and must be checked whatever the list says.
func TestMintAllApproved_WrongPasswordIsRefused(t *testing.T) {
	svc, officer, tas := scopeFixture(t, 1)

	_, _, err := svc.MintAllApprovedZipToken(
		context.Background(), officer, "not-the-password", tas)
	if err == nil {
		t.Fatal("a wrong password must be refused")
	}
	var ue *UserError
	if !errors.As(err, &ue) || ue.Status != 401 {
		t.Errorf("want a 401 UserError, got %#v", err)
	}
}
