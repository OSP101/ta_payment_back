package service

import (
	"os"
	"path/filepath"
	"testing"
)

// Migration 0055 has a BACKFILL, and a backfill is the part of a migration that
// only ever runs once, on data nobody wrote a test for.
//
// This one shipped with the steps in the wrong order — it expanded each existing
// section-day makeup into one row per period BEFORE dropping the unique
// constraint that forbade a second row for the same (section, date). The whole
// suite passed, because the test database is built from scratch and had no rows to
// expand; the real database failed on the very first one and the backend refused
// to boot.
//
// So this test does what the suite could not: it puts a row in the old shape
// there first, then runs the migration file for real.
func TestMigration0055_ExpandsExistingMakeupsPerPeriod(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	// Put the table back in its pre-0055 shape so the file under test can run
	// against the state it was written for. Data first, then the old constraint —
	// which is the very thing the ordering bug tripped over.
	f.exec(`ALTER TABLE makeup_schedules
	          DROP CONSTRAINT uq_makeup_section_original_kind,
	          DROP CONSTRAINT makeup_schedules_kind_check,
	          DROP COLUMN kind`)
	f.exec(`ALTER TABLE makeup_schedules
	          ADD CONSTRAINT uq_makeup_section_original UNIQUE (section_id, original_date)`)

	// The fixture's section teaches a lecture AND a lab on Monday, so one
	// section-day makeup has two periods to expand into.
	holiday := f.addHolidayOnScheduledDay()
	var origID string
	if err := f.Pool.QueryRow(f.ctx, `
		INSERT INTO makeup_schedules (id, section_id, original_date, makeup_date, start_time, end_time, note)
		VALUES (gen_random_uuid(), $1, $2::date, $3::date, '09:00', '11:00', 'เดิม')
		RETURNING id`,
		f.SectionID, holiday, holiday).Scan(&origID); err != nil {
		t.Fatalf("seed old-shape makeup: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(repoRoot(t), "migrations", "0055_makeup_per_period.up.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := f.Pool.Exec(f.ctx, string(body)); err != nil {
		t.Fatalf("migration 0055 failed on a database that already had a makeup row: %v", err)
	}

	// One row per period the section teaches that day, all carrying the original
	// replacement slot — the lecturer's screen must look the same afterwards.
	rows, err := f.Pool.Query(f.ctx,
		`SELECT id::text, kind, TO_CHAR(makeup_date,'YYYY-MM-DD'), start_time::text, note
		   FROM makeup_schedules WHERE section_id = $1 ORDER BY kind`, f.SectionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	ids := map[string]string{}
	for rows.Next() {
		var id, kind, mkDate, start, note string
		if err := rows.Scan(&id, &kind, &mkDate, &start, &note); err != nil {
			t.Fatal(err)
		}
		got[kind] = mkDate + " " + start + " " + note
		ids[kind] = id
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rows (lecture + lab) after expansion, got %d: %v", len(got), got)
	}
	if got["lecture"] != got["lab"] {
		t.Errorf("the expanded rows disagree about the replacement slot: lecture=%q lab=%q — "+
			"the migration changed what the lecturer had filed", got["lecture"], got["lab"])
	}
	// The original id must survive on one of them: audit entries reference it.
	if ids["lab"] != origID && ids["lecture"] != origID {
		t.Errorf("the original row id %s is gone; audit entries pointing at it can no longer be resolved", origID)
	}

	// And the new constraint must be in place, holding the new rule.
	var conname string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT conname FROM pg_constraint WHERE conname = 'uq_makeup_section_original_kind'`,
	).Scan(&conname); err != nil {
		t.Errorf("the per-period unique constraint was not created: %v", err)
	}
}
