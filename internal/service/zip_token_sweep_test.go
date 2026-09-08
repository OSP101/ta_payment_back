package service

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// OPS-03: zipTokens has no cap the way loginAttempts/pwAttempts do (those are
// bounded by real user count; this is keyed by a fresh random token every
// mint) and nothing swept an unconsumed one — staff clicking "เตรียม
// ดาวน์โหลด" and then navigating away, or a failed download, left the entry
// (holding every document id in that batch) for the life of the process.
// This pins that SweepZipTokens removes expired-but-never-consumed tokens.
func TestSweepZipTokens_RemovesExpiredTokens(t *testing.T) {
	svc := &DocsService{}

	mint := func(actor, userID uuid.UUID) string {
		tok, err := svc.mintZipToken(actor, userID, []uuid.UUID{uuid.New()})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	tok1 := mint(uuid.New(), uuid.New())
	tok2 := mint(uuid.New(), uuid.New())
	tok3 := mint(uuid.New(), uuid.New())

	// Sweeping "now" (well before zipTokenTTL) must not touch anything —
	// tokens are still live, and consumers must keep being able to use them.
	svc.SweepZipTokens(time.Now())
	for _, tok := range []string{tok1, tok2, tok3} {
		if _, ok := svc.zipTokens.Load(tok); !ok {
			t.Fatalf("token %s was swept before its TTL elapsed", tok)
		}
	}

	// Sweep as if run well past the TTL — all three, never consumed, must
	// be gone.
	svc.SweepZipTokens(time.Now().Add(zipTokenTTL + time.Second))
	for _, tok := range []string{tok1, tok2, tok3} {
		if _, ok := svc.zipTokens.Load(tok); ok {
			t.Fatalf("token %s survived a sweep well past its TTL", tok)
		}
	}
}
