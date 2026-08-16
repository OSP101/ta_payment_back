// Package demo implements "โหมดทดลอง" — the sandbox staff use to teach new
// TAs, lecturers, and staff the real system, on an ongoing basis, without a
// real academic year's data existing yet or being at risk. A fixed pool of
// fully-isolated copies of this system (one Postgres schema + one
// service.Container each) that anyone staff has authorized (see tiers.go and
// migrations/demo_registry) can log into and drive through every real
// service call.
//
// A permanent feature, not removable BETA scaffolding — see
// config.DemoDatabaseURL's doc comment for the boundary that reflects: every
// piece of this lives in its own Postgres DATABASE, entirely separate from
// production's, so nothing here can compete with production for connections
// or disk, corrupt production's backups, or need production downtime to
// reset.
//
// See docs/PLAN-demo-sandbox.md for the design this implements. The short
// version: nothing here is a mock. A demo slot is migrated by the exact same
// internal/db.Migrate every real deployment uses and served by the exact
// same internal/handler routes (via handler.MountAPI) bound to its own
// service.Container — so every validation rule, every audit-log write, every
// Thai error message behaves identically to production. The ONLY things that
// differ are: which database/schema the slot's pool points at, its outbound
// mail being logged instead of sent (see Bootstrap), and its files living
// under UploadDir/demo/<schema> instead of UploadDir directly.
//
// Slots are pre-provisioned at boot (Bootstrap), never created per request —
// see migration 0086's doc comment for why. "Claiming" a slot only assigns
// an already-migrated schema to an email and wipes+reseeds its data; it
// never runs DDL, so it is cheap enough to do synchronously inside an HTTP
// handler.
package demo

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/auth"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/db"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/pii"
	"ta-payment-back/internal/rbac"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/storage"
)

// ErrFull is returned by Claim when every slot is already owned by someone
// else. The caller (the /demo/enter handler) turns this into a plain Thai
// "sandbox เต็ม" message — there is no queueing in phase 0.
var ErrFull = errors.New("demo: no free workspace slot")

// Slot is one fully-provisioned, independently-migrated sandbox. Its Pool,
// Container, RBAC and Auditor are built once in Bootstrap and live for the
// process's lifetime — Claim/Reset only change the DATA inside SchemaName,
// never these Go values, which is what keeps route mounting (Manager.Mount)
// a one-time, boot-time operation. See package doc.
type Slot struct {
	Index      int
	SchemaName string
	Pool       *pgxpool.Pool
	Container  *service.Container
	RBAC       *rbac.RBAC
	Auditor    *audit.Auditor
}

// Manager owns every slot plus the one demo-only token service every slot's
// mounted routes authenticate against (see DemoAuthenticated). A single
// shared TokenService is safe because isolation between slots comes from
// which Container a slot's routes are bound to and from the path they are
// mounted under (MountAll), not from the token itself — a demo token for
// slot 3 simply carries a user id that only exists in slot 3's schema, so
// AccountGuard's live DB check rejects it against any other slot.
type Manager struct {
	mainPool      *pgxpool.Pool
	cfg           config.Config
	piiCipher     *pii.Cipher
	totpCipher    *pii.Cipher
	encKey        []byte
	migrationsDir string
	tokens        *auth.TokenService

	slots []*Slot
}

