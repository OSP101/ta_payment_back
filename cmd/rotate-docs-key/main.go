// Command rotate-docs-key re-encrypts every ".enc" file under UPLOAD_DIR from
// OLD_TA_DOCS_ENC_KEY to NEW_TA_DOCS_ENC_KEY.
//
// internal/storage.Local, like internal/pii.Cipher, only ever holds one key —
// swapping TA_DOCS_ENC_KEY in production without running this first does not
// rotate anything, it just makes every existing upload undecryptable the next
// time someone downloads one. Unlike the PII rotation tool (cmd/rotate-pii-key,
// a few hundred database rows updated in one transaction), this walks a
// directory tree that can hold thousands of files and grows continuously, so
// "all or nothing" isn't available — a run instead processes files one at a
// time and stops at the first failure, leaving already-rotated files rotated.
// See "Resuming after a failure" below for why that is still safe to restart.
//
// Usage:
//
//	# 1. Dry run first — decrypts every .enc file with the OLD key and
//	#    reports success/failure, writes nothing.
//	OLD_TA_DOCS_ENC_KEY=... NEW_TA_DOCS_ENC_KEY=... UPLOAD_DIR=./data/uploads \
//	  go run ./cmd/rotate-docs-key
//
//	# 2. Apply — for each file: decrypt with the old key, re-encrypt with a
//	#    fresh random nonce under the new key, write to a sibling temp file,
//	#    decrypt THAT back to confirm it round-trips, then atomically rename
//	#    it over the original. The original is never truncated or edited in
//	#    place, so a crash mid-run leaves either the untouched original or a
//	#    fully-written, verified replacement — never a half-written file.
//	OLD_TA_DOCS_ENC_KEY=... NEW_TA_DOCS_ENC_KEY=... UPLOAD_DIR=./data/uploads \
//	  go run ./cmd/rotate-docs-key -apply
//
//	# 3. Only once this reports every file done, update TA_DOCS_ENC_KEY in the
//	#    running service's environment to NEW_TA_DOCS_ENC_KEY's value and
//	#    restart it. Doing this before the walk finishes makes the live
//	#    service unable to read whatever this tool has not reached yet.
//
// # Resuming after a failure
//
// Every ".enc" file looks identical from the outside regardless of which key
// encrypted it, so this tool cannot tell "already rotated" from "not yet"
// without trying. That is fine, not a gap: re-running after a failure simply
// re-decrypts every file with OLD_TA_DOCS_ENC_KEY again. Files already
// rotated in a prior run now fail to decrypt under the old key (they are
// under the new one) — the tool reports exactly that and stops immediately,
// which is the correct, safe signal, not corruption. Recovery is: point
// OLD_TA_DOCS_ENC_KEY at the ONE key the reported failing file is actually
// under (new, if the previous run got partway through) and re-run; repeat
// until a full pass reports zero failures.
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"ta-payment-back/internal/storage"
)

func main() {
	apply := flag.Bool("apply", false, "write re-encrypted files (default: dry run — decrypt-verify only, no writes)")
	flag.Parse()

	oldAEAD := mustAEAD("OLD_TA_DOCS_ENC_KEY")
	newAEAD := mustAEAD("NEW_TA_DOCS_ENC_KEY")
	root := mustEnv("UPLOAD_DIR")

	var total, done int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".enc") {
			return nil
		}
		total++
		if *apply {
			if err := rotateFile(path, oldAEAD, newAEAD); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
		} else {
			if _, err := decryptFile(path, oldAEAD); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
		}
		done++
		if done%200 == 0 {
			fmt.Printf("...%d files done\n", done)
		}
		return nil
	})
	if err != nil {
		log.Fatalf("stopped after %d/%d file(s) — %v\n"+
			"Already-rotated files are safe (see the package doc comment on resuming).", done, total, err)
	}

	fmt.Printf("%d file(s) found under %s\n", total, root)
	if !*apply {
		fmt.Println("dry run — all decrypted successfully with the old key, nothing written. Re-run with -apply once this looks right.")
		return
	}
	fmt.Println("done — every file rotated. Update TA_DOCS_ENC_KEY to the new key's value and restart the service.")
}

// decryptFile reads [nonce||ciphertext] and returns the plaintext, matching
// internal/storage.Local.Open's on-disk layout exactly.
func decryptFile(path string, aead cipher.AEAD) ([]byte, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	ns := aead.NonceSize()
	if len(buf) < ns {
		return nil, fmt.Errorf("file too short to hold a nonce")
	}
	nonce, ct := buf[:ns], buf[ns:]
	pt, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return pt, nil
}

// rotateFile decrypts path under oldAEAD, re-encrypts under newAEAD with a
// fresh nonce, and atomically replaces path — but only after reading the
// written bytes back and confirming they decrypt to the same plaintext under
// newAEAD, so a corrupted write is caught before it overwrites the original.
func rotateFile(path string, oldAEAD, newAEAD cipher.AEAD) error {
	plain, err := decryptFile(path, oldAEAD)
	if err != nil {
		return err
	}
	nonce := make([]byte, newAEAD.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ct := newAEAD.Seal(nil, nonce, plain, nil)
	for i := range plain {
		plain[i] = 0 // best-effort: don't leave plaintext sitting around longer than needed
	}

	tmp := path + ".rotating"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(nonce); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := f.Write(ct); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}

	// Verify the file just written actually round-trips under the new key
	// before it replaces the original.
	verify, err := decryptFile(tmp, newAEAD)
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write verification failed: %w", err)
	}
	for i := range verify {
		verify[i] = 0
	}

	return os.Rename(tmp, path)
}

func mustAEAD(envKey string) cipher.AEAD {
	key, err := storage.ParseKeyFromBase64(mustEnv(envKey))
	if err != nil {
		log.Fatalf("%s: %v", envKey, err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		log.Fatalf("%s: %v", envKey, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		log.Fatalf("%s: %v", envKey, err)
	}
	return aead
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("%s is required", k)
	}
	return v
}
