package service

import (
	"context"
	"fmt"
	"testing"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

// Server-side paging is only correct if the filtering, the sorting and the
// count all happen on the same side as the LIMIT. These tests pin the three
// properties that break when any one of them is left in the browser:
//
//   - a full page comes back even when a filter excludes most of the roster
//     (status filtered after slicing would return a short page)
//   - sorting orders the WHOLE result set, not the page (sorting after slicing
//     puts the wrong 15 people on page 1)
//   - walking every page visits each person exactly once

// newListSvc seeds `n` users; every third one is deactivated.
func newListSvc(t *testing.T, n int) (*UserService, context.Context) {
	t.Helper()
	pool := testutil.NewPool(t)
	ctx := context.Background()
	svc := &UserService{pool: pool, aud: audit.New(pool)}
	for k := 0; k < n; k++ {
		name := fmt.Sprintf("p%03d", k)
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (email, first_name, last_name, is_active)
			 VALUES ($1, $2, 'ทดสอบ', $3)`,
			name+"@example.test", name, k%3 != 0); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}
	return svc, ctx
}

// The status filter runs in SQL, so a page is full even when the filter throws
// most of the roster away. Filtering the page afterwards would yield ~10 rows.
func TestStatusFilterAppliesBeforePaging(t *testing.T) {
	const n = 60
	svc, ctx := newListSvc(t, n)

	items, total, err := svc.List(ctx, UserListFilter{Status: "inactive", Limit: 15})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// k%3 == 0 → inactive: 0,3,…,57 = 20 people.
	if total != 20 {
		t.Fatalf("total must count only inactive users, got %d", total)
	}
	if len(items) != 15 {
		t.Fatalf("a page must be full when enough rows match: want 15, got %d", len(items))
	}
	for _, u := range items {
		if u.IsActive {
			t.Fatalf("an active user leaked into the inactive filter: %s", u.Email)
		}
	}
}

// Sorting descending must reorder the whole set, so page 1 holds the LAST
// names overall. Sorting a page after slicing would still hand back p000….
func TestSortAppliesToTheWholeSetNotThePage(t *testing.T) {
	const n = 60
	svc, ctx := newListSvc(t, n)

	items, _, err := svc.List(ctx, UserListFilter{Sort: "name", Desc: true, Limit: 5})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 5 {
		t.Fatalf("want 5 rows, got %d", len(items))
	}
	if items[0].FirstName != fmt.Sprintf("p%03d", n-1) {
		t.Fatalf("descending page 1 must start at the last name overall, got %q", items[0].FirstName)
	}
}

// An unknown sort key falls back to the default instead of reaching the SQL.
func TestUnknownSortFallsBackToName(t *testing.T) {
	svc, ctx := newListSvc(t, 5)

	items, _, err := svc.List(ctx, UserListFilter{Sort: "u.email; DROP TABLE users --", Limit: 5})
	if err != nil {
		t.Fatalf("an unknown sort key must not reach the query: %v", err)
	}
	if len(items) != 5 || items[0].FirstName != "p000" {
		t.Fatalf("expected the default name ordering, got %d rows starting %q", len(items), items[0].FirstName)
	}
}

// Walking every page visits each person exactly once — no row is skipped by an
// unstable ordering and none is served twice.
func TestPagingCoversEveryUserExactlyOnce(t *testing.T) {
	const n = 47
	const page = 15
	svc, ctx := newListSvc(t, n)

	seen := map[string]int{}
	for offset := 0; offset < n; offset += page {
		items, total, err := svc.List(ctx, UserListFilter{Limit: page, Offset: offset})
		if err != nil {
			t.Fatalf("List at offset %d: %v", offset, err)
		}
		if total != n {
			t.Fatalf("total must stay constant across pages: want %d, got %d", n, total)
		}
		for _, u := range items {
			seen[u.Email]++
		}
	}
	if len(seen) != n {
		t.Fatalf("paging visited %d distinct users, want %d", len(seen), n)
	}
	for email, times := range seen {
		if times != 1 {
			t.Fatalf("%s appeared on %d pages, want 1", email, times)
		}
	}
}

// Role and status compose, and the total reflects both.
func TestRoleAndStatusCompose(t *testing.T) {
	svc, ctx := newListSvc(t, 9)

	// Give the first three users the lecturer role: p000 (inactive), p001, p002.
	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO user_roles (user_id, role)
		 SELECT id, 'lecturer'::role_code FROM users WHERE first_name IN ('p000','p001','p002')`); err != nil {
		t.Fatalf("assign roles: %v", err)
	}

	_, total, err := svc.List(ctx, UserListFilter{Role: "lecturer", Limit: 50})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 3 {
		t.Fatalf("lecturer total: want 3, got %d", total)
	}

	items, total, err := svc.List(ctx, UserListFilter{Role: "lecturer", Status: "active", Limit: 50})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 2 {
		t.Fatalf("active lecturers: want 2, got %d", total)
	}
	for _, u := range items {
		if !u.IsActive {
			t.Fatalf("inactive user %s leaked through", u.Email)
		}
	}
}
