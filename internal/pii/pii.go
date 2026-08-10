// Package pii encrypts personal data that must be recoverable — today, just
// the TA citizen ID number the ปะหน้าจ่ายตรง transfer-cover document prints.
//
// Migration 0047 (29/07/2026) deliberately stopped storing this kind of data
// at all: it arrived in a request, got drawn onto a PDF, and left. The staff
// interview that drove this package asked for it back specifically because the
// transfer-cover sheet needs it — encrypted, so nothing but this system can
// read it back. XChaCha20-Poly1305 rather than the AES-256-GCM the document
// store (internal/storage) already uses: its 24-byte nonce can be drawn at
// random for the life of the system without the birthday-bound collision risk
// a 12-byte AES-GCM nonce carries under frequent reuse of the same key, which
// matters here because a profile can be resubmitted many times.
//
// This key is intentionally a SEPARATE secret from the document-storage key —
// one leaking must not leak the other.
package pii

import (
	"crypto/cipher"
	"crypto/rand"
	"errors"

	"golang.org/x/crypto/chacha20poly1305"
)

// Cipher seals and opens PII values. Safe for concurrent use — the
// underlying AEAD holds no per-call state.
type Cipher struct {
	aead cipher.AEAD
}

// New builds a Cipher from a 32-byte key. Use storage.ParseKeyFromBase64 to
// decode PII_ENC_KEY into that shape — same 32-byte requirement, one parser.
func New(key []byte) (*Cipher, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Seal encrypts plaintext, binding it to aad. Pass the owning row's primary
// key (e.g. the user's id) as aad so a ciphertext copied from one row into
// another fails to decrypt instead of silently reading back as if it
// belonged there. Layout: [24-byte nonce][ciphertext+16-byte tag].
func (c *Cipher) Seal(aad, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	// Seal appends its output to dst — passing nonce as dst is what makes the
	// return value "nonce followed by ciphertext" in one buffer.
	return c.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// Open decrypts a value Seal produced. aad must be byte-identical to what
// Seal was called with, or this fails (see the AAD note on Seal).
func (c *Cipher) Open(aad, sealed []byte) ([]byte, error) {
	if len(sealed) < chacha20poly1305.NonceSizeX {
		return nil, errors.New("pii: ciphertext too short")
	}
	nonce, ct := sealed[:chacha20poly1305.NonceSizeX], sealed[chacha20poly1305.NonceSizeX:]
	return c.aead.Open(nil, nonce, ct, aad)
}
