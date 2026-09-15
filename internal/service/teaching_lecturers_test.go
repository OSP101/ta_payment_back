package service

import (
	"testing"

	"github.com/google/uuid"
)

// ReplaceLecturers closes the gap found during the 2026-09-14 TOR acceptance
// review (TOR §3.2 ข.3): teaching_lecturers was insert-only — set at Create or
// CommitImport, with no way to correct it afterwards. That was a dead end
// whenever an import left a course unmatched, a lecturer changed mid-term, or
// the wrong name got picked, because a course with any TA/request/export can
// no longer be deleted and reopened (see TestDelete_RefusesOnceCourseHasTA and
// friends). These tests exercise the fix, not the old symptom.
//
// newTeachingSvc lives in teaching_section_rules_test.go.

func TestReplaceLecturers_LecturerCannotCall(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)

	other := f.insertUser("lecturer", "other")
	err := svc.ReplaceLecturers(f.ctx, f.LecturerID, f.CourseID, []uuid.UUID{other}, other)
	if err != ErrForbidden {
		t.Fatalf("lecturer actor: got %v, want ErrForbidden", err)
	}
}

func TestReplaceLecturers_TACannotCall(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)

	other := f.insertUser("lecturer", "other")
	err := svc.ReplaceLecturers(f.ctx, f.TAID, f.CourseID, []uuid.UUID{other}, other)
	if err != ErrForbidden {
		t.Fatalf("ta actor: got %v, want ErrForbidden", err)
	}
}

func TestReplaceLecturers_RequiresAtLeastOne(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)

	err := svc.ReplaceLecturers(f.ctx, f.StaffID, f.CourseID, nil, uuid.Nil)
	if err == nil {
		t.Fatal("empty list: expected an error")
	}
}

func TestReplaceLecturers_RequiresPrimaryInList(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)

	a := f.insertUser("lecturer", "a")
	notInList := f.insertUser("lecturer", "b")
	err := svc.ReplaceLecturers(f.ctx, f.StaffID, f.CourseID, []uuid.UUID{a}, notInList)
	if err == nil {
		t.Fatal("primary outside list: expected an error")
	}
}

func TestReplaceLecturers_RejectsNonLecturerRole(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)

	notALecturer := f.insertUser("ta", "impostor")
	err := svc.ReplaceLecturers(f.ctx, f.StaffID, f.CourseID, []uuid.UUID{notALecturer}, notALecturer)
	if err == nil {
		t.Fatal("ta id in lecturer list: expected an error")
	}
}

func TestReplaceLecturers_RejectsInactiveAccount(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)

	deactivated := f.insertUser("lecturer", "gone")
	f.exec(`UPDATE users SET is_active = FALSE WHERE id = $1`, deactivated)
	err := svc.ReplaceLecturers(f.ctx, f.StaffID, f.CourseID, []uuid.UUID{deactivated}, deactivated)
	if err == nil {
		t.Fatal("deactivated lecturer: expected an error")
	}
}

func TestReplaceLecturers_SwapsOwnership(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)

	newLecturer := f.insertUser("lecturer", "new")
	if err := svc.ReplaceLecturers(f.ctx, f.StaffID, f.CourseID, []uuid.UUID{newLecturer}, newLecturer); err != nil {
		t.Fatalf("replace: %v", err)
	}

	// The original lecturer (fixture's default) must no longer own the course...
	if owns, err := lecturerOwnsCourse(f.ctx, f.Pool, f.LecturerID, f.CourseID); err != nil {
		t.Fatal(err)
	} else if owns {
		t.Error("original lecturer still owns the course after being replaced")
	}
	// ...and the new one must.
	if owns, err := lecturerOwnsCourse(f.ctx, f.Pool, newLecturer, f.CourseID); err != nil {
		t.Fatal(err)
	} else if !owns {
		t.Error("new lecturer does not own the course after ReplaceLecturers")
	}

	tc, err := svc.Get(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tc.Lecturers) != 1 || tc.Lecturers[0].ID != newLecturer || !tc.Lecturers[0].IsPrimary {
		t.Errorf("Get().Lecturers = %+v, want exactly [newLecturer, primary]", tc.Lecturers)
	}
}

// This is the exact scenario found in the §3.3 import probe: 124 of 127
// courses came in "unmatched_officer" because the registrar file's officer
// name did not resolve to any account, so the course was created with zero
// rows in teaching_lecturers. Before this fix, staff had no way to attach the
// real lecturer afterwards.
func TestReplaceLecturers_UnmatchedImportCourseBecomesVisible(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)

	// Simulate an import that created the course with no lecturer attached.
	f.exec(`DELETE FROM teaching_lecturers WHERE teaching_course_id = $1`, f.CourseID)
	if owns, err := lecturerOwnsCourse(f.ctx, f.Pool, f.LecturerID, f.CourseID); err != nil {
		t.Fatal(err)
	} else if owns {
		t.Fatal("setup: course should have no lecturer yet")
	}

	if err := svc.ReplaceLecturers(f.ctx, f.StaffID, f.CourseID, []uuid.UUID{f.LecturerID}, f.LecturerID); err != nil {
		t.Fatalf("attach lecturer to unmatched course: %v", err)
	}
	if owns, err := lecturerOwnsCourse(f.ctx, f.Pool, f.LecturerID, f.CourseID); err != nil {
		t.Fatal(err)
	} else if !owns {
		t.Error("lecturer still cannot see the course after being attached")
	}
}

func TestReplaceLecturers_AuditCarriesBeforeAndAfter(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)

	newLecturer := f.insertUser("lecturer", "new")
	if err := svc.ReplaceLecturers(f.ctx, f.StaffID, f.CourseID, []uuid.UUID{newLecturer}, newLecturer); err != nil {
		t.Fatalf("replace: %v", err)
	}

	var beforeText, afterText string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT before::text, after::text FROM audit_logs
		  WHERE action = 'teaching_course.lecturers.replace' AND entity_id = $1
		  ORDER BY at DESC LIMIT 1`, f.CourseID.String()).Scan(&beforeText, &afterText); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if beforeText == "" || beforeText == "null" {
		t.Error("audit Before is empty — cannot tell who the course was attributed to previously")
	}
	if afterText == "" || afterText == "null" {
		t.Error("audit After is empty")
	}
}

func TestReplaceLecturers_MultipleWithExplicitPrimary(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)

	a := f.insertUser("lecturer", "a")
	b := f.insertUser("lecturer", "b")
	if err := svc.ReplaceLecturers(f.ctx, f.StaffID, f.CourseID, []uuid.UUID{a, b}, b); err != nil {
		t.Fatalf("replace: %v", err)
	}
	tc, err := svc.Get(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tc.Lecturers) != 2 {
		t.Fatalf("got %d lecturers, want 2", len(tc.Lecturers))
	}
	var primaryCount int
	for _, l := range tc.Lecturers {
		if l.IsPrimary {
			primaryCount++
			if l.ID != b {
				t.Errorf("primary is %s, want b (%s)", l.ID, b)
			}
		}
	}
	if primaryCount != 1 {
		t.Errorf("got %d primary lecturers, want exactly 1", primaryCount)
	}
}

func TestReplaceLecturers_RejectsDuplicateID(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)

	a := f.insertUser("lecturer", "a")
	err := svc.ReplaceLecturers(f.ctx, f.StaffID, f.CourseID, []uuid.UUID{a, a}, a)
	if err == nil {
		t.Fatal("duplicate id in list: expected an error")
	}
}
