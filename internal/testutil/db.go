// Package testutil provisions throwaway Postgres databases for tests that
// exercise real SQL.
//
// Most of this system's money rules live in SQL — the monthly write lock, the
// daily and weekly hour caps, the 300฿/day ceiling, the cross-course overlap
// check. Testing them against a mock proves nothing: the behaviour under test
// IS the query. So tests that touch those rules get a real database.
//
// Each call to NewPool creates a uniquely named database and drops it when the
// test finishes. Tests are therefore independent and can run in parallel
// without a shared-fixture reset dance.
//
// The database is a CREATE DATABASE ... TEMPLATE clone of a template that had
// every migration applied once, not a fresh migrate per test. Replaying 100+
// migration files for each of ~900 service tests was nearly all of that
// package's 20-minute runtime; a clone is a file copy. The template is keyed
// by a hash of the migration files (see templateName), so it is shared across
// `go test` processes and rebuilt automatically when a migration changes.
// Templates for other hashes are dropped once nothing has claimed them for a
// while (see sweepStaleTemplates).
//
// When no database is reachable the helper SKIPS rather than fails, so
// `go test ./...` still works on a laptop with nothing running. CI is expected
// to provide one; see .github/workflows/ci.yml, which sets TEST_DATABASE_URL
// against a service container.
package testutil

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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

// NewPool returns a pool bound to a uniquely named clone of the migrated
// template database. The database is dropped during test cleanup. Skips the
// test when no server is reachable.
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

	tpl, err := ensureTemplate(ctx, admin, url, MigrationsDir(t))
	if err != nil {
		admin.Close()
		t.Fatalf("prepare template database: %v", err)
	}

	name := fmt.Sprintf("ta_payment_test_%d_%d", os.Getpid(), dbSeq.Add(1))
	// Both identifiers are a fixed prefix plus integers / hex digits, so
	// neither can carry an injectable character.
	if err := cloneDatabase(ctx, admin, name, tpl); err != nil {
		// Another process may have swept this template as stale (a different
		// migration set sharing the server — see sweepStaleTemplates). Forget
		// it and build it again once before giving up.
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "3D000" { // invalid_catalog_name
			admin.Close()
			t.Fatalf("create database %s from %s: %v", name, tpl, err)
		}
		forgetTemplate(tpl)
		if tpl, err = ensureTemplate(ctx, admin, url, MigrationsDir(t)); err == nil {
			err = cloneDatabase(ctx, admin, name, tpl)
		}
		if err != nil {
			admin.Close()
			t.Fatalf("create database %s: %v", name, err)
		}
	}

	pool, err := db.Connect(ctx, replaceDBName(url, name))
	if err != nil {
		dropDatabase(admin, name)
		admin.Close()
		t.Fatalf("connect to %s: %v", name, err)
	}

	t.Cleanup(func() {
		pool.Close()
		dropDatabase(admin, name)
		admin.Close()
	})
	return pool
}

func cloneDatabase(ctx context.Context, admin *pgxpool.Pool, name, tpl string) error {
	_, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`" TEMPLATE "`+tpl+`"`)
	return err
}

const (
	// templatePrefix names every template database this package builds.
	// Deliberately not "ta_payment_test_": nothing that sweeps per-test
	// databases should ever mistake a shared template for one.
	templatePrefix = "ta_payment_tpl_"

	// templateLockKey serializes template builds and sweeps across every
	// process sharing the server. Distinct from internal/db's migration lock,
	// which the build itself takes while holding this one.
	templateLockKey = 8471024

	// templateStaleAfter is how long a template for a DIFFERENT migration set
	// must have gone unclaimed before it is dropped. Another worktree on the
	// same server may still be mid-run on it; each process re-stamps its
	// template when it first resolves it, and a run that loses its template
	// anyway rebuilds it (see NewPool).
	templateStaleAfter = 2 * time.Hour
)

// tplState caches the template resolved by this process, so the lock, the
// hash and the existence check happen once per `go test` binary rather than
// once per test.
var tplState struct {
	sync.Mutex
	name string
}

func forgetTemplate(name string) {
	tplState.Lock()
	defer tplState.Unlock()
	if tplState.name == name {
		tplState.name = ""
	}
}

