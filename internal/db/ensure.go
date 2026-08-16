package db

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
)

// EnsureDatabase creates the database named in targetURL if it doesn't exist
// yet, so a fresh environment self-provisions on boot instead of needing a
// manual `createdb` step someone has to remember (and someone eventually
// won't). Used for DemoDatabaseURL specifically — see that field's doc
// comment in internal/config — never for the production DatabaseURL, which
// is expected to already exist by the time this process starts.
//
// Postgres has no CREATE DATABASE IF NOT EXISTS, and CREATE DATABASE cannot
// run inside a transaction — connecting to the server's own "postgres"
// maintenance database and checking pg_database first is the standard
// workaround. A single connection, not a pool: this runs once at boot and
// closes immediately after.
func EnsureDatabase(ctx context.Context, targetURL string) error {
	u, err := url.Parse(targetURL)
	if err != nil {
		return fmt.Errorf("db: parsing target URL: %w", err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" {
		return fmt.Errorf("db: target URL has no database name")
	}

	adminURL := *u
	adminURL.Path = "/postgres"
	conn, err := pgx.Connect(ctx, adminURL.String())
	if err != nil {
		return fmt.Errorf("db: connecting to maintenance database: %w", err)
	}
	defer conn.Close(ctx)

	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, name,
	).Scan(&exists); err != nil {
		return fmt.Errorf("db: checking pg_database: %w", err)
	}
	if exists {
		return nil
	}

	// name comes from THIS PROCESS'S OWN config (DemoDatabaseURL, derived
	// from or set alongside DatabaseURL — never from request input), so
	// interpolating it into DDL carries no injection surface; CREATE
	// DATABASE has no parameterized form to begin with. Quoted regardless,
	// the same defensive-quoting convention internal/demo's own DDL uses for
	// its schema names.
	quoted := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+quoted); err != nil {
		return fmt.Errorf("db: creating database %s: %w", name, err)
	}
	return nil
}
