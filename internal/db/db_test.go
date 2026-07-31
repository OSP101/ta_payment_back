package db_test

import (
	"context"
	"testing"

	"ta-payment-back/internal/testutil"
)

// The Go side of the calendar guarantee is covered by internal/timeutil. This
// is the other half: every pooled connection must agree that "today" is
// Thailand's today, because CURRENT_DATE decides the monthly write lock
// (resolvePeriodState), the reminder window and the auto-close sweep. A stock
// Postgres image runs UTC, which reports the previous calendar day until
// 07:00 Bangkok.
func TestPoolSessionUsesBangkokTimezone(t *testing.T) {
	pool := testutil.NewPool(t)

	var tz string
	if err := pool.QueryRow(context.Background(), "SHOW timezone").Scan(&tz); err != nil {
		t.Fatalf("SHOW timezone: %v", err)
	}
	if tz != "Asia/Bangkok" {
		t.Fatalf("session timezone = %q, want %q", tz, "Asia/Bangkok")
	}
}

// The setting must hold on every connection the pool hands out, not just the
// first — a runtime parameter applied only at pool construction would leave
// later connections on the server default.
func TestTimezoneHoldsAcrossConnections(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()

	for i := 0; i < 8; i++ {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		var tz string
		err = conn.QueryRow(ctx, "SHOW timezone").Scan(&tz)
		conn.Release()
		if err != nil {
			t.Fatalf("SHOW timezone on conn %d: %v", i, err)
		}
		if tz != "Asia/Bangkok" {
			t.Fatalf("conn %d timezone = %q, want Asia/Bangkok", i, tz)
		}
	}
}

// CURRENT_DATE is what the lock queries actually compare against, so assert on
// it directly rather than trusting that the timezone setting implies it.
func TestCurrentDateFollowsBangkok(t *testing.T) {
	pool := testutil.NewPool(t)

	var sameDay bool
	err := pool.QueryRow(context.Background(),
		`SELECT CURRENT_DATE = (NOW() AT TIME ZONE 'Asia/Bangkok')::date`).Scan(&sameDay)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !sameDay {
		t.Fatal("CURRENT_DATE does not match the Bangkok calendar day")
	}
}

// Guards the harness itself: a fresh database must arrive fully migrated, or
// every DB-backed test built on it would be testing an empty schema.
func TestHarnessAppliesMigrations(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()

	var applied int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied == 0 {
		t.Fatal("no migrations recorded in the fresh database")
	}

	// Spot-check a table the money path depends on.
	var exists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = 'work_logs')`).Scan(&exists); err != nil {
		t.Fatalf("check work_logs: %v", err)
	}
	if !exists {
		t.Fatalf("work_logs missing after %d migrations", applied)
	}
}

// Two pools in the same test binary must not share state.
func TestHarnessGivesIsolatedDatabases(t *testing.T) {
	ctx := context.Background()
	a := testutil.NewPool(t)
	b := testutil.NewPool(t)

	if _, err := a.Exec(ctx,
		`INSERT INTO academic_terms (academic_year, semester) VALUES (2569, 1)`); err != nil {
		t.Fatalf("insert into A: %v", err)
	}

	var inB int
	if err := b.QueryRow(ctx, `SELECT COUNT(*) FROM academic_terms`).Scan(&inB); err != nil {
		t.Fatalf("count in B: %v", err)
	}
	if inB != 0 {
		t.Fatalf("database B saw %d rows written to A — databases are not isolated", inB)
	}
}
