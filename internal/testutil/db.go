// Package testutil provisions throwaway Postgres databases for tests that
// exercise real SQL.
//
// Most of this system's money rules live in SQL — the monthly write lock, the
// daily and weekly hour caps, the 300฿/day ceiling, the cross-course overlap
// check. Testing them against a mock proves nothing: the behaviour under test
// IS the query. So tests that touch those rules get a real database.
//
// Each call to NewPool creates a uniquely named database, applies every
// migration to it, and drops it when the test finishes. Tests are therefore
// independent and can run in parallel without a shared-fixture reset dance.
//
// When no database is reachable the helper SKIPS rather than fails, so
// `go test ./...` still works on a laptop with nothing running. CI is expected
// to provide one; see .github/workflows/ci.yml, which sets TEST_DATABASE_URL
// against a service container.
package testutil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/db"
)

// adminURLEnv names the connection string pointing at a database the test
// process may use to CREATE DATABASE. Its own database is never modified.
const adminURLEnv = "TEST_DATABASE_URL"

// defaultAdminURL matches the docker-compose service shipped with this repo so
// a developer who ran `docker compose up` needs no extra configuration.
const defaultAdminURL = "postgres://itii_database_prod:2b15y2d2h6wzgZIksSSIkwR7udsbuCduKkdIrGtsPDhwJZVoVFdqR7m@localhost:5432/postgres?sslmode=disable"

// dbSeq disambiguates databases created within the same process. Combined with
// the PID it keeps concurrent `go test` invocations from colliding.
var dbSeq atomic.Int64

func adminURL() string {
	if v := os.Getenv(adminURLEnv); v != "" {
		return v
	}
	return defaultAdminURL
}

// MigrationsDir resolves the repo's migrations directory from this source
// file's location, so tests work regardless of the package they run from.
func MigrationsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve testutil source path")
	}
	// internal/testutil/db.go -> repo root
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("migrations dir %s: %v", abs, err)
	}
	return abs
}

// NewPool returns a pool bound to a freshly migrated, uniquely named database.
// The database is dropped during test cleanup. Skips the test when no server is
// reachable.
func NewPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, adminURL())
	if err != nil {
		t.Skipf("no test database configured (%s): %v", adminURLEnv, err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := admin.Ping(pingCtx); err != nil {
		admin.Close()
		t.Skipf("test database unreachable at %s — start it with `docker compose up -d` "+
			"or set %s (%v)", redact(adminURL()), adminURLEnv, err)
	}

	name := fmt.Sprintf("ta_payment_test_%d_%d", os.Getpid(), dbSeq.Add(1))
	// Identifier is composed from a fixed prefix plus two integers, so it can
	// never carry an injectable character.
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		admin.Close()
		t.Fatalf("create database %s: %v", name, err)
	}

	pool, err := db.Connect(ctx, replaceDBName(adminURL(), name))
	if err != nil {
		dropDatabase(admin, name)
		admin.Close()
		t.Fatalf("connect to %s: %v", name, err)
	}
	if err := db.Migrate(ctx, pool, MigrationsDir(t)); err != nil {
		pool.Close()
		dropDatabase(admin, name)
		admin.Close()
		t.Fatalf("migrate %s: %v", name, err)
	}

	t.Cleanup(func() {
		pool.Close()
		dropDatabase(admin, name)
		admin.Close()
	})
	return pool
}

func dropDatabase(admin *pgxpool.Pool, name string) {
	// A lingering connection would make DROP fail; FORCE (PG13+) evicts them.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
}

// replaceDBName swaps the database component of a Postgres URL, preserving
// credentials, host and query parameters.
func replaceDBName(url, name string) string {
	// Split off the query string so a '/' inside it cannot be mistaken for the
	// path separator.
	base, query := url, ""
	if i := strings.IndexByte(url, '?'); i >= 0 {
		base, query = url[:i], url[i:]
	}
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[:i]
	}
	return base + "/" + name + query
}

// redact strips the password from a connection string for safe logging.
func redact(url string) string {
	at := strings.LastIndexByte(url, '@')
	scheme := strings.Index(url, "://")
	if at < 0 || scheme < 0 || at < scheme {
		return url
	}
	return url[:scheme+3] + "***" + url[at:]
}
