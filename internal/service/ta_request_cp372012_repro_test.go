package service

import (
	"github.com/google/uuid"
	"testing"
)

// Reproduces CP373003 -> CP372012 using the timetable read on 2026-11-27.
// Only the overlapping lecture is excluded; labs and off-slot duties survive.
func TestCP372012_PartialCrossCourseClash(t *testing.T) {
	for _, tc := range []struct {
		name, start, end, want string
		work                   WorkloadInput
	}{
		{"sec1_lab_only", "09:00", "12:00", "approved", WorkloadInput{LabHrs: 3}},
		{"sec2_lab_only", "13:00", "16:00", "approved", WorkloadInput{LabHrs: 3}},
		{"sec3_lab_only", "13:00", "16:00", "approved", WorkloadInput{LabHrs: 3}},
		{"lecture_only", "09:00", "12:00", "rejected", WorkloadInput{AttendanceHrs: 1}},
		{"grading_only", "09:00", "12:00", "approved", WorkloadInput{CheckWorkHrs: 2}},
		{"original_workload", "09:00", "12:00", "approved", WorkloadInput{CheckWorkHrs: 2, AttendanceHrs: 1, UGOtherHrs: 2, UGOtherDesc: "เตรียมแลบ", LabHrs: 3}},
		{"nonoverlapping_control", "09:00", "12:00", "approved", WorkloadInput{LabHrs: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRequestFixture(t, fixtureOpts{})
			f.exec(`UPDATE teaching_courses SET code='CP372012' WHERE id=$1`, f.CourseID)
			f.exec(`INSERT INTO ta_class_schedules(id,user_id,term_id,day_of_week,start_time,end_time,is_wba)
			 VALUES(gen_random_uuid(),$1,$2,0,'00:00','00:00',TRUE)`, f.TAID, f.TermID)
			f.exec(`DELETE FROM section_schedules WHERE section_id=$1`, f.SectionID)
			lectureEnd := "18:00"
			if tc.name == "nonoverlapping_control" {
				lectureEnd = "17:00"
			}
			f.exec(`INSERT INTO section_schedules(section_id,kind,day_of_week,start_time,end_time)
			 VALUES($1,'lab',2,$2::time,$3::time),($1,'lecture',2,'16:00',$4::time)`, f.SectionID, tc.start, tc.end, lectureEnd)
			other := f.secondCourseAssignment(fixtureOpts{Workload: workloadHours{CheckWork: 2, UGOther: 2, Lab: 3}})
			f.exec(`UPDATE teaching_courses SET code='CP373003' WHERE id=(SELECT r.teaching_course_id FROM ta_requests r JOIN ta_request_assignments a ON a.request_id=r.id WHERE a.id=$1)`, other)
			// Preserve the duplicate lab period found in the source data too.
			f.exec(`INSERT INTO section_schedules(section_id,kind,day_of_week,start_time,end_time)
			 SELECT section_id,'lab',2,'17:00'::time,'20:00'::time FROM ta_request_assignments WHERE id=$1
			 UNION ALL SELECT section_id,'lab',2,'17:00'::time,'20:00'::time FROM ta_request_assignments WHERE id=$1
			 UNION ALL SELECT section_id,'lecture',3,'17:00'::time,'19:00'::time FROM ta_request_assignments WHERE id=$1`, other)
			preview, err := f.Req.PreviewConflicts(f.ctx, f.TAID, f.CourseID)
			if err != nil {
				t.Fatal(err)
			}
			detail, err := f.Req.PreviewConflictSchedule(f.ctx, f.TAID, f.CourseID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name != "nonoverlapping_control" {
				if len(preview) != 1 || !preview[0].CrossConflict || len(preview[0].BlockedKinds) != 1 || preview[0].BlockedKinds[0] != "lecture" {
					t.Fatalf("preview=%+v", preview)
				}
				if len(detail.Conflicts) != 1 {
					t.Fatalf("pairs=%+v", detail.Conflicts)
				}
				p := detail.Conflicts[0]
				if p.Requested.Kind != "lecture" || p.Existing.Kind != "lab" || p.OverlapStart != "17:00" || p.OverlapEnd != "18:00" {
					t.Fatalf("pair=%+v", p)
				}
			} else if len(preview) != 0 || len(detail.Conflicts) != 0 {
				t.Fatalf("control preview=%+v detail=%+v", preview, detail)
			}
			in := f.createInput()
			in.Assignments[0].Workload = tc.work
			result, err := f.Req.Create(f.ctx, f.LecturerID, in)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != tc.want {
				t.Fatalf("status=%s want=%s checks=%+v", result.Status, tc.want, result.Checks)
			}
			crossFailed := false
			for _, check := range result.Checks {
				if check.Rule == "cross_conflict" && !check.Passed {
					crossFailed = !check.Warning
				}
			}
			if crossFailed {
				t.Fatalf("cross conflict checks=%+v", result.Checks)
			}
			var assignment uuid.UUID
			if err := f.Pool.QueryRow(f.ctx, `SELECT id FROM ta_request_assignments WHERE request_id=$1`, result.ID).Scan(&assignment); err != nil {
				t.Fatal(err)
			}
			var attendance, lab float64
			if err := f.Pool.QueryRow(f.ctx, `SELECT attendance_hrs,lab_hrs FROM ta_workload_forms WHERE assignment_id=$1`, assignment).Scan(&attendance, &lab); err != nil {
				t.Fatal(err)
			}
			if tc.name != "nonoverlapping_control" && attendance != 0 {
				t.Fatalf("blocked attendance survived: %v", attendance)
			}
			if lab != tc.work.LabHrs {
				t.Fatalf("free lab changed: %v", lab)
			}
			var existingStatus string
			if err := f.Pool.QueryRow(f.ctx, `SELECT r.status::text FROM ta_requests r JOIN ta_request_assignments a ON a.request_id=r.id WHERE a.id=$1`, other).Scan(&existingStatus); err != nil {
				t.Fatal(err)
			}
			if existingStatus != "approved" {
				t.Fatal("existing approval changed")
			}
			if tc.name == "grading_only" {
				var reserved int
				if err := f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM ta_assignment_schedules WHERE assignment_id=$1`, assignment).Scan(&reserved); err != nil {
					t.Fatal(err)
				}
				if reserved != 0 {
					t.Fatal("off-slot grading must not reserve classroom meetings")
				}
			}
			if tc.want == "approved" && tc.name != "nonoverlapping_control" {
				ac, err := loadAssignmentContext(f.ctx, f.Pool, assignment)
				if err != nil {
					t.Fatal(err)
				}
				blocked := WorkLog{AssignmentID: assignment, Activity: "lecture", WorkDate: "2026-06-02", StartTime: "16:00", EndTime: "17:00"}
				if err := f.Svc.validateClassWindow(f.ctx, ac, blocked, makeupIndex{}); err == nil {
					t.Fatal("the free fragment of an excluded meeting must not be loggable")
				}
				if tc.work.LabHrs > 0 {
					free := WorkLog{AssignmentID: assignment, Activity: "lab", WorkDate: "2026-06-02", StartTime: tc.start, EndTime: tc.end}
					if err := f.Svc.validateClassWindow(f.ctx, ac, free, makeupIndex{}); err != nil {
						t.Fatalf("free lab rejected: %v", err)
					}
				}
				if err := checkExcludedSessionWorkload(f.ctx, f.Pool, assignment, f.TAID, f.SectionID, WorkloadInput{AttendanceHrs: 1}); err == nil {
					t.Fatal("staff correction must not restore excluded attendance hours")
				}
				if tc.name == "sec1_lab_only" {
					generated, err := f.Svc.Generate(f.ctx, f.TAID, assignment)
					if err != nil {
						t.Fatal(err)
					}
					if len(generated.Entries) == 0 {
						t.Fatal("free labs should still generate work logs")
					}
					for _, log := range generated.Entries {
						if log.Activity == "lecture" {
							t.Fatal("excluded lecture was generated")
						}
					}
					busy, err := f.Svc.ScheduleBusyBlocks(f.ctx, f.TAID, assignment)
					if err != nil {
						t.Fatal(err)
					}
					for _, block := range busy {
						if block.Kind == "teaching" && block.DayOfWeek == 2 && block.StartTime == "16:00" {
							t.Fatal("excluded lecture still reserves time")
						}
					}
				}
			}
			t.Logf("CP372012 status=%s; preview conflicts=%d; overlapping pairs=%d; cross_conflict failed=%v", result.Status, len(preview), len(detail.Conflicts), crossFailed)
		})
	}
}

func TestCrossCoursePartialClash_DeferredAndAdditionalSections(t *testing.T) {
	for _, deferred := range []bool{true, false} {
		name := "add_sections"
		if deferred {
			name = "deferred"
		}
		t.Run(name, func(t *testing.T) {
			f := newRequestFixture(t, fixtureOpts{})
			other := f.secondCourseAssignment(fixtureOpts{})
			f.exec(`INSERT INTO section_schedules(section_id,kind,day_of_week,start_time,end_time)
     SELECT section_id,'lab',2,'17:00','20:00' FROM ta_request_assignments WHERE id=$1`, other)
			var reqID uuid.UUID
			var target uuid.UUID
			if deferred {
				target = f.SectionID
			} else {
				reqID = f.approvedWithTA()
				target = f.addSection("02", 2)
			}
			f.exec(`DELETE FROM section_schedules WHERE section_id=$1`, target)
			f.exec(`INSERT INTO section_schedules(section_id,kind,day_of_week,start_time,end_time) VALUES
     ($1,'lecture',2,'16:00','18:00'),($1,'lab',2,'09:00','12:00')`, target)
			if deferred {
				in := f.createInput()
				in.Assignments[0].Workload = WorkloadInput{AttendanceHrs: 1, LabHrs: 2}
				result, err := f.Req.Create(f.ctx, f.LecturerID, in)
				if err != nil {
					t.Fatal(err)
				}
				reqID = result.ID
				if result.Status != "submitted" {
					t.Fatalf("before timetable: %+v", result)
				}
				f.addTAClass(5, "07:00", "08:00")
				if err := f.Req.ReevaluateForTA(f.ctx, f.TAID, f.TermID); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.addSecs(reqID, ugSec(target, 1, 2)); err != nil {
					t.Fatal(err)
				}
			}
			var state string
			var attendance, lab float64
			var exclusions int
			if err := f.Pool.QueryRow(f.ctx, `SELECT a.state::text,w.attendance_hrs,w.lab_hrs,
    (SELECT count(*) FROM ta_assignment_session_exclusions e WHERE e.assignment_id=a.id)
    FROM ta_request_assignments a JOIN ta_workload_forms w ON w.assignment_id=a.id
    WHERE a.request_id=$1 AND a.section_id=$2`, reqID, target).Scan(&state, &attendance, &lab, &exclusions); err != nil {
				t.Fatal(err)
			}
			if f.status(reqID) != "approved" || state != "trimmed" || attendance != 0 || lab != 2 || exclusions != 1 {
				t.Fatalf("status=%s state=%s attendance=%v lab=%v exclusions=%d", f.status(reqID), state, attendance, lab, exclusions)
			}
		})
	}
}

func TestCrossCoursePartialClash_RemainingMeetingsAndDuties(t *testing.T) {
	for _, tc := range []struct {
		name            string
		all, own        bool
		work            WorkloadInput
		want            string
		attendance, lab float64
	}{
		{"one_of_two_lectures", false, false, WorkloadInput{AttendanceHrs: 3}, "approved", 2, 0},
		{"all_meetings_with_grading", true, false, WorkloadInput{AttendanceHrs: 2, LabHrs: 2, CheckWorkHrs: 2}, "approved", 0, 0},
		{"all_meetings_no_other_work", true, false, WorkloadInput{AttendanceHrs: 2, LabHrs: 2}, "rejected", 0, 0},
		{"own_class_and_other_course_cover_both_lectures", false, true, WorkloadInput{AttendanceHrs: 3, LabHrs: 2}, "approved", 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRequestFixture(t, fixtureOpts{})
			f.addTAClass(5, "07:00", "08:00")
			other := f.secondCourseAssignment(fixtureOpts{})
			day, start, end := 1, "10:00", "11:00"
			if tc.all {
				start, end = "09:00", "16:00"
			} else {
				f.exec(`INSERT INTO section_schedules(section_id,kind,day_of_week,start_time,end_time) VALUES($1,'lecture',2,'09:00','11:00')`, f.SectionID)
			}
			if tc.own {
				f.addTAClass(1, "09:00", "12:00")
				day = 2
			}
			f.exec(`INSERT INTO section_schedules(section_id,kind,day_of_week,start_time,end_time)
     SELECT section_id,'lab',$2,$3::time,$4::time FROM ta_request_assignments WHERE id=$1`, other, day, start, end)
			in := f.createInput()
			in.Assignments[0].Workload = tc.work
			result, err := f.Req.Create(f.ctx, f.LecturerID, in)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != tc.want {
				t.Fatalf("status=%s want=%s checks=%+v", result.Status, tc.want, result.Checks)
			}
			attendance, lab := f.declaredInClass(result.ID)
			if attendance != tc.attendance || lab != tc.lab {
				t.Fatalf("attendance=%v lab=%v", attendance, lab)
			}
			if tc.own {
				// Repeated timetable saves must keep the exclusion and effective hours.
				if err := f.Req.ReevaluateForTA(f.ctx, f.TAID, f.TermID); err != nil {
					t.Fatal(err)
				}
				attendance, lab = f.declaredInClass(result.ID)
				if attendance != 0 || lab != 2 {
					t.Fatalf("reevaluation restored blocked hours: attendance=%v lab=%v", attendance, lab)
				}
			}
		})
	}
}
