package db_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"ta-payment-back/internal/testutil"
)

func TestRestoreMissingAdminOfficerSeats(t *testing.T) {
	ctx := context.Background()
	for _, scenario := range []string{"empty", "partial", "complete"} {
		t.Run(scenario, func(t *testing.T) {
			pool := testutil.NewPool(t)
			if scenario != "complete" {
				if _, err := pool.Exec(ctx, `DELETE FROM admin_officers`); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "partial" {
				// Preserve an assigned active seat, an inactive seat, and a
				// custom title that is outside the standard roster.
				_, err := pool.Exec(ctx, `
					INSERT INTO users (id, email, first_name, last_name)
					VALUES ('01610000-0000-4000-8000-000000000001', 'roster-test@example.test', 'Test', 'Holder');
					INSERT INTO admin_officers (user_id, academic_prefix, full_name, title, is_active)
					VALUES ('01610000-0000-4000-8000-000000000001', 'ดร.', 'Test Holder', 'คณบดีวิทยาลัยการคอมพิวเตอร์', TRUE),
					(NULL, 'เดิม', 'Existing Name', 'รองคณบดีฝ่ายบริหาร', FALSE),
					(NULL, '', 'Custom Holder', 'ตำแหน่งเฉพาะเดิม', FALSE)`)
				if err != nil {
					t.Fatal(err)
				}
			}
			snapshot := func() string {
				t.Helper()
				var data string
				if err := pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(ao) ORDER BY id), '[]'::jsonb)::text FROM admin_officers ao`).Scan(&data); err != nil {
					t.Fatal(err)
				}
				return data
			}
			before := snapshot()
			apply := func(direction string) {
				t.Helper()
				body, err := os.ReadFile(filepath.Join(testutil.MigrationsDir(t), "0161_restore_missing_admin_officer_seats."+direction+".sql"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, string(body)); err != nil {
					t.Fatal(err)
				}
			}
			apply("up")
			var preserved, vacant bool
			var count int
			if err := pool.QueryRow(ctx, `
				SELECT NOT EXISTS (
				  SELECT 1 FROM jsonb_array_elements($1::jsonb) old
				  WHERE NOT EXISTS (SELECT 1 FROM admin_officers ao WHERE to_jsonb(ao) = old)
				), NOT EXISTS (
				  SELECT 1 FROM admin_officers ao
				  WHERE NOT EXISTS (SELECT 1 FROM jsonb_array_elements($1::jsonb) old WHERE old->>'id' = ao.id::text)
				  AND (user_id IS NOT NULL OR academic_prefix <> '' OR full_name <> '' OR is_active)
				), (SELECT count(*) FROM admin_officers)`, before).Scan(&preserved, &vacant, &count); err != nil {
				t.Fatal(err)
			}
			want := 7
			if scenario == "partial" {
				want++ // The custom seat is retained too.
			}
			if !preserved || !vacant || count != want {
				t.Fatalf("preserved=%v new seats vacant=%v rows=%d want=%d", preserved, vacant, count, want)
			}
			after := snapshot()
			apply("up")
			if snapshot() != after {
				t.Fatal("reapplying migration changed roster")
			}
			apply("down")
			if snapshot() != after {
				t.Fatal("rollback changed roster")
			}
		})
	}
}
