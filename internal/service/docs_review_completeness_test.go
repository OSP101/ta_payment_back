package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

// The review queue is filtered to TAs who have submitted all three documents.
//
// The bug this closes: saving the step-1 profile form sets status='submitted'
// before any file exists, so the queue listed people with nothing to review. The
// tests below pin the boundary from both sides, and pin the property that makes
// the change safe — that nobody can fall out of BOTH buckets.

// newTAUser creates an active user holding the ta role and NOTHING else — no
// profile row. This is the state of a TA who has never opened the form, and the
// one a profile-driven query cannot see.
func newTAUser(t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	ta := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active)
		 VALUES ($1, $2, $3, 'ทดสอบ', TRUE)`,
		ta, "rev-"+ta.String()+"@example.test", name); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_roles (user_id, role) VALUES ($1, 'ta')`, ta); err != nil {
		t.Fatalf("grant ta role: %v", err)
	}
	return ta
}

// docReviewFixture creates a TA whose profile is submitted, with `kinds` uploaded.
func docReviewFixture(t *testing.T, pool *pgxpool.Pool, name string, kinds ...string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	ta := newTAUser(t, pool, name)
	if _, err := pool.Exec(ctx,
		`INSERT INTO ta_profiles (user_id, prefix, status, completed_at, current_round)
		 VALUES ($1, 'นาย', 'submitted', NOW(), 1)`, ta); err != nil {
		t.Fatalf("insert profile: %v", err)
	}
	for _, kind := range kinds {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO ta_documents
			  (id, user_id, kind, filename, mime, size_bytes, storage_key, status, round)
			VALUES ($1,$2,$3,$4,'application/pdf',1,$5,'submitted',1)`,
			id, ta, kind, kind+".pdf", "key/"+id.String()); err != nil {
			t.Fatalf("insert doc %s: %v", kind, err)
		}
	}
	return ta
}

func listIDs(t *testing.T, svc *DocsService, bucket string) map[uuid.UUID]PendingProfile {
	t.Helper()
	rows, err := svc.ListReview(context.Background(), bucket)
	if err != nil {
		t.Fatalf("ListReview(%q): %v", bucket, err)
	}
	out := map[uuid.UUID]PendingProfile{}
	for _, r := range rows {
		out[r.UserID] = r
	}
	return out
}

func completenessSvc(t *testing.T) *DocsService {
	t.Helper()
	pool := testutil.NewPool(t)
	return &DocsService{pool: pool, aud: audit.New(pool)}
}

func TestListReview_PendingOnlyIncludesCompleteSubmissions(t *testing.T) {
	svc := completenessSvc(t)

	none := docReviewFixture(t, svc.pool, "ยังไม่ส่ง")
	partial := docReviewFixture(t, svc.pool, "ส่งสองใบ", "national_id", "bank_book")
	full := docReviewFixture(t, svc.pool, "ส่งครบ", requiredDocKinds...)

	pending := listIDs(t, svc, "pending")
	if _, ok := pending[full]; !ok {
		t.Error("a TA with all three documents must appear in the review queue")
	}
	if _, ok := pending[none]; ok {
		t.Error("a TA who only saved the profile form is in the queue — this is the bug")
	}
	if _, ok := pending[partial]; ok {
		t.Error("a TA with 2 of 3 documents is in the queue; the officer would open an incomplete set")
	}
}

// The complement. Hiding people is only acceptable because they are still
// reachable — otherwise a TA who uploads two files and stops is invisible to
// staff forever.
func TestListReview_IncompleteBucketHoldsExactlyTheHiddenPeople(t *testing.T) {
	svc := completenessSvc(t)

	none := docReviewFixture(t, svc.pool, "ยังไม่ส่ง")
	partial := docReviewFixture(t, svc.pool, "ส่งสองใบ", "creditor_form")
	full := docReviewFixture(t, svc.pool, "ส่งครบ", requiredDocKinds...)

	inc := listIDs(t, svc, "incomplete")
	if _, ok := inc[none]; !ok {
		t.Error("a TA with no documents must be reachable in the incomplete bucket")
	}
	if p, ok := inc[partial]; !ok {
		t.Error("a TA with 1 of 3 documents must be in the incomplete bucket")
	} else if p.DocsIn != 1 {
		t.Errorf("docs_in = %d, want 1 — the count is what tells staff whether to nudge", p.DocsIn)
	}
	if _, ok := inc[full]; ok {
		t.Error("a complete submission leaked into the incomplete bucket")
	}
}

// A TA who has never saved the profile form owes three documents just as much as
// one who stalled at 2 of 3 — arguably more. They have no ta_profiles row, so
// anything driven off that table cannot see them at all.
func TestListReview_IncompleteIncludesTAsWhoNeverStarted(t *testing.T) {
	svc := completenessSvc(t)

	never := newTAUser(t, svc.pool, "ไม่เคยเริ่ม")
	stalled := docReviewFixture(t, svc.pool, "ค้างใบเดียว", "national_id", "bank_book")
	done := docReviewFixture(t, svc.pool, "ส่งครบ", requiredDocKinds...)

	inc := listIDs(t, svc, "incomplete")
	p, ok := inc[never]
	if !ok {
		t.Fatal("a TA with no profile row is missing from the incomplete list — staff cannot chase who they cannot see")
	}
	if p.DocsIn != 0 {
		t.Errorf("docs_in = %d, want 0", p.DocsIn)
	}
	if p.Status != "not_started" {
		t.Errorf("status = %q, want %q so the FE can distinguish never-started from stalled", p.Status, "not_started")
	}
	if _, ok := inc[stalled]; !ok {
		t.Error("the stalled TA must still be listed alongside them")
	}
	if _, ok := inc[done]; ok {
		t.Error("a complete submission leaked into the chase list")
	}

	// They must NOT appear in the review queue: there is nothing to review.
	if _, ok := listIDs(t, svc, "pending")[never]; ok {
		t.Error("a TA who never started showed up in the review queue")
	}
}

// Only TAs. The chase list is driven off the role, so a lecturer or an officer
// — who also have no documents — must not be swept in.
func TestListReview_IncompleteOnlyCoversTAs(t *testing.T) {
	svc := completenessSvc(t)
	ctx := context.Background()

	lecturer := uuid.New()
	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active)
		 VALUES ($1, $2, 'อาจารย์', 'ทดสอบ', TRUE)`,
		lecturer, "lec-"+lecturer.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO user_roles (user_id, role) VALUES ($1, 'lecturer')`, lecturer); err != nil {
		t.Fatal(err)
	}

	if _, ok := listIDs(t, svc, "incomplete")[lecturer]; ok {
		t.Error("a lecturer was listed as owing TA documents")
	}
}

// Deactivated accounts are not chased. Someone who has left is not a task.
func TestListReview_IncompleteSkipsInactiveUsers(t *testing.T) {
	svc := completenessSvc(t)
	ctx := context.Background()

	gone := newTAUser(t, svc.pool, "ลาออก")
	if _, err := svc.pool.Exec(ctx, `UPDATE users SET is_active = FALSE WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}

	if _, ok := listIDs(t, svc, "incomplete")[gone]; ok {
		t.Error("an inactive user is on the chase list")
	}
}

