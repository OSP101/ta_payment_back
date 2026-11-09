// Command migrate is a one-shot deployment job, never a runtime API route.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/config"
	"ta-payment-back/internal/db"
)

func main() {
	cfg, err := config.Load()
	if err != nil { log.Fatal("migration configuration invalid") }
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil { log.Fatal("migration database connection failed") }
	defer pool.Close()
	dir := os.Getenv("MIGRATIONS_DIR")
	if dir == "" { dir = "./migrations" }
	if err := db.Migrate(ctx, pool, dir); err != nil { log.Fatal(err) }
	if cfg.DemoMode {
		if err := db.EnsureDatabase(ctx, cfg.DemoDatabaseURL); err != nil { log.Fatal("demo database provisioning failed") }
		demoPool, err := db.Connect(ctx, cfg.DemoDatabaseURL)
		if err != nil { log.Fatal("demo database connection failed") }
		defer demoPool.Close()
		for _, ext := range []string{"pgcrypto", "citext"} {
			if _, err := demoPool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS "`+ext+`"`); err != nil { log.Fatal(err) }
		}
		if err := db.Migrate(ctx, demoPool, filepath.Join(dir, "demo_registry")); err != nil { log.Fatal(err) }
		for i := 0; i < cfg.DemoMaxWorkspaces; i++ {
			schema := fmt.Sprintf("demo_slot_%d", i)
			if _, err := demoPool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+schema); err != nil { log.Fatal(err) }
			// A dedicated pool pins all migration connections to this schema.
			pc := demoPool.Config()
			pc.ConnConfig.RuntimeParams["search_path"] = schema+",public"
			sp, err := pgxpool.NewWithConfig(ctx, pc)
			if err != nil { log.Fatal(err) }
			err = db.Migrate(ctx, sp, dir)
			sp.Close()
			if err != nil { log.Fatal(err) }
		}
	}
	log.Println("required migrations applied")
}
