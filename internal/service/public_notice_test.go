package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/testutil"
)

// The login page is anonymous, so its cards are only ever college-wide and
// only ever shown when there is something to say. These pin both halves.

func newNoticeSvc(t *testing.T) (*PublicNoticeService, context.Context, uuid.UUID) {
	t.Helper()
	pool := testutil.NewPool(t)
	ctx := context.Background()
	termID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO academic_terms (id, academic_year, semester, is_active) VALUES ($1, 2569, 2, TRUE)`, termID); err != nil {
		t.Fatalf("insert term: %v", err)
	}
	docs := &DocumentProgressService{pool: pool}
	return &PublicNoticeService{pool: pool, docs: docs}, ctx, termID
}

func TestPublicNotices_NothingToSayIsEmpty(t *testing.T) {
	svc, ctx, _ := newNoticeSvc(t)
	got, err := svc.compute(ctx, time.Now())
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if !got.empty() {
		t.Fatalf("no window and no document progress must give no cards, got %+v", got)
	}
}

func TestPublicNotices_RequestWindowOnlyWhileLive(t *testing.T) {
	svc, ctx, termID := newNoticeSvc(t)
	now := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	if _, err := svc.pool.Exec(ctx, `
		INSERT INTO ta_request_windows (term_id, opens_at, closes_at, is_open)
		VALUES ($1, $2, $3, TRUE)`, termID, now.AddDate(0, 0, -5), now.AddDate(0, 0, 9)); err != nil {
		t.Fatalf("insert window: %v", err)
	}

	got, err := svc.compute(ctx, now)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	w := got.RequestWindow
	if w == nil {
		t.Fatal("a live window must show a card")
	}
	if w.DaysLeft != 9 || w.TermLabel != "2569/2" {
		t.Fatalf("want 9 days left for 2569/2, got %+v", w)
	}

	after, err := svc.compute(ctx, now.AddDate(0, 0, 10))
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if after.RequestWindow != nil {
		t.Fatalf("a window past its closing time must not show, got %+v", after.RequestWindow)
	}

	if _, err := svc.pool.Exec(ctx, `UPDATE ta_request_windows SET is_open = FALSE`); err != nil {
		t.Fatal(err)
	}
	off, _ := svc.compute(ctx, now)
	if off.RequestWindow != nil {
		t.Fatal("a window staff switched off must not show")
	}
}

func TestPublicNotices_DocumentCardNeedsStageAndNamesNobody(t *testing.T) {
	svc, ctx, termID := newNoticeSvc(t)
	now := time.Now()

	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO document_progress (term_id, stage, updated_by_name) VALUES ($1, 0, 'ชื่อเจ้าหน้าที่ลับ')`, termID); err != nil {
		t.Fatalf("insert progress: %v", err)
	}
	got, _ := svc.compute(ctx, now)
	if len(got.Documents) != 0 {
		t.Fatalf("stage 0 must not show a card, got %+v", got.Documents)
	}

	if _, err := svc.pool.Exec(ctx, `UPDATE document_progress SET stage = 2, note = 'โน้ตภายใน'`); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.compute(ctx, now)
	if len(got.Documents) != 1 || got.Documents[0].Stage != 2 || got.Documents[0].StageLabel != "อาจารย์เซ็นครบ" {
		t.Fatalf("stage 2 must show as อาจารย์เซ็นครบ, got %+v", got.Documents)
	}
	b, _ := json.Marshal(got)
	for _, leak := range []string{"ชื่อเจ้าหน้าที่ลับ", "โน้ตภายใน", termID.String()} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("anonymous payload leaked %q: %s", leak, b)
		}
	}

	// A bundle that finished long ago steps aside.
	if _, err := svc.pool.Exec(ctx,
		`UPDATE document_progress SET stage = 5, updated_at = $1`, now.Add(-docDoneVisibleFor-time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.compute(ctx, now)
	if len(got.Documents) != 0 {
		t.Fatalf("a long-finished bundle must not show, got %+v", got.Documents)
	}
}
