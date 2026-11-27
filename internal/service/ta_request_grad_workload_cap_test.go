package service

import (
	"context"
	"strings"
	"testing"
)

// validateGradWorkloadCaps is a pure function (no DB access), unlike its
// undergrad counterpart which needs a timetable — these tests don't need the
// capFixture/testutil.NewPool infra used in ta_request_schedule_cap_test.go.

func TestGradWorkload_ReviewHoursWithinCapAllowed(t *testing.T) {
	w := WorkloadInput{HelpTeachHrs: 10, GradeHrs: 2}
	if err := validateGradWorkloadCaps(w, "ผู้ช่วย ทดสอบ", "1"); err != nil {
		t.Fatalf("2h ตรวจการบ้าน must be allowed (cap is %.1f): %v", gradReviewHourCap, err)
	}
}

func TestGradWorkload_ReviewHoursOverCapRejected(t *testing.T) {
	w := WorkloadInput{HelpTeachHrs: 8, GradeHrs: 2.5}
	err := validateGradWorkloadCaps(w, "ผู้ช่วย ทดสอบ", "1")
	if err == nil {
		t.Fatal("2.5h ตรวจการบ้าน must exceed the 2h grad cap")
	}
	if !strings.Contains(err.Error(), "2.5") || !strings.Contains(err.Error(), "ตรวจการบ้าน") {
		t.Fatalf("error should name the duty and the declared value, got %q", err)
	}
}

// Preparation and other-work hours are accepted and use independent worklog budgets.
func TestGradWorkload_OtherAndPrepHoursAccepted(t *testing.T) {
	w := WorkloadInput{HelpTeachHrs: 6, GradeHrs: 1, OtherHrs: 2, PrepHrs: 1}
	if err := validateGradWorkloadCaps(w, "ผู้ช่วย ทดสอบ", "1"); err != nil {
		t.Fatalf("other_hrs/prep_hrs must still be accepted for grad TAs (separate duty budgets): %v", err)
	}
}

func TestGradWorkload_HelpTeachAloneIsUnbounded(t *testing.T) {
	// help_teach_hrs (ช่วยสอน) is untouched by this change — only the total
	// (validated elsewhere, in autoDecide's 12h rule) bounds it.
	w := WorkloadInput{HelpTeachHrs: 12, GradeHrs: 0}
	if err := validateGradWorkloadCaps(w, "ผู้ช่วย ทดสอบ", "1"); err != nil {
		t.Fatalf("help_teach_hrs alone must not be capped by validateGradWorkloadCaps: %v", err)
	}
}

// Graduate preparation and other work follow their separate declared budgets.
func TestGradAssignmentContext_DeclaredDutiesAreLoggable(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master", Workload: workloadHours{HelpTeach: 6, Grade: 2, Other: 3, Prep: 1}})
	ac, err := loadAssignmentContext(context.Background(), f.Pool, f.AssignmentID)
	if err != nil {
		t.Fatal(err)
	}
	if !ac.AllowOther || !ac.AllowPrep || ac.WeeklyCapOther != 3 || ac.WeeklyCapPrep != 1 || ac.WeeklyTotalHours != 12 {
		t.Fatalf("wrong graduate workload context: %+v", ac)
	}
}
