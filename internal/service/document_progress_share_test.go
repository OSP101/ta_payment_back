package service

import (
	"errors"
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

	if _, _, err := svc.PublicResolveTerm(f.ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown link must be ErrNotFound, got %v", err)
	}

	link, err := svc.CreateShareLink(f.ctx, f.StaffID, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	termID, label, err := svc.PublicResolveTerm(f.ctx, link.ID)
	if err != nil {
		t.Fatalf("a live link must resolve: %v", err)
	}
	if termID != f.TermID || label == "" {
		t.Fatalf("resolved term/label look wrong: %v %q", termID, label)
	}

	if err := svc.RevokeShareLink(f.ctx, f.StaffID, f.TermID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.PublicResolveTerm(f.ctx, link.ID); !errors.Is(err, ErrNotFound) {
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
	if first.ID == second.ID {
		t.Fatal("a link issued after revoking the last one must be a fresh id")
	}
}

func TestRevokeShareLink_WithNoLiveLinkIsNotFound(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := progressSvc(f)

	if err := svc.RevokeShareLink(f.ctx, f.StaffID, f.TermID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking with nothing live must be ErrNotFound, got %v", err)
	}
}
