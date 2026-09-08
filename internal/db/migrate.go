package db

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrateAdvisoryLockKey is a constant, not a hash of anything — the whole
// system has exactly one caller of this lock, so there is nothing to
// namespace it against.
const migrateAdvisoryLockKey = 8471023

// Migrate applies all up SQL files in dir alphabetically. Tracks applied files
// in table `schema_migrations`. Idempotent.
func Migrate(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	// ล็อกทั้ง loop ไว้ด้วย advisory lock: การเช็ค schema_migrations แล้วค่อย
	// apply ไม่ atomic ข้ามโปรเซส ⇒ สอง instance ที่ boot พร้อมกันตอน rolling
	// deploy จะอ่านว่ายังไม่ apply ทั้งคู่ แล้วรันไฟล์เดียวกัน · ตัวที่แพ้จะชน
	// PK แล้วทำให้ main.go log.Fatalf ตายคาที่แทนที่จะขึ้นมาปกติ
	//
	// ต้องใช้ conn ตัวเดียวกันตลอด — advisory lock ผูกกับ session ถ้าใช้
	// pool ต่อ query ล็อกจะอยู่คนละ connection กับงาน ปล่อยล็อกด้วย defer
	// เสมอ แม้ migration จะพัง
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateAdvisoryLockKey); err != nil {
		return fmt.Errorf("migrate: acquiring advisory lock: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrateAdvisoryLockKey); err != nil {
			log.Printf("migrate: releasing advisory lock: %v", err)
		}
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
        version TEXT PRIMARY KEY,
        applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
    )`); err != nil {
		return err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	files := []string{}
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasSuffix(n, ".up.sql") {
			files = append(files, n)
		}
	}
	sort.Strings(files)

	for _, f := range files {
		version := strings.TrimSuffix(f, ".up.sql")
		var exists int
		if err := conn.QueryRow(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE version = $1`, version).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}
