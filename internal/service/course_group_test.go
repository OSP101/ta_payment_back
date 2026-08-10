package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

// Modelled on the real case the migration 0073 comment cites: CP353301 (CS)
// and SC313302 (IT) are both "Internetworking", both meet Thu 15:00-19:00 in
// SC9524 for their regular section, but their special sections run at
// different hours. The college's own file still merges the course.
func cgInsertCourse(t *testing.T, pool *pgxpool.Pool, term uuid.UUID, code, nameTH string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO teaching_courses (id, term_id, code, name_th, level, credits, lecture_hrs, lab_hrs, num_students)
		VALUES ($1,$2,$3,$4,'undergrad',3,2,2,0)`,
		id, term, code, nameTH); err != nil {
		t.Fatalf("insert course %s: %v", code, err)
	}
	return id
}

func cgInsertSection(t *testing.T, pool *pgxpool.Pool, tcID uuid.UUID, secNo, track, curriculum string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO sections (id, teaching_course_id, sec_no, track, curriculum)
		VALUES ($1,$2,$3,$4::section_track,$5)`,
		id, tcID, secNo, track, curriculum); err != nil {
		t.Fatalf("insert section %s: %v", secNo, err)
	}
	return id
}

func cgInsertSchedule(t *testing.T, pool *pgxpool.Pool, secID uuid.UUID, kind string, dow int, start, end, room string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO section_schedules (id, section_id, kind, day_of_week, start_time, end_time, room)
		VALUES (gen_random_uuid(),$1,$2,$3,$4,$5,$6)`,
		secID, kind, dow, start, end, room); err != nil {
		t.Fatalf("insert schedule: %v", err)
	}
}

// cgActor returns a real users.id — confirmed_by is a foreign key, so tests
// exercising ConfirmCourseGroup can't pass a bare uuid.New().
func cgActor(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, first_name, last_name, is_active) VALUES ($1,$2,'จนท','ทดสอบ',TRUE)`,
		id, "cg-staff-"+id.String()+"@example.test"); err != nil {
		t.Fatalf("insert actor: %v", err)
	}
	return id
}

