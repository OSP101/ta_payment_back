package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/audit"
)

// The point of the whole phase: the trail says what a value WAS, not only what
// it became. "someone set these hours to 54" is not a finding; "someone changed
// them from 40 to 54" is.
func TestWriteAuditedRow_RecordsWhatTheValueWas(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	var logID string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT id FROM work_logs WHERE assignment_id=$1 LIMIT 1`, f.AssignmentID).Scan(&logID); err != nil {
		t.Fatal(err)
	}

	err := writeAuditedRow(f.ctx, f.Pool, audit.New(f.Pool),
		audit.Entry{Action: "test.hours", Entity: "work_log", EntityID: logID},
		"work_logs", logID,
		func(tx pgx.Tx) error {
			_, err := tx.Exec(f.ctx, `UPDATE work_logs SET hours = 5 WHERE id = $1`, logID)
			return err
		})
	if err != nil {
		t.Fatalf("writeAuditedRow: %v", err)
	}

	before, after := readDiff(t, f, "test.hours")
	if before["hours"] == nil {
		t.Fatal("no before-image recorded — this is the whole defect")
	}
	if got := jsonNum(before["hours"]); got != 2 {
		t.Errorf("before.hours = %v, want 2", before["hours"])
	}
	if got := jsonNum(after["hours"]); got != 5 {
		t.Errorf("after.hours = %v, want 5", after["hours"])
	}
}

// Only what moved. A whole-row pair on a wide table buries the one field that
// changed under thirty that did not, and the screen shows two walls of JSON.
func TestWriteAuditedRow_RecordsOnlyWhatChanged(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	var logID string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT id FROM work_logs WHERE assignment_id=$1 LIMIT 1`, f.AssignmentID).Scan(&logID); err != nil {
		t.Fatal(err)
	}
	if err := writeAuditedRow(f.ctx, f.Pool, audit.New(f.Pool),
		audit.Entry{Action: "test.narrow", Entity: "work_log", EntityID: logID},
		"work_logs", logID,
		func(tx pgx.Tx) error {
			_, err := tx.Exec(f.ctx, `UPDATE work_logs SET hours = 7 WHERE id = $1`, logID)
			return err
		}); err != nil {
		t.Fatal(err)
	}
	_, after := readDiff(t, f, "test.narrow")
	if len(after) != 1 {
		t.Errorf("after holds %d fields (%v), want just the one that changed", len(after), keysOf(after))
	}
	if _, ok := after["hours"]; !ok {
		t.Error("the field that actually changed is missing from the diff")
	}
}

// A change that changes nothing is recorded as exactly that, rather than as a
// change with an empty diff that reads like data was lost.
func TestWriteAuditedRow_SaysWhenNothingChanged(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	var logID string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT id FROM work_logs WHERE assignment_id=$1 LIMIT 1`, f.AssignmentID).Scan(&logID); err != nil {
		t.Fatal(err)
	}
	if err := writeAuditedRow(f.ctx, f.Pool, audit.New(f.Pool),
		audit.Entry{Action: "test.noop", Entity: "work_log", EntityID: logID},
		"work_logs", logID,
		func(tx pgx.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var note *string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT note FROM audit_logs WHERE action='test.noop'`).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if note == nil || !strings.Contains(*note, "ไม่มีค่าใดเปลี่ยนแปลง") {
		t.Errorf("note = %v, want it to say nothing changed", note)
	}
}

