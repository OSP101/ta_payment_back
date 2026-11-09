package db_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"ta-payment-back/internal/db"
	"ta-payment-back/internal/testutil"
)

func TestRuntimeSchemaCheckRejectsMissingMigrationWithoutApplyingIt(t *testing.T) {
	p := testutil.NewPool(t)
	ctx := context.Background()
	if err := db.CheckMigrations(ctx,p,testutil.MigrationsDir(t)); err != nil { t.Fatal(err) }
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir,"9999_required.up.sql"),[]byte("CREATE TABLE should_not_exist(id int)"),0600); err != nil { t.Fatal(err) }
	if err := db.CheckMigrations(ctx,p,dir); err == nil { t.Fatal("old schema was accepted") }
	var exists bool
	if err := p.QueryRow(ctx,`SELECT to_regclass('should_not_exist') IS NOT NULL`).Scan(&exists); err != nil { t.Fatal(err) }
	if exists { t.Fatal("runtime readiness executed DDL") }
}
