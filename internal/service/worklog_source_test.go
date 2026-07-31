package service

import (
	"context"
	"strings"
	"testing"
)

// Two rules that the payout-review form rests on:
//
//   - every row records whether the GENERATOR or the TA created it, because the
//     reviewer spends attention on those very differently; and
//   - a TA cannot log any time until their own class timetable is on file,
//     because the faculty form is a grid of classes AND duties, and half of it
//     would otherwise be blank.

// scheduleFixture starts WITHOUT a timetable so each test states for itself when
// the gate is satisfied.
func scheduleFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixture(t, fixtureOpts{NoOwnClassSchedule: true})
}

// addOwnClass gives the TA a class of their own in this term, satisfying the gate.
func (f *fixture) addOwnClassInTerm(day int, start, end string) {
	f.exec(`INSERT INTO ta_class_schedules
	          (id, user_id, term_id, course_code, course_name, sec_no, kind, day_of_week, start_time, end_time)
	        VALUES (gen_random_uuid(), $1, $2, 'XX101', 'วิชาของตัวเอง', '1', 'lecture', $3, $4::time, $5::time)`,
		f.TAID, f.TermID, day, start, end)
}

// THE GATE. Without a timetable of their own, a TA cannot log time at all.
func TestWorklog_RefusesUntilOwnClassScheduleIsFiled(t *testing.T) {
	f := scheduleFixture(t)

	_, err := f.upsert(f.entry(day(10), "09:00", "11:00", 2))
	if err == nil {
		t.Fatal("logging time must be refused until the TA files their own class schedule")
	}
	if !strings.Contains(err.Error(), "ตารางเรียนของคุณ") {
		t.Errorf("the refusal must say what to do, got: %v", err)
	}

	// Filing it unblocks the same write.
	f.addOwnClassInTerm(3, "08:00", "09:00")
	if _, err := f.upsert(f.entry(day(10), "09:00", "11:00", 2)); err != nil {
		t.Fatalf("logging must be allowed once the schedule exists: %v", err)
	}
}

// The gate is per TERM, not per user: last term's timetable says nothing about
// this term's clashes.
func TestWorklog_OwnScheduleFromAnotherTermDoesNotCount(t *testing.T) {
	f := scheduleFixture(t)
	ctx := context.Background()

	var otherTerm string
	if err := f.Pool.QueryRow(ctx, `
		INSERT INTO academic_terms (id, academic_year, semester, starts_on, ends_on, is_active, months)
		VALUES (gen_random_uuid(), 2560, 1, '2017-06-01', '2017-10-01', FALSE, 4)
		RETURNING id`).Scan(&otherTerm); err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO ta_class_schedules
	          (id, user_id, term_id, course_code, sec_no, kind, day_of_week, start_time, end_time)
	        VALUES (gen_random_uuid(), $1, $2::uuid, 'OLD101', '1', 'lecture', 1, '08:00', '09:00')`,
		f.TAID, otherTerm)

	if _, err := f.upsert(f.entry(day(10), "09:00", "11:00", 2)); err == nil {
		t.Fatal("a timetable from a different term must not satisfy the gate")
	}
}

// Auto-generated rows must be labelled as such — this is what lets the review
// screen say "the lecturer's timetable produced this" rather than making a staff
// member judge it.
func TestWorklog_GeneratedRowsAreLabelledAuto(t *testing.T) {
	f := scheduleFixture(t)
	f.addOwnClassInTerm(3, "08:00", "09:00")
	ctx := context.Background()

	if _, err := f.Svc.Generate(ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	var auto, manual int
	if err := f.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE source = 'auto'),
		       COUNT(*) FILTER (WHERE source = 'manual')
		  FROM work_logs WHERE assignment_id = $1`, f.AssignmentID).Scan(&auto, &manual); err != nil {
		t.Fatal(err)
	}
	if auto == 0 {
		t.Fatal("the generator produced no rows marked 'auto'")
	}
	if manual != 0 {
		t.Errorf("%d generated rows were left as 'manual' — they would be sent for review as hand-typed claims", manual)
	}
}

// ...and a row the TA types stays 'manual', including when it happens to look
// exactly like a class period. Origin is a fact about who wrote it, not about
// what it resembles.
func TestWorklog_HandTypedRowsStayManual(t *testing.T) {
	f := scheduleFixture(t)
	f.addOwnClassInTerm(3, "08:00", "09:00")
	ctx := context.Background()

	// Typed by hand, at times that could just as easily have come from the
	// timetable — the label must still say who really wrote it.
	id := f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))

	var src string
	if err := f.Pool.QueryRow(ctx, `SELECT source FROM work_logs WHERE id = $1`, id).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != "manual" {
		t.Errorf("source = %q for a hand-typed row, want manual — a row that merely resembles the timetable is still a claim", src)
	}
}

// Editing a generated row must not relabel it. The reviewer's question is "did a
// machine propose these hours", and an edit does not change the answer.
func TestWorklog_EditingAGeneratedRowKeepsItAuto(t *testing.T) {
	f := scheduleFixture(t)
	f.addOwnClassInTerm(3, "08:00", "09:00")
	ctx := context.Background()

	if _, err := f.Svc.Generate(ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var id, dateStr string
	if err := f.Pool.QueryRow(ctx, `
		SELECT id::text, TO_CHAR(work_date,'YYYY-MM-DD') FROM work_logs
		 WHERE assignment_id = $1 AND source = 'auto' ORDER BY work_date LIMIT 1`,
		f.AssignmentID).Scan(&id, &dateStr); err != nil {
		t.Fatal(err)
	}

	w := f.entry(dateStr, "09:00", "10:00", 1)
	w.ID = mustUUID(t, id)
	if _, err := f.upsert(w); err != nil {
		t.Fatalf("edit: %v", err)
	}

	var src string
	if err := f.Pool.QueryRow(ctx, `SELECT source FROM work_logs WHERE id = $1`, id).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != "auto" {
		t.Errorf("source = %q after editing a generated row, want auto", src)
	}
}
