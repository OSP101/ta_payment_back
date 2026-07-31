package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/antivirus"
	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

// The three outcomes of a scan are clean / infected / could-not-scan, and the
// whole value of the control rests on the third not collapsing into the first.
// These tests hold that line, and check the thing that is easy to get wrong even
// when the policy is right: that a rejected file leaves nothing behind.

// countingStore wraps the package's existing memStore and records how many
// files actually reached storage — "was it rejected" and "did it get written
// anyway" are different questions, and only the second one matters here.
type countingStore struct {
	*memStore
	saved int
}

func (c *countingStore) Save(dir, name string, r io.Reader) (string, int64, error) {
	c.saved++
	return c.memStore.Save(dir, name, r)
}

type fakeScanner struct {
	err     error
	enabled bool
	calls   int
}

func (f *fakeScanner) Scan(context.Context, io.Reader) error { f.calls++; return f.err }
func (f *fakeScanner) Enabled() bool                         { return f.enabled }

func avFixture(t *testing.T, sc antivirus.Scanner) (*DocsService, *countingStore, uuid.UUID) {
	t.Helper()
	pool := testutil.NewPool(t)
	store := &countingStore{memStore: newMemStore()}
	svc := &DocsService{pool: pool, aud: audit.New(pool), store: store, av: sc}

	uid := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, first_name, last_name, is_active)
		 VALUES ($1, $2, 'สแกน', 'ทดสอบ', TRUE)`,
		uid, "av-"+uid.String()+"@example.test"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return svc, store, uid
}

func pdfBytes() []byte {
	return append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("x"), 400)...)
}

func TestUpload_CleanFileIsStored(t *testing.T) {
	sc := &fakeScanner{enabled: true}
	svc, store, uid := avFixture(t, sc)

	body := pdfBytes()
	if _, err := svc.Upload(context.Background(), uid, "national_id", "id.pdf",
		"application/pdf", int64(len(body)), bytes.NewReader(body)); err != nil {
		t.Fatalf("clean upload must succeed: %v", err)
	}
	if sc.calls != 1 {
		t.Errorf("scanner called %d times, want 1", sc.calls)
	}
	if store.saved != 1 {
		t.Errorf("stored %d files, want 1", store.saved)
	}
}

// An infected file must never reach storage. Storing then quarantining would
// mean the bytes were written, indexed and briefly downloadable.
func TestUpload_InfectedFileIsRejectedAndNeverStored(t *testing.T) {
	sc := &fakeScanner{enabled: true, err: &antivirus.ErrInfected{Signature: "Eicar-Test-Signature"}}
	svc, store, uid := avFixture(t, sc)

	body := pdfBytes()
	_, err := svc.Upload(context.Background(), uid, "national_id", "id.pdf",
		"application/pdf", int64(len(body)), bytes.NewReader(body))
	if err == nil {
		t.Fatal("infected upload must be refused")
	}
	if store.saved != 0 {
		t.Errorf("infected file reached storage (%d saves)", store.saved)
	}
	// The signature is for the audit trail, not for the uploader — telling them
	// which rule fired only helps them iterate around it.
	if strings.Contains(err.Error(), "Eicar") {
		t.Errorf("refusal leaks the signature name: %q", err)
	}
	var rows int
	if e := svc.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM ta_documents WHERE user_id = $1`, uid).Scan(&rows); e != nil {
		t.Fatal(e)
	}
	if rows != 0 {
		t.Errorf("infected upload created %d document rows", rows)
	}
}

// FAIL-CLOSED. If the scanner cannot answer, the upload is refused. Treating a
// broken scanner as a clean result disables the control precisely when nobody is
// watching.
func TestUpload_ScannerUnavailableRejects(t *testing.T) {
	sc := &fakeScanner{enabled: true, err: errors.New("dial tcp 127.0.0.1:3310: connection refused")}
	svc, store, uid := avFixture(t, sc)

	body := pdfBytes()
	_, err := svc.Upload(context.Background(), uid, "national_id", "id.pdf",
		"application/pdf", int64(len(body)), bytes.NewReader(body))
	if err == nil {
		t.Fatal("upload must be refused when the scan cannot complete")
	}
	var ue *UserError
	if !errors.As(err, &ue) || ue.Status != 503 {
		t.Errorf("want a 503 UserError so the client can distinguish an outage, got %#v", err)
	}
	if store.saved != 0 {
		t.Errorf("unscanned file reached storage (%d saves)", store.saved)
	}
}

// With no scanner configured the upload proceeds — otherwise no environment
// could run without clamd. The warning for this lives at startup, not here.
func TestUpload_NoScannerConfiguredStillWorks(t *testing.T) {
	sc := &fakeScanner{enabled: false, err: errors.New("must not be consulted")}
	svc, store, uid := avFixture(t, sc)

	body := pdfBytes()
	if _, err := svc.Upload(context.Background(), uid, "national_id", "id.pdf",
		"application/pdf", int64(len(body)), bytes.NewReader(body)); err != nil {
		t.Fatalf("upload must work with scanning disabled: %v", err)
	}
	if sc.calls != 0 {
		t.Errorf("disabled scanner was called %d times", sc.calls)
	}
	if store.saved != 1 {
		t.Errorf("stored %d files, want 1", store.saved)
	}
}

// A client can lie about Content-Length, so the cap has to hold against the
// bytes actually sent — and it must do so without buffering them all first.
func TestUpload_OversizeBodyRejectedEvenWhenSizeHeaderLies(t *testing.T) {
	sc := &fakeScanner{enabled: true}
	svc, store, uid := avFixture(t, sc)

	body := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("x"), int(maxDocBytes)+1024)...)
	_, err := svc.Upload(context.Background(), uid, "national_id", "id.pdf",
		"application/pdf", 100 /* lie */, bytes.NewReader(body))
	if err == nil {
		t.Fatal("oversized body must be refused despite a small declared size")
	}
	if !strings.Contains(err.Error(), maxDocMBLabel+" MB") {
		t.Errorf("refusal should quote the limit, got %q", err)
	}
	if store.saved != 0 {
		t.Errorf("oversized file reached storage (%d saves)", store.saved)
	}
}

// Non-PDF content must be refused before the scanner is even consulted: the
// cheap structural check comes first.
func TestUpload_NonPDFRejectedBeforeScanning(t *testing.T) {
	sc := &fakeScanner{enabled: true}
	svc, store, uid := avFixture(t, sc)

	body := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, bytes.Repeat([]byte("x"), 100)...)
	if _, err := svc.Upload(context.Background(), uid, "national_id", "id.pdf",
		"application/pdf", int64(len(body)), bytes.NewReader(body)); err == nil {
		t.Fatal("a PNG renamed .pdf must be refused")
	}
	if sc.calls != 0 {
		t.Errorf("scanner ran on a file already known to be the wrong type (%d calls)", sc.calls)
	}
	if store.saved != 0 {
		t.Errorf("rejected file reached storage (%d saves)", store.saved)
	}
}
