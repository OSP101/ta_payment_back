package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/antivirus"
	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

// End-to-end: the real Upload path against a real clamd. Skipped unless both
// CLAMAV_ADDR and AV_PROBE_SIG are set.
//
// The fake-scanner tests prove the policy and this proves the wiring — that the
// service actually talks to the daemon, actually refuses what it flags, and
// actually writes nothing when it does.
//
// AV_PROBE_SIG must be a string a temporary pattern signature has been loaded
// for. EICAR cannot be used here: the upload path requires a %PDF prefix, and
// EICAR's signature is hash-based on the exact 68-byte file, so a PDF containing
// it is not detected (see antivirus.TestLive_EmbeddedEicarIsNotDetected_ByDesign).
//
// Setup, and the trap in it:
//
//	PROBE=MY-PROBE-STRING
//	HEX=$(printf '%s' "$PROBE" | xxd -p | tr -d '\n')
//	docker exec ta_payment_clamav sh -c \
//	  "printf 'MyProbe:0:*:%s\n' '$HEX' > /var/lib/clamav/zz_probe.ndb; \
//	   printf 'zRELOAD\0' | nc -w 5 127.0.0.1 3310"
//	# WAIT. RELOAD is asynchronous and takes ~10s on a full database. Confirm
//	# with clamdscan BEFORE running this test, or it fails with "accepted" and
//	# looks exactly like a broken scan path:
//	docker exec ta_payment_clamav sh -c \
//	  "printf '%s' '$PROBE' > /tmp/p; clamdscan --no-summary /tmp/p"
//	CLAMAV_ADDR=127.0.0.1:3310 AV_PROBE_SIG="$PROBE" \
//	  go test ./internal/service/ -run TestLiveAV -v -count=1
//	# Then remove it again — a stray signature in the shared volume outlives the
//	# test run and will start flagging real uploads.
func TestLiveAV_InfectedUploadRefusedAndNotStored(t *testing.T) {
	addr := os.Getenv("CLAMAV_ADDR")
	probe := os.Getenv("AV_PROBE_SIG")
	if addr == "" || probe == "" {
		t.Skip("needs CLAMAV_ADDR and AV_PROBE_SIG")
	}

	pool := testutil.NewPool(t)
	store := &countingStore{memStore: newMemStore()}
	svc := &DocsService{
		pool: pool, aud: audit.New(pool), store: store,
		av: antivirus.New(addr, 30*time.Second),
	}
	ctx := context.Background()

	uid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active)
		 VALUES ($1, $2, 'สแกนจริง', 'ทดสอบ', TRUE)`,
		uid, "avlive-"+uid.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}

	// A clean PDF goes through the real scanner and is stored.
	clean := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("clean "), 500)...)
	if _, err := svc.Upload(ctx, uid, "national_id", "clean.pdf",
		"application/pdf", int64(len(clean)), bytes.NewReader(clean)); err != nil {
		t.Fatalf("clean upload refused by the live scanner: %v", err)
	}
	if store.saved != 1 {
		t.Fatalf("clean upload stored %d files, want 1", store.saved)
	}

	// A PDF carrying the flagged pattern, past the first chunk so the framing is
	// exercised too.
	var bad bytes.Buffer
	bad.WriteString("%PDF-1.7\n")
	bad.Write(bytes.Repeat([]byte("A"), 70_000))
	bad.WriteString(probe)
	bad.WriteString("\n%%EOF\n")

	before := store.saved
	_, err := svc.Upload(ctx, uid, "bank_book", "bad.pdf",
		"application/pdf", int64(bad.Len()), bytes.NewReader(bad.Bytes()))
	if err == nil {
		t.Fatal("flagged upload was accepted")
	}
	var ue *UserError
	if !errors.As(err, &ue) || ue.Status != 422 {
		t.Errorf("want 422 UserError (detection), got %#v", err)
	}
	if store.saved != before {
		t.Errorf("flagged file reached storage (%d -> %d saves)", before, store.saved)
	}
	var rows int
	if e := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ta_documents WHERE user_id=$1 AND kind='bank_book'`, uid).Scan(&rows); e != nil {
		t.Fatal(e)
	}
	if rows != 0 {
		t.Errorf("flagged upload created %d document rows", rows)
	}
	t.Logf("live scanner refused the flagged PDF: %v", err)
}
