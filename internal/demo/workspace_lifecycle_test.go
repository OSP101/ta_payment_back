package demo

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/auth"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/db"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/testutil"
)

func lifecycleManager(t *testing.T) (*Manager, *Slot) {
	t.Helper()
	slot, registry := newTestSlot(t)
	ctx := context.Background()
	// Registry has a separate migration history in the demo database in production.
	// This throwaway fixture already has the application history in public.
	files, err := filepath.Glob(filepath.Join(testutil.MigrationsDir(t), "demo_registry", "*.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		sql, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := registry.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := registry.Exec(ctx, `INSERT INTO demo_workspaces(slot_index,schema_name) VALUES($1,$2)`, slot.Index, slot.SchemaName); err != nil {
		t.Fatal(err)
	}
	if err := AddAuthorizedTester(ctx, registry, "tester@example.org", TierStaff, "", "test"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DemoJWTSecret: "demo-test-secret", DemoIdleReclaimDays: 14, JWTLifetime: time.Hour}
	slot.Container.Cfg.JWTLifetime = time.Hour
	return &Manager{mainPool: registry, cfg: cfg, slots: []*Slot{slot}, tokens: auth.NewTokenService(cfg.DemoJWTSecret, "demo-test")}, slot
}

func requireDemoLogin(t *testing.T, m *Manager, slot *Slot) {
	t.Helper()
	app := fiber.New()
	h := &LoginHandler{Svc: slot.Container, Tokens: m.tokens, Manager: m, SlotIndex: slot.Index, BasePath: slotBasePath(slot.Index)}
	app.Post("/login", h.Login)
	// Exercise every picker account, including the HttpOnly slot-claim proof.
	for _, acc := range demoAccounts {
		req := httptest.NewRequest("POST", "/login", strings.NewReader(fmt.Sprintf(`{"email":%q,"password":%q}`, acc.Email, DemoPassword)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Cookie", claimCookieName+"="+m.SignSlotClaim(slot.Index, "tester@example.org"))
		res, err := app.Test(req, -1)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("picker login %s returned %d", acc.Email, res.StatusCode)
		}
	}
}

func TestDemoWorkspaceFreshClaimResetAndLogin(t *testing.T) {
	ctx := context.Background()
	m, slot := lifecycleManager(t)
	claimed, tier, err := m.Claim(ctx, "tester@example.org")
	if err != nil || claimed != slot || tier != TierStaff {
		t.Fatalf("claim: %v %v", tier, err)
	}
	requireDemoLogin(t, m, slot)
	if _, err := stepTerm(ctx, slot.Container); err != nil {
		t.Fatal(err)
	}
	if _, err := stepCourses(ctx, slot.Container); err != nil {
		t.Fatal(err)
	}
	if _, err := stepTASchedules(ctx, slot.Container); err != nil {
		t.Fatal(err)
	}
	if err := m.ResetSlot(ctx, slot); err != nil {
		t.Fatalf("reset after migration 0162: %v", err)
	}
	requireDemoLogin(t, m, slot)
	if got := countRows(t, slot.Pool, "academic_terms"); got != 0 {
		t.Fatalf("reset left %d terms", got)
	}
	if got := countRows(t, m.mainPool, "public.users"); got != 0 {
		t.Fatalf("demo wrote %d production users", got)
	}
}

func TestDemoWorkspaceFailedSeedRollsBackClaimAndData(t *testing.T) {
	ctx := context.Background()
	m, slot := lifecycleManager(t)
	var originalID string
	if err := slot.Pool.QueryRow(ctx, `SELECT id::text FROM users WHERE email='staff@demo.local'`).Scan(&originalID); err != nil {
		t.Fatal(err)
	}
	if _, err := slot.Pool.Exec(ctx, `CREATE FUNCTION reject_demo_seed() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected seed failure'; END $$;
 CREATE TRIGGER reject_seed BEFORE INSERT ON users FOR EACH ROW EXECUTE FUNCTION reject_demo_seed()`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Claim(ctx, "tester@example.org"); err == nil || !strings.Contains(err.Error(), "injected seed failure") {
		t.Fatalf("want seed failure, got %v", err)
	}
	var owner *string
	if err := m.mainPool.QueryRow(ctx, `SELECT owner_email::text FROM demo_workspaces WHERE slot_index=$1`, slot.Index).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != nil {
		t.Fatal("failed claim retained ownership")
	}
	var currentID string
	if err := slot.Pool.QueryRow(ctx, `SELECT id::text FROM users WHERE email='staff@demo.local'`).Scan(&currentID); err != nil || currentID != originalID {
		t.Fatalf("failed seed changed original data: %v", err)
	}
	if _, err := slot.Pool.Exec(ctx, `DROP TRIGGER reject_seed ON users`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Claim(ctx, "tester@example.org"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	requireDemoLogin(t, m, slot)
}

func TestDemoWorkspaceRepairsLegacyEmptyClaimAndPreservesResume(t *testing.T) {
	ctx := context.Background()
	m, slot := lifecycleManager(t)
	tx, err := slot.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tables, err := slotTableNames(ctx, slot)
	if err != nil {
		t.Fatal(err)
	}
	if err := truncateTables(ctx, tx, slot.SchemaName, tables); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.mainPool.Exec(ctx, `UPDATE demo_workspaces SET owner_email='tester@example.org' WHERE slot_index=$1`, slot.Index); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Claim(ctx, "tester@example.org"); err != nil {
		t.Fatalf("repair empty legacy claim: %v", err)
	}
	requireDemoLogin(t, m, slot)
	var id string
	if err := slot.Pool.QueryRow(ctx, `SELECT id::text FROM users WHERE email='staff@demo.local'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := stepTerm(ctx, slot.Container); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Claim(ctx, "tester@example.org"); err != nil {
		t.Fatal(err)
	}
	var resumed string
	if err := slot.Pool.QueryRow(ctx, `SELECT id::text FROM users WHERE email='staff@demo.local'`).Scan(&resumed); err != nil {
		t.Fatal(err)
	}
	if id != resumed || countRows(t, slot.Pool, "academic_terms") != 1 {
		t.Fatal("resume reset an existing workspace")
	}
}

func TestDemoSnapshotResetGuardAndCheckpoint(t *testing.T) {
	ctx := context.Background()
	m, slot := lifecycleManager(t)
	// Even with the reset flag, production cannot truncate history.
	tx, err := m.mainPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('ta_payment.demo_reset','on',true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `TRUNCATE public.ta_request_calculation_snapshots`); err == nil {
		t.Fatal("production snapshot protection bypassed")
	}
	tx.Rollback(ctx)
	if _, err := slot.Pool.Exec(ctx, `TRUNCATE ta_request_calculation_snapshots`); err == nil {
		t.Fatal("demo truncate allowed without explicit reset")
	}
	for _, step := range []func(context.Context, *service.Container) (string, error){stepTerm, stepCourses, stepTASchedules, stepTARequests} {
		if _, err := step(ctx, slot.Container); err != nil {
			t.Fatal(err)
		}
	}
	snapshots := countRows(t, slot.Pool, "ta_request_calculation_snapshots")
	if snapshots == 0 {
		t.Fatal("scenario created no calculation snapshots")
	}
	for _, stmt := range []string{
		`UPDATE ta_request_calculation_snapshots SET event='changed'`,
		`DELETE FROM ta_request_calculation_snapshots`,
	} {
		if _, err := slot.Pool.Exec(ctx, stmt); err == nil {
			t.Fatalf("append-only guard accepted %s", stmt)
		}
	}
	if err := m.SaveCheckpoint(ctx, slot); err != nil {
		t.Fatal(err)
	}
	// Mimic an older checkpoint that included derived edit-version rows.
	if _, err := slot.Pool.Exec(ctx, `CREATE TABLE `+pgxIdentifier(checkpointSchemaName(slot), "edit_resource_versions")+` AS TABLE edit_resource_versions`); err != nil {
		t.Fatal(err)
	}
	if _, err := slot.Pool.Exec(ctx, `INSERT INTO ta_request_calculation_snapshots(id,request_id,actor_id,event,inputs) SELECT gen_random_uuid(),request_id,actor_id,'after_checkpoint',inputs FROM ta_request_calculation_snapshots LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if err := m.RestoreCheckpoint(ctx, slot); err != nil {
		t.Fatalf("checkpoint restore: %v", err)
	}
	if got := countRows(t, slot.Pool, "ta_request_calculation_snapshots"); got != snapshots {
		t.Fatalf("restored %d snapshots, want %d", got, snapshots)
	}
	if err := m.resetSlotData(ctx, slot); err != nil {
		t.Fatal(err)
	}
	if countRows(t, slot.Pool, "ta_request_calculation_snapshots") != 0 {
		t.Fatal("reset retained demo snapshots")
	}
	// A slot-only maintenance reset must never accept production as its target.
	if err := m.resetSlotData(ctx, &Slot{Pool: m.mainPool, SchemaName: "public", Container: slot.Container}); err == nil {
		t.Fatal("reset accepted production")
	}
	// Replaying migrations is idempotent after the forward repair.
	if err := db.CheckMigrations(ctx, slot.Pool, testutil.MigrationsDir(t)); err != nil {
		t.Fatal(err)
	}
}

// A single registry connection must initialize multiple independently typed
// schemas without reusing the first schema's enum parameter descriptions.
func TestDemoClaimsAcrossSchemasOnSameConnection(t *testing.T) {
	ctx := context.Background()
	m, first := lifecycleManager(t)
	cfg := m.mainPool.Config().Copy()
	cfg.MaxConns = 1
	registry, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registry.Close)
	m.mainPool = registry
	schema := "demo_slot_8"
	if _, err := registry.Exec(ctx, `CREATE SCHEMA `+quoteIdent(schema)); err != nil {
		t.Fatal(err)
	}
	pool, err := openSlotPool(ctx, cfg.ConnString(), schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, testutil.MigrationsDir(t)); err != nil {
		t.Fatal(err)
	}
	second := &Slot{Index: 8, SchemaName: schema, Pool: pool, Container: &service.Container{Pool: pool, Cfg: config.Config{IsDemoSlot: true}}}
	m.slots = append(m.slots, second)
	if _, err := registry.Exec(ctx, `INSERT INTO demo_workspaces(slot_index,schema_name) VALUES(8,$1)`, schema); err != nil {
		t.Fatal(err)
	}
	if err := AddAuthorizedTester(ctx, registry, "second@example.org", TierStaff, "", "test"); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"tester@example.org", "second@example.org"} {
		if _, _, err := m.Claim(ctx, email); err != nil {
			t.Fatalf("claim across schemas: %v", err)
		}
	}
	for _, slot := range []*Slot{first, second} {
		if n := countRows(t, slot.Pool, "users"); n != len(demoAccounts) {
			t.Fatalf("%s has %d accounts", slot.SchemaName, n)
		}
		var hash string
		if err := slot.Pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE email='ta1@demo.local'`).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		if !auth.CheckPassword(hash, DemoPassword) {
			t.Fatalf("%s password mismatch", slot.SchemaName)
		}
	}
}
