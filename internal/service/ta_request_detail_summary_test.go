package service

import "testing"

// UAT DEF-009: Detail embeds the list's summary but did not select its fields,
// so a late request came back as on time (and trimmed/dropped/can_cancel as
// zero values) from GET /ta-requests/:id while the list said otherwise.
func TestDetail_CarriesTheSummaryFieldsTheListHas(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.exec(`UPDATE ta_requests SET is_late = TRUE WHERE id = $1`, f.RequestID)
	f.exec(`UPDATE ta_request_assignments SET state = 'trimmed' WHERE id = $1`, f.AssignmentID)

	svc := &TARequestService{pool: f.Pool}
	d, err := svc.Detail(f.ctx, f.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if !d.IsLate {
		t.Error("is_late = false, want true — the list already reports it late")
	}
	if d.TrimmedCount != 1 {
		t.Errorf("trimmed_count = %d, want 1", d.TrimmedCount)
	}
}

// The detail view must return the actual form values, including descriptions,
// even after a section is trimmed or dropped by timetable checks.
func TestDetail_CarriesWorkloadFormValues(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.exec(`UPDATE ta_workload_forms SET help_teach_hrs=1.25, help_teach_desc='ช่วยอธิบายโจทย์', prep_hrs=0.5, prep_desc='เตรียมใบงาน', grade_hrs=0.75, grade_desc='ตรวจแบบฝึกหัด', other_hrs=0.25, other_desc='ตอบคำถาม', check_work_hrs=0.5, attendance_hrs=0.25, ug_other_hrs=0.25, ug_other_desc='จัดเอกสาร', lab_hrs=1, lab_other_hrs=0.25, lab_other_desc='เตรียมอุปกรณ์' WHERE assignment_id=$1`, f.AssignmentID)
	f.exec(`UPDATE ta_request_assignments SET state='trimmed' WHERE id=$1`, f.AssignmentID)
	svc := &TARequestService{pool: f.Pool}
	d, err := svc.Detail(f.ctx, f.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Assignments) != 1 {
		t.Fatalf("assignments = %d", len(d.Assignments))
	}
	a := d.Assignments[0]
	want := WorkloadInput{HelpTeachHrs: 1.25, HelpTeachDesc: "ช่วยอธิบายโจทย์", PrepHrs: 0.5, PrepDesc: "เตรียมใบงาน", GradeHrs: 0.75, GradeDesc: "ตรวจแบบฝึกหัด", OtherHrs: 0.25, OtherDesc: "ตอบคำถาม", CheckWorkHrs: 0.5, AttendanceHrs: 0.25, UGOtherHrs: 0.25, UGOtherDesc: "จัดเอกสาร", LabHrs: 1, LabOtherHrs: 0.25, LabOtherDesc: "เตรียมอุปกรณ์"}
	if a.Workload == nil || *a.Workload != want {
		t.Fatalf("workload = %+v, want %+v", a.Workload, want)
	}
	if a.TotalHrs != 5 || a.State != "trimmed" {
		t.Fatalf("summary changed: %+v", a)
	}
	f.exec(`DELETE FROM ta_workload_forms WHERE assignment_id=$1`, f.AssignmentID)
	d, err = svc.Detail(f.ctx, f.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Assignments[0].Workload != nil {
		t.Fatalf("missing form should be null: %+v", d.Assignments[0].Workload)
	}
}
