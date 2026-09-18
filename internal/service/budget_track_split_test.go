package service

import "testing"

// UsedBahtRegular/UsedBahtSpecial feed the lecturer-facing usage bars (course
// overview card + home-page course card) that split "how much of the budget
// is spent" by which pool it came from. This pins that the split actually
// lands each TA's spend in the right pool, and that the two halves always
// add up to the same total the old single UsedBaht figure reported.
func TestBudgetCompute_SplitsUsedBahtByTrack(t *testing.T) {
	// The fixture's own assignment: a graduate TA on ภาคปกติ, billed hourly
	// against the regular pool at the fixture's own graduate rate.
	f := newFixture(t, fixtureOpts{
		Level: "master", Track: "regular",
		Rates: rateOverrides{GradRegularHourly: 50},
	})
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES (gen_random_uuid(), $1, $2::date, '09:00', '11:00', 2, 'review', 'approved')`,
		f.AssignmentID, day(10))
	// 2 ชม. × ฿50 = ฿100 → the regular pool.

	// A second, undergrad TA on ภาคพิเศษ (a new section on the same request),
	// billed hourly against the special pool.
	specialAssignment := f.siblingAssignment("special", nil)
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES (gen_random_uuid(), $1, $2::date, '13:00', '16:00', 3, 'review', 'approved')`,
		specialAssignment, day(11))
	// 3 ชม. × ฿50 (fixture default undergrad_special) = ฿150 → the special pool.

	budget := &BudgetService{pool: f.Pool}
	snap, err := budget.Compute(f.ctx, f.CourseID)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}

	if snap.UsedBahtRegular != 100 {
		t.Errorf("UsedBahtRegular = %v, want 100 (the graduate ภาคปกติ TA's spend)", snap.UsedBahtRegular)
	}
	if snap.UsedBahtSpecial != 150 {
		t.Errorf("UsedBahtSpecial = %v, want 150 (the undergrad ภาคพิเศษ TA's spend)", snap.UsedBahtSpecial)
	}
	if snap.UsedBaht != snap.UsedBahtRegular+snap.UsedBahtSpecial {
		t.Errorf("UsedBaht = %v, want it to equal UsedBahtRegular(%v) + UsedBahtSpecial(%v)",
			snap.UsedBaht, snap.UsedBahtRegular, snap.UsedBahtSpecial)
	}
}
