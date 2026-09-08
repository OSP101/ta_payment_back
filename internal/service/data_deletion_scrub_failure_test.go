package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/testutil"
)

// deleteFailingStore's Delete always errors — everything else delegates to a real
// memStore so Save/Open behave normally and only the deletion step fails.
type deleteFailingStore struct{ *memStore }

func (f *deleteFailingStore) Delete(key string) error { return errors.New("disk unavailable (simulated)") }

// PDPA-01: ScrubUserDocuments/avatar-delete used to run AFTER ReviewDeletion's
// transaction committed, with the error only log.Printf'd — a request could
// be recorded as status='approved' with a durable "erased" audit row while
// the actual files were still sitting on disk. This pins that a scrub
// failure now surfaces: scrub_completed_at stays NULL, scrub_error is set,
// and ReviewDeletion itself returns an error instead of nil.
func TestReviewDeletion_ScrubFailureLeavesScrubCompletedAtNull(t *testing.T) {
	pool := testutil.NewPool(t)
	svc := newDeletionSvc(t, pool)
	svc.store = &deleteFailingStore{memStore: newMemStore()}

	admin := mkDeletionTestUser(t, pool)
	taID := mkDeletionTestUser(t, pool)
	ctx := context.Background()

	// Give the account an avatar so runScrub has something to fail on.
	if _, err := pool.Exec(ctx, `UPDATE users SET avatar_key = 'avatars/x.jpg' WHERE id = $1`, taID); err != nil {
		t.Fatal(err)
	}
	if err := svc.RequestDeletion(ctx, taID, ""); err != nil {
		t.Fatal(err)
	}
	var reqID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM data_deletion_requests WHERE user_id=$1`, taID).Scan(&reqID); err != nil {
		t.Fatal(err)
	}

	err := svc.ReviewDeletion(ctx, admin, reqID, true, "")
	if err == nil {
		t.Fatal("ReviewDeletion must return an error when the scrub fails, got nil")
	}

	var scrubCompletedAt *string
	var scrubError *string
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status, scrub_completed_at::text, scrub_error FROM data_deletion_requests WHERE id=$1`, reqID,
	).Scan(&status, &scrubCompletedAt, &scrubError); err != nil {
		t.Fatal(err)
	}
	// The account closure itself is NOT rolled back — that part succeeded and
	// is correct even though the file scrub failed.
	if status != "approved" {
		t.Errorf("status = %q, want approved (account closure must not roll back)", status)
	}
	if scrubCompletedAt != nil {
		t.Errorf("scrub_completed_at = %v, want NULL — the delete failed", *scrubCompletedAt)
	}
	if scrubError == nil || *scrubError == "" {
		t.Error("scrub_error should record the failure reason")
	}
}

// The sweeper must pick up exactly the row the failure above left behind, and
// clear it once the underlying storage problem is gone.
func TestSweepPendingScrubs_RetriesAndClearsOnSuccess(t *testing.T) {
	pool := testutil.NewPool(t)
	svc := newDeletionSvc(t, pool)
	fs := &deleteFailingStore{memStore: newMemStore()}
	svc.store = fs

	admin := mkDeletionTestUser(t, pool)
	taID := mkDeletionTestUser(t, pool)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `UPDATE users SET avatar_key = 'avatars/x.jpg' WHERE id = $1`, taID); err != nil {
		t.Fatal(err)
	}
	if err := svc.RequestDeletion(ctx, taID, ""); err != nil {
		t.Fatal(err)
	}
	var reqID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM data_deletion_requests WHERE user_id=$1`, taID).Scan(&reqID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReviewDeletion(ctx, admin, reqID, true, ""); err == nil {
		t.Fatal("expected the scrub to fail on the first attempt")
	}

	// "Fix the disk": swap in a store whose Delete succeeds, same underlying
	// data.
	svc.store = fs.memStore

	n, err := svc.SweepPendingScrubs(ctx)
	if err != nil {
		t.Fatalf("SweepPendingScrubs: %v", err)
	}
	if n != 1 {
		t.Fatalf("SweepPendingScrubs fixed %d requests, want 1", n)
	}

	var scrubCompletedAt *string
	var scrubError *string
	if err := pool.QueryRow(ctx,
		`SELECT scrub_completed_at::text, scrub_error FROM data_deletion_requests WHERE id=$1`, reqID,
	).Scan(&scrubCompletedAt, &scrubError); err != nil {
		t.Fatal(err)
	}
	if scrubCompletedAt == nil {
		t.Error("scrub_completed_at should be set after a successful retry")
	}
	if scrubError != nil {
		t.Errorf("scrub_error should be cleared after a successful retry, got %q", *scrubError)
	}
}

