package service

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/testutil"
)

// Until 15/09/2026 (TOR §3.7 ข.7, found during the acceptance review)
// DocsService had no NotifyService at all — a rejected document or profile
// was silent, and the TA only found out by opening /ta/documents on their own
// initiative. These tests exercise the fix: one notification per REVIEW
// ACTION, not per document, so approving three files in a row (or rejecting
// them in one batch) does not spam the bell — see NotifyService.Send's
// unread-coalesce behaviour, which this relies on rather than re-implements.

// docsNotifyFixture returns (svc, taID, officerID, docsByKind). officerID is a
// real users row — ta_documents.reviewed_by and ta_profiles.verified_by are
// foreign keys, so a bare uuid.New() actor (fine for tests that never read
// the FK-checked column back) is not enough once Review notifies and every
// review path here writes reviewed_by/verified_by.
func docsNotifyFixture(t *testing.T) (*DocsService, uuid.UUID, uuid.UUID, map[string]uuid.UUID) {
	t.Helper()
	pool := testutil.NewPool(t)
	svc := &DocsService{
		pool: pool, aud: audit.New(pool),
		notify: &NotifyService{pool: pool, mailer: mail.New(config.Config{})},
	}
	ctx := context.Background()

	ta := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active)
		 VALUES ($1, $2, 'แจ้งเตือน', 'ทดสอบ', TRUE)`,
		ta, "notify-"+ta.String()+"@example.test"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	officer := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active)
		 VALUES ($1, $2, 'จ.น.', 'ทดสอบ', TRUE)`,
		officer, "notify-officer-"+officer.String()+"@example.test"); err != nil {
		t.Fatalf("insert officer: %v", err)
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
	return svc, ta, officer, docs
}

func notificationTitles(t *testing.T, svc *DocsService, ta uuid.UUID) []string {
	t.Helper()
	rows, err := svc.pool.Query(context.Background(),
		`SELECT title FROM notifications WHERE user_id = $1 AND channel = 'in_app' ORDER BY created_at`, ta)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var title string
		if err := rows.Scan(&title); err != nil {
			t.Fatal(err)
		}
		out = append(out, title)
	}
	return out
}

func TestReview_RejectNotifiesWithReason(t *testing.T) {
	svc, ta, actor, docs := docsNotifyFixture(t)
	ctx := context.Background()

	if err := svc.Review(ctx, actor, docs["national_id"], false, "รูปเบลอ อ่านเลขไม่ได้"); err != nil {
		t.Fatalf("Review reject: %v", err)
	}

	titles := notificationTitles(t, svc, ta)
	if len(titles) != 1 {
		t.Fatalf("got %d notifications, want 1: %v", len(titles), titles)
	}
	want := "เอกสารต้องแก้ไข สำเนาบัตรประจำตัวประชาชน"
	if titles[0] != want {
		t.Errorf("title = %q, want %q", titles[0], want)
	}

	var body string
	if err := svc.pool.QueryRow(ctx,
		`SELECT body FROM notifications WHERE user_id = $1 AND channel = 'in_app'`, ta).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "เนื่องจาก รูปเบลอ อ่านเลขไม่ได้") {
		t.Errorf("body = %q, want it to give the rejection reason", body)
	}
}

func TestReview_ApproveNotifiesPerDocument(t *testing.T) {
	svc, ta, actor, docs := docsNotifyFixture(t)
	ctx := context.Background()

	// Approve one of three — not the profile-completing one — so this checks
	// the per-document notice, not the auto-approval notice.
	if err := svc.Review(ctx, actor, docs["national_id"], true, ""); err != nil {
		t.Fatalf("Review approve: %v", err)
	}

	titles := notificationTitles(t, svc, ta)
	if len(titles) != 1 {
		t.Fatalf("got %d notifications, want 1: %v", len(titles), titles)
	}
	want := "เอกสารผ่านการตรวจสอบ สำเนาบัตรประจำตัวประชาชน"
	if titles[0] != want {
		t.Errorf("title = %q, want %q", titles[0], want)
	}
}

