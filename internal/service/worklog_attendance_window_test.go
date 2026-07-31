package service

import (
	"strings"
	"testing"
	"time"
)

// 31/07/2026 — two generator rules read straight off the college's own signed
// claim forms (test_webapp_it/*.xlsx, TA ชนาธิป, SC362004):
//
//  1. เช็คชื่อ is the LAST attendance_hrs of the lecture. A 13.00–15.00 lecture
//     with a declared 1h attendance bills as "14.00 - 15.00 เช็คชื่อ" — never as
//     the whole period.
//  2. A makeup carries its own time window. The forms show a Tuesday class on a
//     daytime closure made up the SAME evening 20.00–21.00; Generate used to
//     drop the makeup's times on the floor and re-plant the original hour.

// The fixture section teaches Monday 09:00–12:00 (lecture) + 13:00–16:00 (lab).

func attendanceFixture(t *testing.T, attendanceHrs float64) *fixture {
	t.Helper()
	f := newFixture(t, fixtureOpts{})
	f.exec(`UPDATE ta_workload_forms
	           SET attendance_hrs = $1, lab_hrs = 3, check_work_hrs = 0, ug_other_hrs = 0
	         WHERE assignment_id = $2`, attendanceHrs, f.AssignmentID)
	return f
}

func TestGenerate_LectureDutyIsTheAttendanceWindow(t *testing.T) {
	f := attendanceFixture(t, 1)

	if _, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var start, end string
	var hours float64
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT start_time::text, end_time::text, hours FROM work_logs
		WHERE assignment_id=$1 AND activity='lecture' LIMIT 1`,
		f.AssignmentID).Scan(&start, &end, &hours); err != nil {
		t.Fatalf("no lecture row generated: %v", err)
	}
	if !strings.HasPrefix(start, "11:00") || !strings.HasPrefix(end, "12:00") || hours != 1 {
		t.Errorf("lecture duty = %s–%s %.1fh, want 11:00–12:00 1.0h — the last "+
			"declared hour of the 09:00–12:00 lecture, as the signed forms bill it",
			start, end, hours)
	}
}

// Declared attendance ≥ the period leaves the row alone (SC363101 declares the
// full 2h and its forms show the whole window).
func TestGenerate_FullAttendanceKeepsTheWholeLecture(t *testing.T) {
	f := attendanceFixture(t, 3)

	if _, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var start string
	var hours float64
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT start_time::text, hours FROM work_logs
		WHERE assignment_id=$1 AND activity='lecture' LIMIT 1`,
		f.AssignmentID).Scan(&start, &hours); err != nil {
		t.Fatalf("no lecture row generated: %v", err)
	}
	if !strings.HasPrefix(start, "09:00") || hours != 3 {
		t.Errorf("lecture duty = %s %.1fh, want 09:00 3.0h (declared covers the period)", start, hours)
	}
}

// A makeup's explicit window must become the row — date AND times.
func TestGenerate_MakeupWithExplicitTimeIsUsedVerbatim(t *testing.T) {
	f := attendanceFixture(t, 1)

	// Move one Monday lecture to the following Saturday evening.
	orig := nextWeekday(1)
	origDay, _ := time.Parse("2006-01-02", orig)
	sat := origDay.AddDate(0, 0, 5).Format("2006-01-02")
	f.exec(`INSERT INTO makeup_schedules (section_id, original_date, makeup_date, kind, start_time, end_time)
	        VALUES ($1, $2::date, $3::date, 'lecture', '20:00', '21:00')`,
		f.SectionID, orig, sat)

	if _, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var start, end, note string
	var hours float64
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT start_time::text, end_time::text, hours, COALESCE(note,'')
		FROM work_logs WHERE assignment_id=$1 AND work_date=$2::date AND activity='lecture'`,
		f.AssignmentID, sat).Scan(&start, &end, &hours, &note); err != nil {
		t.Fatalf("no row generated on the makeup date %s: %v", sat, err)
	}
	if !strings.HasPrefix(start, "20:00") || !strings.HasPrefix(end, "21:00") || hours != 1 {
		t.Errorf("makeup row = %s–%s %.1fh, want the filed 20:00–21:00 1.0h — "+
			"Generate must not re-plant the original hour on the new date", start, end, hours)
	}
	if !strings.Contains(note, "ชดเชย") {
		t.Errorf("note %q should carry (ชดเชย)", note)
	}
}

// The arrangement the college actually uses: a daytime closure, and the class
// made up the SAME DAY in the evening. The explicit window dodges the closure,
// so the row must survive — and still read as a makeup.
func TestGenerate_SameDayEveningMakeupSurvivesDaytimeClosure(t *testing.T) {
	f := attendanceFixture(t, 1)

	orig := nextWeekday(1)
	f.exec(`INSERT INTO public_holidays (holiday_date, name_th, source, start_time, end_time)
	        VALUES ($1::date, 'งานแฟร์ (ทดสอบ)', 'faculty', '08:00', '18:00')`, orig)
	f.exec(`INSERT INTO makeup_schedules (section_id, original_date, makeup_date, kind, start_time, end_time)
	        VALUES ($1, $2::date, $2::date, 'lecture', '20:00', '21:00')`,
		f.SectionID, orig)

	if _, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var start, note string
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT start_time::text, COALESCE(note,'') FROM work_logs
		WHERE assignment_id=$1 AND work_date=$2::date AND activity='lecture'`,
		f.AssignmentID, orig).Scan(&start, &note); err != nil {
		t.Fatalf("same-day evening makeup was dropped — the nested-closure check "+
			"must test the makeup's own window, not the original period's: %v", err)
	}
	if !strings.HasPrefix(start, "20:00") {
		t.Errorf("row starts %s, want 20:00", start)
	}
	if !strings.Contains(note, "ชดเชย") {
		t.Errorf("a same-day time-shifted class is still a makeup; note = %q", note)
	}
}
