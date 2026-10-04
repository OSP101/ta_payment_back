package service

import "testing"

// Editing a makeup used to be DELETE-then-POST from the browser. When the new
// date was refused, the old makeup (and the TA's draft hours on it) was already
// gone. ReplaceMakeup checks the new values first, so a refusal changes nothing.
func TestReplaceMakeup_RefusedDateKeepsTheOldMakeup(t *testing.T) {
	f, holiday := twoPeriodFixture(t)
	oldDate := nextMonday(1)
	if err := f.teaching().AddMakeup(f.ctx, f.LecturerID, f.SectionID, MakeupSchedule{
		OriginalDate: holiday, MakeupDate: oldDate, Kind: "lab",
		StartTime: strPtr("13:00"), EndTime: strPtr("15:00"),
	}); err != nil {
		t.Fatalf("AddMakeup: %v", err)
	}
	var id string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT id::text FROM makeup_schedules WHERE section_id=$1 AND kind='lab'`, f.SectionID).Scan(&id); err != nil {
		t.Fatal(err)
	}

	// Far outside the course window: must be refused.
	err := f.teaching().ReplaceMakeup(f.ctx, f.LecturerID, f.SectionID, mustUUID(t, id), MakeupSchedule{
		MakeupDate: "2045-12-31", StartTime: strPtr("13:00"), EndTime: strPtr("15:00"),
	})
	if err == nil {
		t.Fatal("a makeup date outside the course must be refused")
	}
	var still string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT to_char(makeup_date,'YYYY-MM-DD') FROM makeup_schedules WHERE id=$1`, id).Scan(&still); err != nil {
		t.Fatalf("the old makeup was deleted by a refused edit: %v", err)
	}
	if still != oldDate {
		t.Errorf("old makeup date = %s, want %s unchanged", still, oldDate)
	}

	// A valid move goes through and keeps the period's identity.
	newDate := nextMonday(2)
	if err := f.teaching().ReplaceMakeup(f.ctx, f.LecturerID, f.SectionID, mustUUID(t, id), MakeupSchedule{
		MakeupDate: newDate, StartTime: strPtr("13:00"), EndTime: strPtr("15:00"),
	}); err != nil {
		t.Fatalf("valid replace: %v", err)
	}
	var got, kind string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT to_char(makeup_date,'YYYY-MM-DD'), kind FROM makeup_schedules WHERE section_id=$1 AND original_date=$2::date AND kind='lab'`,
		f.SectionID, holiday).Scan(&got, &kind); err != nil {
		t.Fatal(err)
	}
	if got != newDate {
		t.Errorf("makeup date = %s, want %s", got, newDate)
	}
}
