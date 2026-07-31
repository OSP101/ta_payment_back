package service

import (
	"strings"
	"testing"
)

// 31/07/2026: a LECTURE period may overlap the TA's own class; a LAB may not.
//
// Before this, any overlap ruled the TA out of the course. The reason to split
// them is physical: a lab has the TA in a room running it, a lecture duty
// (attendance, handing out sheets) does not, so a TA with a class at that hour
// can still hold the job.
//
// The rule has to hold at three gates or it produces a worse state than before:
// the request gate (can this TA be asked), the entry gate (can they log the
// hours), and the generator (does it produce the rows). Letting a TA be REQUESTED
// for a lecture the system then refuses to record would be the deadlock the split
// exists to remove.

// clashFixture puts the TA's own class exactly on top of one of the section's
// periods, and makes that period the given kind.
func clashFixture(t *testing.T, kind string) *fixture {
	t.Helper()
	f := newFixture(t, fixtureOpts{NoOwnClassSchedule: true})
	// The fixture section teaches Monday 09:00–12:00 lecture and 13:00–16:00 lab.
	slot := map[string][2]string{
		"lecture": {"09:00", "12:00"},
		"lab":     {"13:00", "16:00"},
	}[kind]
	f.exec(`INSERT INTO ta_class_schedules
	          (id, user_id, term_id, course_code, sec_no, kind, day_of_week, start_time, end_time)
	        VALUES (gen_random_uuid(), $1, $2, 'OWN101', '1', 'lecture', 1, $3::time, $4::time)`,
		f.TAID, f.TermID, slot[0], slot[1])
	return f
}

func requestSvc(f *fixture) *TARequestService {
	return &TARequestService{pool: f.Pool}
}

// A lecture that overlaps the TA's class no longer blocks the request.
func TestRequestGate_LectureOverlapIsAllowed(t *testing.T) {
	f := clashFixture(t, "lecture")

	err := requestSvc(f).checkOwnClassConflict(f.ctx, f.Pool, f.TAID, f.SectionID, "ทีเอ ทดสอบ")
	if err != nil {
		t.Errorf("a lecture overlapping the TA's own class must no longer rule them out: %v", err)
	}
}

// A lab that overlaps still does.
func TestRequestGate_LabOverlapIsStillBlocked(t *testing.T) {
	f := clashFixture(t, "lab")

	err := requestSvc(f).checkOwnClassConflict(f.ctx, f.Pool, f.TAID, f.SectionID, "ทีเอ ทดสอบ")
	if err == nil {
		t.Fatal("a lab overlapping the TA's own class must still block the request")
	}
	if !strings.Contains(err.Error(), "ปฏิบัติการ") {
		t.Errorf("the refusal should name the lab period, got: %v", err)
	}
}

// The entry gate must agree with the request gate. If it did not, a TA could be
// assigned to a lecture and then be unable to record a single hour of it.
func TestEntryGate_LectureHoursCanBeLoggedOverOwnClass(t *testing.T) {
	f := clashFixture(t, "lecture")

	// Monday, inside the overlapping 09:00–12:00 window.
	w := f.entry(nextWeekday(1), "09:00", "11:00", 2)
	w.Activity = "lecture"
	if _, err := f.upsert(w); err != nil {
		t.Errorf("lecture hours overlapping the TA's own class must be loggable — "+
			"otherwise they are assigned work the system refuses to record: %v", err)
	}
}

func TestEntryGate_LabHoursOverOwnClassAreStillRefused(t *testing.T) {
	f := clashFixture(t, "lab")

	w := f.entry(nextWeekday(1), "13:00", "15:00", 2)
	w.Activity = "lab"
	if _, err := f.upsert(w); err == nil {
		t.Fatal("lab hours overlapping the TA's own class must still be refused")
	}
}

// Grading keeps its old behaviour: the change asked for was about lecture
// periods, and quietly widening it would authorise hours nobody decided on.
func TestEntryGate_ReviewHoursOverOwnClassStayBlocked(t *testing.T) {
	f := clashFixture(t, "lecture")

	w := f.entry(nextWeekday(1), "09:00", "11:00", 2)
	w.Activity = "review"
	if _, err := f.upsert(w); err == nil {
		t.Error("review hours over the TA's own class should keep their previous behaviour")
	}
}

// The generator must produce the lecture occurrences it used to skip, or the TA
// is allowed to work hours the system never offers them.
func TestGenerate_NoLongerSkipsClashingLectures(t *testing.T) {
	f := clashFixture(t, "lecture")

	res, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var lectures int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM work_logs WHERE assignment_id=$1 AND activity='lecture'`,
		f.AssignmentID).Scan(&lectures); err != nil {
		t.Fatal(err)
	}
	if lectures == 0 {
		t.Errorf("no lecture rows generated — the generator is still skipping periods the "+
			"request gate now allows (skipped: %+v)", res.SkippedOwnClass)
	}
}