func newCourseGroupFixture(t *testing.T) (svc *TeachingService, ctx context.Context, pool *pgxpool.Pool, term uuid.UUID) {
	t.Helper()
	pool = testutil.NewPool(t)
	svc = &TeachingService{pool: pool, aud: audit.New(pool)}
	ctx = context.Background()
	term = uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO academic_terms (id, academic_year, semester) VALUES ($1, 2569, 1)`, term); err != nil {
		t.Fatalf("insert term: %v", err)
	}
	return svc, ctx, pool, term
}

func TestDetectCourseGroups_MatchesOnSharedNameAndSchedule(t *testing.T) {
	svc, ctx, pool, term := newCourseGroupFixture(t)

	cs := cgInsertCourse(t, pool, term, "CP353301", "INTERNETWORKING")
	csReg := cgInsertSection(t, pool, cs, "1", "regular", "CS")
	cgInsertSchedule(t, pool, csReg, "lecture", 4, "15:00", "17:00", "SC9524")
	cgInsertSchedule(t, pool, csReg, "lab", 4, "17:00", "19:00", "SC9524")
	csSpecial := cgInsertSection(t, pool, cs, "2", "special", "CS")
	cgInsertSchedule(t, pool, csSpecial, "lecture", 4, "08:30", "10:30", "SC9524")
	cgInsertSchedule(t, pool, csSpecial, "lab", 4, "10:30", "12:30", "SC9524")

	it := cgInsertCourse(t, pool, term, "SC313302", "INTERNETWORKING")
	itReg := cgInsertSection(t, pool, it, "1", "regular", "IT")
	cgInsertSchedule(t, pool, itReg, "lecture", 4, "15:00", "17:00", "SC9524")
	cgInsertSchedule(t, pool, itReg, "lab", 4, "17:00", "19:00", "SC9524")
	itSpecial := cgInsertSection(t, pool, it, "2", "special", "IT")
	// Special section runs at a DIFFERENT hour than CP353301's special section
	// — only the regular sections coincide, same as the real data.
	cgInsertSchedule(t, pool, itSpecial, "lecture", 4, "15:00", "17:00", "SC9524")
	cgInsertSchedule(t, pool, itSpecial, "lab", 4, "17:00", "19:00", "SC9524")

	candidates, err := svc.DetectCourseGroups(ctx, term)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(candidates), candidates)
	}
	c := candidates[0]
	if len(c.Courses) != 2 {
		t.Fatalf("group has %d courses, want 2: %+v", len(c.Courses), c.Courses)
	}
	// Sorted by code — CP353301 before SC313302.
	if c.Courses[0].Code != "CP353301" || c.Courses[1].Code != "SC313302" {
		t.Errorf("courses = %v, want [CP353301, SC313302]", c.Courses)
	}
	if c.SuggestedPrimaryID != c.Courses[0].ID {
		t.Errorf("suggested primary should default to the first (code-sorted) course")
	}
}

// A course with the same name but NO coinciding schedule (a genuine
// coincidence of name only, e.g. two unrelated "Seminar" courses in different
// rooms) must never be proposed — merging by name alone would be exactly the
// silent wrong-merge the whole review step exists to prevent.
func TestDetectCourseGroups_SameNameDifferentScheduleNotProposed(t *testing.T) {
	svc, ctx, pool, term := newCourseGroupFixture(t)

	a := cgInsertCourse(t, pool, term, "CP111111", "SEMINAR")
	secA := cgInsertSection(t, pool, a, "1", "regular", "CS")
	cgInsertSchedule(t, pool, secA, "lecture", 1, "09:00", "12:00", "A101")

	b := cgInsertCourse(t, pool, term, "SC222222", "SEMINAR")
	secB := cgInsertSection(t, pool, b, "1", "regular", "IT")
	cgInsertSchedule(t, pool, secB, "lecture", 2, "13:00", "16:00", "B202")

	candidates, err := svc.DetectCourseGroups(ctx, term)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Errorf("got %d candidates, want 0: %+v", len(candidates), candidates)
	}
}

// Same room/day/time but a DIFFERENT course name (two unrelated classes that
// happen to share a slot) must not merge on the schedule coincidence alone.
func TestDetectCourseGroups_SameScheduleDifferentNameNotProposed(t *testing.T) {
	svc, ctx, pool, term := newCourseGroupFixture(t)

	a := cgInsertCourse(t, pool, term, "CP111111", "DATA STRUCTURE")
	secA := cgInsertSection(t, pool, a, "1", "regular", "CS")
	cgInsertSchedule(t, pool, secA, "lecture", 3, "13:00", "15:00", "SC9524")

	b := cgInsertCourse(t, pool, term, "SC222222", "OPERATING SYSTEMS")
	secB := cgInsertSection(t, pool, b, "1", "regular", "IT")
	cgInsertSchedule(t, pool, secB, "lecture", 3, "13:00", "15:00", "SC9524")

	candidates, err := svc.DetectCourseGroups(ctx, term)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Errorf("got %d candidates, want 0: %+v", len(candidates), candidates)
	}
}

// A course already confirmed into a group must not be re-proposed alongside
// a third code that happens to match it too.
func TestDetectCourseGroups_ExcludesAlreadyGroupedCourses(t *testing.T) {
	svc, ctx, pool, term := newCourseGroupFixture(t)

	a := cgInsertCourse(t, pool, term, "CP111111", "INTERNETWORKING")
	secA := cgInsertSection(t, pool, a, "1", "regular", "CS")
	cgInsertSchedule(t, pool, secA, "lecture", 4, "15:00", "17:00", "SC9524")

	b := cgInsertCourse(t, pool, term, "SC222222", "INTERNETWORKING")
	secB := cgInsertSection(t, pool, b, "1", "regular", "IT")
	cgInsertSchedule(t, pool, secB, "lecture", 4, "15:00", "17:00", "SC9524")

	groupID, err := svc.ConfirmCourseGroup(ctx, cgActor(t, pool), term, a, []uuid.UUID{a, b}, "CS")
	if err != nil {
		t.Fatalf("ConfirmCourseGroup: %v", err)
	}
	if groupID == uuid.Nil {
		t.Fatal("expected a group id")
	}

	candidates, err := svc.DetectCourseGroups(ctx, term)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Errorf("got %d candidates after confirming, want 0 (both already grouped): %+v", len(candidates), candidates)
	}
}

func TestConfirmCourseGroup_RejectsPrimaryNotInGroup(t *testing.T) {
	svc, ctx, pool, term := newCourseGroupFixture(t)
	a := cgInsertCourse(t, pool, term, "CP111111", "X")
	b := cgInsertCourse(t, pool, term, "SC222222", "X")
	// A real, unrelated course — chosen specifically so the foreign key on
	// primary_course_id is satisfied and cannot incidentally do this check's
	// job. Only the explicit membership check catches this case.
	outsider := cgInsertCourse(t, pool, term, "CP999999", "UNRELATED")

	if _, err := svc.ConfirmCourseGroup(ctx, cgActor(t, pool), term, outsider, []uuid.UUID{a, b}, "CS"); err == nil {
		t.Error("expected an error when primary is not one of the merged courses")
	}
}

func TestConfirmCourseGroup_RejectsDoubleMembership(t *testing.T) {
	svc, ctx, pool, term := newCourseGroupFixture(t)
	a := cgInsertCourse(t, pool, term, "CP111111", "X")
	b := cgInsertCourse(t, pool, term, "SC222222", "X")
	c := cgInsertCourse(t, pool, term, "CP333333", "Y")

	if _, err := svc.ConfirmCourseGroup(ctx, cgActor(t, pool), term, a, []uuid.UUID{a, b}, "CS"); err != nil {
		t.Fatalf("first group: %v", err)
	}
	// b is already a member of the first group — merging it into a second
	// group would let its money print (and be counted) twice.
	if _, err := svc.ConfirmCourseGroup(ctx, cgActor(t, pool), term, b, []uuid.UUID{b, c}, "CS"); err == nil {
		t.Error("expected an error: b is already a member of another group")
	}
}
