package service

import (
	"github.com/google/uuid"
	"testing"
)

func TestTAAssignmentWorkload_RequestDetailsAndScope(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.exec(`UPDATE ta_workload_forms SET attendance_hrs=1.5, check_work_hrs=2,
  ug_other_hrs=0.5, ug_other_desc='ตอบคำถามเพิ่มเติม', lab_other_hrs=1,
  lab_other_desc='เตรียมอุปกรณ์', help_teach_desc=NULL WHERE assignment_id=$1`, f.AssignmentID)
	svc := &TeachingService{pool: f.Pool}
	rows, err := svc.ListAssignmentsForTA(f.ctx, f.TAID, &f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d assignments", len(rows))
	}
	w := rows[0].Workload
	if w == nil || w.AttendanceHrs != 1.5 || w.CheckWorkHrs != 2 || w.UGOtherDesc != "ตอบคำถามเพิ่มเติม" || w.LabOtherHrs != 1 || w.LabOtherDesc != "เตรียมอุปกรณ์" || w.HelpTeachDesc != "" {
		t.Fatalf("request details not preserved: %+v", w)
	}
	other := uuid.New()
	rows, err = svc.ListAssignmentsForTA(f.ctx, other, &f.CourseID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("other TA sees assignments: %v %v", rows, err)
	}
	f.exec(`UPDATE ta_request_assignments SET state='dropped' WHERE id=$1`, f.AssignmentID)
	rows, err = svc.ListAssignmentsForTA(f.ctx, f.TAID, &f.CourseID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("dropped assignment visible: %v %v", rows, err)
	}
}

func TestTAAssignmentWorkload_MissingForm(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoWorkloadForm: true})
	svc := &TeachingService{pool: f.Pool}
	rows, err := svc.ListAssignmentsForTA(f.ctx, f.TAID, &f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Workload != nil {
		t.Fatalf("missing form should be null: %+v", rows)
	}
}
