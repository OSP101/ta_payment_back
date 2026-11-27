package service

import "testing"

func TestGradWorklogDuties(t *testing.T) {
	for _, level := range []string{"master", "phd"} {
		t.Run(level, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{Level: level, Workload: workloadHours{HelpTeach: 2, Grade: 1, Prep: 2, Other: 3}})
			declared, err := f.Svc.declaredDutyHours(f.ctx, f.AssignmentID)
			if err != nil {
				t.Fatal(err)
			}
			if declared[DutyPrep] != 2 || declared[DutyGradOther] != 3 || declared[DutyReview] != 1 || declared[DutyOtherLecture] != 0 || declared[DutyOtherLab] != 0 {
				t.Fatalf("wrong graduate budgets: %v", declared)
			}
			// Graduate preparation and other work have separate budgets. UG kinds cannot borrow them.
			if err := f.Svc.enforceReviewCap(f.ctx, f.AssignmentID, DutyOtherLecture, nil, 1); err == nil {
				t.Fatal("graduate must not use UG other duty")
			}
			// The shared fixture seeds all-day review windows for unrelated tests.
			f.exec(`DELETE FROM ta_review_schedules WHERE assignment_id=$1`, f.AssignmentID)
			for _, in := range []TAReviewScheduleInput{
				{Kind: DutyPrep, DayOfWeek: 3, StartTime: "17:00", EndTime: "18:00"},
				{Kind: DutyGradOther, DayOfWeek: 4, StartTime: "17:00", EndTime: "18:00"},
			} {
				if _, err := f.Svc.AddTAReviewSchedule(f.ctx, f.TAID, f.AssignmentID, in); err != nil {
					t.Fatalf("save %s schedule: %v", in.Kind, err)
				}
			}
			if _, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID); err != nil {
				t.Fatal(err)
			}
			assignments, err := (&TeachingService{pool: f.Pool}).ListAssignmentsForTA(f.ctx, f.TAID, &f.CourseID)
			if err != nil {
				t.Fatal(err)
			}
			if len(assignments) != 1 || assignments[0].WeeklyCapPrep != 2 || assignments[0].WeeklyCapOther != 3 || assignments[0].HoursByActivity["prep"] <= 0 {
				t.Fatalf("wrong assignment response: %+v", assignments)
			}
			lecture := "lecture"
			if f.countBy("prep", nil) == 0 || f.countBy("other", &lecture) == 0 {
				t.Fatal("graduate preparation/other slots must both generate")
			}
			f.exec(`DELETE FROM work_logs WHERE assignment_id=$1`, f.AssignmentID)
			_, tue := sameWeekDays()
			if _, err := f.upsert(WorkLog{AssignmentID: f.AssignmentID, WorkDate: tue, StartTime: "17:00", EndTime: "19:00", Hours: 2, Activity: "prep"}); err != nil {
				t.Fatalf("manual graduate prep: %v", err)
			}
			if _, err := f.upsert(WorkLog{AssignmentID: f.AssignmentID, WorkDate: tue, StartTime: "19:00", EndTime: "20:00", Hours: 1, Activity: "other", ParentKind: &lecture}); err != nil {
				t.Fatalf("manual graduate other work: %v", err)
			}
			// A second manual row must not borrow the independent other-work budget.
			if _, err := f.upsert(WorkLog{AssignmentID: f.AssignmentID, WorkDate: tue, StartTime: "20:00", EndTime: "21:00", Hours: 1, Activity: "prep"}); err == nil {
				t.Fatal("prep must enforce its independent weekly cap")
			}
		})
	}
}

func TestUndergradCannotUseGraduateDuties(t *testing.T) {
	f := newFixture(t, fixtureOpts{Workload: workloadHours{UGOther: 3}})
	for _, kind := range []string{DutyPrep, DutyGradOther} {
		if err := f.Svc.enforceReviewCap(f.ctx, f.AssignmentID, kind, nil, 1); err == nil {
			t.Fatalf("undergrad must not nominate %s", kind)
		}
	}
	_, tue := sameWeekDays()
	if _, err := f.upsert(WorkLog{AssignmentID: f.AssignmentID, WorkDate: tue, StartTime: "17:00", EndTime: "18:00", Hours: 1, Activity: "prep"}); err == nil {
		t.Fatal("undergrad must not log graduate prep")
	}
}

func TestGraduateDutySlotValidation(t *testing.T) {
	for _, kind := range []string{DutyPrep, DutyGradOther} {
		if err := validateReviewInput(TAReviewScheduleInput{Kind: kind, DayOfWeek: 2, StartTime: "09:00", EndTime: "11:00"}); err != nil {
			t.Fatal(err)
		}
		if err := validateReviewInput(TAReviewScheduleInput{Kind: kind, DayOfWeek: 2, StartTime: "09:00", EndTime: "17:00"}); err == nil {
			t.Fatalf("%s must be split into rows of at most 7h", kind)
		}
	}
}
