package service

import "testing"

// The 24/07/2026 meeting gave lecturers the ability to correct their own
// course's work logs ("อาจารย์ตรวจสอบ ตีกลับ อนุมัติ หรือ แก้ไขได้").
//
// StaffUpsert previously had exactly one caller behind an admin/staff route
// guard, so it never checked the actor itself. Adding a second, less
// privileged caller makes that guard insufficient: without an ownership rule
// inside the service, any lecturer could rewrite any course's hours.

func TestStaffUpsert_LecturerCanEditOwnCourse(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	id := f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))

	edited := f.entry(day(10), "09:00", "10:00", 1)
	edited.ID = id
	// f.LecturerID teaches this course (see insertCourse).
	if _, err := f.Svc.StaffUpsert(f.ctx, f.LecturerID, false, edited); err != nil {
		t.Fatalf("the course's own lecturer must be able to correct a row: %v", err)
	}

	var hours float64
	if err := f.Pool.QueryRow(f.ctx, `SELECT hours FROM work_logs WHERE id=$1`, id).Scan(&hours); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if hours != 1 {
		t.Errorf("hours = %v, want the edited 1", hours)
	}
}

func TestStaffUpsert_LecturerCannotEditAnotherCourse(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	id := f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))

	// A lecturer who teaches nothing here.
	outsider := f.insertUser("lecturer", "outsider")

	edited := f.entry(day(10), "09:00", "10:00", 1)
	edited.ID = id
	_, err := f.Svc.StaffUpsert(f.ctx, outsider, false, edited)
	if err != ErrForbidden {
		t.Fatalf("a lecturer from another course must be refused, got: %v", err)
	}

	var hours float64
	if err := f.Pool.QueryRow(f.ctx, `SELECT hours FROM work_logs WHERE id=$1`, id).Scan(&hours); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if hours != 2 {
		t.Errorf("hours = %v — the refused edit must not have landed", hours)
	}
}

// Staff keep blanket access; the ownership rule applies only to the
// unprivileged caller.
func TestStaffUpsert_StaffStillEditsAnyCourse(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	id := f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	staff := f.insertUser("staff", "officer")

	edited := f.entry(day(10), "09:00", "10:00", 1)
	edited.ID = id
	if _, err := f.Svc.StaffUpsert(f.ctx, staff, true, edited); err != nil {
		t.Fatalf("staff must retain access to every course: %v", err)
	}
}

func TestStaffDelete_LecturerOwnershipEnforced(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	id := f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	outsider := f.insertUser("lecturer", "outsider")

	if err := f.Svc.StaffDelete(f.ctx, outsider, false, id); err != ErrForbidden {
		t.Fatalf("delete from another lecturer must be refused, got: %v", err)
	}
	if n := f.countLogs(); n != 1 {
		t.Fatalf("row must survive a refused delete, found %d", n)
	}

	if err := f.Svc.StaffDelete(f.ctx, f.LecturerID, false, id); err != nil {
		t.Fatalf("the course's own lecturer must be able to delete: %v", err)
	}
	if n := f.countLogs(); n != 0 {
		t.Errorf("row should be gone, found %d", n)
	}
}