// Bootstrap ensures the demo_workspaces registry (migrations/demo_registry,
// already applied to mainPool by the caller — see main.go) has exactly
// cfg.DemoMaxWorkspaces rows, then for each one: creates its schema if
// missing, opens a dedicated small pool scoped to it via search_path,
// replays every real migration into it (idempotent — db.Migrate tracks
// applied versions per schema, same as it does for the real database), and
// builds that slot's service.Container. mainPool and every slot pool point
// at cfg.DemoDatabaseURL — a database entirely separate from production's
// own (see that field's doc comment in internal/config).
//
// Must run before the caller starts accepting traffic (see Mount's doc
// comment on why route registration cannot happen after Listen). Safe to
// call on every boot: an already-migrated slot's Migrate call is a fast
// no-op, and existing owner_email assignments in demo_workspaces are left
// untouched, so a redeploy does not evict anyone mid-walkthrough.
func Bootstrap(ctx context.Context, mainPool *pgxpool.Pool, cfg config.Config, piiCipher *pii.Cipher, totpCipher *pii.Cipher, encKey []byte, migrationsDir string) (*Manager, error) {
	if cfg.DemoMaxWorkspaces <= 0 {
		return nil, fmt.Errorf("demo: DEMO_MAX_WORKSPACES must be positive, got %d", cfg.DemoMaxWorkspaces)
	}
	m := &Manager{
		mainPool:      mainPool,
		cfg:           cfg,
		piiCipher:     piiCipher,
		totpCipher:    totpCipher,
		encKey:        encKey,
		migrationsDir: migrationsDir,
		tokens:        auth.NewTokenService(cfg.DemoJWTSecret, cfg.JWTIssuer+"-demo"),
	}

	for i := 0; i < cfg.DemoMaxWorkspaces; i++ {
		schemaName := fmt.Sprintf("demo_slot_%d", i)
		if _, err := mainPool.Exec(ctx,
			`INSERT INTO demo_workspaces (slot_index, schema_name) VALUES ($1,$2) ON CONFLICT (slot_index) DO NOTHING`,
			i, schemaName); err != nil {
			return nil, fmt.Errorf("demo: registering slot %d: %w", i, err)
		}

		// schemaName is built entirely from a loop index this process
		// controls (never from request input), so interpolating it into DDL
		// here carries no injection surface — there is nothing a caller
		// could supply that reaches this string.
		if _, err := mainPool.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, schemaName)); err != nil {
			return nil, fmt.Errorf("demo: creating schema %s: %w", schemaName, err)
		}

		slotPool, err := openSlotPool(ctx, cfg.DemoDatabaseURL, schemaName)
		if err != nil {
			return nil, fmt.Errorf("demo: connecting slot %d: %w", i, err)
		}
		if err := db.Migrate(ctx, slotPool, migrationsDir); err != nil {
			return nil, fmt.Errorf("demo: migrating slot %d: %w", i, err)
		}

		slotCfg := cfg
		// The ONLY correct signal for "skip mandatory 2FA, this is a demo
		// account" — see config.Config.IsDemoSlot's own doc comment. cfg.DemoMode
		// is NOT usable for this: it is true on the real production Config too
		// whenever the sandbox feature is merely switched on, since main.go
		// builds its own real service.Container with the same cfg. IsDemoSlot is
		// set here, on the per-slot copy only, and never on cfg itself.
		slotCfg.IsDemoSlot = true
		// Never send real email from a sandbox. mail.Mailer already no-ops
		// (logs and returns nil) whenever SMTPHost is empty — see
		// internal/mail.Mailer.Send — so this is the whole fix, not a
		// stand-in implementation. A real in-app "outbox" viewer (so staff
		// can see what WOULD have been sent) is phase 2 work; the log line
		// is the phase 0 substitute.
		slotCfg.SMTPHost = ""
		mailer := mail.New(slotCfg)

		store, err := storage.NewLocalWithKey(filepath.Join(cfg.UploadDir, "demo", schemaName), encKey)
		if err != nil {
			return nil, fmt.Errorf("demo: storage for slot %d: %w", i, err)
		}

		auditor := audit.New(slotPool)
		container := service.NewContainer(slotPool, store, mailer, auditor, slotCfg, piiCipher, totpCipher)

		m.slots = append(m.slots, &Slot{
			Index:      i,
			SchemaName: schemaName,
			Pool:       slotPool,
			Container:  container,
			RBAC:       rbac.New(slotPool),
			Auditor:    auditor,
		})
	}

	log.Printf("demo: sandbox ready — %d slot(s) migrated under demo_slot_N schemas", len(m.slots))
	return m, nil
}

