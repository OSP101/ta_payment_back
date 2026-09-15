package service

import (
	"strings"
	"testing"
)

// A file with valid zip magic bytes but corrupt/truncated internals (still
// possible after the handler's extension+magic-byte gate in ImportExcel,
// which only rules out non-zip uploads) used to surface excelize's raw Go
// error ("zip: not a valid zip file" or similar) straight to the client.
// parseNormalizedSheet now wraps it — see TOR §3.3 ข.3-4.
func TestParseNormalizedSheet_WrapsCorruptZipInThai(t *testing.T) {
	// Valid zip local-file-header magic, followed by garbage — enough to pass
	// a magic-byte sniff but not enough to be a real .xlsx.
	corrupt := append([]byte("PK\x03\x04"), []byte("not actually a valid xlsx archive")...)

	_, _, _, err := parseNormalizedSheet(corrupt)
	if err == nil {
		t.Fatal("expected an error for a corrupt file, got nil")
	}
	ue, ok := err.(*UserError)
	if !ok {
		t.Fatalf("error type = %T, want *UserError (so the client gets a clean 4xx, not a 500 with a raw Go error)", err)
	}
	if !strings.Contains(ue.Msg, "เปิดไฟล์ Excel ไม่ได้") {
		t.Errorf("message = %q, want a Thai explanation", ue.Msg)
	}
	if strings.Contains(ue.Msg, "zip:") == false {
		// Not required, but the wrapped error should still carry SOME reason
		// past the Thai sentence, not swallow it entirely.
		t.Logf("note: wrapped message does not echo the underlying zip error: %q", ue.Msg)
	}
}