// ensureTemplate returns the name of a fully migrated template database for
// the current migration set, building it if no process has yet.
//
// Completeness is carried by the NAME: the build happens under a scratch name
// and is renamed to the final one only after every migration has committed
// and all connections to it are closed. So "the final name exists" means
// "the template is complete" — a crashed build leaves only a scratch database,
// which the next builder drops.
func ensureTemplate(ctx context.Context, admin *pgxpool.Pool, adminURL, migrationsDir string) (string, error) {
	tplState.Lock()
	defer tplState.Unlock()
	if tplState.name != "" {
		return tplState.name, nil
	}

	name, err := templateName(migrationsDir)
	if err != nil {
		return "", err
	}

	// Session-level advisory lock: it must live on one connection for the
	// whole build, and Postgres releases it by itself if this process dies.
	conn, err := admin.Acquire(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, templateLockKey); err != nil {
		return "", fmt.Errorf("template lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, templateLockKey)
	}()

	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		if err := buildTemplate(ctx, admin, adminURL, migrationsDir, name); err != nil {
			return "", err
		}
	}
	// Claim it: the timestamp tells other processes' sweeps it is in use.
	if _, err := conn.Exec(ctx, `COMMENT ON DATABASE "`+name+`" IS '`+
		strconv.FormatInt(time.Now().Unix(), 10)+`'`); err != nil {
		return "", fmt.Errorf("stamp template %s: %w", name, err)
	}
	// Holding the lock, no build is in progress anywhere — safe to sweep.
	sweepStaleTemplates(ctx, admin, name)

	tplState.name = name
	return name, nil
}

func buildTemplate(ctx context.Context, admin *pgxpool.Pool, adminURL, migrationsDir, name string) error {
	scratch := name + "_build"
	// Left over by a build that died mid-way; the lock guarantees nobody is
	// still working on it.
	dropDatabase(admin, scratch)
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+scratch+`"`); err != nil {
		return fmt.Errorf("create %s: %w", scratch, err)
	}
	pool, err := db.Connect(ctx, replaceDBName(adminURL, scratch))
	if err != nil {
		dropDatabase(admin, scratch)
		return fmt.Errorf("connect to %s: %w", scratch, err)
	}
	err = db.Migrate(ctx, pool, migrationsDir)
	// A clone fails while ANY session is connected to its template, so the
	// migration pool must be gone before the template is published.
	pool.Close()
	if err != nil {
		dropDatabase(admin, scratch)
		return fmt.Errorf("migrate %s: %w", scratch, err)
	}
	// ALLOW_CONNECTIONS false keeps a stray psql session from making every
	// clone fail; cloning does not connect, so it is unaffected.
	if _, err := admin.Exec(ctx, `ALTER DATABASE "`+scratch+`" WITH ALLOW_CONNECTIONS false`); err != nil {
		dropDatabase(admin, scratch)
		return fmt.Errorf("seal %s: %w", scratch, err)
	}
	if _, err := admin.Exec(ctx, `ALTER DATABASE "`+scratch+`" RENAME TO "`+name+`"`); err != nil {
		dropDatabase(admin, scratch)
		return fmt.Errorf("publish %s: %w", name, err)
	}
	return nil
}

// sweepStaleTemplates drops templates for other migration sets that no
// process has claimed within templateStaleAfter, plus any scratch build left
// by a crash. Best-effort: a failure here never fails a test. Caller must
// hold templateLockKey.
func sweepStaleTemplates(ctx context.Context, admin *pgxpool.Pool, current string) {
	rows, err := admin.Query(ctx,
		`SELECT datname, COALESCE(shobj_description(oid, 'pg_database'), '')
		   FROM pg_database
		  WHERE starts_with(datname, $1) AND datname <> $2`, templatePrefix, current)
	if err != nil {
		return
	}
	var stale []string
	for rows.Next() {
		var name, stamp string
		if rows.Scan(&name, &stamp) != nil {
			continue
		}
		claimed, perr := strconv.ParseInt(stamp, 10, 64)
		if perr != nil || time.Since(time.Unix(claimed, 0)) > templateStaleAfter {
			stale = append(stale, name)
		}
	}
	rows.Close()
	for _, name := range stale {
		if isTemplateIdent(name) {
			dropDatabase(admin, name)
		}
	}
}

// isTemplateIdent re-checks a name read back from pg_database before it is
// spliced into DROP DATABASE: prefix, hex digits, optional "_build".
func isTemplateIdent(name string) bool {
	rest, ok := strings.CutPrefix(name, templatePrefix)
	if !ok {
		return false
	}
	rest = strings.TrimSuffix(rest, "_build")
	if rest == "" {
		return false
	}
	for _, c := range rest {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// templateName derives the template's name from everything that decides its
// contents: each *.up.sql migration's file name and bytes (exactly the set
// db.Migrate applies, in the order it applies them) and the migrator's own
// source. Any change produces a new name, so a stale template is never
// cloned — it is simply no longer asked for.
func templateName(migrationsDir string) (string, error) {
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return "", err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	h := sha256.New()
	add := func(label, path string) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		fmt.Fprintf(h, "%s\x00", label)
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		_, _ = h.Write([]byte{0})
		return nil
	}
	for _, f := range files {
		if err := add(f, filepath.Join(migrationsDir, f)); err != nil {
			return "", err
		}
	}
	// migrationsDir is <repo>/migrations; the migrator lives beside it.
	if err := add("migrate.go", filepath.Join(migrationsDir, "..", "internal", "db", "migrate.go")); err != nil {
		return "", err
	}
	return templatePrefix + hex.EncodeToString(h.Sum(nil))[:16], nil
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
