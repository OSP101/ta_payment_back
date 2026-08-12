package service

import (
	"testing"

	"github.com/google/uuid"
)

// 2026 staff meeting correction: request submission must NOT refuse a grad
// (master/phd) TA just because some day's real timetable exceeds
// grad_regular_daily_hour_cap (6 ชม./วัน) — that check now belongs only to
// the worklog entry point (worklog.go's validateGradRegularClassWindow), not
// to Create(). Grad submission is bound only by the 10–12 ชม./สัปดาห์ total.
// Undergrad is unaffected: enforceDailyHourFeasibility must still run for
// them.

// addEightHourSection creates a section whose ONE weekday session runs 8
// hours straight — long enough to exceed both the grad (6h) and undergrad
// (7h) daily caps, reproducing the reported bug (a Friday section totaling
// 8.0 ชม. tripped the "เกินเพดาน 6.0 ชม./วัน" refusal for a grad TA).
func (rf *requestFixture) addEightHourSection(secNo string, dayOfWeek int) uuid.UUID {
	id := uuid.New()
	rf.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track)
	         VALUES ($1, $2, $3, 'regular')`, id, rf.CourseID, secNo)
	rf.exec(`INSERT INTO section_schedules (section_id, kind, day_of_week, start_time, end_time)
	         VALUES ($1, 'lecture', $2, '08:00', '16:00')`, id, dayOfWeek)
	return id
}

func TestCreate_GradTA_NotBlockedByDailyHourFeasibility(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{Level: "master"})
	// fixtureOpts.Level only feeds ta_request_assignments.level (skipped here
	// since NoRequest=true) — validateTA's authoritative level comes from
	// users.study_level, which insertUser always hardcodes to 'undergrad'.
	// Set it explicitly so Create() actually resolves this TA as grad.
	rf.exec(`UPDATE users SET study_level = 'master' WHERE id = $1`, rf.TAID)
	fri := rf.addEightHourSection("02", 5) // Friday, 8h — over the 6h grad daily cap

	in := rf.inputFor([]SectionWorkload{
		{SectionID: fri, Workload: WorkloadInput{HelpTeachHrs: 10}},
	}, "master")

	if _, err := rf.Req.Create(rf.ctx, rf.LecturerID, in); err != nil {
		t.Fatalf("a grad TA's request must not be refused for exceeding the daily-hour cap (that check now belongs to worklog entry, not submission): %v", err)
	}
}

// Undergrad must still be refused — only the grad exemption changed.
func TestCreate_UndergradTA_StillBlockedByDailyHourFeasibility(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{Level: "undergrad"})
	fri := rf.addEightHourSection("02", 5) // Friday, 8h — over the 7h undergrad daily cap

	in := rf.inputFor([]SectionWorkload{
		{SectionID: fri, Workload: WorkloadInput{AttendanceHrs: 7}},
	}, "undergrad")

	if _, err := rf.Req.Create(rf.ctx, rf.LecturerID, in); err == nil {
		t.Fatal("an undergrad request over the daily-hour cap must still be refused at submission")
	}
}
