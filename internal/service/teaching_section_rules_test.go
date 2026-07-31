package service

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// teaching_section_rules_test.go covers the split of authority over a course's
// sections between staff and the lecturer who teaches it.
//
// The rule, in the lecturer's words: they cannot add a section; they cannot
// change a timetable that came from the registrar file; and where the file left
// the timetable blank they may fill it in exactly once, after which it belongs
// to staff. Route-level RBAC cannot express any of that — a lecturer legitimately
// holds the role that reaches these endpoints — so the whole rule lives in the
// service and this file is the only thing standing behind it.

func newTeachingSvc(t *testing.T, f *fixture) *TeachingService {
	t.Helper()
	return &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
}

// blankSection adds a second section with no schedule rows — the "WBA" case,
// where the registrar listed the group but not when it meets.
func blankSection(f *fixture, secNo string) uuid.UUID {
	id := uuid.New()
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track)
	        VALUES ($1, $2, $3, 'regular'::section_track)`, id, f.CourseID, secNo)
	return id
}

func oneLecture() []SectionSchedule {
	return []SectionSchedule{{Kind: "lecture", DayOfWeek: 2, StartTime: "13:00", EndTime: "16:00"}}
}

func TestLecturerFillsBlankTimetableOnce(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)
	secID := blankSection(f, "02")

	if err := svc.ReplaceSectionSchedules(f.ctx, f.LecturerID, f.CourseID, secID, oneLecture()); err != nil {
		t.Fatalf("lecturer's first write into a blank timetable must succeed, got %v", err)
	}

	var n int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM section_schedules WHERE section_id=$1`, secID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("schedule rows = %d, want 1", n)
	}

	// The second attempt is refused — and says which of the two locks it hit,
	// since "you already did this" and "the file already said this" send the
	// lecturer to different next steps.
	err := svc.ReplaceSectionSchedules(f.ctx, f.LecturerID, f.CourseID, secID, oneLecture())
	if err == nil {
		t.Fatal("second write must be refused")
	}
	if !strings.Contains(err.Error(), "กำหนดได้ครั้งเดียว") {
		t.Fatalf("refusal should name the one-shot rule, got %q", err)
	}
}

func TestLecturerCannotEditImportedTimetable(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)

	// f.SectionID already carries the fixture's imported Monday lecture+lab and
	// has never been touched by a lecturer.
	err := svc.ReplaceSectionSchedules(f.ctx, f.LecturerID, f.CourseID, f.SectionID, oneLecture())
	if err == nil {
		t.Fatal("lecturer must not rewrite an imported timetable")
	}
	if !strings.Contains(err.Error(), "มาจากไฟล์") {
		t.Fatalf("refusal should point at the imported file, got %q", err)
	}

	// Refused means unchanged, not partially applied: the delete and the
	// inserts share a transaction with the check.
	var n int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM section_schedules WHERE section_id=$1`, f.SectionID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("imported schedule rows = %d, want the original 2", n)
	}
}

func TestLecturerCannotSpendTheOneWriteOnNothing(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)
	secID := blankSection(f, "02")

	if err := svc.ReplaceSectionSchedules(f.ctx, f.LecturerID, f.CourseID, secID, nil); err == nil {
		t.Fatal("saving an empty timetable must be refused")
	}

	// The write was refused, so it was not spent: the real timetable still goes in.
	if err := svc.ReplaceSectionSchedules(f.ctx, f.LecturerID, f.CourseID, secID, oneLecture()); err != nil {
		t.Fatalf("the one write must survive a rejected empty save, got %v", err)
	}
}

func TestStaffRewriteTimetableFreely(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)
	staffID := f.insertUser("staff", "staff")

	for i := 0; i < 2; i++ {
		if err := svc.ReplaceSectionSchedules(f.ctx, staffID, f.CourseID, f.SectionID, oneLecture()); err != nil {
			t.Fatalf("staff write #%d must succeed, got %v", i+1, err)
		}
	}

	// Staff writes leave the lecturer's one-shot stamp alone — it records who
	// filled the blank, not who saved last. A staff edit must not hand the
	// lecturer a fresh write, nor consume one they never used.
	var setAt *string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT schedule_set_by_lecturer_at::text FROM sections WHERE id=$1`, f.SectionID).Scan(&setAt); err != nil {
		t.Fatal(err)
	}
	if setAt != nil {
		t.Fatalf("staff write must not stamp schedule_set_by_lecturer_at, got %q", *setAt)
	}
}

