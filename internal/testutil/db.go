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

	"ta-payment-back/internal/config"
	"ta-payment-back/internal/db"
)

// adminURLEnv names the connection string pointing at a database the test
// process may use to CREATE DATABASE. Its own database is never modified.
const adminURLEnv = "TEST_DATABASE_URL"

// dbSeq disambiguates databases created within the same process. Combined with
// the PID it keeps concurrent `go test` invocations from colliding.
var dbSeq atomic.Int64

// adminURL resolves the connection string used to CREATE/DROP throwaway test
// databases. TEST_DATABASE_URL (CI) wins outright; otherwise it is built from
// DB_HOST/DB_PORT/DB_USER/DB_PASSWORD, loaded from the repo's own .env — the
// same file `docker compose up` and the running server already use.
//
// This used to be a literal connection string with a real password baked into
// this file. That value sat in git history (and on the private GitHub remote)
// for weeks before anyone noticed, and every password rotation silently broke
// it again until someone hardcoded the new one back in. Reading .env instead
// means there is exactly one place the password lives, and it is the place
// already excluded from git.
func adminURL(t *testing.T) string {
	t.Helper()
	if v := os.Getenv(adminURLEnv); v != "" {
		return v
	}
	config.LoadDotEnv(filepath.Join(repoRoot(t), ".env"))
	user, pass, host, port := os.Getenv("DB_USER"), os.Getenv("DB_PASSWORD"), os.Getenv("DB_HOST"), os.Getenv("DB_PORT")
	if user == "" || host == "" || port == "" {
		// No .env and no TEST_DATABASE_URL — NewPool's caller skips on the
		// ensuing connection failure, same as it always has.
		return ""
	}
	// The admin connection targets Postgres's own "postgres" maintenance
	// database (needed to CREATE DATABASE), never DB_NAME — creating a
	// sibling database requires connecting to some OTHER database first.
	return fmt.Sprintf("postgres://%s:%s@%s:%s/postgres?sslmode=disable", user, pass, host, port)
}

// repoRoot locates the repository root from this source file's own path, the
// same trick MigrationsDir uses — works regardless of which package's tests
// are running, since `go test` sets the working directory to the package
// under test, not the repo root.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve testutil source path")
	}
	abs, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return abs
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

	url := adminURL(t)
	if url == "" {
		t.Skipf("no test database configured — set %s, or create a .env with "+
			"DB_HOST/DB_PORT/DB_USER/DB_PASSWORD (see .env.example)", adminURLEnv)
	}
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("no test database configured (%s): %v", adminURLEnv, err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := admin.Ping(pingCtx); err != nil {
		admin.Close()
		t.Skipf("test database unreachable at %s — start it with `docker compose up -d` "+
			"or set %s (%v)", redact(url), adminURLEnv, err)
	}

	name := fmt.Sprintf("ta_payment_test_%d_%d", os.Getpid(), dbSeq.Add(1))
	// Identifier is composed from a fixed prefix plus two integers, so it can
	// never carry an injectable character.
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		admin.Close()
		t.Fatalf("create database %s: %v", name, err)
	}

	pool, err := db.Connect(ctx, replaceDBName(url, name))
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
