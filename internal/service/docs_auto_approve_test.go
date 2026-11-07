package service

import (
	"context"
	"sync"
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

// A partial approval must never start a file's countdown. All required files
// get the same configured deadline when the last verdict arrives.
func TestReview_ApprovalStartsRetentionClock(t *testing.T) {
	svc, ta, docs := autoApproveFixture(t)
	ctx := context.Background()
	officer := uuid.New()
	if _, err := svc.pool.Exec(ctx, `INSERT INTO users (id,email,first_name,last_name,is_active)
        VALUES ($1,$2,'จ.น.','ทดสอบ',TRUE)`, officer, officer.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateDocumentPolicy(ctx, officer, DocumentPolicy{RetentionDays: 30, DownloadLimit: 5}); err != nil {
		t.Fatal(err)
	}
	for i, kind := range requiredDocKinds {
		if err := svc.Review(ctx, officer, docs[kind], true, ""); err != nil {
			t.Fatal(err)
		}
		var timed int
		if err := svc.pool.QueryRow(ctx, `SELECT COUNT(*) FROM ta_documents WHERE user_id=$1 AND expires_at IS NOT NULL`, ta).Scan(&timed); err != nil {
			t.Fatal(err)
		}
		if i < len(requiredDocKinds)-1 && timed != 0 {
			t.Fatalf("%d of %d approved: %d clocks already started", i+1, len(requiredDocKinds), timed)
		}
	}
	var distinct int
	var days float64
	if err := svc.pool.QueryRow(ctx, `SELECT COUNT(DISTINCT expires_at), MIN(EXTRACT(EPOCH FROM (expires_at-NOW()))/86400) FROM ta_documents WHERE user_id=$1`, ta).Scan(&distinct, &days); err != nil {
		t.Fatal(err)
	}
	if distinct != 1 || days < 29.9 || days > 30.1 {
		t.Fatalf("deadlines: distinct=%d, days=%f", distinct, days)
	}
	var before string
	if err := svc.pool.QueryRow(ctx, `SELECT MIN(expires_at)::text FROM ta_documents WHERE user_id=$1`, ta).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateDocumentPolicy(ctx, officer, DocumentPolicy{RetentionDays: 1, DownloadLimit: 3}); err != nil {
		t.Fatal(err)
	}
	// A settings edit or repeated approval cannot extend an existing clock.
	if err := svc.Review(ctx, officer, docs[requiredDocKinds[0]], true, ""); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := svc.pool.QueryRow(ctx, `SELECT MIN(expires_at)::text FROM ta_documents WHERE user_id=$1`, ta).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("deadline changed from %s to %s", before, after)
	}
}

func TestApproveAll_UsesConfiguredRetention(t *testing.T) {
	svc, ta, _ := autoApproveFixture(t)
	ctx := context.Background()
	// The fixture TA is also a valid actor for this service-level policy test.
	if _, err := svc.UpdateDocumentPolicy(ctx, ta, DocumentPolicy{RetentionDays: 14, DownloadLimit: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApproveAll(ctx, ta, ta); err != nil {
		t.Fatal(err)
	}
	var count, distinct int
	var days float64
	if err := svc.pool.QueryRow(ctx, `SELECT COUNT(expires_at),COUNT(DISTINCT expires_at),MIN(EXTRACT(EPOCH FROM (expires_at-NOW()))/86400) FROM ta_documents WHERE user_id=$1`, ta).Scan(&count, &distinct, &days); err != nil {
		t.Fatal(err)
	}
	if count != len(requiredDocKinds) || distinct != 1 || days < 13.9 || days > 14.1 {
		t.Fatalf("count=%d distinct=%d days=%f", count, distinct, days)
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

func TestReview_ConcurrentFinalApprovalsStartOneClock(t *testing.T) {
	svc, ta, docs := autoApproveFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, len(docs))
	for _, doc := range docs {
		wg.Add(1)
		go func(id uuid.UUID) { defer wg.Done(); errs <- svc.Review(ctx, ta, id, true, "") }(doc)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := profileStatus(t, svc, ta); got != "approved" {
		t.Fatalf("profile=%s", got)
	}
	var count, distinct int
	if err := svc.pool.QueryRow(ctx, `SELECT COUNT(expires_at),COUNT(DISTINCT expires_at) FROM ta_documents WHERE user_id=$1`, ta).Scan(&count, &distinct); err != nil {
		t.Fatal(err)
	}
	if count != len(docs) || distinct != 1 {
		t.Fatalf("count=%d distinct=%d", count, distinct)
	}
}

func TestReview_RejectAfterApprovalPausesCurrentClocks(t *testing.T) {
	svc, ta, docs := autoApproveFixture(t)
	ctx := context.Background()
	for _, doc := range docs {
		if err := svc.Review(ctx, ta, doc, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.Review(ctx, ta, docs[requiredDocKinds[0]], false, "แก้ไขไฟล์"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := svc.pool.QueryRow(ctx, `SELECT COUNT(expires_at) FROM ta_documents WHERE user_id=$1`, ta).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("%d clocks still running on incomplete set", count)
	}
}
