package service

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

// A download limit is only worth having if it cannot be walked past, so these
// tests attack it rather than demonstrate it: exhaust it, race it, and try every
// route around it — a second officer, a new submission round, another TA. There
// is no way to restore a spent allowance, and one test holds the refusal message
// to that so it never starts implying otherwise.

func quotaFixture(t *testing.T) (*DocsService, uuid.UUID, uuid.UUID) {
	t.Helper()
	pool := testutil.NewPool(t)
	svc := &DocsService{pool: pool, aud: audit.New(pool)}

	ta := uuid.New()
	officer := uuid.New()
	for _, u := range []struct {
		id  uuid.UUID
		tag string
	}{{ta, "quota-ta"}, {officer, "quota-officer"}} {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO users (id, email, first_name, last_name, is_active)
			 VALUES ($1, $2, 'ทดสอบ', 'ผู้ใช้', TRUE)`,
			u.id, u.tag+"-"+u.id.String()+"@example.test"); err != nil {
			t.Fatalf("insert %s: %v", u.tag, err)
		}
	}
	return svc, officer, ta
}

func TestDocDownloadQuota_ExhaustsAfterTheLimit(t *testing.T) {
	svc, officer, ta := quotaFixture(t)
	ctx := context.Background()

	for i := 1; i <= maxDocDownloads; i++ {
		if err := svc.recordDocDownload(ctx, officer, ta, 1); err != nil {
			t.Fatalf("download %d of %d must be allowed, got %v", i, maxDocDownloads, err)
		}
	}

	err := svc.recordDocDownload(ctx, officer, ta, 1)
	if err == nil {
		t.Fatalf("download %d must be refused", maxDocDownloads+1)
	}
	if !strings.Contains(err.Error(), "ครบ") {
		t.Errorf("refusal should say the allowance is spent, got %q", err)
	}
}

// The whole point of a cap is that concurrency cannot defeat it. Under READ
// COMMITTED a plain count-then-insert lets two transactions both read "1 used"
// and both write; the advisory lock in recordDocDownload is what prevents it.
func TestDocDownloadQuota_SurvivesConcurrentClicks(t *testing.T) {
	svc, officer, ta := quotaFixture(t)
	ctx := context.Background()

	const attempts = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0

	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()
			if err := svc.recordDocDownload(ctx, officer, ta, 1); err == nil {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if granted != maxDocDownloads {
		t.Fatalf("%d of %d concurrent attempts were granted, want exactly %d",
			granted, attempts, maxDocDownloads)
	}
	var rows int
	if err := svc.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ta_doc_downloads WHERE user_id = $1 AND round = 1`, ta).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != maxDocDownloads {
		t.Fatalf("recorded %d downloads, want %d", rows, maxDocDownloads)
	}
}

// A second officer must not get their own allowance: the thing being rationed
// is copies of the TA's documents, not clicks per person.
func TestDocDownloadQuota_IsSharedAcrossOfficers(t *testing.T) {
	svc, officer, ta := quotaFixture(t)
	ctx := context.Background()

	for i := 0; i < maxDocDownloads; i++ {
		if err := svc.recordDocDownload(ctx, officer, ta, 1); err != nil {
			t.Fatalf("first officer download %d: %v", i+1, err)
		}
	}

	other := uuid.New()
	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active)
		 VALUES ($1, $2, 'อีกคน', 'ทดสอบ', TRUE)`,
		other, "quota-officer2-"+other.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := svc.recordDocDownload(ctx, other, ta, 1); err == nil {
		t.Fatal("a second officer must not get a fresh allowance")
	}
}

// A new submission round must NOT hand back the allowance. Approval means all
// three documents passed and an approved TA does not resubmit, so the only way a
// round changes is somebody rejecting a file deliberately — which would make
// reject-then-reapprove a way to mint two more copies of a national ID. The
// quota deliberately ignores `round` to close that.
func TestDocDownloadQuota_NewRoundDoesNotRestoreAllowance(t *testing.T) {
	svc, officer, ta := quotaFixture(t)
	ctx := context.Background()

	for i := 0; i < maxDocDownloads; i++ {
		if err := svc.recordDocDownload(ctx, officer, ta, 1); err != nil {
			t.Fatalf("round 1 download %d: %v", i+1, err)
		}
	}
	if err := svc.recordDocDownload(ctx, officer, ta, 2); err == nil {
		t.Fatal("a new round must not reopen the allowance — that is the bypass")
	}
}

// The refusal must not promise a way out that does not exist: there is none.
func TestDocDownloadQuota_RefusalPromisesNoRecovery(t *testing.T) {
	svc, officer, ta := quotaFixture(t)
	ctx := context.Background()

	for i := 0; i < maxDocDownloads; i++ {
		if err := svc.recordDocDownload(ctx, officer, ta, 1); err != nil {
			t.Fatalf("download %d: %v", i+1, err)
		}
	}
	err := svc.recordDocDownload(ctx, officer, ta, 1)
	if err == nil {
		t.Fatal("quota should be exhausted")
	}
	for _, forbidden := range []string{"ส่งใหม่", "รอบใหม่", "คืนโควตา"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("refusal offers %q as a way out, but none exists: %q", forbidden, err)
		}
	}
}

// One TA hitting the cap must not affect anyone else's downloads.
func TestDocDownloadQuota_IsPerTA(t *testing.T) {
	svc, officer, ta := quotaFixture(t)
	ctx := context.Background()

	for i := 0; i < maxDocDownloads; i++ {
		if err := svc.recordDocDownload(ctx, officer, ta, 1); err != nil {
			t.Fatalf("download %d: %v", i+1, err)
		}
	}

	otherTA := uuid.New()
	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active)
		 VALUES ($1, $2, 'ทีเอ', 'อีกคน', TRUE)`,
		otherTA, "quota-ta2-"+otherTA.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := svc.recordDocDownload(ctx, officer, otherTA, 1); err != nil {
		t.Fatalf("another TA's allowance must be untouched, got %v", err)
	}
}
