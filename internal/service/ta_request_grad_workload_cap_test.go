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

// other_hrs/prep_hrs are still accepted (and uncapped here) — the office
// wants lecturers able to reach the 10-12h/week regulation total without
// being forced to inflate ช่วยสอน. What must NOT happen is these hours
// becoming loggable/payable — that's enforced separately, in
// loadAssignmentContext (authz.go): see TestGradAssignmentContext_OtherIsNeverLoggable.
func TestGradWorkload_OtherAndPrepHoursAccepted(t *testing.T) {
	w := WorkloadInput{HelpTeachHrs: 6, GradeHrs: 1, OtherHrs: 2, PrepHrs: 1}
	if err := validateGradWorkloadCaps(w, "ผู้ช่วย ทดสอบ", "1"); err != nil {
		t.Fatalf("other_hrs/prep_hrs must still be accepted for grad TAs (paperwork-only): %v", err)
	}
}

func TestGradWorkload_HelpTeachAloneIsUnbounded(t *testing.T) {
	// help_teach_hrs (ช่วยสอน) is untouched by this change — only the total
	// (validated elsewhere, in autoDecide's 10-12h rule) bounds it.
	w := WorkloadInput{HelpTeachHrs: 12, GradeHrs: 0}
	if err := validateGradWorkloadCaps(w, "ผู้ช่วย ทดสอบ", "1"); err != nil {
		t.Fatalf("help_teach_hrs alone must not be capped by validateGradWorkloadCaps: %v", err)
	}
}

// TestGradAssignmentContext_OtherIsNeverLoggable is the DB-backed companion
// to the pure-function tests above: even when a grad assignment's workload
// form declares other_hrs/prep_hrs > 0 (now allowed again, for the 10-12h
// regulation total), loadAssignmentContext must still report AllowOther =
// false and WeeklyCapOther = 0 — those fields are administrative only and
// must never open a loggable/payable "other" activity for a grad TA.
func TestGradAssignmentContext_OtherIsNeverLoggable(t *testing.T) {
	f := newFixture(t, fixtureOpts{
		Level: "master",
		Workload: workloadHours{
			HelpTeach: 6, Grade: 2, Other: 3, Prep: 2,
		},
	})
	ac, err := loadAssignmentContext(context.Background(), f.Pool, f.AssignmentID)
	if err != nil {
		t.Fatalf("loadAssignmentContext: %v", err)
	}
	if ac.AllowOther {
		t.Fatal("AllowOther must stay false for grad even when other_hrs/prep_hrs > 0")
	}
	if ac.WeeklyCapOther != 0 {
		t.Fatalf("WeeklyCapOther must stay 0 for grad, got %.2f", ac.WeeklyCapOther)
	}
	// WeeklyTotalHours (the term-ceiling input) must also exclude other/prep
	// — help(6) + grade(2), not +other(3)+prep(2).
	if ac.WeeklyTotalHours != 8 {
		t.Fatalf("WeeklyTotalHours must exclude non-loggable other/prep, want 8 got %.2f", ac.WeeklyTotalHours)
	}
}
