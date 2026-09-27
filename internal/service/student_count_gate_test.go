package service

import (
	"strings"
	"testing"
)

// The export and worklog gates refuse a course with no students either way,
// but the message must say which case it is: "-" (staff never filled it in)
// versus an entered 0 (nobody enrolled).
func TestStudentCountGateTellsMissingFromZero(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true, NumStudents: 40})
	wl := &WorkLogService{pool: f.Pool}

	set := func(sql string) {
		t.Helper()
		if _, err := f.Pool.Exec(f.ctx, sql, f.CourseID); err != nil {
			t.Fatal(err)
		}
	}
	gate := func() string {
		t.Helper()
		err := wl.assertStudentCountFilled(f.ctx, f.CourseID)
		if err == nil {
			return ""
		}
		return err.Error()
	}

	if msg := gate(); msg != "" {
		t.Fatalf("40 students must pass the gate, got %q", msg)
	}

	set(`UPDATE teaching_courses SET num_students=0, num_students_regular=0, num_students_special=0,
	       num_students_regular_entered=FALSE, num_students_special_entered=FALSE WHERE id=$1`)
	if msg := gate(); !strings.Contains(msg, "ยังไม่ได้กรอกจำนวนนักศึกษา") {
		t.Fatalf("not-entered course: got %q, want the ยังไม่ได้กรอก message", msg)
	}

	set(`UPDATE teaching_courses SET num_students_regular_entered=TRUE WHERE id=$1`)
	if msg := gate(); !strings.Contains(msg, "ไม่มีนักศึกษาลงทะเบียน (0 คน)") {
		t.Fatalf("entered-0 course: got %q, want the 0 คน message", msg)
	}
}
