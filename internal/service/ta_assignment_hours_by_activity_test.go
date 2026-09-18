package service

import "testing"

// HoursByActivity feeds the worklog page's segmented progress bar (บรรยาย/
// ปฏิบัติการ/ตรวจงาน/... in different colours instead of one solid fill) —
// this pins that it matches HoursLogged's own status filter exactly (rejected
// rows excluded, everything else counted) so the segments always add up to
// the bar's own total instead of drifting apart from it.
func TestListAssignmentsForTA_HoursByActivity(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	teaching := &TeachingService{pool: f.Pool}

	insertRow := func(activity string, hours float64, status string) {
		f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status, source)
		        VALUES (gen_random_uuid(), $1, $2::date, '09:00', '10:00', $3, $4, $5, 'manual')`,
			f.AssignmentID, day(10), hours, activity, status)
	}
	insertRow("lecture", 2, "draft")
	insertRow("lab", 3, "approved")
	insertRow("review", 1, "submitted")
	insertRow("makeup", 1.5, "draft")
	insertRow("other", 0.5, "draft")
	// Rejected rows are excluded from HoursLogged — the breakdown must agree,
	// or a TA's bar and its segments would tell two different totals.
	insertRow("lecture", 50, "rejected")

	list, err := teaching.ListAssignmentsForTA(f.ctx, f.TAID, &f.CourseID)
	if err != nil {
		t.Fatalf("ListAssignmentsForTA: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 assignment, got %d", len(list))
	}
	got := list[0].HoursByActivity
	want := map[string]float64{"lecture": 2, "lab": 3, "review": 1, "makeup": 1.5, "other": 0.5}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("hours_by_activity[%q] = %v, want %v (full map: %+v)", k, got[k], v, got)
		}
	}
	sum := 0.0
	for _, v := range got {
		sum += v
	}
	if sum != list[0].HoursLogged {
		t.Errorf("sum of hours_by_activity = %v, want it to equal hours_logged = %v", sum, list[0].HoursLogged)
	}
}
