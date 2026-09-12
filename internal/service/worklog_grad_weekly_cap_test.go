package service

import (
	"testing"
)

// The bug this pins: Generate checked daily/baht/term ceilings but never the
// WEEKLY per-activity cap (weeklyCapBuckets) that enforceWeeklyActivityCap
// (manual entry) and recheckCapsForApproval (the approval-time gate) both
// enforce. A graduate TA whose section meets for lecture AND lab in the same
// week — the ordinary case, not an edge case — has those two activities
// share ONE help_teach_hrs budget. Generate happily minted the full
// attendance-trimmed lecture row PLUS the full lab row every week
// regardless of whether their sum fit that shared budget, so a TA never
// typed a wrong number and a lecturer could not approve a single month —
// exactly CP410872's real, currently-stuck 68 hours. The default fixture
// puts lecture 09:00-12:00 and lab 13:00-16:00 on the SAME weekday
// (fixture_test.go's insertSectionSchedule), so this reproduces without any
// custom timetable.
func TestGenerate_GradWeeklySharedCapStopsLectureLabOvercommit(t *testing.T) {
	f := newFixture(t, fixtureOpts{
		Level: "master",
		// help_teach_hrs=2: lecture trims to the flat 1h attendance duty,
		// leaving only 1h of budget — not enough for the 3h lab, so the lab
		// row for that week must be skipped rather than created and later
		// rejected at approval.
		Workload: workloadHours{HelpTeach: 2},
	})
	res, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(res.SkippedWeeklyCap) == 0 {
		t.Fatal("expected SkippedWeeklyCap to report the skipped lab rows, got none")
	}
	found := false
	for _, sg := range res.SkippedWeeklyCap {
		if sg.Reason == "ช่วยสอน (บรรยาย+ปฏิบัติการ)" && sg.Count > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a ช่วยสอน (บรรยาย+ปฏิบัติการ) skip reason, got %+v", res.SkippedWeeklyCap)
	}

	// The real assertion: no week's lecture+lab total exceeds the declared
	// 2h shared cap. Grouping by work_date is enough here since the fixture
	// only ever schedules one lecture+lab pair per week (Monday).
	weekly := map[string]float64{}
	for _, r := range res.Entries {
		if r.Activity == "lecture" || r.Activity == "lab" {
			weekly[r.WorkDate] += r.Hours
		}
	}
	for date, hrs := range weekly {
		if hrs > 2.0+0.01 {
			t.Fatalf("week of %s generated %.2f ชม. of lecture+lab, exceeds the 2.0 shared cap Generate must respect", date, hrs)
		}
	}

	// End-to-end: submit and approve everything Generate produced. Before
	// the fix this would fail with the "อนุมัติไม่ได้ เกินโควตา" Conflict every
	// single week — the exact symptom reported.
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, "", false); err != nil {
		t.Fatalf("Approve must succeed on rows Generate itself created: %v", err)
	}
}
