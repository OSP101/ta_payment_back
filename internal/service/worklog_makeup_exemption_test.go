package service

import (
	"testing"

	"github.com/google/uuid"
)

// The weekly cap exempts a relocated class hour from the week it landed in —
// the hour belongs to the origin week whose class was cancelled. The exemption
// was keyed on (source='auto' AND note LIKE '%ชดเชย%'), guarding against a TA
// TYPING a manual row with "ชดเชย" in the note.
//
// It did not guard the other direction. `note` is free text the TA may edit on
// any draft row, and the TA edit path leaves `source` alone — so a TA could take
// a generated row, add "ชดเชย" to its note, and watch it drop out of its own
// week's total. The freed quota then paid for a second row in the same week,
// every week of the term.
//
// The exemption now keys on makeup_schedules, which only staff and lecturers can
// write.

func makeupExemptionFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, fixtureOpts{})
	// One declared attendance hour a week — so a second lecture hour in the same
	// week is exactly what the cap exists to refuse.
	f.exec(`UPDATE ta_workload_forms
	           SET attendance_hrs = 1, lab_hrs = 3, check_work_hrs = 0, ug_other_hrs = 0
	         WHERE assignment_id = $1`, f.AssignmentID)
	return f
}

// insertAutoRow plants a generated row the way Generate does — source='auto',
// which is the half of the exemption a TA cannot set from a request body.
func (f *fixture) insertAutoRow(date, start, end string, hours float64, activity, note string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO work_logs
	          (id, assignment_id, work_date, start_time, end_time, hours, activity, note, status, source)
	        VALUES ($1,$2,$3::date,$4::time,$5::time,$6,$7,$8,'draft','auto')`,
		id, f.AssignmentID, date, start, end, hours, activity, note)
	return id
}

// The exploit, end to end: edit a generated row's note, then spend the quota it
// vacated. Both halves must be refused — the second row is over the declared cap.
func TestWeeklyCap_EditingANoteCannotBuyExtraQuota(t *testing.T) {
	f := makeupExemptionFixture(t)
	mon := mondayInTerm().Format("2006-01-02")

	// A generated เช็คชื่อ row inside the real 09:00–12:00 lecture period. This
	// alone consumes the whole 1.0 h/week attendance quota.
	id := f.insertAutoRow(mon, "09:00", "10:00", 1, "lecture", "เช็คชื่อ")

	// The TA edits only the note — the word "ชดเชย" is the whole payload.
	if _, err := f.upsert(WorkLog{
		ID: id, AssignmentID: f.AssignmentID, WorkDate: mon,
		StartTime: "09:00", EndTime: "10:00", Hours: 1, Activity: "lecture",
		Note: strPtr("เช็คชื่อ ชดเชย"),
	}); err != nil {
		t.Fatalf("editing a note is a normal action and should succeed: %v", err)
	}

	// A second lecture hour the same week, also inside the real period. The
	// declared quota is 1.0 h and one hour is already logged, so this is over.
	_, err := f.upsert(WorkLog{
		AssignmentID: f.AssignmentID, WorkDate: mon,
		StartTime: "11:00", EndTime: "12:00", Hours: 1, Activity: "lecture",
	})
	if err == nil {
		t.Fatal("a note containing ชดเชย must not vacate the week's quota — " +
			"the TA just billed 2.0 h against a declared 1.0 h/week")
	}
}

// The exemption must still do its job for a REAL makeup: a class relocated onto
// another date is filed in makeup_schedules by staff, and its hour belongs to
// the week whose class was cancelled.
func TestWeeklyCap_GenuineMakeupIsStillExempt(t *testing.T) {
	f := makeupExemptionFixture(t)
	mon := mondayInTerm()
	monISO := mon.Format("2006-01-02")
	// The lecture from a PREVIOUS week, relocated onto this Monday by staff.
	origin := mon.AddDate(0, 0, -7).Format("2006-01-02")
	f.exec(`INSERT INTO makeup_schedules (section_id, original_date, makeup_date, kind, start_time, end_time)
	        VALUES ($1, $2::date, $3::date, 'lecture', '09:00', '10:00')`,
		f.SectionID, origin, monISO)

	// The relocated hour, as Generate writes it.
	f.insertAutoRow(monISO, "09:00", "10:00", 1, "lecture", "เช็คชื่อ(ชดเชย)")

	// This week's own regular เช็คชื่อ still fits: the relocated hour is not
	// charged to the week it landed in.
	if _, err := f.upsert(WorkLog{
		AssignmentID: f.AssignmentID, WorkDate: monISO,
		StartTime: "11:00", EndTime: "12:00", Hours: 1, Activity: "lecture",
	}); err != nil {
		t.Fatalf("a filed makeup must stay exempt from the destination week: %v", err)
	}
}
