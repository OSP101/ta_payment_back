package service

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/audit"
)

// writeAudited is the guarantee behind 45 call sites: the change and the audit
// row recording it land together or neither does. One test here covers all of
// them, and it is the only thing standing between that promise and a quiet
// return to "write first, audit afterwards and hope".
func TestWriteAudited_RollsBackTheWriteWhenTheAuditFails(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	// Force this one audit write to fail. The fixture owns a throwaway
	// database, so the constraint cannot reach another test.
	f.exec(`ALTER TABLE audit_logs
	        ADD CONSTRAINT no_probe_audit CHECK (action <> 'test.probe')`)

	err := writeAudited(f.ctx, f.Pool, audit.New(f.Pool),
		audit.Entry{Action: "test.probe", Entity: "teaching_course", EntityID: f.CourseID.String()},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(f.ctx,
				`UPDATE teaching_courses SET name_th='CHANGED-BY-TEST' WHERE id=$1`, f.CourseID)
			return err
		})
	if err == nil {
		t.Fatal("writeAudited reported success while the audit row could not be written")
	}

	var name string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT name_th FROM teaching_courses WHERE id=$1`, f.CourseID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name == "CHANGED-BY-TEST" {
		t.Error("the change survived an audit that failed — the record and the data now " +
			"disagree, which is exactly what writing the audit after the commit does")
	}
}

// The happy path: both land, and the caller sees no error.
func TestWriteAudited_CommitsBothTogether(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	if err := writeAudited(f.ctx, f.Pool, audit.New(f.Pool),
		audit.Entry{Action: "test.ok", Entity: "teaching_course", EntityID: f.CourseID.String()},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(f.ctx,
				`UPDATE teaching_courses SET name_th='RENAMED' WHERE id=$1`, f.CourseID)
			return err
		}); err != nil {
		t.Fatalf("writeAudited: %v", err)
	}

	var name string
	var audits int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT name_th FROM teaching_courses WHERE id=$1`, f.CourseID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM audit_logs WHERE action='test.ok'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if name != "RENAMED" || audits != 1 {
		t.Errorf("name = %q, audit rows = %d; want RENAMED and 1", name, audits)
	}
}

// A refused write must not leave an audit row claiming it happened.
func TestWriteAudited_RecordsNothingWhenTheWriteIsRefused(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	if err := writeAudited(f.ctx, f.Pool, audit.New(f.Pool),
		audit.Entry{Action: "test.refused", Entity: "teaching_course"},
		func(pgx.Tx) error { return Invalid("nope") }); err == nil {
		t.Fatal("writeAudited hid the write's own refusal")
	}

	var n int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM audit_logs WHERE action='test.refused'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d audit row(s) written for a change that was refused", n)
	}
}
