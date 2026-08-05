package service

import (
	"strings"
	"testing"
)

// A period of ANY kind may not overlap the TA's own class. A TA sitting in their
// own lecture is not available to do anything else that hour.
//
// This rule has been reversed once and is worth stating carefully, because both
// positions were reasonable and the second one is easy to mistake for a bug:
//
//	31/07/2026 — LECTURE was exempted. The argument was physical: a lab has the
//	             TA in a room running it, while a lecture duty (attendance,
//	             handing out sheets) does not, so a TA with a class at that hour
//	             could still hold the job. Blocking it had been ruling TAs out of
//	             whole courses — จิรายุ lost two of his three.
//	05/08/2026 — Un-exempted by the college. A TA whose own class covers every
//	             lecture of a group does not get that group's เช็คชื่อ duty.
//
// The rule holds at three gates and must hold at all of them together: the
// request gate (can this TA be asked), the entry gate (can they log the hours),
// and the generator (does it produce the rows). They read one predicate —
// clashBlockingKind / BlockingSessionSQL — precisely so they cannot drift apart
// again. They did drift once: the 31/07 exemption was applied to the request
// gate alone, so a passing test asserted a rule that was dead in production.
//
// What the reversal does NOT do is refuse the submission. A fully-clashing
// section is still written and then trimmed — see TestCreate_TrimsLectureOnlyClash
// — because refusing would make the verdict depend on whether the TA filed their
// timetable before or after the lecturer pressed Send.

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

// A lecture that overlaps the TA's class blocks the request gate again.
func TestRequestGate_LectureOverlapIsBlocked(t *testing.T) {
	f := clashFixture(t, "lecture")

	err := requestSvc(f).checkOwnClassConflict(f.ctx, f.Pool, f.TAID, f.SectionID, "ทีเอ ทดสอบ")
	if err == nil {
		t.Fatal("a lecture overlapping the TA's own class must be reported as a conflict")
	}
	if !strings.Contains(err.Error(), "ทับซ้อน") {
		t.Errorf("the refusal should say what overlaps, got: %v", err)
	}
}

// A lab that overlaps always did.
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

// The entry gate must agree with the request gate — one predicate, one answer.
func TestEntryGate_LectureHoursOverOwnClassAreRefused(t *testing.T) {
	f := clashFixture(t, "lecture")

	// Monday, inside the overlapping 09:00–12:00 window.
	w := f.entry(nextWeekday(1), "09:00", "11:00", 2)
	w.Activity = "lecture"
	if _, err := f.upsert(w); err == nil {
		t.Fatal("lecture hours overlapping the TA's own class must be refused")
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

// Grading never had the exemption and still does not.
func TestEntryGate_ReviewHoursOverOwnClassStayBlocked(t *testing.T) {
	f := clashFixture(t, "lecture")

	w := f.entry(nextWeekday(1), "09:00", "11:00", 2)
	w.Activity = "review"
	if _, err := f.upsert(w); err == nil {
		t.Error("review hours over the TA's own class must stay blocked")
	}
}

// The generator must skip what the entry gate would refuse, or it plants rows the
// TA can neither edit nor submit — and it must SAY it skipped them, so the TA can
// see the hours were not silently lost.
func TestGenerate_SkipsClashingLectures(t *testing.T) {
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
	if lectures != 0 {
		t.Errorf("the generator planted %d lecture rows the entry gate would refuse", lectures)
	}
	if len(res.SkippedOwnClass) == 0 {
		t.Error("the skip must be reported — silently dropping the hours is what the TA cannot see")
	}
}
