package db

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CheckMigrations is read-only and fails startup on an old/unprovisioned schema.
// Runtime roles do not need schema ownership or CREATE privileges.
func CheckMigrations(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	files, err := os.ReadDir(dir)
	if err != nil { return err }
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".up.sql") { continue }
		var applied bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, strings.TrimSuffix(f.Name(), ".up.sql")).Scan(&applied); err != nil { return err }
		if !applied { return fmt.Errorf("required migration missing: %s", f.Name()) }
	}
	return nil
}
