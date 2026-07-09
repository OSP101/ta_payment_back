package storage

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// TestEncryptedRoundtrip ensures TA docs written through the encrypted store
// come back byte-identical when re-opened but are unreadable on disk. This is
// the guarantee PDPA / policy demands for the national-ID copy + bank book.
func TestEncryptedRoundtrip(t *testing.T) {
	root := t.TempDir()
	key := newTestKey(t)
	st, err := NewLocalWithKey(root, key)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	plain := []byte("SECRET national id copy — must not be readable at rest")
	storageKey, size, err := st.Save("ta_docs", "id.pdf", bytes.NewReader(plain))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if int(size) != len(plain) {
		t.Errorf("reported size %d, want %d", size, len(plain))
	}
	if !strings.HasSuffix(storageKey, ".enc") {
		t.Errorf("encrypted files should carry .enc suffix, got %q", storageKey)
	}

	// Raw bytes on disk must not contain plaintext.
	on, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(storageKey)))
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if bytes.Contains(on, plain) {
		t.Error("plaintext leaked into encrypted blob on disk")
	}

	rc, err := st.Open(storageKey)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read decrypted: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("roundtrip mismatch: got %q", got)
	}
}

// TestPlainWhenNoKey confirms that when no key is configured (dev/test
// bootstrap), files are stored unencrypted so the existing code path still
// works. This matches the "warn but boot" behavior in main.go.
func TestPlainWhenNoKey(t *testing.T) {
	root := t.TempDir()
	st, err := NewLocal(root)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	plain := []byte("hello world")
	storageKey, _, err := st.Save("ta_docs", "note.pdf", bytes.NewReader(plain))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if strings.HasSuffix(storageKey, ".enc") {
		t.Errorf("plaintext file should not carry .enc suffix, got %q", storageKey)
	}
	rc, err := st.Open(storageKey)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("mismatch: got %q", got)
	}
}

func TestRejectsBadExtension(t *testing.T) {
	root := t.TempDir()
	st, _ := NewLocal(root)
	if _, _, err := st.Save("ta_docs", "shady.exe", bytes.NewReader([]byte("payload"))); err == nil {
		t.Error("expected .exe upload to be rejected")
	}
}

func TestParseKey(t *testing.T) {
	// hex
	if _, err := ParseKeyFromBase64(strings.Repeat("a", 64)); err != nil {
		t.Errorf("hex key: %v", err)
	}
	// b64 wrong length
	if _, err := ParseKeyFromBase64("dGVzdA=="); err == nil {
		t.Error("expected error on short key")
	}
	// empty
	if _, err := ParseKeyFromBase64(""); err == nil {
		t.Error("expected error on empty key")
	}
}
