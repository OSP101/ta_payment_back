// audit_snapshot.go records what a row looked like BEFORE a change, and after.
//
// The audit trail could say what a value became and never what it had been.
// Measured on the live table before this: 416 rows, 136 with an `after`, and
// ZERO with a `before`. For a system that moves money that is the wrong half —
// "someone set these hours to 54" is not a finding; "someone changed them from
// 40 to 54 on Tuesday" is.
//
// The mechanism is deliberately generic rather than forty hand-written "read
// the old values" blocks. Forty of those would drift: each would pick its own
// subset of columns, and the one column an investigation needed would be the
// one that writer happened not to list. A whole-row snapshot cannot have that
// gap, and a column added by a later migration appears in the trail without
// anybody remembering to add it.
package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

// auditRedactedColumns never reach the audit trail.
//
// A snapshot is a whole row, so it would otherwise copy secrets into a table
// that is kept for years and readable by every admin — turning the record of a
// password reset into a second copy of the hash. The values are replaced rather
// than dropped, so the trail still shows that the column CHANGED, which is the
// part an investigation needs.
var auditRedactedColumns = map[string]bool{
	"password_hash":   true,
	"totp_secret_enc": true,
	"citizen_id_enc":  true,
	// Recovery codes are login credentials in their own right.
	"recovery_codes":     true,
	"recovery_code_hash": true,
}

const auditRedacted = "[redacted]"

// auditSnapshotTables is the allowlist of tables a snapshot may read.
//
// The table name is interpolated into SQL — it cannot be a bind parameter — so
// it must never be able to come from a request. Every caller passes a literal
// today; this list is what keeps that true when one of them stops being a
// literal.
var auditSnapshotTables = map[string]bool{
	"work_logs":                true,
	"users":                    true,
	"teaching_courses":         true,
	"sections":                 true,
	"academic_terms":           true,
	"submission_periods":       true,
	"submission_period_status": true,
	"pay_rates":                true,
	"budget_caps":              true,
	"holidays":                 true,
	"announcements":            true,
	"ta_requests":              true,
	"ta_request_assignments":   true,
	"ta_profiles":              true,
	"ta_documents":             true,
	"admin_officers":           true,
	"ta_enrollments":           true,
	"ta_review_schedules":      true,
}

// snapshotRow reads one row as a JSON object on the caller's transaction.
//
// Reading INSIDE the transaction is the whole point: a "before" fetched on the
// pool beforehand is a value from before someone else's concurrent commit, and
// the trail would then report a change that never happened between those two
// numbers.
//
// A missing row returns (nil, nil), not an error — "there was nothing here" is
// a legitimate before-image for a create.
func snapshotRow(ctx context.Context, tx pgx.Tx, table string, id any) (map[string]any, error) {
	if !auditSnapshotTables[table] {
		return nil, fmt.Errorf("audit snapshot: %q is not an allowed table", table)
	}
	var raw []byte
	err := tx.QueryRow(ctx,
		`SELECT to_jsonb(t) FROM `+table+` t WHERE t.id = $1`, id).Scan(&raw)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	for col := range row {
		if auditRedactedColumns[col] {
			row[col] = auditRedacted
		}
	}
	return row, nil
}

// auditDiff drops the fields that did not change.
//
// A whole-row before/after pair on a wide table buries the one field that
// moved under thirty that did not, and the screen shows it as two walls of
// JSON. Keeping only the differing keys — on BOTH sides, so the pair still
// reads as "40 → 54" — is what makes the record answer a question at a glance.
//
// Returns (nil, nil) when nothing changed, so a no-op update is recorded as
// exactly that rather than as a change with an empty diff.
func auditDiff(before, after map[string]any) (map[string]any, map[string]any) {
	if before == nil || after == nil {
		return before, after
	}
	b, a := map[string]any{}, map[string]any{}
	for k, av := range after {
		bv, had := before[k]
		if !had || !jsonEqual(bv, av) {
			b[k], a[k] = bv, av
		}
	}
	// A column that existed before and is gone after (dropped mid-flight) is
	// still a change worth showing.
	for k, bv := range before {
		if _, had := after[k]; !had {
			b[k], a[k] = bv, nil
		}
	}
	if len(a) == 0 {
		return nil, nil
	}
	return b, a
}

func jsonEqual(a, b any) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ja) == string(jb)
}

