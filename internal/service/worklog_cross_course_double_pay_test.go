package service

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// 01/10/2026 — double pay across two courses.
//
// Lecture overlap between two courses is allowed at REQUEST time on purpose, so
// a TA can hold two approved courses whose lectures run at the same hour.
// Generate never called enforceNoOverlap, so it wrote 09:00–12:00 in BOTH
// courses on the same days; submit + approve then paid 15 hours of work as 30
// (real repro: 600 ฿ owed, 1,200 ฿ approved). These tests pin each gate.

// secondCourseSameMondayLecture wires a second course whose section meets on
// Monday 09:00–12:00 — the same slot as the fixture's own lecture.
func secondCourseSameMondayLecture(f *fixture) uuid.UUID {
	other := f.secondCourseAssignment(fixtureOpts{})
	f.exec(`INSERT INTO section_schedules (section_id, kind, day_of_week, start_time, end_time)
	        SELECT section_id, 'lecture', 1, '09:00', '12:00'
	          FROM ta_request_assignments WHERE id = $1`, other)
	return other
}

func countOverlappingAcrossCourses(f *fixture, a, b uuid.UUID) int {
	f.t.Helper()
	var n int
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT COUNT(*) FROM work_logs x JOIN work_logs y
		  ON x.work_date = y.work_date AND x.start_time < y.end_time AND y.start_time < x.end_time
		 WHERE x.assignment_id = $1 AND y.assignment_id = $2`, a, b).Scan(&n); err != nil {
		f.t.Fatalf("count overlaps: %v", err)
	}
	return n
}

func TestGenerate_SkipsSlotsAnotherCourseAlreadyHolds(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	other := secondCourseSameMondayLecture(f)

	first, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID)
	if err != nil {
		t.Fatalf("generate first course: %v", err)
	}
	if len(first.Entries) == 0 {
		t.Fatal("precondition: the first course should generate its Monday lectures")
	}
	res, err := f.Svc.Generate(f.ctx, f.TAID, other)
	if err != nil {
		t.Fatalf("generate second course: %v", err)
	}
	if n := countOverlappingAcrossCourses(f, f.AssignmentID, other); n != 0 {
		t.Fatalf("the second course generated %d rows on hours the first already holds — that is double pay", n)
	}
	if len(res.SkippedOverlap) == 0 || res.SkippedOverlap[0].Count == 0 || len(res.SkippedOverlap[0].Dates) == 0 {
		t.Fatalf("the skipped sessions must be reported with their dates, got %+v", res.SkippedOverlap)
	}
	if !strings.Contains(res.SkippedOverlap[0].Reason, "09:00–12:00") {
		t.Errorf("reason should name the other course's time, got %q", res.SkippedOverlap[0].Reason)
	}
	if len(res.Entries) == 0 && res.EmptyReason == "" {
		t.Error("a run that creates nothing must say why")
	}
}

// Co-taught sibling sections are ONE sitting (rule B2) — Generate must keep
// writing both, exactly as before.
func TestGenerate_CotaughtSiblingStillGeneratesSameHours(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	sib := f.cotaughtSiblingAssignment("special")
	if _, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	res, err := f.Svc.Generate(f.ctx, f.TAID, sib)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SkippedOverlap) != 0 {
		t.Fatalf("co-taught sibling must not be treated as an overlap: %+v", res.SkippedOverlap)
	}
	if len(res.Entries) == 0 {
		t.Fatal("co-taught sibling should still get its rows")
	}
}

// A row that skipped the per-row gate (generated before this fix, or rejected
// while its twin was written) must be caught when it is submitted.
func TestSubmit_RefusesRowOverlappingAnotherCourse(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	other := f.secondCourseAssignment(fixtureOpts{})
	d := day(10)
	f.exec(`INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES ($1, $2::date, '09:00', '12:00', 3, 'review', 'approved')`, f.AssignmentID, d)
	// Rejected rows don't count as busy, so this is how a twin slips past Upsert.
	f.exec(`INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES ($1, $2::date, '10:00', '12:00', 2, 'review', 'rejected')`, other, d)

	err := f.Svc.Submit(f.ctx, f.TAID, other)
	if err == nil {
		t.Fatal("resubmitting a rejected row that overlaps an approved row of another course must be refused")
	}
	var code string
	_ = f.Pool.QueryRow(f.ctx, `SELECT code FROM teaching_courses WHERE id=$1`, f.CourseID).Scan(&code)
	if !strings.Contains(err.Error(), code) || !strings.Contains(err.Error(), "09:00–12:00") {
		t.Errorf("message should name the other course and its time, got: %v", err)
	}
}

func TestApprove_RefusesRowOverlappingApprovedHoursElsewhere(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	other := f.secondCourseAssignment(fixtureOpts{})
	d := day(10)
	f.exec(`INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES ($1, $2::date, '09:00', '12:00', 3, 'review', 'approved')`, f.AssignmentID, d)
	f.exec(`INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity, status, submitted_at)
	        VALUES ($1, $2::date, '09:00', '12:00', 3, 'review', 'submitted', NOW())`, other, d)

	err := f.Svc.Approve(f.ctx, f.LecturerID, other, "", false)
	if err == nil || !strings.Contains(err.Error(), "ซ้อน") {
		t.Fatalf("approving hours another course already pays for must be refused, got: %v", err)
	}
	err = f.Svc.ApproveMany(f.ctx, f.LecturerID, []uuid.UUID{other}, "", false)
	if err == nil {
		t.Fatal("ApproveMany must apply the same gate")
	}
}

// The database itself refuses two approved rows of one TA in two courses on the
// same minute, so no future path can bill it twice.
func TestTrigger_RefusesCrossCourseApprovedOverlap(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	other := f.secondCourseAssignment(fixtureOpts{})
	d := day(10)
	f.exec(`INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES ($1, $2::date, '09:00', '12:00', 3, 'review', 'approved')`, f.AssignmentID, d)
	_, err := f.Pool.Exec(f.ctx, `INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES ($1, $2::date, '11:00', '13:00', 2, 'review', 'approved')`, other, d)
	if err == nil || !strings.Contains(err.Error(), "ซ้อน") {
		t.Fatalf("trigger must refuse cross-course approved overlap, got: %v", err)
	}
	// Back-to-back is fine.
	if _, err := f.Pool.Exec(f.ctx, `INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES ($1, $2::date, '12:00', '13:00', 1, 'review', 'approved')`, other, d); err != nil {
		t.Fatalf("touching edges must be allowed: %v", err)
	}
}