func TestReview_LastApprovalAlsoNotifiesProfileComplete(t *testing.T) {
	svc, ta, actor, docs := docsNotifyFixture(t)
	ctx := context.Background()

	if err := svc.Review(ctx, actor, docs["national_id"], true, ""); err != nil {
		t.Fatalf("approve 1: %v", err)
	}
	if err := svc.Review(ctx, actor, docs["bank_book"], true, ""); err != nil {
		t.Fatalf("approve 2: %v", err)
	}
	if err := svc.Review(ctx, actor, docs["creditor_form"], true, ""); err != nil {
		t.Fatalf("approve 3 (completes the set): %v", err)
	}

	titles := notificationTitles(t, svc, ta)
	found := false
	for _, ti := range titles {
		if ti == "ข้อมูลส่วนตัวผ่านการตรวจสอบแล้ว" {
			found = true
		}
	}
	if !found {
		t.Errorf("no profile-complete notification among %v", titles)
	}
}

func TestApproveAll_SendsExactlyOneNotification(t *testing.T) {
	svc, ta, actor, _ := docsNotifyFixture(t)
	ctx := context.Background()

	if _, err := svc.ApproveAll(ctx, actor, ta); err != nil {
		t.Fatalf("ApproveAll: %v", err)
	}

	titles := notificationTitles(t, svc, ta)
	if len(titles) != 1 {
		t.Fatalf("got %d notifications for approving 3 docs + profile in one call, want 1: %v",
			len(titles), titles)
	}
	if titles[0] != "เอกสารผ่านการตรวจสอบแล้ว" {
		t.Errorf("title = %q", titles[0])
	}
}

func TestRejectBatch_SendsOneNotificationNotOnePerFile(t *testing.T) {
	svc, ta, actor, docs := docsNotifyFixture(t)
	ctx := context.Background()

	items := []RejectItem{
		{DocID: docs["national_id"], Reason: "ภาพเบลอ"},
		{DocID: docs["bank_book"], Reason: "ไม่เห็นเลขบัญชี"},
		{DocID: docs["creditor_form"], Reason: "ลายเซ็นไม่ตรง"},
	}
	if err := svc.RejectBatch(ctx, actor, ta, items); err != nil {
		t.Fatalf("RejectBatch: %v", err)
	}

	titles := notificationTitles(t, svc, ta)
	if len(titles) != 1 {
		t.Fatalf("got %d notifications for rejecting 3 files in one batch, want 1 (must not spam "+
			"one per file): %v", len(titles), titles)
	}
	if titles[0] != "เอกสารต้องแก้ไข 3 รายการ" {
		t.Errorf("title = %q", titles[0])
	}
}

func TestReviewProfile_NotifiesBothOutcomes(t *testing.T) {
	svc, ta, actor, docs := docsNotifyFixture(t)
	ctx := context.Background()

	if err := svc.ReviewProfile(ctx, actor, ta, false, "ข้อมูลไม่ตรงกับเอกสาร"); err != nil {
		t.Fatalf("ReviewProfile reject: %v", err)
	}
	titles := notificationTitles(t, svc, ta)
	if len(titles) != 1 || titles[0] != "ข้อมูลส่วนตัวต้องแก้ไข" {
		t.Fatalf("after reject: titles = %v", titles)
	}

	// Approve requires all three docs to already be approved (RULE C6).
	for _, id := range docs {
		if err := svc.pool.QueryRow(ctx,
			`UPDATE ta_documents SET status='approved' WHERE id=$1 RETURNING id`, id).Scan(new(uuid.UUID)); err != nil {
			t.Fatalf("pre-approve doc: %v", err)
		}
	}
	if err := svc.ReviewProfile(ctx, actor, ta, true, ""); err != nil {
		t.Fatalf("ReviewProfile approve: %v", err)
	}
	titles = notificationTitles(t, svc, ta)
	last := titles[len(titles)-1]
	if last != "ข้อมูลส่วนตัวผ่านการตรวจสอบแล้ว" {
		t.Errorf("after approve: last title = %q", last)
	}
}
