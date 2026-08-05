package service

import (
	"testing"
)

// The point of a duty schedule is that "สร้างอัตโนมัติ" turns it into real work
// log rows. Before this, only grading had a slot table, and its loop sat inside
// the AllowReview gate — so a TA whose lecturer asked only for "อื่น ๆ" had
// nowhere to nominate the time and would have got nothing back either way.
//
// These tests drive Generate itself, which is what the button calls.

// addDutySlot nominates a weekly slot of the given kind, at an hour that does
// not touch the fixture section's own periods (09:00–12:00 lecture, 13:00–16:00
// lab) or the TA's own class.
func (f *fixture) addDutySlot(kind string, day int, start, end string) {
	f.t.Helper()
	f.exec(`INSERT INTO ta_review_schedules (id, assignment_id, kind, day_of_week, start_time, end_time)
	        VALUES (gen_random_uuid(), $1, $2, $3, $4::time, $5::time)`,
		f.AssignmentID, kind, day, start, end)
}

func (f *fixture) countBy(activity string, parentKind *string) int {
	f.t.Helper()
	var n int
	q := `SELECT COUNT(*) FROM work_logs WHERE assignment_id=$1 AND activity=$2 AND parent_kind IS NOT DISTINCT FROM $3`
	if err := f.Pool.QueryRow(f.ctx, q, f.AssignmentID, activity, parentKind).Scan(&n); err != nil {
		f.t.Fatalf("count %s: %v", activity, err)
	}
	return n
}

// Grading slots expand to activity='review', as they always did.
func TestGenerate_ReviewSlotsBecomeReviewRows(t *testing.T) {
	f := newFixture(t, fixtureOpts{Workload: workloadHours{CheckWork: 2}})
	f.addDutySlot(DutyReview, 3, "17:00", "18:00") // Wednesday evening

	if _, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if n := f.countBy("review", nil); n == 0 {
		t.Fatal("a nominated grading slot must expand into review rows across the term")
	}
}

// The new half: an "อื่น ๆ" slot expands to activity='other' carrying the
// parent_kind that says which session it hangs off. parent_kind is what the
// claim book and the daily-cap rules use to place the hours.
func TestGenerate_OtherSlotsCarryTheirParentKind(t *testing.T) {
	lecture, lab := "lecture", "lab"

	t.Run("lecture side", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{Workload: workloadHours{UGOther: 2}})
		f.addDutySlot(DutyOtherLecture, 3, "17:00", "18:00")

		if _, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID); err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if n := f.countBy("other", &lecture); n == 0 {
			t.Fatal("an other_lecture slot must expand into other rows with parent_kind=lecture")
		}
		if n := f.countBy("other", &lab); n != 0 {
			t.Fatalf("%d rows landed on the wrong parent kind", n)
		}
	})

	t.Run("lab side", func(t *testing.T) {
		// lab_other_hrs is what authorises this one — the column added with the
		// either-or lab choice.
		f := newFixture(t, fixtureOpts{Workload: workloadHours{UGOther: 0}})
		f.exec(`UPDATE ta_workload_forms SET lab_other_hrs = 2 WHERE assignment_id = $1`, f.AssignmentID)
		f.addDutySlot(DutyOtherLab, 3, "17:00", "18:00")

		if _, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID); err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if n := f.countBy("other", &lab); n == 0 {
			t.Fatal("an other_lab slot must expand into other rows with parent_kind=lab")
		}
	})
}

// The workload gate still applies per kind: a slot whose duty the lecturer did
// not declare generates nothing, even though the row exists. (Add refuses such
// a slot, but one can survive a later edit to the workload form.)
func TestGenerate_SkipsSlotsWhoseDutyWasNotDeclared(t *testing.T) {
	f := newFixture(t, fixtureOpts{Workload: workloadHours{CheckWork: 2}}) // no UGOther
	f.addDutySlot(DutyOtherLecture, 3, "17:00", "18:00")

	if _, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	lecture := "lecture"
	if n := f.countBy("other", &lecture); n != 0 {
		t.Fatalf("%d other rows generated for a duty the lecturer never declared", n)
	}
}

// Grading being undeclared must not stop other-work from generating. The two
// used to share one gate, which is why declaring only "อื่น ๆ" produced nothing.
func TestGenerate_OtherWorkDoesNotDependOnGradingBeingDeclared(t *testing.T) {
	f := newFixture(t, fixtureOpts{Workload: workloadHours{UGOther: 2}}) // CheckWork = 0
	f.addDutySlot(DutyOtherLecture, 3, "17:00", "18:00")

	if _, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	lecture := "lecture"
	if n := f.countBy("other", &lecture); n == 0 {
		t.Fatal("other-work must generate on its own — it does not need grading hours to exist")
	}
}
