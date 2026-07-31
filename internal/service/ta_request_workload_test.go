package service

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Per-section workload declaration (phase 1, item C).
//
// The lecturers asked to declare hours "ต่อกลุ่มเรียน (section)", bounded by
// the course's weekly contact hours — the figures inside the Thai credit
// notation 3(3-0-6). Two consequences fall out of that:
//
//   - the ceiling is per group, NOT multiplied by how many groups the TA
//     covers, because a TA cannot spend more time on a group than the group
//     meets;
//   - sections taught in the same sitting are one piece of work, so their
//     hours count once ("เบิกได้แค่ก้อนเดียว").

// addSection adds another section to the fixture's course, optionally meeting
// at the same time as the original (co-taught) or on a different day.
func (rf *requestFixture) addSection(secNo string, dayOfWeek int) uuid.UUID {
	id := uuid.New()
	rf.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track)
	         VALUES ($1, $2, $3, 'regular')`, id, rf.CourseID, secNo)
	rf.exec(`INSERT INTO section_schedules (section_id, kind, day_of_week, start_time, end_time)
	         VALUES ($1, 'lecture', $2, '09:00', '12:00'), ($1, 'lab', $2, '13:00', '16:00')`,
		id, dayOfWeek)
	return id
}

// inputFor builds a request covering the given sections with per-section hours.
func (rf *requestFixture) inputFor(per []SectionWorkload, level string) CreateTARequestInput {
	ids := make([]uuid.UUID, 0, len(per))
	for _, p := range per {
		ids = append(ids, p.SectionID)
	}
	return CreateTARequestInput{
		TeachingCourseID: rf.CourseID,
		ReimburseScope:   "both",
		Assignments: []AssignmentInput{{
			SectionIDs:       ids,
			TAID:             rf.TAID,
			Level:            level,
			SectionWorkloads: per,
		}},
	}
}

// ---------------------------------------------------------------------------
// The per-section ceiling comes from the credit notation
// ---------------------------------------------------------------------------

// The fixture course is 3 credits with lecture_hrs=3 and lab_hrs=3.
func TestWorkload_SectionCapFromContactHours(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})

	in := rf.inputFor([]SectionWorkload{{
		SectionID: rf.SectionID,
		Workload:  WorkloadInput{AttendanceHrs: 3.5}, // course only meets 3h/week
	}}, "undergrad")

	_, err := rf.Req.Create(rf.ctx, rf.LecturerID, in)
	if err == nil {
		t.Fatal("hours above the course's weekly lecture hours must be refused")
	}
	if !strings.Contains(err.Error(), "เกินชั่วโมงของวิชาต่อสัปดาห์") {
		t.Errorf("message should point at the contact-hour ceiling, got: %v", err)
	}
}

func TestWorkload_SectionCapAllowsExactlyTheContactHours(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})

	in := rf.inputFor([]SectionWorkload{{
		SectionID: rf.SectionID,
		Workload:  WorkloadInput{AttendanceHrs: 3, LabHrs: 3},
	}}, "undergrad")

	if _, err := rf.Req.Create(rf.ctx, rf.LecturerID, in); err != nil {
		t.Fatalf("declaring exactly the course's contact hours must be allowed: %v", err)
	}
}

// The ceiling must NOT scale with the number of sections. Before per-section
// declarations the cap was lectureHrs × nSections, which would have let a TA
// covering two groups claim 6h against one group that meets for 3.
func TestWorkload_CapDoesNotScaleWithSectionCount(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	second := rf.addSection("02", 2) // Tuesday — a separate sitting

	in := rf.inputFor([]SectionWorkload{
		{SectionID: rf.SectionID, Workload: WorkloadInput{AttendanceHrs: 5}},
		{SectionID: second, Workload: WorkloadInput{AttendanceHrs: 1}},
	}, "undergrad")

	_, err := rf.Req.Create(rf.ctx, rf.LecturerID, in)
	if err == nil {
		t.Fatal("5h on a group that meets 3h/week must be refused regardless of how many groups the TA covers")
	}
}

// Each section is judged on its own, so the error must name the offending one.
func TestWorkload_ErrorNamesTheSection(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	second := rf.addSection("02", 2)

	in := rf.inputFor([]SectionWorkload{
		{SectionID: rf.SectionID, Workload: WorkloadInput{AttendanceHrs: 2}},
		{SectionID: second, Workload: WorkloadInput{AttendanceHrs: 9}},
	}, "undergrad")

	_, err := rf.Req.Create(rf.ctx, rf.LecturerID, in)
	if err == nil {
		t.Fatal("the over-cap section must be refused")
	}
	if !strings.Contains(err.Error(), "Sec 02") {
		t.Errorf("message should name Sec 02, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Every section gets its own workload row
// ---------------------------------------------------------------------------

func TestWorkload_EachSectionStoresItsOwnHours(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	second := rf.addSection("02", 2)

	in := rf.inputFor([]SectionWorkload{
		{SectionID: rf.SectionID, Workload: WorkloadInput{AttendanceHrs: 3}},
		{SectionID: second, Workload: WorkloadInput{AttendanceHrs: 1}},
	}, "undergrad")
	if _, err := rf.Req.Create(rf.ctx, rf.LecturerID, in); err != nil {
		t.Fatalf("create: %v", err)
	}

	rows, err := rf.Pool.Query(rf.ctx, `
		SELECT sec.sec_no, w.attendance_hrs
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		JOIN ta_workload_forms w ON w.assignment_id = a.id
		ORDER BY sec.sec_no`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	got := map[string]float64{}
	for rows.Next() {
		var secNo string
		var hrs float64
		if err := rows.Scan(&secNo, &hrs); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[secNo] = hrs
	}
	if len(got) != 2 {
		t.Fatalf("expected a workload row per section, got %d: %v", len(got), got)
	}
	if got["01"] != 3 || got["02"] != 1 {
		t.Errorf("per-section hours not stored independently: %v", got)
	}
}

// ---------------------------------------------------------------------------
// Co-taught sections are one piece of work
// ---------------------------------------------------------------------------

// Sections meeting in the same slot are detected from their timetables — the
// lecturer never declares the grouping.
func TestWorkload_DetectsCotaughtSectionsFromTimetable(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	same := rf.addSection("02", 1) // Monday, identical hours → co-taught

	in := rf.inputFor([]SectionWorkload{
		{SectionID: rf.SectionID, Workload: WorkloadInput{AttendanceHrs: 3}},
		{SectionID: same, Workload: WorkloadInput{AttendanceHrs: 3}},
	}, "undergrad")
	if _, err := rf.Req.Create(rf.ctx, rf.LecturerID, in); err != nil {
		t.Fatalf("create: %v", err)
	}

	var groups []int
	rows, err := rf.Pool.Query(rf.ctx,
		`SELECT COALESCE(cotaught_group, 0) FROM ta_request_assignments ORDER BY 1`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var g int
		if err := rows.Scan(&g); err != nil {
			t.Fatalf("scan: %v", err)
		}
		groups = append(groups, g)
	}
	if len(groups) != 2 {
		t.Fatalf("expected 2 assignments, got %d", len(groups))
	}
	if groups[0] == 0 || groups[0] != groups[1] {
		t.Errorf("sections meeting in the same slot must share a co-taught group, got %v", groups)
	}
}

func TestWorkload_SeparateSlotsAreNotCotaught(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	other := rf.addSection("02", 2) // Tuesday

	in := rf.inputFor([]SectionWorkload{
		{SectionID: rf.SectionID, Workload: WorkloadInput{AttendanceHrs: 3}},
		{SectionID: other, Workload: WorkloadInput{AttendanceHrs: 3}},
	}, "undergrad")
	if _, err := rf.Req.Create(rf.ctx, rf.LecturerID, in); err != nil {
		t.Fatalf("create: %v", err)
	}

	var grouped int
	if err := rf.Pool.QueryRow(rf.ctx,
		`SELECT COUNT(*) FROM ta_request_assignments WHERE cotaught_group IS NOT NULL`).Scan(&grouped); err != nil {
		t.Fatalf("query: %v", err)
	}
	if grouped != 0 {
		t.Errorf("sections meeting at different times must not be grouped, got %d grouped", grouped)
	}
}

// A graduate TA must land in 10–12 h/week. Co-taught sections describe the
// same hours, so covering two groups in one sitting must not double the total
// and push a compliant declaration out of range.
func TestWorkload_CotaughtHoursCountOnceForGradRange(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{Level: "master"})
	same := rf.addSection("02", 1) // same slot as section 01

	// 11 h/week declared for each group — one sitting, so the person works 11.
	grad := WorkloadInput{HelpTeachHrs: 3, PrepHrs: 3, GradeHrs: 3, OtherHrs: 2}
	in := rf.inputFor([]SectionWorkload{
		{SectionID: rf.SectionID, Workload: grad},
		{SectionID: same, Workload: grad},
	}, "master")

	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, c := range res.Checks {
		if c.Rule != "workload" {
			continue
		}
		if !c.Passed {
			t.Fatalf("co-taught hours were counted twice, pushing the total out of 10–12: %s", c.Message)
		}
		if !strings.Contains(c.Message, "11.00") {
			t.Errorf("weekly total should be 11.00 (counted once), got: %s", c.Message)
		}
	}
}