// A delete keeps the WHOLE row. It is the last chance to record what was in it,
// and a diff against nothing would record nothing.
func TestWriteAuditedRow_KeepsTheWholeRowOnDelete(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	var logID string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT id FROM work_logs WHERE assignment_id=$1 LIMIT 1`, f.AssignmentID).Scan(&logID); err != nil {
		t.Fatal(err)
	}
	if err := writeAuditedRow(f.ctx, f.Pool, audit.New(f.Pool),
		audit.Entry{Action: "test.gone", Entity: "work_log", EntityID: logID},
		"work_logs", logID,
		func(tx pgx.Tx) error {
			_, err := tx.Exec(f.ctx, `DELETE FROM work_logs WHERE id = $1`, logID)
			return err
		}); err != nil {
		t.Fatal(err)
	}
	before, after := readDiff(t, f, "test.gone")
	if after != nil {
		t.Errorf("after = %v on a delete, want null", after)
	}
	for _, col := range []string{"hours", "work_date", "activity", "status"} {
		if _, ok := before[col]; !ok {
			t.Errorf("before-image of a deleted row is missing %q — the contents are gone for good", col)
		}
	}
}

// A snapshot is a whole row, so it would otherwise copy secrets into a table
// kept for years and readable by every admin. Redacted, not dropped: the trail
// must still show that the column changed.
func TestSnapshotRow_RedactsSecrets(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	tx, err := f.Pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)

	row, err := snapshotRow(f.ctx, tx, "users", f.TAID)
	if err != nil {
		t.Fatalf("snapshotRow: %v", err)
	}
	got, ok := row["password_hash"]
	if !ok {
		t.Fatal("password_hash missing entirely — a changed credential must still show as changed")
	}
	if got != auditRedacted {
		t.Errorf("password_hash = %v, want %q — the audit trail must not hold a second copy of it",
			got, auditRedacted)
	}
	if row["email"] == nil {
		t.Error("ordinary columns must survive the redaction pass")
	}
}

// The table name is interpolated into SQL and can never be a bind parameter, so
// an unlisted name must be refused rather than run.
func TestSnapshotRow_RefusesATableOutsideTheAllowlist(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	tx, err := f.Pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)

	if _, err := snapshotRow(f.ctx, tx, "users WHERE 1=1; DROP TABLE users; --", f.TAID); err == nil {
		t.Fatal("an arbitrary table name was accepted into an interpolated query")
	}
}

/* ---------------------------- helpers ---------------------------------- */

func readDiff(t *testing.T, f *fixture, action string) (before, after map[string]any) {
	t.Helper()
	var b, a []byte
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT before, after FROM audit_logs WHERE action=$1`, action).Scan(&b, &a); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if b != nil {
		_ = json.Unmarshal(b, &before)
	}
	if a != nil {
		_ = json.Unmarshal(a, &after)
	}
	return before, after
}

func jsonNum(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case string:
		var f float64
		_, _ = fmt.Sscanf(n, "%f", &f)
		return f
	}
	return -1
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The privilege-escalation question the trail most needs to answer: an account
// gaining "admin", side by side with what it held before. `in` (the request)
// was recorded as the After and carries only the NEW roles, so this was
// unanswerable — and roles live in their own table the row snapshot cannot see.
func TestUserUpdate_RecordsTheRolesTheAccountHeldBefore(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	admin := f.insertUser("admin", "roleadmin")
	f.exec(`INSERT INTO user_roles (user_id, role) VALUES ($1,'admin'::role_code)
	        ON CONFLICT DO NOTHING`, admin)

	target := f.insertUser("ta", "rolemover")
	roles := []string{"ta", "admin"}
	if _, err := f.users().Update(f.ctx, admin, target, UpdateUserInput{Roles: &roles}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	before, after := readDiff(t, f, "user.update")
	if before == nil {
		t.Fatal("no before-image on a role change")
	}
	bRoles := toStrings(before["roles"])
	aRoles := toStrings(after["roles"])
	if len(bRoles) != 1 || bRoles[0] != "ta" {
		t.Errorf("before.roles = %v, want [ta] — what the account held before the grant", bRoles)
	}
	if len(aRoles) != 2 {
		t.Errorf("after.roles = %v, want both roles", aRoles)
	}
	var sawAdmin bool
	for _, r := range aRoles {
		if r == "admin" {
			sawAdmin = true
		}
	}
	if !sawAdmin {
		t.Error("the granted admin role is missing from the after-image")
	}
}

// Hours are what the money is computed from, so an edit has to be reversible on
// paper: the trail must name the figure that was replaced.
func TestWorkLogUpdate_RecordsTheHoursItReplaced(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	var logID uuid.UUID
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT id FROM work_logs WHERE assignment_id=$1 LIMIT 1`, f.AssignmentID).Scan(&logID); err != nil {
		t.Fatal(err)
	}
	w := f.entry(day(10), "09:00", "13:00", 4)
	w.ID = logID
	if _, err := f.Svc.Upsert(f.ctx, f.TAID, w); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	before, after := readDiff(t, f, "worklog.update")
	if before == nil {
		t.Fatal("no before-image on a work-log edit — the replaced hours are gone")
	}
	if jsonNum(before["hours"]) != 2 || jsonNum(after["hours"]) != 4 {
		t.Errorf("hours %v → %v, want 2 → 4", before["hours"], after["hours"])
	}
}

func toStrings(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