// openSlotPool mirrors internal/db.Connect (same timezone pin — every date
// rule in this system assumes Asia/Bangkok, see internal/timeutil) but adds
// search_path so unqualified DDL/DML from the replayed migrations and every
// service query lands in schemaName, falling back to public only for
// objects a slot never creates itself (pgcrypto/citext's functions).
// Deliberately small pools: DEMO_MAX_WORKSPACES slots each holding a full
// pgxpool would otherwise multiply the connection budget by N for a feature
// a handful of people use at a time.
func openSlotPool(ctx context.Context, databaseURL, schemaName string) (*pgxpool.Pool, error) {
	pcfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	pcfg.MaxConns = 4
	pcfg.MinConns = 0
	pcfg.MaxConnLifetime = 30 * time.Minute
	if pcfg.ConnConfig.RuntimeParams == nil {
		pcfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	pcfg.ConnConfig.RuntimeParams["timezone"] = "Asia/Bangkok"
	pcfg.ConnConfig.RuntimeParams["search_path"] = schemaName + ",public"

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// Tokens returns the shared demo TokenService — used by both DemoAuthenticated
// and the per-slot LoginHandler so a token issued by one can be parsed by
// the other. See Manager's doc comment for why sharing it across slots is
// safe.
func (m *Manager) Tokens() *auth.TokenService { return m.tokens }

// Slots returns every provisioned slot, in index order — Mount is the only
// expected caller.
func (m *Manager) Slots() []*Slot { return m.slots }

// Accounts lists the seeded demo logins, for the /demo/enter response to
// hand the frontend a role picker without hardcoding a copy of seedSlot's
// list.
func (m *Manager) Accounts() []DemoAccount { return demoAccounts }

// Claim finds ownerEmail's existing slot (a resume — its data is left
// exactly as they last left it) or assigns them a free one (a fresh start —
// its data is wiped and reseeded). If none are free, it falls back to
// reclaiming the least-recently-active CLAIMED slot, but only if it has sat
// untouched past config.DemoIdleReclaimDays — see that field's doc comment
// for why a fixed-size pool needs this for a feature meant to onboard
// people indefinitely. Returns ErrNotAuthorized if ownerEmail has no
// demo_authorized_testers row (checked before either the resume or the
// free-slot path, so a revoked email can't even resume a slot it already
// held), or ErrFull if every slot is either free-less AND still active. The
// returned Tier is what /api/demo/enter uses to decide which account cards
// to offer — see AccountsForTier.
func (m *Manager) Claim(ctx context.Context, ownerEmail string) (*Slot, Tier, error) {
	ownerEmail = strings.ToLower(strings.TrimSpace(ownerEmail))
	if ownerEmail == "" {
		return nil, "", errors.New("demo: email is required")
	}
	tier, ok, err := LookupTier(ctx, m.mainPool, ownerEmail)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "", ErrNotAuthorized
	}

	if slot, err, ok := m.resumeOwnedSlot(ctx, ownerEmail); ok {
		return slot, tier, err
	}

	tx, err := m.mainPool.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)

	var claimedIndex int
	err = tx.QueryRow(ctx,
		`SELECT slot_index FROM demo_workspaces
		 WHERE owner_email IS NULL
		 ORDER BY slot_index
		 LIMIT 1
		 FOR UPDATE SKIP LOCKED`,
	).Scan(&claimedIndex)
	if isNoRows(err) {
		// No free slot — reach for the least-recently-active claimed one,
		// but only if it's genuinely been sitting idle. COALESCE covers rows
		// from before last_active_at existed (migration 0089) and the
		// vanishingly small window between a fresh claim and its first
		// touch, in both cases falling back to claimed_at.
		err = tx.QueryRow(ctx,
			`SELECT slot_index FROM demo_workspaces
			 WHERE owner_email IS NOT NULL
			   AND COALESCE(last_active_at, claimed_at) < NOW() - make_interval(days => $1)
			 ORDER BY COALESCE(last_active_at, claimed_at) ASC
			 LIMIT 1
			 FOR UPDATE SKIP LOCKED`,
			m.cfg.DemoIdleReclaimDays,
		).Scan(&claimedIndex)
		if isNoRows(err) {
			return nil, "", ErrFull
		}
		if err != nil {
			return nil, "", err
		}
	} else if err != nil {
		return nil, "", err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE demo_workspaces SET owner_email=$1, claimed_at=NOW(), last_reset_at=NOW(), last_active_at=NOW() WHERE slot_index=$2`,
		ownerEmail, claimedIndex); err != nil {
		if isUniqueViolation(err) {
			// Lost a race: between our own lookup above and this UPDATE, a
			// CONCURRENT Claim call for this exact email won and already
			// claimed a (different) slot — the partial unique index on
			// owner_email (migration 0090) is what makes this UPDATE fail
			// instead of silently succeeding twice. Their slot is the
			// correct answer now; resume it rather than failing this
			// request or retrying into the same race again.
			_ = tx.Rollback(ctx)
			if slot, err, ok := m.resumeOwnedSlot(ctx, ownerEmail); ok {
				return slot, tier, err
			}
			return nil, "", fmt.Errorf("demo: lost claim race for %s but no owned slot found on retry", ownerEmail)
		}
		return nil, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", err
	}

	slot, err := m.slotByIndex(claimedIndex)
	if err != nil {
		return nil, "", err
	}
	if err := m.resetSlotData(ctx, slot); err != nil {
		return nil, "", fmt.Errorf("demo: seeding newly-claimed slot %d: %w", claimedIndex, err)
	}
	return slot, tier, nil
}

// resumeOwnedSlot looks up whatever slot ownerEmail currently owns, if any.
// The bool return is "does ownerEmail own a slot" — false means Claim should
// proceed to the free/idle-reclaim path, NOT that an error occurred (a plain
// error there would conflate "no slot yet" with "lookup failed"). Shared by
// Claim's normal resume path and its unique-violation retry path (see the
// doc comment on that Exec call) so both go through the identical
// lookup-and-touch-activity logic.
func (m *Manager) resumeOwnedSlot(ctx context.Context, ownerEmail string) (*Slot, error, bool) {
	var existing *int
	if err := m.mainPool.QueryRow(ctx,
		`SELECT slot_index FROM demo_workspaces WHERE owner_email = $1`, ownerEmail,
	).Scan(&existing); err != nil && !isNoRows(err) {
		return nil, err, true
	}
	if existing == nil {
		return nil, nil, false
	}
	slot, err := m.slotByIndex(*existing)
	if err == nil {
		// Best-effort: a resume is activity too, but failing to record that
		// must never block the resume itself.
		_, _ = m.mainPool.Exec(ctx, `UPDATE demo_workspaces SET last_active_at=NOW() WHERE slot_index=$1`, *existing)
	}
	return slot, err, true
}

// isUniqueViolation reports whether err is a Postgres unique_violation
// (SQLSTATE 23505) — hardcoded rather than pulled from a constants package,
// since this is the one place in the codebase that needs it and Postgres's
// error codes are a stable, documented part of its wire protocol, not an
// implementation detail likely to drift.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// TouchSlotActivity marks idx as active right now — called after every
// successful demo login (see LoginHandler.Login) so Claim's idle-reclaim
// fallback above never mistakes someone still using their slot for one
// safe to hand to a new tester. Best-effort by design at the call site: a
// login must never fail because this bookkeeping update did.
func (m *Manager) TouchSlotActivity(ctx context.Context, idx int) error {
	_, err := m.mainPool.Exec(ctx, `UPDATE demo_workspaces SET last_active_at=NOW() WHERE slot_index=$1`, idx)
	return err
}

// Release frees ownerEmail's slot, if they hold one, WITHOUT wiping its
// data immediately — the next claimant's Claim() call wipes+reseeds it as
// normal, same as any other newly-claimed slot. Used when staff revoke
// someone's demo access (see admin.go's DELETE /demo-testers/:email) so
// revoking access also gives the slot back to the pool, instead of leaving
// it permanently held by someone who can no longer log into it anyway.
// Reports whether a slot was actually released, purely for the caller's own
// response — not an error either way if ownerEmail held nothing.
func (m *Manager) Release(ctx context.Context, ownerEmail string) (bool, error) {
	ownerEmail = strings.ToLower(strings.TrimSpace(ownerEmail))
	tag, err := m.mainPool.Exec(ctx, `UPDATE demo_workspaces SET owner_email=NULL WHERE owner_email=$1`, ownerEmail)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// TierForSlotIndex resolves the tier CURRENTLY authorized for whoever owns
// slot idx, looked up fresh — never cached — so a tier change or an outright
// revocation in demo_authorized_testers takes effect on that slot's very
// next login, not its next visit. LoginHandler.Login is the one caller that
// matters: the same 8 slots get reassigned to many different real testers
// over the sandbox's lifetime, so trusting a tier resolved once at Mount
// (boot) time would keep granting a departed tester's old access to
// whoever claims their old slot next.
func (m *Manager) TierForSlotIndex(ctx context.Context, idx int) (Tier, error) {
	// owner_email is nullable — NULL means the slot is free (see migration
	// 0086's own comment) — which is the everyday state for most of the 8
	// slots at any given moment. A plain string scan target errors on that
	// NULL (pgx v5 rejects NULL→non-pointer-string), and every slot's login
	// route is reachable unconditionally regardless of claim status (see
	// handlers.go's Mount), so an unclaimed slot's login attempt must reach
	// ErrNotAuthorized here, not a raw scan error surfaced as a 500.
	var email *string
	if err := m.mainPool.QueryRow(ctx,
		`SELECT owner_email FROM demo_workspaces WHERE slot_index = $1`, idx,
	).Scan(&email); err != nil {
		return "", err
	}
	if email == nil {
		return "", ErrNotAuthorized
	}
	tier, ok, err := LookupTier(ctx, m.mainPool, *email)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrNotAuthorized
	}
	return tier, nil
}

// Reset wipes and reseeds ownerEmail's own slot ("เริ่มใหม่ทั้งหมด" — see the
// plan doc's simulator panel). Ownership and slot assignment are untouched;
// only the data inside it changes. Used by the unauthenticated /api/demo/reset
// endpoint, where email is the only identity available.
func (m *Manager) Reset(ctx context.Context, ownerEmail string) (*Slot, error) {
	ownerEmail = strings.ToLower(strings.TrimSpace(ownerEmail))
	var idx int
	if err := m.mainPool.QueryRow(ctx,
		`SELECT slot_index FROM demo_workspaces WHERE owner_email = $1`, ownerEmail,
	).Scan(&idx); err != nil {
		if isNoRows(err) {
			return nil, errors.New("demo: no workspace claimed for this email")
		}
		return nil, err
	}
	slot, err := m.slotByIndex(idx)
	if err != nil {
		return nil, err
	}
	if err := m.ResetSlot(ctx, slot); err != nil {
		return nil, err
	}
	return slot, nil
}

// ResetSlot does the same wipe-and-reseed as Reset, keyed by an
// already-resolved Slot instead of an email — what the simulator panel's own
// in-session "เริ่มใหม่ทั้งหมด" route uses (handlers.go), since a request
// already inside a slot's authed routes has no reason to look its owner's
// email back up just to find the slot it is already standing in.
func (m *Manager) ResetSlot(ctx context.Context, slot *Slot) error {
	if err := m.resetSlotData(ctx, slot); err != nil {
		return err
	}
	// The panel's own reset confirmation tells staff this is irreversible
	// ("การกระทำนี้ย้อนกลับไม่ได้") — a checkpoint surviving the reset would
	// make that untrue, so it goes too.
	if _, err := slot.Pool.Exec(ctx, `DROP SCHEMA IF EXISTS `+quoteIdent(checkpointSchemaName(slot))+` CASCADE`); err != nil {
		return fmt.Errorf("clearing checkpoint on reset: %w", err)
	}
	_, err := m.mainPool.Exec(ctx,
		`UPDATE demo_workspaces SET last_reset_at=NOW(), last_active_at=NOW(), checkpoint_saved_at=NULL WHERE slot_index=$1`, slot.Index)
	return err
}

func (m *Manager) slotByIndex(i int) (*Slot, error) {
	for _, s := range m.slots {
		if s.Index == i {
			return s, nil
		}
	}
	return nil, fmt.Errorf("demo: slot %d not provisioned", i)
}

// resetSlotData empties every real table in slot's schema (everything
// db.Migrate created, i.e. everything except its own schema_migrations
// bookkeeping table) and reseeds it. TRUNCATE ... CASCADE follows foreign
// keys wherever they point, but every table in a slot schema was created by
// replaying the migrations with search_path pinned to that schema alone —
// their REFERENCES clauses resolved only within it, so none of them point
// at public or at another slot. CASCADE here therefore never leaves
// slot.SchemaName; information_schema.tables is filtered to exactly that
// schema, so the table list itself can't name one that would.
func (m *Manager) resetSlotData(ctx context.Context, slot *Slot) error {
	tables, err := slotTableNames(ctx, slot)
	if err != nil {
		return err
	}
	if err := truncateTables(ctx, slot.Pool.Exec, slot.SchemaName, tables); err != nil {
		return fmt.Errorf("truncating slot %d: %w", slot.Index, err)
	}
	return seedSlot(ctx, slot.Container)
}

// slotTableNames lists every real table in slot's schema — everything
// db.Migrate created, i.e. everything except its own schema_migrations
// bookkeeping table. Shared by resetSlotData and checkpoint.go's save/
// restore, which all need the same "every table this slot has" list.
func slotTableNames(ctx context.Context, slot *Slot) ([]string, error) {
	return tableNamesInSchema(ctx, slot.Pool, slot.SchemaName)
}

// tableNamesInSchema is slotTableNames generalized to an arbitrary schema in
// the same pool — RestoreCheckpoint needs the CHECKPOINT schema's own table
// list (see that function's doc comment for why: it must not assume the
// live schema and the checkpoint schema still have the same tables), which
// slot.SchemaName-bound slotTableNames can't express.
func tableNamesInSchema(ctx context.Context, pool *pgxpool.Pool, schema string) ([]string, error) {
	rows, err := pool.Query(ctx,
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema = $1 AND table_type = 'BASE TABLE' AND table_name <> 'schema_migrations'`,
		schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		tables = append(tables, t)
	}
	return tables, rows.Err()
}

