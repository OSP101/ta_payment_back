// checkpoint.go implements "บันทึกจุดตรวจสอบ" / "ย้อนกลับไปจุดตรวจสอบ" — see
// docs/PLAN-demo-sandbox.md §5 and migration 0087's doc comment. One named
// checkpoint per workspace (not several): saving overwrites whatever was
// there before. Staff who want more than one saved point today do it the
// same way they always could — open a second workspace via /demo with a
// different email.
//
// A checkpoint is a full table-by-table copy of the slot's live data into a
// sibling schema (<slot>_checkpoint), NOT a pg_dump/pg_restore: those
// binaries may not exist wherever this process runs, and this needs nothing
// they provide beyond what plain SQL already does here. "CREATE TABLE ... AS
// TABLE ..." copies structure+rows with no server-side tooling dependency.
package demo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

func checkpointSchemaName(slot *Slot) string {
	return slot.SchemaName + "_checkpoint"
}

// SaveCheckpoint overwrites this slot's one checkpoint with its current
// data. CREATE TABLE ... AS TABLE carries no constraints, indexes, or
// defaults into the copy — irrelevant here, since checkpoint tables are only
// ever read back by RestoreCheckpoint, never queried or written to by the
// running application.
func (m *Manager) SaveCheckpoint(ctx context.Context, slot *Slot) error {
	tables, err := slotTableNames(ctx, slot)
	if err != nil {
		return err
	}
	cp := checkpointSchemaName(slot)
	if _, err := slot.Pool.Exec(ctx, `DROP SCHEMA IF EXISTS `+quoteIdent(cp)+` CASCADE`); err != nil {
		return fmt.Errorf("demo: clearing previous checkpoint: %w", err)
	}
	if _, err := slot.Pool.Exec(ctx, `CREATE SCHEMA `+quoteIdent(cp)); err != nil {
		return err
	}
	for _, t := range tables {
		stmt := fmt.Sprintf("CREATE TABLE %s AS TABLE %s", pgxIdentifier(cp, t), pgxIdentifier(slot.SchemaName, t))
		if _, err := slot.Pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("demo: copying %s into checkpoint: %w", t, err)
		}
	}
	_, err = m.mainPool.Exec(ctx,
		`UPDATE demo_workspaces SET checkpoint_saved_at = NOW() WHERE slot_index = $1`, slot.Index)
	return err
}

// RestoreCheckpoint replaces the slot's live data with whatever
// SaveCheckpoint last stored, entirely inside one transaction — a restore
// that can't finish leaves the workspace exactly as it was, never half
// truncated. Table order isn't known upfront (no hand-maintained dependency
// list to keep in sync with 87 migrations' worth of foreign keys), so this
// retries whatever inserts fail on a foreign key until a full pass makes no
// further progress — the same fixed-point approach a topological sort
// reaches, without needing to build the dependency graph explicitly. Each
// attempt runs under its own SAVEPOINT so one table's failure doesn't abort
// rows already restored earlier in the same pass.
func (m *Manager) RestoreCheckpoint(ctx context.Context, slot *Slot) error {
	cp := checkpointSchemaName(slot)
	var exists bool
	if err := slot.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`, cp,
	).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errors.New("demo: ห้องนี้ยังไม่เคยบันทึกจุดตรวจสอบไว้")
	}
	// The CHECKPOINT schema's own tables, not the live schema's — a
	// checkpoint saved before a later migration added a new table has no
	// row for that table in cp, and asking the live schema for its list
	// would include it anyway, making every restore attempt fail forever on
	// "relation cp.newtable does not exist" (indistinguishable, to the
	// retry loop below, from a genuine FK-ordering failure that eventually
	// resolves). Restoring exactly what the checkpoint actually has, and
	// leaving any newer live-only table untouched, is the correct fallback:
	// there is no checkpointed data for that table to restore in the first
	// place.
	tables, err := tableNamesInSchema(ctx, slot.Pool, cp)
	if err != nil {
		return err
	}

	tx, err := slot.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := truncateTables(ctx, tx.Exec, slot.SchemaName, tables); err != nil {
		return err
	}

	pending := tables
	for len(pending) > 0 {
		var stillPending []string
		var lastErr error
		for _, t := range pending {
			if _, err := tx.Exec(ctx, "SAVEPOINT restore_step"); err != nil {
				return err
			}
			stmt := fmt.Sprintf("INSERT INTO %s SELECT * FROM %s", pgxIdentifier(slot.SchemaName, t), pgxIdentifier(cp, t))
			if _, err := tx.Exec(ctx, stmt); err != nil {
				lastErr = err
				stillPending = append(stillPending, t)
				if _, rbErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT restore_step"); rbErr != nil {
					return rbErr
				}
				continue
			}
			if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT restore_step"); err != nil {
				return err
			}
		}
		if len(stillPending) == len(pending) {
			return fmt.Errorf("demo: คืนค่าจุดตรวจสอบไม่สำเร็จ (ตารางที่เหลือติดกันเอง: %v): %w", stillPending, lastErr)
		}
		pending = stillPending
	}

	return tx.Commit(ctx)
}

// CheckpointSavedAt reports when this slot's checkpoint was last saved, nil
// if it has never been saved — what the panel's "ย้อนกลับ" button uses to
// decide whether it has anything to restore, and what timestamp to show.
// Read from public.demo_workspaces (the main registry) rather than probing
// for the checkpoint schema's existence, since that column is exactly what
// SaveCheckpoint keeps in sync with the schema it describes.
func (m *Manager) CheckpointSavedAt(ctx context.Context, slot *Slot) (*time.Time, error) {
	var savedAt *time.Time
	err := m.mainPool.QueryRow(ctx,
		`SELECT checkpoint_saved_at FROM demo_workspaces WHERE slot_index = $1`, slot.Index,
	).Scan(&savedAt)
	return savedAt, err
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
