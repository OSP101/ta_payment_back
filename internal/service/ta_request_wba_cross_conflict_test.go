package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestConcurrentRequests_OverlappingLecturesApproveOnlyOne(t *testing.T) {
	f := newRequestFixture(t, fixtureOpts{})
	f.addTAClass(5, "07:00", "08:00")
	f.exec(`DELETE FROM section_schedules WHERE section_id=$1`, f.SectionID)
	f.exec(`INSERT INTO section_schedules(section_id,kind,day_of_week,start_time,end_time) VALUES($1,'lecture',1,'09:00','12:00')`, f.SectionID)
	course, section := uuid.New(), uuid.New()
	f.exec(`INSERT INTO teaching_courses(id,term_id,code,name_th,level,credits,lecture_hrs,num_students,num_students_regular)
	 VALUES($1,$2,'CON101','Concurrent course','undergrad',3,3,40,40)`, course, f.TermID)
	f.exec(`INSERT INTO teaching_lecturers(teaching_course_id,lecturer_id,is_primary) VALUES($1,$2,true)`, course, f.LecturerID)
	f.exec(`INSERT INTO sections(id,teaching_course_id,sec_no,track) VALUES($1,$2,'01','regular')`, section, course)
	f.exec(`INSERT INTO section_schedules(section_id,kind,day_of_week,start_time,end_time) VALUES($1,'lecture',1,'10:00','13:00')`, section)
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	type outcome struct {
		result *CreateResult
		err    error
	}
	start, done := make(chan struct{}), make(chan outcome, 2)
	for _, pair := range [][2]uuid.UUID{{f.CourseID, f.SectionID}, {course, section}} {
		in := f.createInput()
		in.TeachingCourseID = pair[0]
		in.Assignments[0].SectionIDs = []uuid.UUID{pair[1]}
		in.Assignments[0].Workload = WorkloadInput{AttendanceHrs: 2}
		go func() {
			<-start
			result, err := f.Req.Create(ctx, f.LecturerID, in)
			done <- outcome{result, err}
		}()
	}
	close(start)
	counts := map[string]int{}
	for range 2 {
		got := <-done
		if got.err != nil {
			t.Fatal(got.err)
		}
		counts[got.result.Status]++
	}
	if counts["approved"] != 1 || counts["rejected"] != 1 {
		t.Fatalf("one TA cannot approve for both overlapping courses: %v", counts)
	}
}

// A registrar WBA section is filled by its lecturer after the first course
// already has this TA. Lecture/lecture must get the same verdict as lab/lab.
func TestRequest_WBAFilledWithOverlappingCrossCourseTime(t *testing.T) {
	for _, kind := range []string{"lecture", "lab"} {
		t.Run(kind, func(t *testing.T) {
			f := newRequestFixture(t, fixtureOpts{})
			f.addTAClass(5, "07:00", "08:00")
			f.exec(`DELETE FROM section_schedules WHERE section_id=$1`, f.SectionID)
			f.exec(`INSERT INTO section_schedules(section_id,kind,day_of_week,start_time,end_time) VALUES($1,$2,1,'09:00','12:00')`, f.SectionID, kind)
			in := f.createInput()
			in.Assignments[0].Workload = WorkloadInput{AttendanceHrs: 2}
			if kind == "lab" {
				in.Assignments[0].Workload = WorkloadInput{LabHrs: 2}
			}
			first, err := f.Req.Create(f.ctx, f.LecturerID, in)
			if err != nil || first.Status != "approved" {
				t.Fatalf("first request: %+v, %v", first, err)
			}
			course, section := uuid.New(), uuid.New()
			f.exec(`INSERT INTO teaching_courses(id,term_id,code,name_th,level,credits,lecture_hrs,lab_hrs,num_students,num_students_regular)
			 VALUES($1,$2,'WBA101','WBA course','undergrad',3,3,3,40,40)`, course, f.TermID)
			f.exec(`INSERT INTO teaching_lecturers(teaching_course_id,lecturer_id,is_primary) VALUES($1,$2,true)`, course, f.LecturerID)
			f.exec(`INSERT INTO sections(id,teaching_course_id,sec_no,track) VALUES($1,$2,'01','regular')`, section, course)
			svc := newTeachingSvc(t, f.fixture)
			if err := svc.ReplaceSectionSchedules(f.ctx, f.LecturerID, course, section, []SectionSchedule{{Kind: kind, DayOfWeek: 1, StartTime: "10:00", EndTime: "13:00"}}); err != nil {
				t.Fatal(err)
			}
			preview, err := f.Req.PreviewConflicts(f.ctx, f.TAID, course)
			if err != nil || len(preview) != 1 || !preview[0].CrossConflict {
				t.Fatalf("preview must block cross-course overlap: %+v, %v", preview, err)
			}
			in.TeachingCourseID = course
			in.Assignments[0].SectionIDs = []uuid.UUID{section}
			second, err := f.Req.Create(f.ctx, f.LecturerID, in)
			if err != nil || second.Status != "rejected" {
				t.Fatalf("overlapping request must be rejected: %+v, %v", second, err)
			}
			if f.status(first.ID) != "approved" {
				t.Fatal("first approval changed")
			}
		})
	}
}

