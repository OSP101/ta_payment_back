package service

import (
	"testing"

	"github.com/google/uuid"
)

// The lecturer's "อนุมัติรายงานบันทึกเวลา TA" list is one row PER ASSIGNMENT, so a
// TA helping with two sections of one course produces two rows. When those
// sections are co-taught the rows also carry identical hour totals, because the
// generator writes the same sitting against each section.
//
// Found on live data: CP321002 showed สุพพิธาน twice, "รอพิจารณา 98 ชม." on both,
// with nothing on screen telling them apart — the honest reading was that the
// same request had arrived twice, and that 196 hours were waiting. Export rule
// B2 settles that day once, at the regular rate.
//
// These tests pin what the row has to carry for the screen to be readable at
// all: which section it is, and whether it shares its hours with another.

// pendingCoTaughtFixture gives one TA two co-scheduled sections of one course
// and submits the same two hours against both, the way the generator does.
func pendingCoTaughtFixture(t *testing.T) (*fixture, uuid.UUID) {
	t.Helper()
	f := newFixture(t, fixtureOpts{})
	sibling := f.cotaughtSiblingAssignment("regular")

	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	second := f.entry(day(10), "09:00", "11:00", 2)
	second.AssignmentID = sibling
	f.mustUpsert(second)

	// The list only shows rows awaiting review.
	f.exec(`UPDATE work_logs SET status='submitted', submitted_at=now()
	        WHERE assignment_id IN ($1, $2)`, f.AssignmentID, sibling)
	return f, sibling
}

func TestListPending_TellsCoTaughtSectionsApart(t *testing.T) {
	f, sibling := pendingCoTaughtFixture(t)

	rows, err := f.Svc.ListPending(f.ctx, f.LecturerID, false)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (one per assignment)", len(rows))
	}

	bySection := map[string]PendingReport{}
	for _, r := range rows {
		if r.SecNo == "" {
			t.Fatalf("assignment %s came back with no section — the two rows read as "+
				"the same TA twice, with nothing to choose between them", r.ID)
		}
		bySection[r.SecNo] = r
	}
	if len(bySection) != 2 {
		t.Fatalf("the two rows share section %v — they must name different sections", bySection)
	}

	// Both rows must also point at the same person, so the screen can group them.
	var taIDs = map[uuid.UUID]bool{}
	for _, r := range rows {
		taIDs[r.TAID] = true
		if r.TAID == uuid.Nil {
			t.Error("ta_id is empty — the client cannot group two sections under one " +
				"person by name alone, and two TAs can share a name")
		}
	}
	if len(taIDs) != 1 {
		t.Errorf("the two rows report %d different TAs; both belong to one", len(taIDs))
	}

	// ...and carry the marker that says their hours are the same hours.
	for _, r := range rows {
		if r.CoTaughtGroup == nil {
			t.Fatalf("sec %s has no cotaught_group — without it the screen adds 2.0 and "+
				"2.0 into 4.0 hours waiting, which is not what the payout pays", r.SecNo)
		}
	}
	if *rows[0].CoTaughtGroup != *rows[1].CoTaughtGroup {
		t.Errorf("the co-taught pair reports different groups (%d vs %d), so nothing links them",
			*rows[0].CoTaughtGroup, *rows[1].CoTaughtGroup)
	}
	_ = sibling
}

// GroupHours is what the reviewer sees before opening anything, so it has to be
// the number the payout will settle.
//
// The trap is assuming co-taught sections are copies of each other. They are
// not: on live CP321002 the pair held 98 submitted rows but only 82 distinct
// sittings — some hours shared, most not. Both easy answers are wrong there,
// adding (196) and de-duplicating wholesale (98); the truth was 164.
//
// This fixture reproduces that shape in miniature: a 2h shared sitting plus 1h
// each section worked alone. Adding the sections gives 6, taking either alone
// gives 3, and the truth is 4.
func TestListPending_GroupHoursCountsSharedSittingsOnce(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	sibling := f.cotaughtSiblingAssignment("regular")

	// The shared sitting — same day, same clock, written against both sections.
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	shared := f.entry(day(10), "09:00", "11:00", 2)
	shared.AssignmentID = sibling
	f.mustUpsert(shared)

	// ...and an hour each section worked on its own.
	f.mustUpsert(f.entry(day(11), "09:00", "10:00", 1))
	own := f.entry(day(12), "09:00", "10:00", 1)
	own.AssignmentID = sibling
	f.mustUpsert(own)

	f.exec(`UPDATE work_logs SET status='submitted', submitted_at=now()
	        WHERE assignment_id IN ($1, $2)`, f.AssignmentID, sibling)

	rows, err := f.Svc.ListPending(f.ctx, f.LecturerID, false)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r.TotalHours != 3 {
			t.Errorf("sec %s total_hours = %.1f, want 3 (its own rows)", r.SecNo, r.TotalHours)
		}
		if r.GroupHours != 4 {
			t.Errorf("sec %s group_hours = %.1f, want 4 — the 2h sitting counted once, "+
				"plus the 1h each section worked alone. 6 means the shared sitting was "+
				"counted twice; 3 means a section's own hours were thrown away along "+
				"with the duplicate", r.SecNo, r.GroupHours)
		}
	}
}

// A section with no co-taught partner has nothing to merge, so the group total
// is simply its own. Without this the LEFT JOIN could return NULL and the
// screen would show a blank or a zero where the hours belong.
func TestListPending_GroupHoursFallsBackToTheSectionsOwn(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "12:00", 3))
	f.exec(`UPDATE work_logs SET status='submitted', submitted_at=now()
	        WHERE assignment_id = $1`, f.AssignmentID)

	rows, err := f.Svc.ListPending(f.ctx, f.LecturerID, false)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].GroupHours != 3 {
		t.Errorf("group_hours = %.1f, want 3 — a lone section's group is itself",
			rows[0].GroupHours)
	}
}

// Sections that merely belong to the same request are NOT co-taught, and their
// hours DO add up. If the marker leaked onto them the screen would under-report
// what is waiting — the opposite error, and a harder one to notice.
func TestListPending_LeavesUnrelatedSectionsUngrouped(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	// group=nil — same request, different sitting.
	sibling := f.siblingAssignment("regular", nil)

	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	second := f.entry(day(11), "09:00", "11:00", 2)
	second.AssignmentID = sibling
	f.mustUpsert(second)
	f.exec(`UPDATE work_logs SET status='submitted', submitted_at=now()
	        WHERE assignment_id IN ($1, $2)`, f.AssignmentID, sibling)

	rows, err := f.Svc.ListPending(f.ctx, f.LecturerID, false)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r.CoTaughtGroup != nil {
			t.Errorf("sec %s was marked co-taught (group %d) when it is not — the screen "+
				"would fold two separate sittings into one and show half the hours",
				r.SecNo, *r.CoTaughtGroup)
		}
	}
}

// A lecturer must not see another lecturer's courses. The section fields are
// new; the scoping they travel with is not, and it has to keep holding.
func TestListPending_StaysScopedToTheLecturersOwnCourses(t *testing.T) {
	f, _ := pendingCoTaughtFixture(t)

	stranger := f.insertUser("lecturer", "outsider")
	rows, err := f.Svc.ListPending(f.ctx, stranger, false)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a lecturer who teaches none of these courses got %d rows", len(rows))
	}
}
