package service

import (
	"testing"

	"github.com/google/uuid"
)

// A collision with the TA's own class is not all-or-nothing. เช็คชื่อ has to
// happen inside the lecture and สอนปฏิบัติการ inside the lab, but ตรวจงาน and
// อื่น ๆ are done outside the session — so a section whose lectures all clash
// is still perfectly workable for grading.
//
// The old rule dropped such a section outright, taking the off-slot duties with
// it. The real case that exposed it: a TA doing grading only, on a section
// whose one lab sat on a class of theirs.

// ownClassAt puts a class of the TA's own on the timetable, so any section
// session in that window collides.
func (f *capFixture) ownClassAt(day int, start, end string) {
	f.t.Helper()
	var term uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`SELECT term_id FROM teaching_courses WHERE id=$1`, f.tc).Scan(&term); err != nil {
		f.t.Fatalf("term lookup: %v", err)
	}
	f.exec(`INSERT INTO ta_class_schedules (id,user_id,term_id,day_of_week,start_time,end_time,is_wba)
	        VALUES (gen_random_uuid(),$1,$2,$3,$4::time,$5::time,FALSE)`,
		f.ta, term, day, start, end)
}

func (f *capFixture) clashOf(secID uuid.UUID) kindClash {
	f.t.Helper()
	k, err := sectionClashByKind(f.ctx, f.pool, f.ta, secID)
	if err != nil {
		f.t.Fatalf("sectionClashByKind: %v", err)
	}
	return k
}

// Lectures fully blocked, labs untouched: the two verdicts must not bleed.
func TestClashIsDecidedPerKind(t *testing.T) {
	f := newCapFixture(t)
	sec := f.addSection("1",
		[3]string{"lecture", "09:00", "12:00"},
		[3]string{"lab", "13:00", "16:00"})
	f.ownClassAt(1, "09:00", "12:00") // covers the lecture only

	got := f.clashOf(sec)
	if !got.fullyBlocked("lecture") {
		t.Fatalf("the lecture is fully covered by the TA's class, got %+v", got)
	}
	if got.fullyBlocked("lab") {
		t.Fatalf("the lab is free and must not be blocked, got %+v", got)
	}
}

// A fully-clashing section keeps the duties done outside the session. It used
// to be dropped whole, which took grading and other work down with the in-class
// duty — the case that exposed it was a TA doing grading only, on a section
// whose one lab sat on a class of theirs.
func TestOffSlotWorkKeepsAFullyClashingAssignment(t *testing.T) {
	f := newCapFixture(t)
	sec := f.addSection("1", [3]string{"lab", "13:00", "16:00"})
	f.ownClassAt(1, "13:00", "16:00")

	asg := f.approveAssignment(sec)
	// Grading declared: the assignment survives even though no session is workable.
	f.declareWorkload(asg, 2, 0)
	if got, err := offSlotHours(f.ctx, f.pool, asg); err != nil || got != 2 {
		t.Fatalf("off-slot hours: got %.2f err %v", got, err)
	}

	// Nothing off-slot declared: there is genuinely no work left, so dropping
	// is right.
	other := f.addSection("2", [3]string{"lab", "13:00", "16:00"})
	asg2 := f.approveAssignment(other)
	f.declareWorkload(asg2, 0, 2) // lab teaching only
	if got, err := offSlotHours(f.ctx, f.pool, asg2); err != nil || got != 0 {
		t.Fatalf("a lab-teaching-only assignment has no off-slot work: got %.2f err %v", got, err)
	}
}

// The agreed rule: a partial collision costs nothing. Losing one lecture of two
// must not cost the duty — the TA still covers the other.
func TestPartialClashDoesNotBlockTheDuty(t *testing.T) {
	f := newCapFixture(t)
	sec := f.addSection("1",
		[3]string{"lecture", "09:00", "10:00"},
		[3]string{"lecture", "13:00", "14:00"})
	f.ownClassAt(1, "09:00", "10:00") // covers one of the two lectures

	got := f.clashOf(sec)
	c := got["lecture"]
	if c.Total != 2 || c.Clashing != 1 {
		t.Fatalf("want 1 of 2 lectures clashing, got %+v", c)
	}
	if got.fullyBlocked("lecture") {
		t.Fatal("a partial clash must not block the duty")
	}
	if err := validateUndergradSectionCaps(ugWorkload(0, 2, 0, 0, 0), "ผู้ช่วย", "1", f.weekly(sec)); err != nil {
		t.Fatalf("เช็คชื่อ must still be declarable on a partial clash: %v", err)
	}
}

// A kind with no sessions is absent, not blocked — otherwise a lecture-only
// course would report its (nonexistent) labs as fully clashing.
func TestAbsentKindIsNotReportedAsBlocked(t *testing.T) {
	f := newCapFixture(t)
	sec := f.addSection("1", [3]string{"lecture", "09:00", "12:00"})
	f.ownClassAt(1, "09:00", "12:00")

	got := f.clashOf(sec)
	if got.fullyBlocked("lab") {
		t.Fatal("a section with no labs must not report the lab kind as blocked")
	}
	if !got.fullyBlocked("lecture") {
		t.Fatal("the lecture is covered and should be blocked")
	}
}

// approveAssignment wires one approved assignment for the fixture's TA.
func (f *capFixture) approveAssignment(secID uuid.UUID) uuid.UUID {
	f.t.Helper()
	req, asg := uuid.New(), uuid.New()
	f.exec(`INSERT INTO ta_requests (id,teaching_course_id,lecturer_id,reimburse_scope,status,submitted_at)
	        VALUES ($1,$2,$3,'both','approved',NOW())`, req, f.tc, f.lec)
	f.exec(`INSERT INTO ta_request_assignments (id,request_id,section_id,ta_id,level)
	        VALUES ($1,$2,$3,$4,'undergrad')`, asg, req, secID, f.ta)
	return asg
}

// declareWorkload attaches a workload form: grading hours (off-slot) and lab
// teaching hours (in-slot).
func (f *capFixture) declareWorkload(asg uuid.UUID, checkWork, lab float64) {
	f.t.Helper()
	f.exec(`INSERT INTO ta_workload_forms (assignment_id, check_work_hrs, lab_hrs)
	        VALUES ($1,$2,$3)`, asg, checkWork, lab)
}
