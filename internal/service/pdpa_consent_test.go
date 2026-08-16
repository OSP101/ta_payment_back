package service

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

func pdpaConsentFixture(t *testing.T) (svc *DocsService, ctx context.Context, userID uuid.UUID) {
	t.Helper()
	pool := testutil.NewPool(t)
	svc = &DocsService{pool: pool, aud: audit.New(pool), store: newMemStore(), pii: testPIICipher(t)}
	ctx = context.Background()
	userID = uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active) VALUES ($1,$2,'ทดสอบ','ทีเอ',TRUE)`,
		userID, "pdpa-"+userID.String()+"@example.test"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return svc, ctx, userID
}

func TestHasPdpaConsent_FalseUntilRecorded(t *testing.T) {
	svc, ctx, userID := pdpaConsentFixture(t)

	ok, err := svc.HasPdpaConsent(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("HasPdpaConsent = true before any consent was recorded")
	}

	if err := svc.RecordPdpaConsent(ctx, userID, "127.0.0.1", "test-agent"); err != nil {
		t.Fatal(err)
	}

	ok, err = svc.HasPdpaConsent(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("HasPdpaConsent = false after RecordPdpaConsent succeeded")
	}
}

func TestRecordPdpaConsent_IdempotentAndAudits(t *testing.T) {
	svc, ctx, userID := pdpaConsentFixture(t)

	if err := svc.RecordPdpaConsent(ctx, userID, "127.0.0.1", "test-agent"); err != nil {
		t.Fatal(err)
	}
	// A second acceptance of the same version must not error (double-click,
	// retried request) — UNIQUE(user_id, version) + ON CONFLICT DO NOTHING.
	if err := svc.RecordPdpaConsent(ctx, userID, "127.0.0.1", "test-agent"); err != nil {
		t.Fatalf("second RecordPdpaConsent errored: %v", err)
	}

	var count int
	if err := svc.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM pdpa_consents WHERE user_id=$1`, userID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("pdpa_consents row count = %d, want 1", count)
	}

	var action, ip string
	if err := svc.pool.QueryRow(ctx,
		`SELECT action, ip::text FROM audit_logs WHERE action = 'user.pdpa_consent' AND entity_id = $1 ORDER BY at DESC LIMIT 1`,
		userID.String(),
	).Scan(&action, &ip); err != nil {
		t.Fatalf("expected an audit row for the consent: %v", err)
	}
	// audit_logs.ip is INET, which renders back as text with a /32 suffix.
	if ip != "127.0.0.1/32" {
		t.Errorf("audit ip = %q, want 127.0.0.1/32", ip)
	}
}

func TestUpsertProfile_RefusesWithoutPdpaConsent(t *testing.T) {
	svc, ctx, userID := pdpaConsentFixture(t)

	err := svc.UpsertProfile(ctx, userID, TAProfile{
		StudentID: "653020123-4", Prefix: "นาย", Phone: "0812345678",
		NationalID: "1234567890123",
	})
	if err == nil {
		t.Fatal("expected UpsertProfile to refuse without PDPA consent")
	}
	if ue, ok := err.(*UserError); !ok || ue.Status != 403 {
		t.Errorf("expected a 403 UserError, got %#v", err)
	}
}

func TestUpsertProfile_SucceedsAfterPdpaConsent(t *testing.T) {
	svc, ctx, userID := pdpaConsentFixture(t)
	if err := svc.RecordPdpaConsent(ctx, userID, "127.0.0.1", "test-agent"); err != nil {
		t.Fatal(err)
	}

	err := svc.UpsertProfile(ctx, userID, TAProfile{
		StudentID: "653020123-4", Prefix: "นาย", Phone: "0812345678",
		NationalID: "1234567890123",
		BankName:   "ธนาคารกสิกรไทย", AccountName: "นาย ทดสอบ ทีเอ", AccountNo: "1234567890",
		SignatureSVG: "<svg></svg>",
	})
	if err != nil {
		t.Fatalf("UpsertProfile after consent: %v", err)
	}
}
