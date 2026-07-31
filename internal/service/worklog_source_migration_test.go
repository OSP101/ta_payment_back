package service

import (
	"os"
	"path/filepath"
	"testing"
)

// Migration 0057 backfills `source` for rows written before the column existed,
// and a backfill only ever runs once — on data no test was written for.
//
// The first version of it matched generated rows against section_schedules only.
// On the real database that labelled 42 of 98 rows 'manual': every grading row
// (generated from ta_review_schedules, a different table) and every row shifted
// onto a makeup date (different weekday). Since 'manual' is what the review
// screen asks a human to look at, the feature would have arrived pointing at
// everything.
//
// This test builds one row of each generated shape and asserts the backfill
// recognises all of them.
func TestMigration0057_BackfillsEveryGeneratedShape(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	ctx := f.ctx

	// Put the table back in its pre-0057 shape.
	f.exec(`ALTER TABLE work_logs DROP COLUMN source`)

	// The fixture's section teaches Monday (day_of_week 1) 09:00–12:00 lecture and
	// 13:00–16:00 lab. Build a row of every shape the generator can produce.
	monday := nextWeekday(1)
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, note, status)
	        VALUES (gen_random_uuid(), $1, $2::date, '09:00', '12:00', 3, 'lecture', 'เช็คชื่อ', 'draft')`,
		f.AssignmentID, monday)

	// A grading row, generated from the TA's own review timetable.
	f.exec(`INSERT INTO ta_review_schedules (id, assignment_id, day_of_week, start_time, end_time)
	        VALUES (gen_random_uuid(), $1, 6, '10:00', '12:00')`, f.AssignmentID)
	saturday := nextWeekday(6)
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, note, status)
	        VALUES (gen_random_uuid(), $1, $2::date, '10:00', '12:00', 2, 'review', 'ตรวจงาน', 'draft')`,
		f.AssignmentID, saturday)

	// A class shifted onto a makeup date — lands on a weekday the section does
	// not normally teach, so the weekly-slot match alone cannot see it.
	wednesday := nextWeekday(3)
	f.exec(`INSERT INTO makeup_schedules (id, section_id, original_date, makeup_date, kind)
	        VALUES (gen_random_uuid(), $1, $2::date, $3::date, 'lecture')`,
		f.SectionID, monday, wednesday)
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, note, status)
	        VALUES (gen_random_uuid(), $1, $2::date, '09:00', '12:00', 3, 'lecture', 'เช็คชื่อ(ชดเชย)', 'draft')`,
		f.AssignmentID, wednesday)

	// A row the TA really did type: same shape as a class period, but the note is
	// their own words. It must stay manual.
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, note, status)
	        VALUES (gen_random_uuid(), $1, $2::date, '09:00', '12:00', 3, 'lecture', 'ช่วยคุมสอบย่อย', 'draft')`,
		f.AssignmentID, nextWeekday(1))

	body, err := os.ReadFile(filepath.Join(repoRoot(t), "migrations", "0057_worklog_source.up.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	// The index is created by the migration; drop it so the file can re-run.
	f.exec(`DROP INDEX IF EXISTS work_logs_assignment_source_idx`)
	if _, err := f.Pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("migration 0057 failed: %v", err)
	}

	got := map[string]string{}
	rows, err := f.Pool.Query(ctx,
		`SELECT COALESCE(note,''), source FROM work_logs WHERE assignment_id = $1`, f.AssignmentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var note, src string
		if err := rows.Scan(&note, &src); err != nil {
			t.Fatal(err)
		}
		got[note] = src
	}

	for _, note := range []string{"เช็คชื่อ", "ตรวจงาน", "เช็คชื่อ(ชดเชย)"} {
		if got[note] != "auto" {
			t.Errorf("row %q was labelled %q, want auto — the reviewer would be asked to "+
				"examine a row the generator wrote", note, got[note])
		}
	}
	if got["ช่วยคุมสอบย่อย"] != "manual" {
		t.Errorf("a hand-written row was labelled %q, want manual — it would be hidden from review",
			got["ช่วยคุมสอบย่อย"])
	}
}
