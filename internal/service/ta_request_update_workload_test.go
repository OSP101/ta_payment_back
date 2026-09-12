package service

import (
	"context"
	"strings"
	"testing"
)

// UpdateAssignmentWorkload is the correction path that didn't exist before
// this session: TARequestService.Cancel refuses once work_logs exist and its
// own error message says "contact staff", but staff had no tool. These tests
// pin that the new tool reuses the EXACT SAME validation Create does (so a
// correction can never smuggle in a value Create itself would refuse), and
// that it never touches work_logs already on the books.

func TestUpdateAssignmentWorkload_GradHelpTeachCorrectionTakesEffect(t *testing.T) {
	f := newFixture(t, fixtureOpts{
		Level:    "master",
		Workload: workloadHours{HelpTeach: 2, Grade: 1},
	})
	svc := &TARequestService{pool: f.Pool, aud: f.Svc.aud}

	err := svc.UpdateAssignmentWorkload(f.ctx, f.StaffID, f.AssignmentID, WorkloadInput{
		HelpTeachHrs: 3, GradeHrs: 1,
	})
	if err != nil {
		t.Fatalf("UpdateAssignmentWorkload: %v", err)
	}

	ac, err := loadAssignmentContext(f.ctx, f.Pool, f.AssignmentID)
	if err != nil {
		t.Fatalf("loadAssignmentContext: %v", err)
	}
	if ac.WeeklyCapLecture != 3 || ac.WeeklyCapLab != 3 {
		t.Fatalf("correction did not take effect: WeeklyCapLecture=%.1f WeeklyCapLab=%.1f, want 3/3",
			ac.WeeklyCapLecture, ac.WeeklyCapLab)
	}
}

func TestUpdateAssignmentWorkload_GradReviewOverCapRejected(t *testing.T) {
	f := newFixture(t, fixtureOpts{
		Level:    "master",
		Workload: workloadHours{HelpTeach: 3, Grade: 1},
	})
	svc := &TARequestService{pool: f.Pool, aud: f.Svc.aud}

	err := svc.UpdateAssignmentWorkload(f.ctx, f.StaffID, f.AssignmentID, WorkloadInput{
		HelpTeachHrs: 3, GradeHrs: 2.5,
	})
	if err == nil {
		t.Fatal("2.5h ตรวจการบ้าน must be rejected — same gradReviewHourCap Create enforces")
	}
	if !strings.Contains(err.Error(), "ตรวจการบ้าน") {
		t.Fatalf("expected the same error validateGradWorkloadCaps gives at Create time, got %q", err)
	}
}

func TestUpdateAssignmentWorkload_UndergradOverTimetableRejected(t *testing.T) {
	// newFixture's default section_schedules (fixture_test.go's
	// insertSectionSchedule) is lecture 09:00-12:00 (3h) + lab 13:00-16:00
	// (3h) — same ceiling validateUndergradSectionCaps enforces at Create.
	f := newFixture(t, fixtureOpts{
		Workload: workloadHours{CheckWork: 3},
	})
	svc := &TARequestService{pool: f.Pool, aud: f.Svc.aud}

	err := svc.UpdateAssignmentWorkload(f.ctx, f.StaffID, f.AssignmentID, WorkloadInput{
		CheckWorkHrs: 5, // exceeds the 3h the section actually meets for
	})
	if err == nil {
		t.Fatal("5h ช่วยตรวจงาน must be rejected against a 3h lecture section")
	}
}

func TestListAssignmentsForTAInCourse_ReturnsCurrentWorkload(t *testing.T) {
	f := newFixture(t, fixtureOpts{
		Level:    "phd",
		Workload: workloadHours{HelpTeach: 2, Grade: 1},
	})
	svc := &TARequestService{pool: f.Pool, aud: f.Svc.aud}

	out, err := svc.ListAssignmentsForTAInCourse(f.ctx, f.TAID, f.CourseID)
	if err != nil {
		t.Fatalf("ListAssignmentsForTAInCourse: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 assignment, got %d: %+v", len(out), out)
	}
	got := out[0]
	if got.AssignmentID != f.AssignmentID {
		t.Errorf("assignment_id = %s, want %s", got.AssignmentID, f.AssignmentID)
	}
	if got.Level != "phd" {
		t.Errorf("level = %q, want phd", got.Level)
	}
	if got.Workload.HelpTeachHrs != 2 || got.Workload.GradeHrs != 1 {
		t.Errorf("workload = %+v, want help_teach=2 grade=1", got.Workload)
	}
}

func TestUpdateAssignmentWorkload_LeavesExistingWorkLogsUntouched(t *testing.T) {
	f := newFixture(t, fixtureOpts{
		Level:    "master",
		Workload: workloadHours{HelpTeach: 3, Grade: 1},
	})
	if _, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var before float64
	if err := f.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(hours),0) FROM work_logs WHERE assignment_id=$1`,
		f.AssignmentID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("fixture setup problem: Generate produced nothing to test against")
	}

	svc := &TARequestService{pool: f.Pool, aud: f.Svc.aud}
	if err := svc.UpdateAssignmentWorkload(f.ctx, f.StaffID, f.AssignmentID, WorkloadInput{
		HelpTeachHrs: 5, GradeHrs: 1,
	}); err != nil {
		t.Fatalf("UpdateAssignmentWorkload: %v", err)
	}

	var after float64
	if err := f.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(hours),0) FROM work_logs WHERE assignment_id=$1`,
		f.AssignmentID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("correcting the declared workload must not rewrite existing work_logs — "+
			"had %.2f hours, now %.2f", before, after)
	}
}