// Union property: pending ∪ incomplete must cover every submitted profile. This
// is what makes the filter a partition rather than a hole.
func TestListReview_PendingAndIncompletePartitionSubmitted(t *testing.T) {
	svc := completenessSvc(t)
	ctx := context.Background()

	want := []uuid.UUID{
		docReviewFixture(t, svc.pool, "ก"),
		docReviewFixture(t, svc.pool, "ข", "national_id"),
		docReviewFixture(t, svc.pool, "ค", "national_id", "bank_book"),
		docReviewFixture(t, svc.pool, "ง", requiredDocKinds...),
	}

	seen := map[uuid.UUID]int{}
	for _, b := range []string{"pending", "incomplete"} {
		for id := range listIDs(t, svc, b) {
			seen[id]++
		}
	}
	for _, id := range want {
		switch seen[id] {
		case 0:
			t.Errorf("%s appears in neither bucket — they are invisible to staff", id)
		case 1: // correct
		default:
			t.Errorf("%s appears in %d buckets; the two must be disjoint", id, seen[id])
		}
	}

	// And the dashboard card must agree with the pending page, or it links to a
	// list that does not match its own number.
	var card int
	if err := svc.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ta_profiles p
		 WHERE p.status IN ('submitted','needs_fix') AND `+ProfileDocsInSQL("p.user_id")+` = 3`).Scan(&card); err != nil {
		t.Fatal(err)
	}
	if got := len(listIDs(t, svc, "pending")); card != got {
		t.Errorf("dashboard card counts %d, pending page shows %d", card, got)
	}
}

// A rejected document still counts as submitted: its owner has to stay in the
// queue, because the officer who rejected it is the one who needs to see the
// replacement land. Only a MISSING document hides someone.
func TestListReview_RejectedDocumentStillCountsAsSubmitted(t *testing.T) {
	svc := completenessSvc(t)
	ctx := context.Background()

	ta := docReviewFixture(t, svc.pool, "ถูกตีกลับ", requiredDocKinds...)
	if _, err := svc.pool.Exec(ctx,
		`UPDATE ta_documents SET status='rejected', reject_reason='ไม่ชัด'
		 WHERE user_id=$1 AND kind='bank_book'`, ta); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.pool.Exec(ctx,
		`UPDATE ta_profiles SET status='needs_fix' WHERE user_id=$1`, ta); err != nil {
		t.Fatal(err)
	}

	if _, ok := listIDs(t, svc, "pending")[ta]; !ok {
		t.Error("a TA with a rejected document dropped out of the queue")
	}
}

// Superseding is how a re-upload replaces a file, and for a moment in that flow
// the old row is superseded before the new one exists. Such a TA is genuinely
// incomplete and must be treated as such — counting superseded rows would let a
// TA appear complete with a file that no longer exists.
func TestListReview_SupersededDocumentDoesNotCount(t *testing.T) {
	svc := completenessSvc(t)
	ctx := context.Background()

	ta := docReviewFixture(t, svc.pool, "กำลังส่งใหม่", requiredDocKinds...)
	if _, err := svc.pool.Exec(ctx,
		`UPDATE ta_documents SET superseded_at = NOW()
		 WHERE user_id=$1 AND kind='national_id'`, ta); err != nil {
		t.Fatal(err)
	}

	if _, ok := listIDs(t, svc, "pending")[ta]; ok {
		t.Error("a superseded document was counted as present")
	}
	if p, ok := listIDs(t, svc, "incomplete")[ta]; !ok {
		t.Error("expected them in the incomplete bucket instead")
	} else if p.DocsIn != 2 {
		t.Errorf("docs_in = %d, want 2", p.DocsIn)
	}
}