func TestReplaceSectionSchedules_RefusesExistingCrossCourseBookingsAtomically(t *testing.T) {
	for _, status := range []string{"submitted", "approved"} {
		t.Run(status, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{})
			other := f.secondCourseAssignment(fixtureOpts{})
			var course, section uuid.UUID
			if err := f.Pool.QueryRow(f.ctx, `SELECT r.teaching_course_id,a.section_id FROM ta_request_assignments a JOIN ta_requests r ON r.id=a.request_id WHERE a.id=$1`, other).Scan(&course, &section); err != nil {
				t.Fatal(err)
			}
			f.exec(`UPDATE ta_requests SET status=$2::ta_request_status WHERE id=(SELECT request_id FROM ta_request_assignments WHERE id=$1)`, other, status)
			svc := newTeachingSvc(t, f)
			err := svc.ReplaceSectionSchedules(f.ctx, f.LecturerID, course, section, []SectionSchedule{{Kind: "lecture", DayOfWeek: 1, StartTime: "10:00", EndTime: "12:00"}})
			if userErrStatus(err) != 409 || !strings.Contains(err.Error(), "ทับซ้อน") {
				t.Fatalf("must refuse overlapping booking: %v", err)
			}
			var count int
			var stamped bool
			if err := f.Pool.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM section_schedules WHERE section_id=$1),schedule_set_by_lecturer_at IS NOT NULL FROM sections WHERE id=$1`, section).Scan(&count, &stamped); err != nil {
				t.Fatal(err)
			}
			if count != 0 || stamped {
				t.Fatalf("refused save persisted timetable or spent one-shot: count=%d stamp=%v", count, stamped)
			}
			// A valid correction must still be available after rollback.
			if err := svc.ReplaceSectionSchedules(f.ctx, f.LecturerID, course, section, []SectionSchedule{{Kind: "lecture", DayOfWeek: 2, StartTime: "10:00", EndTime: "12:00"}}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCrossRequestConflict_IgnoresReleasedBookings(t *testing.T) {
	for _, released := range []string{"cancelled", "rejected", "dropped"} {
		t.Run(released, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{})
			other := f.secondCourseAssignment(fixtureOpts{})
			var section uuid.UUID
			if err := f.Pool.QueryRow(f.ctx, `SELECT section_id FROM ta_request_assignments WHERE id=$1`, other).Scan(&section); err != nil {
				t.Fatal(err)
			}
			f.exec(`INSERT INTO section_schedules(section_id,kind,day_of_week,start_time,end_time) VALUES($1,'lecture',1,'10:00','12:00')`, section)
			if released == "dropped" {
				f.exec(`UPDATE ta_request_assignments SET state='dropped' WHERE id=$1`, other)
			} else {
				f.exec(`UPDATE ta_requests SET status=$2::ta_request_status WHERE id=(SELECT request_id FROM ta_request_assignments WHERE id=$1)`, other, released)
			}
			req := &TARequestService{pool: f.Pool}
			if err := req.checkCrossRequestConflict(f.ctx, f.Pool, f.TAID, f.SectionID, f.TermID, f.CourseID, f.RequestID, []string{"approved"}, "TA"); err != nil {
				t.Fatal(err)
			}
			preview, err := req.PreviewConflictSchedule(f.ctx, f.TAID, f.CourseID)
			if err != nil || len(preview.Conflicts) != 0 {
				t.Fatalf("released booking blocked preview: %+v, %v", preview, err)
			}
			if err := newTeachingSvc(t, f).ReplaceSectionSchedules(f.ctx, f.StaffID, f.CourseID, f.SectionID, []SectionSchedule{{Kind: "lecture", DayOfWeek: 1, StartTime: "10:00", EndTime: "12:00"}}); err != nil {
				t.Fatalf("released booking blocked timetable correction: %v", err)
			}
		})
	}
}
