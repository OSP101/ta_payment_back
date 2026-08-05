package service

import (
	"context"
	"fmt"
	"testing"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

// List clamped its page size with `if limit <= 0 || limit > 200 { limit = 50 }`
// — one condition doing two different jobs. An over-large limit fell back to
// the SMALLEST page instead of the ceiling, so the users screen (which asks for
// 500) received 50 rows while the total said 51. The account sorting last was
// invisible, and nothing on screen suggested the list was truncated.
//
// The assertions below are on the TAIL of the list on purpose: a test that only
// checked len(items) > 50 would pass on a fallback of 60 just as happily.

func TestListReturnsEveryUserWhenLimitExceedsCeiling(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	svc := &UserService{pool: pool, aud: audit.New(pool)}

	// 51 users named so that plain first_name ordering is predictable, with the
	// last one carrying a distinct name — it is the row the bug dropped.
	const n = 51
	for k := 0; k < n; k++ {
		name := fmt.Sprintf("user%03d", k)
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (email, first_name, last_name, is_active)
			 VALUES ($1, $2, 'ทดสอบ', TRUE)`,
			name+"@example.test", name); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}

	items, total, err := svc.List(ctx, UserListFilter{Limit: 500})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != n {
		t.Fatalf("total should count every user: want %d, got %d", n, total)
	}
	if len(items) != total {
		t.Fatalf("the list must hold as many rows as the total it reports: total=%d, rows=%d", total, len(items))
	}
	last := items[len(items)-1]
	if last.FirstName != fmt.Sprintf("user%03d", n-1) {
		t.Fatalf("the last-sorting user must be present, got %q", last.FirstName)
	}
}

// A limit past the ceiling clamps to it rather than collapsing to the default.
func TestListClampsRatherThanCollapsing(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	svc := &UserService{pool: pool, aud: audit.New(pool)}

	const n = maxUserPage + 10
	rows := make([][]any, 0, n)
	for k := 0; k < n; k++ {
		name := fmt.Sprintf("u%04d", k)
		rows = append(rows, []any{name + "@example.test", name})
	}
	for _, r := range rows {
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (email, first_name, last_name, is_active)
			 VALUES ($1, $2, 'ทดสอบ', TRUE)`, r...); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	items, total, err := svc.List(ctx, UserListFilter{Limit: 100000})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != n {
		t.Fatalf("total: want %d, got %d", n, total)
	}
	if len(items) != maxUserPage {
		t.Fatalf("an over-large limit must clamp to %d, got %d rows", maxUserPage, len(items))
	}
}

// A zero or negative limit still means "use the default", not "no rows".
func TestListDefaultsWhenLimitUnset(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	svc := &UserService{pool: pool, aud: audit.New(pool)}

	for k := 0; k < 60; k++ {
		name := fmt.Sprintf("d%03d", k)
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (email, first_name, last_name, is_active)
			 VALUES ($1, $2, 'ทดสอบ', TRUE)`, name+"@example.test", name); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	items, _, err := svc.List(ctx, UserListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 50 {
		t.Fatalf("an unset limit should page at 50, got %d", len(items))
	}
}