// truncateTables empties every listed table via one TRUNCATE ... CASCADE.
// CASCADE follows foreign keys wherever they point, but every table in a
// slot schema was created by replaying the migrations with search_path
// pinned to that schema alone — their REFERENCES clauses resolved only
// within it, so none of them point at public or at another slot; CASCADE
// here can never leave schema. execFn is *pgxpool.Pool.Exec or pgx.Tx.Exec —
// RestoreCheckpoint needs the transactional one so a truncate that
// succeeds but a restore that fails rolls back together.
func truncateTables(ctx context.Context, execFn func(context.Context, string, ...any) (pgconn.CommandTag, error), schema string, tables []string) error {
	if len(tables) == 0 {
		return nil
	}
	qualified := make([]string, len(tables))
	for i, t := range tables {
		// Table names come from the database's own catalog, not request
		// input, but they are quoted properly regardless — cheap and
		// removes any need to trust that every table this project ever
		// adds stays a plain lowercase identifier.
		qualified[i] = pgxIdentifier(schema, t)
	}
	_, err := execFn(ctx, "TRUNCATE TABLE "+strings.Join(qualified, ", ")+" CASCADE")
	return err
}

func pgxIdentifier(schema, table string) string {
	return quoteIdent(schema) + "." + quoteIdent(table)
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
