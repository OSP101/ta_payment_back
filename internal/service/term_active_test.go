package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

// "ภาคเรียนปัจจุบัน" is a singleton, but UpsertTerm wrote is_active straight
// through: ticking the box on a new term left the previous one ticked as well,
// so two terms both claimed to be current and the winner depended on row order.
//
// These tests pin the demotion, and that it happens on both paths (create and
// update) — the bug was in both.

func newUpsertSvc(t *testing.T) (*TeachingService, context.Context, uuid.UUID) {
	t.Helper()
	pool := testutil.NewPool(t)
	svc := &TeachingService{pool: pool, aud: audit.New(pool)}
	actor := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id,email,first_name,last_name,is_active) VALUES ($1,$2,'จนท','ทดสอบ',TRUE)`,
		actor, "staff-"+actor.String()+"@example.test"); err != nil {
		t.Fatalf("insert actor: %v", err)
	}
	return svc, context.Background(), actor
}

func termInput(year, sem int, active bool) Term {
	d := func(s string) *string { return &s }
	return Term{
		AcademicYear: year, Semester: sem, Months: 4, IsActive: active,
		StartsOn: d("2026-06-22"), EndsOn: d("2026-10-18"),
		MidtermStartsOn: d("2026-08-01"), MidtermEndsOn: d("2026-08-07"),
		FinalStartsOn: d("2026-10-12"), FinalEndsOn: d("2026-10-18"),
	}
}

// activeTerms returns every term currently flagged active, newest first.
func activeTerms(t *testing.T, svc *TeachingService, ctx context.Context) []string {
	t.Helper()
	rows, err := svc.pool.Query(ctx,
		`SELECT academic_year || '/' || semester FROM academic_terms
		 WHERE is_active ORDER BY academic_year DESC, semester DESC`)
	if err != nil {
		t.Fatalf("query actives: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// Creating a term as active retires whichever term was active before it.
func TestCreatingActiveTermDemotesPrevious(t *testing.T) {
	svc, ctx, actor := newUpsertSvc(t)

	first := termInput(2569, 1, true)
	if _, err := svc.UpsertTerm(ctx, actor, first); err != nil {
		t.Fatalf("create first term: %v", err)
	}
	if got := activeTerms(t, svc, ctx); len(got) != 1 || got[0] != "2569/1" {
		t.Fatalf("expected 2569/1 active, got %v", got)
	}

	second := termInput(2569, 2, true)
	if _, err := svc.UpsertTerm(ctx, actor, second); err != nil {
		t.Fatalf("create second active term: %v", err)
	}
	got := activeTerms(t, svc, ctx)
	if len(got) != 1 {
		t.Fatalf("exactly one term may be active, got %v", got)
	}
	if got[0] != "2569/2" {
		t.Fatalf("the newly created term should be the current one, got %v", got)
	}
}

// The same rule on the update path: editing an old term and ticking active
// retires the one that held it.
func TestUpdatingTermToActiveDemotesPrevious(t *testing.T) {
	svc, ctx, actor := newUpsertSvc(t)

	old, err := svc.UpsertTerm(ctx, actor, termInput(2569, 1, false))
	if err != nil {
		t.Fatalf("create inactive term: %v", err)
	}
	if _, err := svc.UpsertTerm(ctx, actor, termInput(2569, 2, true)); err != nil {
		t.Fatalf("create active term: %v", err)
	}

	promote := termInput(2569, 1, true)
	promote.ID = old.ID
	if _, err := svc.UpsertTerm(ctx, actor, promote); err != nil {
		t.Fatalf("promote old term: %v", err)
	}
	got := activeTerms(t, svc, ctx)
	if len(got) != 1 || got[0] != "2569/1" {
		t.Fatalf("expected only 2569/1 active after promotion, got %v", got)
	}
}

// Unticking the only active term is allowed — a year can end before the next
// one is set up. Zero active terms must not be forced back to one.
func TestTermMayBeDeactivatedLeavingNoneActive(t *testing.T) {
	svc, ctx, actor := newUpsertSvc(t)

	term, err := svc.UpsertTerm(ctx, actor, termInput(2569, 1, true))
	if err != nil {
		t.Fatalf("create active term: %v", err)
	}
	off := termInput(2569, 1, false)
	off.ID = term.ID
	if _, err := svc.UpsertTerm(ctx, actor, off); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if got := activeTerms(t, svc, ctx); len(got) != 0 {
		t.Fatalf("expected no active term, got %v", got)
	}
}

// The same (year, semester) cannot exist twice — the other half of the rule.
func TestDuplicateYearSemesterRejected(t *testing.T) {
	svc, ctx, actor := newUpsertSvc(t)

	if _, err := svc.UpsertTerm(ctx, actor, termInput(2569, 1, true)); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := svc.UpsertTerm(ctx, actor, termInput(2569, 1, false))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a duplicate (year, semester) must be ErrConflict, got %v", err)
	}
}
