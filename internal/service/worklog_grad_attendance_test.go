package service

import (
	"math"
	"testing"
)

// เช็คชื่อ is billed for a SLICE of the lecture period, and how long that slice
// runs depends on where the answer can come from.
//
// An undergrad's workload form states it as attendance_hrs and always has.
// A graduate form has no such field — help_teach_hrs is a weekly BUDGET covering
// บรรยาย and ปฏิบัติการ together, not a duration — so the trim could not run
// there and the entire class period was billed instead. CP363205 charged 2 hours
// against a signed form saying 1 and broke the very help_teach ceiling the row
// was declared under, which is why its lecturer could not approve any month.
//
// A graduate's slice is therefore a flat one hour, set by the system (college
// decision, 07/09/2026). Undergrad courses keep their declared figure: the
// college has signed forms on both sides of this.

// twoHourLectureFixture builds an assignment whose lecture period runs longer
// than the attendance duty, at the given level.
func twoHourLectureFixture(t *testing.T, level string) *fixture {
	t.Helper()
	if level == "undergrad" {
		return newFixture(t, fixtureOpts{
			Workload: workloadHours{Attendance: 2, Lab: 3, CheckWork: 4, UGOther: 2},
		})
	}
	return newFixture(t, fixtureOpts{
		Level: level,
		// ช่วยสอน 3 ชม./สัปดาห์ = เช็คชื่อ 1 + ปฏิบัติการ 2, ตรวจงาน 2 — CP363205's
		// real ใบคำขอ once it was corrected against the signed timetable.
		Workload: workloadHours{HelpTeach: 3, Grade: 2, Other: 1, Prep: 2},
	})
}

// attendanceRows returns the generated เช็คชื่อ rows.
func attendanceRows(t *testing.T, f *fixture) []WorkLog {
	t.Helper()
	if _, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	rows, err := f.Svc.List(f.ctx, f.TAID, f.AssignmentID, false)
	if err != nil {
		t.Fatal(err)
	}
	var out []WorkLog
	for _, r := range rows {
		if r.Activity == "lecture" {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		t.Fatal("no เช็คชื่อ rows were generated at all")
	}
	return out
}

// THE REGRESSION, at the level that had it. A graduate's เช็คชื่อ was billed for
// the whole class period.
func TestAttendanceDuty_GraduateIsBilledOneHourNotTheWholePeriod(t *testing.T) {
	f := twoHourLectureFixture(t, "phd")
	for _, r := range attendanceRows(t, f) {
		if math.Abs(r.Hours-1) > 0.01 {
			t.Fatalf("เช็คชื่อ on %s billed %.1f hrs, want 1 — the class period is "+
				"longer than the duty, and billing the whole of it both overstates "+
				"the claim and breaks the weekly ceiling", r.WorkDate, r.Hours)
		}
	}
}

// An undergrad keeps its own declared length, and the flat rule must NOT reach
// it. The college has real courses on both sides — SC362004 bills one hour of a
// two-hour lecture, SC363101 declares and bills the whole period — so imposing
// one hour everywhere would underpay the second against a form somebody signed.
// See TestGenerate_FullAttendanceKeepsTheWholeLecture, which is that course.
func TestAttendanceDuty_UndergradKeepsItsDeclaredLength(t *testing.T) {
	f := twoHourLectureFixture(t, "undergrad") // declares attendance_hrs = 2
	for _, r := range attendanceRows(t, f) {
		if math.Abs(r.Hours-2) > 0.01 {
			t.Fatalf("เช็คชื่อ on %s billed %.1f hrs, want the declared 2 — the flat "+
				"graduate rule has leaked onto an undergrad course whose signed "+
				"form says otherwise", r.WorkDate, r.Hours)
		}
	}
}

// The billed hour sits at the END of the period, which is how the signed forms
// read it ("17.00 - 18.00" of a 17.00–19.00 class is the first hour; the trim
// keeps the row ending with the class).
func TestAttendanceDuty_TrimKeepsTheRowInsideItsPeriod(t *testing.T) {
	f := twoHourLectureFixture(t, "phd")
	for _, r := range attendanceRows(t, f) {
		sm, ok1 := parseHM(r.StartTime)
		em, ok2 := parseHM(r.EndTime)
		if !ok1 || !ok2 {
			t.Fatalf("unparseable window %s-%s", r.StartTime, r.EndTime)
		}
		if em-sm != 60 {
			t.Errorf("%s %s-%s spans %d minutes, want 60", r.WorkDate, r.StartTime, r.EndTime, em-sm)
		}
	}
}

// A period no longer than the duty is left alone rather than stretched.
func TestAttendanceDuty_ShorterPeriodIsNotPaddedOut(t *testing.T) {
	f := twoHourLectureFixture(t, "phd")
	// Squeeze the lecture to a single hour: the trim must not touch it.
	f.exec(`UPDATE section_schedules SET end_time = start_time + interval '1 hour'
	         WHERE section_id = $1 AND kind = 'lecture'`, f.SectionID)
	for _, r := range attendanceRows(t, f) {
		if math.Abs(r.Hours-1) > 0.01 {
			t.Errorf("a one-hour lecture produced a %.1f-hour เช็คชื่อ row on %s",
				r.Hours, r.WorkDate)
		}
	}
}
