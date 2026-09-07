package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The public share link is what staff paste into the department LINE group
// so lecturers and TAs can check the board without an account — see
// migration 0084. It must be idempotent to issue, refuse anything not
// currently live without distinguishing "never existed" from "revoked", and
// revoking it must actually cut off PublicResolveTerm.

func TestCreateShareLink_IsIdempotent(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := progressSvc(f)

	first, err := svc.CreateShareLink(f.ctx, f.StaffID, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.CreateShareLink(f.ctx, f.StaffID, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("issuing a link twice must return the same live link, got %s then %s", first.ID, second.ID)
	}
}

func TestPublicResolveTerm_UnknownAndRevokedLinksLookIdentical(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := progressSvc(f)

	if _, _, err := svc.PublicResolveTerm(f.ctx, uuid.New().String()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown link must be ErrNotFound, got %v", err)
	}

	link, err := svc.CreateShareLink(f.ctx, f.StaffID, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	termID, label, err := svc.PublicResolveTerm(f.ctx, link.Slug)
	if err != nil {
		t.Fatalf("a live link must resolve: %v", err)
	}
	if termID != f.TermID || label == "" {
		t.Fatalf("resolved term/label look wrong: %v %q", termID, label)
	}

	if err := svc.RevokeShareLink(f.ctx, f.StaffID, f.TermID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.PublicResolveTerm(f.ctx, link.Slug); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revoked link must be ErrNotFound, got %v", err)
	}
}

func TestCreateShareLink_AfterRevokeIssuesANewID(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := progressSvc(f)

	first, err := svc.CreateShareLink(f.ctx, f.StaffID, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokeShareLink(f.ctx, f.StaffID, f.TermID); err != nil {
		t.Fatal(err)
	}
	second, err := svc.CreateShareLink(f.ctx, f.StaffID, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || first.Slug == second.Slug {
		t.Fatal("a link issued after revoking the last one must be a fresh id and slug")
	}
}

func TestRevokeShareLink_WithNoLiveLinkIsNotFound(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := progressSvc(f)

	if err := svc.RevokeShareLink(f.ctx, f.StaffID, f.TermID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking with nothing live must be ErrNotFound, got %v", err)
	}
}

// The URL token is short so it can be pasted into a chat and read off a phone,
// but it is also the ONLY thing standing between a stranger and the board, so
// shortening it may not make it guessable or ambiguous.
func TestShareLinkSlug_IsShortUnguessableAndUnambiguous(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := progressSvc(f)

	link, err := svc.CreateShareLink(f.ctx, f.StaffID, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	if len(link.Slug) != shareLinkSlugLen {
		t.Errorf("slug %q is %d characters, want %d", link.Slug, len(link.Slug), shareLinkSlugLen)
	}
	if len(link.Slug) >= len(link.ID.String()) {
		t.Errorf("slug %q is no shorter than the UUID it replaced", link.Slug)
	}
	// Characters a reader could confuse for one another must not appear at all.
	for _, bad := range []string{"0", "o", "1", "l", "i", "O", "I", "L"} {
		if strings.Contains(link.Slug, bad) {
			t.Errorf("slug %q contains the ambiguous character %q", link.Slug, bad)
		}
	}
	for _, c := range link.Slug {
		if !strings.ContainsRune(shareLinkAlphabet, c) {
			t.Errorf("slug %q contains %q, which is outside the alphabet", link.Slug, c)
		}
	}

	// Distinct draws, not a counter or a seeded generator: 200 links must not
	// collide and must not be sequential.
	seen := map[string]bool{link.Slug: true}
	for i := 0; i < 200; i++ {
		s, err := newShareLinkSlug()
		if err != nil {
			t.Fatal(err)
		}
		if seen[s] {
			t.Fatalf("newShareLinkSlug repeated %q within %d draws", s, i+1)
		}
		seen[s] = true
	}
}

// A link already pasted into a group chat carries the old UUID. It must keep
// working — the point of the change is a shorter link, not a broken one.
func TestPublicResolveTerm_StillAcceptsALinkIssuedBeforeSlugs(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := progressSvc(f)

	link, err := svc.CreateShareLink(f.ctx, f.StaffID, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	termID, _, err := svc.PublicResolveTerm(f.ctx, link.ID.String())
	if err != nil {
		t.Fatalf("the pre-slug URL form stopped resolving: %v", err)
	}
	if termID != f.TermID {
		t.Errorf("resolved %v, want %v", termID, f.TermID)
	}

	// And revoking still cuts BOTH forms off, not just the new one.
	if err := svc.RevokeShareLink(f.ctx, f.StaffID, f.TermID); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{link.Slug, link.ID.String()} {
		if _, _, err := svc.PublicResolveTerm(f.ctx, token); !errors.Is(err, ErrNotFound) {
			t.Errorf("revoked link still resolves via %q: %v", token, err)
		}
	}
}
