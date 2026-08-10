package pii

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealOpenRoundtrip(t *testing.T) {
	c, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	aad := []byte("user-123")
	plaintext := []byte("1234567890123")

	sealed, err := c.Seal(aad, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Open(aad, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("Open = %q, want %q", got, plaintext)
	}
}

// Ciphertext must never equal the plaintext, and must not even contain it as
// a substring — a weak or absent cipher could still "encrypt" by just
// prefixing bytes.
func TestSealActuallyObscuresThePlaintext(t *testing.T) {
	c, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("1234567890123")
	sealed, err := c.Seal([]byte("aad"), plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, plaintext) {
		t.Error("sealed value contains the plaintext verbatim")
	}
}

// Two calls sealing the SAME plaintext must never produce the same
// ciphertext — a repeated nonce (or none at all) would leak that two rows
// share a value, and with XChaCha20-Poly1305 specifically would break the
// cipher's confidentiality guarantee outright.
func TestSealNoncesNeverRepeat(t *testing.T) {
	c, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("1234567890123")
	aad := []byte("same-user")

	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		sealed, err := c.Seal(aad, plaintext)
		if err != nil {
			t.Fatal(err)
		}
		key := string(sealed)
		if seen[key] {
			t.Fatalf("iteration %d produced a repeated ciphertext", i)
		}
		seen[key] = true
	}
}

func TestOpenFailsWithWrongAAD(t *testing.T) {
	c, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := c.Seal([]byte("user-A"), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Open([]byte("user-B"), sealed); err == nil {
		t.Error("Open succeeded with a different AAD — a value could be moved between rows undetected")
	}
}

func TestOpenFailsWithWrongKey(t *testing.T) {
	c1, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	c2, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := c1.Seal([]byte("aad"), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Open([]byte("aad"), sealed); err == nil {
		t.Error("Open succeeded under a different key")
	}
}

func TestOpenFailsOnTamperedCiphertext(t *testing.T) {
	c, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := c.Seal([]byte("aad"), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0xFF // flip a bit inside the auth tag
	if _, err := c.Open([]byte("aad"), tampered); err == nil {
		t.Error("Open succeeded on a tampered ciphertext")
	}
}

func TestOpenFailsOnTruncatedCiphertext(t *testing.T) {
	c, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Open([]byte("aad"), []byte("short")); err == nil {
		t.Error("Open succeeded on input shorter than the nonce")
	}
}

func TestNewRejectsWrongKeySize(t *testing.T) {
	if _, err := New([]byte("too-short")); err == nil {
		t.Error("New accepted a key that is not 32 bytes")
	}
}
