package service

import (
	"github.com/google/uuid"
	"testing"
)

// Staff reproduction: two special-track sections total seven Friday class
// hours, but the TA is requested for only two hours per section (four weekly).
func TestRequest_PartialWorkOnSevenHourClassDay(t *testing.T) {
	f := newRequestFixture(t, fixtureOpts{})
	first := f.addEightHourSection("02", 5)
	second := f.addEightHourSection("03", 5)
	f.exec(`UPDATE section_schedules SET start_time='08:00',end_time='11:00' WHERE section_id=$1`, first)
	f.exec(`UPDATE section_schedules SET start_time='11:00',end_time='15:00' WHERE section_id=$1`, second)
	f.exec(`UPDATE sections SET track='special' WHERE id=ANY($1)`, []uuid.UUID{first, second})
	work := []SectionWorkload{{SectionID: first, Workload: WorkloadInput{AttendanceHrs: 2}}, {SectionID: second, Workload: WorkloadInput{AttendanceHrs: 2}}}
	in := f.inputFor(work, "undergrad")
	res, err := f.Req.Create(f.ctx, f.LecturerID, in)
	if err != nil {
		t.Fatalf("4 weekly hours on a 7-hour class day must be requestable: %v", err)
	}
	if res.Status != "submitted" {
		t.Fatalf("without a TA timetable, want submitted, got %s", res.Status)
	}
	if _, err := f.Req.Edit(f.ctx, f.LecturerID, res.ID, in); err != nil {
		t.Fatalf("editing the same partial work must succeed: %v", err)
	}
}

func TestAddSections_PartialWorkOnSevenHourClassDay(t *testing.T) {
	f := newRequestFixture(t, fixtureOpts{})
	first := f.addEightHourSection("02", 5)
	second := f.addEightHourSection("03", 5)
	f.exec(`UPDATE section_schedules SET start_time='08:00',end_time='11:00' WHERE section_id=$1`, first)
	f.exec(`UPDATE section_schedules SET start_time='11:00',end_time='15:00' WHERE section_id=$1`, second)
	f.exec(`UPDATE sections SET track='special' WHERE id=ANY($1)`, []uuid.UUID{first, second})
	in := f.inputFor([]SectionWorkload{{SectionID: first, Workload: WorkloadInput{AttendanceHrs: 2}}}, "undergrad")
	res, err := f.Req.Create(f.ctx, f.LecturerID, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Req.AddSections(f.ctx, f.LecturerID, res.ID, AddSectionsInput{TAID: f.TAID, Sections: []SectionWorkload{{SectionID: second, Workload: WorkloadInput{AttendanceHrs: 2}}}}); err != nil {
		t.Fatalf("adding partial work must not count all seven class hours: %v", err)
	}
}
