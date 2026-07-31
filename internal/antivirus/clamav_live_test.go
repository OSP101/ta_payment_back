package antivirus

// Live integration test against a real clamd. Skipped unless CLAMAV_ADDR is set,
// so the normal test run stays hermetic:
//
//	CLAMAV_ADDR=127.0.0.1:3310 go test ./internal/antivirus/ -v
//
// This is the only test that proves the wire protocol is right. The fake scanner
// used elsewhere proves the POLICY (fail-closed, scan-before-store) but would
// happily pass with a client that never speaks correct INSTREAM.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// eicar builds the standard antivirus test string at runtime rather than storing
// it as a literal. A source file containing the literal is itself detected by
// most scanners, which would mean this repository could not be checked out onto
// a machine running antivirus without being quarantined.
func eicar() []byte {
	parts := []string{
		`X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-`,
		`STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`,
	}
	return []byte(strings.Join(parts, ""))
}

func liveScanner(t *testing.T) Scanner {
	t.Helper()
	addr := os.Getenv("CLAMAV_ADDR")
	if addr == "" {
		t.Skip("set CLAMAV_ADDR=host:port to run the live clamd test")
	}
	sc := New(addr, 30*time.Second)
	if !sc.Enabled() {
		t.Fatalf("New(%q) returned a disabled scanner", addr)
	}
	return sc
}

func TestLive_EicarIsDetected(t *testing.T) {
	sc := liveScanner(t)

	err := sc.Scan(context.Background(), bytes.NewReader(eicar()))
	if err == nil {
		t.Fatal("clamd reported the EICAR test file as clean — the signature database is not loaded, or the reply is being misparsed")
	}
	var infected *ErrInfected
	if !errors.As(err, &infected) {
		t.Fatalf("want *ErrInfected, got %T: %v — a scan failure and a detection must not look alike", err, err)
	}
	if !strings.Contains(strings.ToLower(infected.Signature), "eicar") {
		t.Errorf("signature = %q, expected it to name Eicar", infected.Signature)
	}
	t.Logf("detected signature: %s", infected.Signature)
}

// The happy path has to be proven too: a client that returns ErrInfected for
// everything would pass the test above.
func TestLive_CleanFileIsClean(t *testing.T) {
	sc := liveScanner(t)

	body := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("harmless payload "), 500)...)
	if err := sc.Scan(context.Background(), bytes.NewReader(body)); err != nil {
		t.Fatalf("clean file was not reported clean: %v", err)
	}
}

// EICAR embedded in a larger file is NOT detected — and that is ClamAV's
// behaviour, not a defect here.
//
// The EICAR signature is hash-based on the exact 68-byte file, so any padding
// changes the hash and the match is lost. Verified independently with clamscan
// inside the container, which bypasses this client entirely:
//
//	/tmp/pure.txt:     Eicar-Test-Signature FOUND
//	/tmp/embedded.pdf: OK
//
// This test pins that fact so nobody later "fixes" a bug that does not exist by
// rewriting the chunking. It also states the real consequence plainly: EICAR
// cannot be used to prove that embedded payloads are caught. Multi-chunk
// transmission was proven separately, with a temporary pattern signature loaded
// into clamd, detecting a probe string at byte 0, 65k, 196k and 524k.
func TestLive_EmbeddedEicarIsNotDetected_ByDesign(t *testing.T) {
	sc := liveScanner(t)

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.7\n")
	buf.Write(bytes.Repeat([]byte("padding "), 200))
	buf.Write(eicar())
	buf.Write(bytes.Repeat([]byte(" padding"), 200))
	buf.WriteString("\n%%EOF\n")

	err := sc.Scan(context.Background(), bytes.NewReader(buf.Bytes()))
	if err != nil {
		// If a future ClamAV gains a pattern-based EICAR signature this starts
		// failing. That is good news, not a regression — relax the assertion.
		t.Fatalf("embedded EICAR is now detected (%v); ClamAV behaviour changed, update this test", err)
	}
}

// Multi-chunk path: a length-prefix bug in the INSTREAM framing loop would show
// up as a clean result on a file larger than chunkSize. This only proves the
// large file scans without error — proving every chunk ARRIVES needs a
// pattern-based signature, which EICAR is not (see the test above).
func TestLive_LargeFileScansWithoutError(t *testing.T) {
	sc := liveScanner(t)

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.7\n")
	// Deliberately not a chunk multiple, so the final short chunk is exercised.
	buf.Write(bytes.Repeat([]byte("A"), chunkSize*3+17))
	if err := sc.Scan(context.Background(), bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("multi-chunk clean file failed: %v", err)
	}
}

// A scanner pointed at nothing must report a failure, not a clean result — this
// is what makes the fail-closed policy in service.scanUpload meaningful.
func TestLive_UnreachableDaemonIsAnError(t *testing.T) {
	// Port 1 is reserved and never listening.
	sc := New("127.0.0.1:1", 2*time.Second)
	err := sc.Scan(context.Background(), bytes.NewReader([]byte("%PDF-1.7\n")))
	if err == nil {
		t.Fatal("unreachable clamd reported clean")
	}
	var infected *ErrInfected
	if errors.As(err, &infected) {
		t.Fatalf("connection failure misreported as a detection: %v", err)
	}
}
