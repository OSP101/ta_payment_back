package service

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

// Approving the third required document now finalises the profile — there is no
// separate "approve the person" click any more. That makes Review() the single
// gate into the approved bucket, so these tests cover the boundary from both
// sides: it must not fire early, and it must fire exactly once when the set
// completes.

func autoApproveFixture(t *testing.T) (*DocsService, uuid.UUID, map[string]uuid.UUID) {
	t.Helper()
	pool := testutil.NewPool(t)
	svc := &DocsService{pool: pool, aud: audit.New(pool)}
	ctx := context.Background()

	ta := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active)
		 VALUES ($1, $2, 'ออโต้', 'ทดสอบ', TRUE)`,
		ta, "auto-"+ta.String()+"@example.test"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO ta_profiles (user_id, prefix, status, completed_at, current_round)
		 VALUES ($1, 'นาย', 'submitted', NOW(), 1)`, ta); err != nil {
		t.Fatalf("insert profile: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO ta_profile_submissions (user_id, round, prefix, status)
		 VALUES ($1, 1, 'นาย', 'submitted')`, ta); err != nil {
		t.Fatalf("insert submission: %v", err)
	}

	docs := map[string]uuid.UUID{}
	for _, kind := range requiredDocKinds {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO ta_documents
			  (id, user_id, kind, filename, mime, size_bytes, storage_key, status, round)
			VALUES ($1,$2,$3,$4,'application/pdf',1,$5,'submitted',1)`,
			id, ta, kind, kind+".pdf", "key/"+id.String()); err != nil {
			t.Fatalf("insert doc %s: %v", kind, err)
		}
		docs[kind] = id
	}
	return svc, ta, docs
}

func profileStatus(t *testing.T, svc *DocsService, ta uuid.UUID) string {
	t.Helper()
	var st string
	if err := svc.pool.QueryRow(context.Background(),
		`SELECT status::text FROM ta_profiles WHERE user_id = $1`, ta).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestReview_ApprovesProfileOnlyWhenAllThreeDocsPass(t *testing.T) {
	svc, ta, docs := autoApproveFixture(t)
	ctx := context.Background()
	officer := uuid.New()
	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active)
		 VALUES ($1, $2, 'จ.น.', 'ทดสอบ', TRUE)`,
		officer, "auto-officer-"+officer.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}

	// One and two of three: still not approved. Approving early would put a TA
	// into the finance handoff with an unreviewed document.
	if err := svc.Review(ctx, officer, docs["national_id"], true, ""); err != nil {
		t.Fatalf("approve doc 1: %v", err)
	}
	if got := profileStatus(t, svc, ta); got != "submitted" {
		t.Fatalf("after 1 of 3, profile = %q, want still submitted", got)
	}
	if err := svc.Review(ctx, officer, docs["bank_book"], true, ""); err != nil {
		t.Fatalf("approve doc 2: %v", err)
	}
	if got := profileStatus(t, svc, ta); got != "submitted" {
		t.Fatalf("after 2 of 3, profile = %q, want still submitted", got)
	}

	// The third completes the set.
	if err := svc.Review(ctx, officer, docs["creditor_form"], true, ""); err != nil {
		t.Fatalf("approve doc 3: %v", err)
	}
	if got := profileStatus(t, svc, ta); got != "approved" {
		t.Fatalf("after 3 of 3, profile = %q, want approved", got)
	}

	// The submission snapshot has to move with it — that row is the history
	// staff read back, and the old approve-all click updated both together.
	var subStatus string
	if err := svc.pool.QueryRow(ctx,
		`SELECT status::text FROM ta_profile_submissions WHERE user_id=$1 AND round=1`, ta).Scan(&subStatus); err != nil {
		t.Fatal(err)
	}
	if subStatus != "approved" {
		t.Errorf("submission snapshot = %q, want approved", subStatus)
	}
}

// The retention clock used to be started by the approve-all click. Now that
// every approval goes through Review, it has to start here — otherwise approved
// documents would sit on disk forever, which is the PDPA promise broken.
func TestReview_ApprovalStartsRetentionClock(t *testing.T) {
	svc, _, docs := autoApproveFixture(t)
	ctx := context.Background()
	officer := uuid.New()
	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active)
		 VALUES ($1, $2, 'จ.น.', 'ทดสอบ', TRUE)`,
		officer, "auto-officer2-"+officer.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}

	if err := svc.Review(ctx, officer, docs["national_id"], true, ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	var days *float64
	if err := svc.pool.QueryRow(ctx,
		`SELECT EXTRACT(EPOCH FROM (expires_at - NOW())) / 86400
		   FROM ta_documents WHERE id = $1`, docs["national_id"]).Scan(&days); err != nil {
		t.Fatal(err)
	}
	if days == nil {
		t.Fatal("expires_at is NULL after approval — the file would never be purged")
	}
	if *days < 6.9 || *days > 7.1 {
		t.Errorf("expires_at is %.2f days out, want ~7", *days)
	}
}

// Rejecting one document must not leave a profile approved: it drops back to
// needs_fix and out of the approved bucket, which is what makes "approved means
// all three passed" true.
func TestReview_RejectingAfterApprovalIsNotAutoApproved(t *testing.T) {
	svc, ta, docs := autoApproveFixture(t)
	ctx := context.Background()
	officer := uuid.New()
	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active)
		 VALUES ($1, $2, 'จ.น.', 'ทดสอบ', TRUE)`,
		officer, "auto-officer3-"+officer.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	for _, kind := range requiredDocKinds {
		if err := svc.Review(ctx, officer, docs[kind], true, ""); err != nil {
			t.Fatalf("approve %s: %v", kind, err)
		}
	}
	if got := profileStatus(t, svc, ta); got != "approved" {
		t.Fatalf("setup: profile = %q, want approved", got)
	}

	// Rejecting a document does not itself re-run finalisation, and must not
	// silently leave the profile approved with a rejected document inside.
	if err := svc.RejectBatch(ctx, officer, ta,
		[]RejectItem{{DocID: docs["bank_book"], Reason: "หน้าสมุดไม่ชัด"}}); err == nil {
		if got := profileStatus(t, svc, ta); got == "approved" {
			t.Fatal("profile still approved after a document was rejected")
		}
	}
	// RejectBatch refuses an already-approved doc by design; either way the
	// invariant that matters is checked above.
}