func TestSectionRosterIsStaffOnly(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)
	staffID := f.insertUser("staff", "staff")

	t.Run("lecturer cannot add", func(t *testing.T) {
		_, err := svc.AddSection(f.ctx, f.LecturerID, f.CourseID,
			AddSectionInput{SecNo: "99", Track: "regular"})
		if err == nil {
			t.Fatal("lecturer must not add a section")
		}
		if !strings.Contains(err.Error(), "เจ้าหน้าที่") {
			t.Fatalf("refusal should name staff as the owner, got %q", err)
		}
	})

	t.Run("lecturer cannot delete", func(t *testing.T) {
		if err := svc.DeleteSection(f.ctx, f.LecturerID, f.CourseID, f.SectionID); err == nil {
			t.Fatal("lecturer must not delete a section")
		}
		var alive bool
		if err := f.Pool.QueryRow(f.ctx,
			`SELECT EXISTS(SELECT 1 FROM sections WHERE id=$1)`, f.SectionID).Scan(&alive); err != nil {
			t.Fatal(err)
		}
		if !alive {
			t.Fatal("section was deleted despite the refusal")
		}
	})

	t.Run("lecturer cannot rename or restudent", func(t *testing.T) {
		newNo, n := "77", 5
		err := svc.UpdateSection(f.ctx, f.LecturerID, f.CourseID, f.SectionID,
			UpdateSectionInput{SecNo: &newNo, NumStudents: &n})
		if err == nil {
			t.Fatal("lecturer must not edit section identity or headcount")
		}
	})

	// The three calls the staff section-roster UI makes. Covered together
	// because a rule that only blocks lecturers is worthless if it also blocks
	// the people who are supposed to do the job.
	t.Run("staff can add, edit and delete", func(t *testing.T) {
		newID, err := svc.AddSection(f.ctx, staffID, f.CourseID,
			AddSectionInput{SecNo: "98", Track: "regular", NumStudents: 12})
		if err != nil {
			t.Fatalf("staff add must succeed, got %v", err)
		}

		renamed, n := "97", 34
		if err := svc.UpdateSection(f.ctx, staffID, f.CourseID, newID,
			UpdateSectionInput{SecNo: &renamed, NumStudents: &n}); err != nil {
			t.Fatalf("staff edit must succeed, got %v", err)
		}
		var gotNo string
		var gotN int
		if err := f.Pool.QueryRow(f.ctx,
			`SELECT sec_no, num_students FROM sections WHERE id=$1`, newID).Scan(&gotNo, &gotN); err != nil {
			t.Fatal(err)
		}
		if gotNo != renamed || gotN != n {
			t.Fatalf("section = (%q, %d), want (%q, %d)", gotNo, gotN, renamed, n)
		}

		if err := svc.DeleteSection(f.ctx, staffID, f.CourseID, newID); err != nil {
			t.Fatalf("staff delete must succeed, got %v", err)
		}
	})
}

// Opening a course is the roster rule taken to its root: a lecturer cannot add
// a section, so they cannot conjure a whole course full of them either.
func TestOpeningACourseIsStaffOnly(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)
	staffID := f.insertUser("staff", "staff")

	in := CreateTeachingCourseInput{
		TermID: f.TermID, Code: "CP999001", NameTH: "วิชาทดสอบเปิดเอง",
		Level: "undergrad", Credits: 3, LectureHrs: 3,
		LecturerIDs: []uuid.UUID{f.LecturerID},
	}

	_, err := svc.Create(f.ctx, f.LecturerID, in)
	if err == nil {
		t.Fatal("lecturer must not open a course")
	}
	if !strings.Contains(err.Error(), "เจ้าหน้าที่") {
		t.Fatalf("refusal should name staff as the owner, got %q", err)
	}
	var n int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM teaching_courses WHERE code=$1 AND term_id=$2`,
		in.Code, f.TermID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("refused create still wrote %d course rows", n)
	}

	if _, err := svc.Create(f.ctx, staffID, in); err != nil {
		t.Fatalf("staff create must succeed, got %v", err)
	}
}

// The course-level twins of the section roster rules: headcount and date
// range. Both feed the budget and the TA hour ceiling, so a lecturer nudging
// either would move money without staff ever seeing it.
func TestCourseHeadcountAndDatesAreStaffOnly(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true, NumStudents: 40})
	svc := newTeachingSvc(t, f)
	staffID := f.insertUser("staff", "staff")

	t.Run("lecturer cannot set headcount", func(t *testing.T) {
		if err := svc.SetNumStudents(f.ctx, f.LecturerID, f.CourseID, 999, 999, 0); err == nil {
			t.Fatal("lecturer must not set the headcount")
		}
		var n int
		if err := f.Pool.QueryRow(f.ctx,
			`SELECT num_students FROM teaching_courses WHERE id=$1`, f.CourseID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 40 {
			t.Fatalf("headcount = %d, want the original 40", n)
		}
	})

	t.Run("lecturer cannot move the date range", func(t *testing.T) {
		d := "2099-01-01"
		if err := svc.UpdateSettings(f.ctx, f.LecturerID, f.CourseID,
			UpdateSettingsInput{StartsOn: &d}); err == nil {
			t.Fatal("lecturer must not move the course date range")
		}
	})

	t.Run("staff can do both", func(t *testing.T) {
		if err := svc.SetNumStudents(f.ctx, staffID, f.CourseID, -1, 25, 5); err != nil {
			t.Fatalf("staff headcount edit must succeed, got %v", err)
		}
		var n int
		if err := f.Pool.QueryRow(f.ctx,
			`SELECT num_students FROM teaching_courses WHERE id=$1`, f.CourseID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 30 {
			t.Fatalf("headcount = %d, want 25+5=30", n)
		}

		d := "2099-01-01"
		if err := svc.UpdateSettings(f.ctx, staffID, f.CourseID,
			UpdateSettingsInput{StartsOn: &d}); err != nil {
			t.Fatalf("staff date edit must succeed, got %v", err)
		}
	})
}

// A lecturer who is not on the course gets the ownership refusal, not the
// one-shot one — the object-level check must run before the role-shaped rules,
// or a stranger could learn whether a section's timetable is blank.
func TestForeignLecturerRefusedOnOwnership(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)
	stranger := f.insertUser("lecturer", "stranger")
	secID := blankSection(f, "02")

	err := svc.ReplaceSectionSchedules(f.ctx, stranger, f.CourseID, secID, oneLecture())
	if err != ErrForbidden {
		t.Fatalf("want bare ErrForbidden for a non-owner, got %v", err)
	}
}
