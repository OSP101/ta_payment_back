package service

import "testing"

// The TA landing card is one row per COURSE, with the hours and money split by
// track. It used to be one row per (course, track): a TA on sec 1 ภาคปกติ and
// sec 3 ภาคพิเศษ of SC362005 came back twice, the screen's lookup by course id
// kept whichever row arrived last, and the other track's hours vanished.
//
// The split follows the money. The lecture the two sections share is regular
// work (rule B2 bills it once, on the regular side); only the hour worked for
// the special section alone is special — and priced at the special rate.
func TestTaOverview_OneRowPerCourseSplitByTrack(t *testing.T) {
	f := newFixture(t, fixtureOpts{Rates: rateOverrides{UndergradRegular: 40, UndergradSpecial: 50}})
	special := f.cotaughtSiblingAssignment("special")

	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	shared := f.entry(day(10), "09:00", "11:00", 2)
	shared.AssignmentID = special
	f.mustUpsert(shared)
	own := f.entry(day(12), "13:00", "14:00", 1)
	own.AssignmentID = special
	f.mustUpsert(own)
	// Still with the TA: one more regular hour, unsubmitted.
	f.mustUpsert(f.entry(day(11), "09:00", "10:00", 1))

	f.exec(`UPDATE work_logs SET status='approved'
	        WHERE assignment_id IN ($1, $2) AND work_date <> $3`, f.AssignmentID, special, day(11))

	dash := &DashboardService{pool: f.Pool}
	rows, err := dash.TaOverview(f.ctx, f.TAID, nil)
	if err != nil {
		t.Fatalf("TaOverview: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 — one per course, not one per track", len(rows))
	}
	r := rows[0]
	if r.HoursApproved != 3 || r.HoursApprovedRegular != 2 || r.HoursApprovedSpecial != 1 {
		t.Errorf("approved hours = %.1f (%.1f regular / %.1f special), want 3 (2 / 1)",
			r.HoursApproved, r.HoursApprovedRegular, r.HoursApprovedSpecial)
	}
	if r.HoursPending != 1 || r.HoursPendingRegular != 1 || r.HoursPendingSpecial != 0 {
		t.Errorf("pending hours = %.1f (%.1f / %.1f), want 1 (1 / 0)",
			r.HoursPending, r.HoursPendingRegular, r.HoursPendingSpecial)
	}
	if r.EstimatedBahtRegular != 80 || r.EstimatedBahtSpecial != 50 || r.EstimatedBaht != 130 {
		t.Errorf("baht = %.0f (%.0f regular / %.0f special), want 130 (80 / 50)",
			r.EstimatedBaht, r.EstimatedBahtRegular, r.EstimatedBahtSpecial)
	}
	if r.Stage != "approved" {
		t.Errorf("stage = %q, want approved (a draft row does not make the course 'submitted')", r.Stage)
	}
}

// The lecturer's course card counts the same way: sittings, split by billed
// track. Row-level sums doubled every co-taught คาบ and hid the split.
func TestLecturerOverview_HoursAreSittingsSplitByTrack(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	special := f.cotaughtSiblingAssignment("special")

	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	shared := f.entry(day(10), "09:00", "11:00", 2)
	shared.AssignmentID = special
	f.mustUpsert(shared)
	own := f.entry(day(12), "13:00", "14:00", 1)
	own.AssignmentID = special
	f.mustUpsert(own)
	f.exec(`UPDATE work_logs SET status='submitted', submitted_at=now()
	        WHERE assignment_id IN ($1, $2)`, f.AssignmentID, special)

	dash := &DashboardService{pool: f.Pool}
	rows, err := dash.LecturerOverview(f.ctx, f.LecturerID, nil, &BudgetService{pool: f.Pool})
	if err != nil {
		t.Fatalf("LecturerOverview: %v", err)
	}
	var found *LecturerCourseStatus
	for i := range rows {
		if rows[i].TeachingCourseID == f.CourseID {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatal("course missing from the lecturer's overview")
	}
	if found.TACount != 1 || found.TAsPending != 1 {
		t.Errorf("ta_count/pending = %d/%d, want 1/1", found.TACount, found.TAsPending)
	}
	if found.HoursPending != 3 || found.HoursPendingRegular != 2 || found.HoursPendingSpecial != 1 {
		t.Errorf("pending hours = %.1f (%.1f / %.1f), want 3 (2 / 1) — 5 means the shared "+
			"sitting was counted per section", found.HoursPending, found.HoursPendingRegular, found.HoursPendingSpecial)
	}
}
