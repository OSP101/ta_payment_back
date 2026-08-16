-- 14/08/2026 (moved to its own database 15/08/2026 — see below) — registry
-- for "โหมดทดลอง" (demo sandbox), the permanent onboarding feature staff use
-- to teach new TAs/lecturers/staff the real system without a real academic
-- year's data. See docs/PLAN-demo-sandbox.md.
--
-- This migration lives in migrations/demo_registry/, applied ONLY against
-- config.DemoDatabaseURL — a database entirely separate from production's
-- own (see that field's doc comment in internal/config). It used to live in
-- the main migrations/ directory and run against production's public
-- schema alongside every real table; moved out once the sandbox stopped
-- being removable BETA scaffolding and became something meant to stay
-- forever, so it earned the same "never touch production's database" bar
-- as everything else that isn't production. Everything else the demo
-- subsystem touches lives inside this SAME database's own `demo_slot_N`
-- schemas — a full independent clone of the real schema, migrated by the
-- same internal/db.Migrate (against the main migrations/ dir, not this
-- one), created and reset by internal/demo. Dropping this whole database is
-- the entire uninstall, if the feature is ever retired after all.
--
-- Rows are pre-provisioned at boot, one per slot 0..DEMO_MAX_WORKSPACES-1
-- (see internal/demo.Bootstrap) — never inserted per-login. A slot is either
-- free (owner_email NULL) or claimed by one officer's email, so "ข้อมูลของ
-- ใครของมัน" (each officer's data is their own) holds without needing a
-- workspace per login attempt.
CREATE TABLE demo_workspaces (
    slot_index    INT PRIMARY KEY,
    schema_name   TEXT NOT NULL UNIQUE,
    -- NULL = free. Set on claim, cleared only by an explicit release/reset
    -- action — a slot survives a server restart still assigned to whoever
    -- had it, so their in-progress walkthrough isn't lost to a redeploy.
    owner_email   CITEXT,
    claimed_at    TIMESTAMPTZ,
    last_reset_at TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE demo_workspaces IS
    'Registry of fixed demo-sandbox slots, each backed by its own demo_slot_N schema in this SEPARATE demo database. See internal/demo and docs/PLAN-demo-sandbox.md.';
