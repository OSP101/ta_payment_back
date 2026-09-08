package db_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/config"
	"ta-payment-back/internal/db"
	"ta-payment-back/internal/testutil"
)

// adminURLForConcurrentTest duplicates just enough of testutil's unexported
// adminURL() to create a throwaway database of our own — this test needs a
// database that has NOT already been migrated (testutil.NewPool always
// migrates), to actually exercise two processes racing to apply the SAME
// not-yet-applied migration.
func adminURLForConcurrentTest(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		return v
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve source path")
	}
	repoRoot, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	config.LoadDotEnv(filepath.Join(repoRoot, ".env"))
	user, pass, host, port := os.Getenv("DB_USER"), os.Getenv("DB_PASSWORD"), os.Getenv("DB_HOST"), os.Getenv("DB_PORT")
	if user == "" || host == "" || port == "" {
		t.Skip("no test database configured — set TEST_DATABASE_URL or create a .env")
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%s/postgres?sslmode=disable", user, pass, host, port)
}

// OPS-02: two instances booting at once (rolling deploy) used to race on
// "check schema_migrations, then apply" — not atomic across processes — and
// the loser crashed at boot on a primary-key collision instead of just
// no-oping. This runs Migrate concurrently against one freshly created,
// unmigrated database and asserts every goroutine succeeds.
func TestMigrate_ConcurrentCallsAllSucceed(t *testing.T) {
	adminURL := adminURLForConcurrentTest(t)
	admin, err := pgxpool.New(context.Background(), adminURL)
	if err != nil {
		t.Skipf("test database unreachable: %v", err)
	}
	defer admin.Close()
	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := admin.Ping(pingCtx); err != nil {
		t.Skipf("test database unreachable: %v", err)
	}

	name := fmt.Sprintf("ta_payment_test_migrate_concurrent_%d", os.Getpid())
	if _, err := admin.Exec(context.Background(), `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`); err != nil {
		t.Fatalf("pre-clean %s: %v", name, err)
	}
	if _, err := admin.Exec(context.Background(), `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
	})

	dbURL := replaceDBNameForTest(adminURL, name)
	migrationsDir := testutil.MigrationsDir(t)

	const n = 4
	pools := make([]*pgxpool.Pool, n)
	for i := range pools {
		p, err := db.Connect(context.Background(), dbURL)
		if err != nil {
			t.Fatalf("connect %d: %v", i, err)
		}
		pools[i] = p
		defer p.Close()
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = db.Migrate(context.Background(), pools[i], migrationsDir)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("Migrate goroutine %d failed: %v", i, err)
		}
	}

	// Exactly one row per migration file, no duplicate-key survivors from a
	// half-finished racer.
	var files int
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			files++
		}
	}
	var applied int
	if err := pools[0].QueryRow(context.Background(), `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != files {
		t.Errorf("schema_migrations has %d rows, want %d (one per migration file)", applied, files)
	}
}

func replaceDBNameForTest(url, name string) string {
	base, query := url, ""
	if i := strings.IndexByte(url, '?'); i >= 0 {
		base, query = url[:i], url[i:]
	}
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[:i]
	}
	return base + "/" + name + query
}