// writeAuditedRow is writeAudited with the before/after images filled in.
//
// It snapshots the row, runs the change, snapshots it again, and records the
// difference — all on one transaction, so the change and its own record commit
// together and the two snapshots cannot straddle somebody else's write.
//
// Use it for anything that CHANGES or DELETES an existing row. A pure create
// has no before-image and gains nothing from it; those keep passing After on
// their own.
func writeAuditedRow(
	ctx context.Context,
	pool *pgxpool.Pool,
	aud *audit.Auditor,
	e audit.Entry,
	table string,
	id any,
	write func(tx pgx.Tx) error,
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	before, err := snapshotRow(ctx, tx, table, id)
	if err != nil {
		return err
	}
	if err := write(tx); err != nil {
		return err
	}
	after, err := snapshotRow(ctx, tx, table, id)
	if err != nil {
		return err
	}

	setAuditDiff(&e, before, after)
	if err := aud.LogTx(ctx, tx, e); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func appendNote(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + " · " + add
}

// latestRowSnapshot reads the currently-effective row of a versioned table
// (pay_rates, budget_caps) as a JSON object, or nil when there is none yet.
//
// These tables are append-only by design — a change is a NEW row with a later
// effective_from — so the "before" for one of them is not the same row read
// earlier, it is the row this one supersedes.
func latestRowSnapshot(ctx context.Context, pool *pgxpool.Pool, table string) (map[string]any, error) {
	if !auditSnapshotTables[table] {
		return nil, fmt.Errorf("audit snapshot: %q is not an allowed table", table)
	}
	var raw []byte
	err := pool.QueryRow(ctx,
		`SELECT to_jsonb(t) FROM `+table+` t ORDER BY t.effective_from DESC, t.created_at DESC LIMIT 1`).Scan(&raw)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	for col := range row {
		if auditRedactedColumns[col] {
			row[col] = auditRedacted
		}
	}
	return row, nil
}

// setAuditDiff puts a pair of snapshots onto an entry.
//
// Separate from writeAuditedRow because several writers already own their
// transaction — they change more than one table, or run a recompute between the
// write and the commit — and cannot hand control to a wrapper.
func setAuditDiff(e *audit.Entry, before, after map[string]any) {
	// A delete leaves no "after". Recorded as the full before-image against a
	// null after, which is what "this row used to exist and said this" looks
	// like — and the only chance to keep the contents at all.
	if after == nil {
		e.Before, e.After = before, nil
		return
	}
	if b, a := auditDiff(before, after); a != nil {
		// Each side assigned only when it actually has content. A nil map put
		// into an `any` is a non-nil interface holding a nil map — see below —
		// and a create (no before-image at all) would otherwise store the JSON
		// text `null` where the column means "there was nothing here".
		if b != nil {
			e.Before = b
		}
		e.After = a
		return
	}
	// Assigned NOT AT ALL rather than assigned the nil maps. A nil map put into
	// an `any` is a non-nil interface holding a nil map, so the audit writer
	// would see "there is a value here", marshal it, and store the JSON text
	// `null` in a column whose whole meaning is the difference between "no
	// before-image" and "the before-image was nothing".
	e.Note = appendNote(e.Note, "ไม่มีค่าใดเปลี่ยนแปลง")
}

// periodStatus reads the workflow state of one (period, TA, course) cell.
//
// submission_period_status is keyed by that triple rather than by an id, so the
// generic row snapshot cannot address it. These transitions are the paperwork's
// spine — ตรวจแล้ว → ส่งออก → ส่งการเงิน, and the reversals — and the state a
// cell was in before it moved is what tells a mistaken sign-off from a repeat
// of one that had already happened.
//
// Returns nil when the cell has never been written, which is itself the
// before-image of a first sign-off.
func periodStatus(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, periodID, taID, tcID any) (map[string]any, error) {
	var status string
	var exportedAt, financeSentAt *string
	err := q.QueryRow(ctx, `
		SELECT status::text,
		       -- TZH:TZM, never OF/TZ/TZH alone: under a whole-hour zone those
		       -- collapse to "+07" and drop the minutes, which is the bug
		       -- TestNoTruncatedTimezoneOffsetInSQL guards the whole repo against.
		       to_char(exported_at,     'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'),
		       to_char(finance_sent_at, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM')
		FROM submission_period_status
		WHERE submission_period_id = $1 AND ta_id = $2 AND teaching_course_id = $3`,
		periodID, taID, tcID).Scan(&status, &exportedAt, &financeSentAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return map[string]any{
		"status": status, "exported_at": exportedAt, "finance_sent_at": financeSentAt,
	}, nil
}
