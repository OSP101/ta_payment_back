package storage

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Store interface {
	Save(kind, filename string, r io.Reader) (key string, size int64, err error)
	Open(key string) (io.ReadCloser, error)
	Delete(key string) error
	Path(key string) string
	Encrypted() bool
}

type Local struct {
	root string
	// aead is set when TA_DOCS_ENC_KEY is configured. When non-nil, files
	// are transparently encrypted on Save and decrypted on Open. Layout on
	// disk is: [12-byte nonce || AES-256-GCM ciphertext-with-tag].
	aead cipher.AEAD
}

// ParseKeyFromBase64 decodes a 32-byte AES-256 key. Accepts either
//   - 64 hex characters (openssl rand -hex 32), or
//   - base64 (standard or URL-safe) that decodes to 32 bytes.
//
// Named "…FromBase64" for backward compatibility with the wiring in main.
func ParseKeyFromBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty key")
	}
	if len(s) == 64 && isHex(s) {
		return hex.DecodeString(s)
	}
	var (
		key []byte
		err error
	)
	if strings.ContainsAny(s, "-_") {
		key, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	} else {
		key, err = base64.StdEncoding.DecodeString(s)
	}
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("key must decode to 32 bytes (AES-256); accepts 64 hex chars or 44/43-char base64")
	}
	return key, nil
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func NewLocal(root string) (*Local, error) {
	return NewLocalWithKey(root, nil)
}

func NewLocalWithKey(root string, key []byte) (*Local, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	l := &Local{root: root}
	if len(key) > 0 {
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		l.aead = aead
	}
	return l, nil
}

var allowedMIMEExt = map[string]bool{
	".pdf": true, ".jpg": true, ".jpeg": true, ".png": true, ".webp": true,
	".xlsx": true, ".zip": true, ".docx": true,
}

func (l *Local) Encrypted() bool { return l.aead != nil }

func (l *Local) Save(kind, filename string, r io.Reader) (string, int64, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	if !allowedMIMEExt[ext] {
		return "", 0, errors.New("file type not allowed")
	}
	day := time.Now().Format("2006/01/02")
	dir := filepath.Join(l.root, kind, day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, err
	}
	// Encrypted files carry a .enc suffix so operators can tell at a glance.
	storedName := uuid.NewString() + ext
	if l.aead != nil {
		storedName += ".enc"
	}
	full := filepath.Join(dir, storedName)

	if l.aead == nil {
		f, err := os.Create(full)
		if err != nil {
			return "", 0, err
		}
		defer f.Close()
		n, err := io.Copy(f, r)
		if err != nil {
			return "", 0, err
		}
		key := filepath.ToSlash(filepath.Join(kind, day, storedName))
		return key, n, nil
	}

	// Read plaintext fully so we can seal it in one shot. TA docs are
	// small (<20 MB) so buffering is fine; streaming AEAD would require
	// a chunked format we don't need.
	plaintext, err := io.ReadAll(r)
	if err != nil {
		return "", 0, err
	}
	nonce := make([]byte, l.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", 0, err
	}
	ct := l.aead.Seal(nil, nonce, plaintext, nil)

	if err := writeFile600(full, nonce, ct); err != nil {
		return "", 0, err
	}
	key := filepath.ToSlash(filepath.Join(kind, day, storedName))
	return key, int64(len(plaintext)), nil
}

func writeFile600(full string, nonce, ct []byte) error {
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(nonce); err != nil {
		return err
	}
	if _, err := f.Write(ct); err != nil {
		return err
	}
	return nil
}

func (l *Local) Open(key string) (io.ReadCloser, error) {
	p := filepath.Join(l.root, filepath.FromSlash(key))
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	// Legacy plaintext files (uploaded before encryption was enabled) end
	// in the plain extension; encrypted files carry ".enc". Fall back to
	// plaintext streaming when the marker is absent so old uploads keep
	// working after the switch.
	if l.aead == nil || !strings.HasSuffix(key, ".enc") {
		return f, nil
	}
	defer f.Close()
	buf, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	ns := l.aead.NonceSize()
	if len(buf) < ns {
		return nil, errors.New("encrypted blob too short")
	}
	nonce, ct := buf[:ns], buf[ns:]
	pt, err := l.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(pt)), nil
}

func (l *Local) Delete(key string) error {
	return os.Remove(filepath.Join(l.root, filepath.FromSlash(key)))
}

func (l *Local) Path(key string) string {
	return filepath.Join(l.root, filepath.FromSlash(key))
}
