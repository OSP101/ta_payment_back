package service

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"ta-payment-back/internal/audit"
)

// backdateAudit writes a row and moves it into the past.
//
// Two statements because `at` defaults to NOW() and the table refuses UPDATE —
// so the row is inserted with an explicit `at` instead of being moved
// afterwards, which the append-only trigger would (correctly) block.
func backdateAudit(t *testing.T, f *fixture, action, ago string) {
	t.Helper()
	f.exec(`INSERT INTO audit_logs (at, action, entity, entity_id)
	        VALUES (NOW() - $1::interval, $2, 'thing', 'x')`, ago, action)
}

func auditSvcWithStore(f *fixture) (*AuditService, *memStore) {
	m := newMemStore()
	return &AuditService{pool: f.Pool, store: m}, m
}

// The retention period is enforced by the DATABASE, not by the purge job.
// Dropping the trigger to purge and putting it back would open a window in
// which every row is deletable — and the purge is the one moment something
// going wrong would be least visible.
func TestAuditRetention_DatabaseRefusesToDeleteAnythingInsideTheWindow(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	backdateAudit(t, f, "probe.recent", "1 year")
	backdateAudit(t, f, "probe.expired", "6 years")

	// A purge job with a mistaken WHERE clause is the threat model here.
	if _, err := f.Pool.Exec(f.ctx, `DELETE FROM audit_logs WHERE action = 'probe.recent'`); err == nil {
		t.Error("a row one year old was deleted — the retention rule is not enforced by the database")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("delete refused with %v, want the append-only guard", err)
	}

	// …and a genuinely expired row IS removable, or retention could never run.
	if _, err := f.Pool.Exec(f.ctx, `DELETE FROM audit_logs WHERE action = 'probe.expired'`); err != nil {
		t.Errorf("a six-year-old row could not be deleted: %v — retention can never run", err)
	}

	// UPDATE stays refused at any age. Nothing legitimate rewrites history.
	if _, err := f.Pool.Exec(f.ctx,
		`UPDATE audit_logs SET action='tampered' WHERE action='probe.recent'`); err == nil {
		t.Error("an audit row was rewritten; age must not make history editable")
	}
}

// Archive, verify, THEN delete. The rows must be readable back out of the
// store, because the difference between "the write returned no error" and "the
// bytes are there" only shows up years later with the originals gone.
func TestPurgeExpiredAudit_ArchivesEveryRowItDeletes(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc, store := auditSvcWithStore(f)
	for _, a := range []string{"probe.old1", "probe.old2", "probe.old3"} {
		backdateAudit(t, f, a, "6 years")
	}
	backdateAudit(t, f, "probe.keep", "1 year")

	res, err := svc.PurgeExpiredAudit(f.ctx, audit.New(f.Pool))
	if err != nil {
		t.Fatalf("PurgeExpiredAudit: %v", err)
	}
	if res.Deleted != 3 || res.Archived != 3 {
		t.Fatalf("archived %d, deleted %d, want 3 and 3", res.Archived, res.Deleted)
	}

	// The row inside retention is untouched.
	var kept int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM audit_logs WHERE action='probe.keep'`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Error("a row inside the retention window was purged")
	}

	// The archive holds the actual rows, one JSON object per line.
	rc, err := store.Open(res.StorageKey)
	if err != nil {
		t.Fatalf("archive not readable: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	for sc.Scan() {
		var row map[string]any
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatalf("archive line is not JSON: %v", err)
		}
		if a, ok := row["action"].(string); ok {
			seen[a] = true
		}
	}
	for _, a := range []string{"probe.old1", "probe.old2", "probe.old3"} {
		if !seen[a] {
			t.Errorf("%s was deleted but is not in the archive — the evidence is gone", a)
		}
	}
	if seen["probe.keep"] {
		t.Error("a row that was NOT deleted was written into the archive")
	}

	// The archive is registered with a checksum, so a later reader can tell
	// whether the file they have is the file that was written.
	var count int64
	var digest string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT row_count, sha256 FROM audit_archives ORDER BY created_at DESC LIMIT 1`).
		Scan(&count, &digest); err != nil {
		t.Fatalf("no audit_archives row: %v", err)
	}
	if count != 3 {
		t.Errorf("audit_archives.row_count = %d, want 3", count)
	}
	if digest != res.SHA256 || len(digest) != 64 {
		t.Errorf("checksum %q does not match the run's %q", digest, res.SHA256)
	}
}

// The purge records itself, in the table it just deleted from — which is
// append-only, so the account of the gap cannot itself be removed.
func TestPurgeExpiredAudit_RecordsItsOwnGap(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc, _ := auditSvcWithStore(f)
	backdateAudit(t, f, "probe.gone", "6 years")

	if _, err := svc.PurgeExpiredAudit(f.ctx, audit.New(f.Pool)); err != nil {
		t.Fatal(err)
	}
	var note string
	var after []byte
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT note, after FROM audit_logs WHERE action='audit_log.purge'`).Scan(&note, &after); err != nil {
		t.Fatalf("the purge left no record of itself: %v", err)
	}
	if !strings.Contains(note, "5 ปี") {
		t.Errorf("purge note = %q, want it to name the policy", note)
	}
	var got AuditPurgeResult
	if err := json.Unmarshal(after, &got); err != nil {
		t.Fatal(err)
	}
	if got.Deleted != 1 || got.SHA256 == "" {
		t.Errorf("purge record = %+v, want the count and the archive checksum", got)
	}
}

// Nothing expired means nothing written: an empty archive file every night
// would be noise in the store and a row in audit_archives that covers nothing.
func TestPurgeExpiredAudit_DoesNothingWhenNothingHasExpired(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc, store := auditSvcWithStore(f)
	backdateAudit(t, f, "probe.young", "1 year")

	res, err := svc.PurgeExpiredAudit(f.ctx, audit.New(f.Pool))
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 0 || res.StorageKey != "" {
		t.Errorf("purge did %+v on an up-to-date table, want nothing", res)
	}
	if len(store.files) != 0 {
		t.Errorf("%d archive file(s) written with nothing to archive", len(store.files))
	}
	var n int
	if err := f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM audit_archives`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d audit_archives row(s) covering nothing", n)
	}
}

// If the archive cannot be written, NOTHING is deleted. This is the ordering
// the whole file exists to guarantee.
func TestPurgeExpiredAudit_DeletesNothingWhenTheArchiveFails(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc, _ := auditSvcWithStore(f)
	svc.store = failingStore{}
	backdateAudit(t, f, "probe.survivor", "6 years")

	if _, err := svc.PurgeExpiredAudit(f.ctx, audit.New(f.Pool)); err == nil {
		t.Fatal("purge reported success with no archive written")
	}
	var n int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM audit_logs WHERE action='probe.survivor'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("rows were deleted even though the archive was never written — unrecoverable")
	}
}

// failingStore accepts the write and then cannot read it back, which is the
// failure a naive "did Save return an error?" check misses entirely.
type failingStore struct{ memStore }

func (failingStore) Save(kind, filename string, r io.Reader) (string, int64, error) {
	return "audit_archives/pretend", 42, nil
}
func (failingStore) Open(key string) (io.ReadCloser, error) { return nil, io.ErrUnexpectedEOF }
func (failingStore) Delete(key string) error                { return nil }
func (failingStore) Path(key string) string                 { return key }
func (failingStore) Encrypted() bool                        { return false }
