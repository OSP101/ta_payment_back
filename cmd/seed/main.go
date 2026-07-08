// Package main seeds an initial admin user and reference data.
// Usage: go run ./cmd/seed
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/google/uuid"

	"ta-payment-back/internal/auth"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/db"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, envDefault("MIGRATIONS_DIR", "./migrations")); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	adminEmail := envDefault("SEED_ADMIN_EMAIL", "admin@coco.kku.ac.th")
	adminPass := envDefault("SEED_ADMIN_PASSWORD", "changeme123!")

	var exists int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE email=$1`, adminEmail).Scan(&exists)
	if exists == 0 {
		hash, _ := auth.HashPassword(adminPass)
		id := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, email, first_name, last_name, password_hash, is_active, profile_completed)
			 VALUES ($1,$2,$3,$4,$5,TRUE,TRUE)`,
			id, adminEmail, "Admin", "COCO", hash); err != nil {
			log.Fatalf("insert admin: %v", err)
		}
		for _, r := range []string{"admin", "staff"} {
			if _, err := pool.Exec(ctx,
				`INSERT INTO user_roles (user_id, role) VALUES ($1,$2::role_code) ON CONFLICT DO NOTHING`,
				id, r); err != nil {
				log.Fatalf("insert role: %v", err)
			}
		}
		fmt.Printf("Seeded admin: %s / %s\n", adminEmail, adminPass)
	} else {
		fmt.Printf("Admin %s exists — skipping user seed\n", adminEmail)
	}

	// Reference data: pay rate + hour caps + budget cap (defaults; edit via UI)
	// undergrad hourly + graduate flat monthly + workload-formula constants from CC finance sheet.
	pool.Exec(ctx, `INSERT INTO pay_rates (id, effective_from,
	                    undergrad_regular, undergrad_special,
	                    graduate_regular, graduate_special_lumpsum,
	                    ug_lecture_hours_per_credit, ug_lab_hours_per_credit,
	                    baseline_students_lecture, baseline_students_lab,
	                    ug_workload_rate_regular, ug_workload_rate_special, term_months, note)
	                SELECT gen_random_uuid(), CURRENT_DATE,
	                       40, 50,
	                       3000, 4000,
	                       3, 4.5, 60, 30, 200, 250, 4, 'seed defaults from Excel workbook tab 2_59 ป.ตรี'
	                WHERE NOT EXISTS (SELECT 1 FROM pay_rates)`)
	pool.Exec(ctx, `INSERT INTO budget_caps (id, effective_from, per_course_max, note)
	                SELECT gen_random_uuid(), CURRENT_DATE, 20000, 'seed default'
	                WHERE NOT EXISTS (SELECT 1 FROM budget_caps)`)
	for credits, hrs := range map[int]int{1: 10, 2: 20, 3: 30, 4: 40} {
		pool.Exec(ctx, `INSERT INTO hour_caps (id, credits, hours_cap, note) VALUES (gen_random_uuid(),$1,$2,'seed')
		                ON CONFLICT (credits) DO NOTHING`, credits, hrs)
	}
	fmt.Println("Seed complete.")
}

func envDefault(k, d string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return d
}
