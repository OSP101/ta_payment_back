package service

import (
	"strings"
	"testing"
	"time"

	"ta-payment-back/internal/timeutil"
)

// Partial-day holidays (migration 0058).
//
// The report: a คณะ holiday is usually not a whole day off. The college holds a
// ceremony in the morning and teaching resumes after lunch. Recorded as a
// full-day closure — the only shape the table could hold — the system cancelled
// BOTH periods of the day, told the lecturer to reschedule a class that was
// never actually cancelled, and then refused the one slot that made the most
// sense: the free half of the same day.
//
// What these tests pin is the boundary, not any one scenario. A closure blocks
// exactly what it overlaps, half-open at both ends, and every reader of the
// table — validator, generator, impacts page, the badge that links to it — has
// to draw that boundary in the same place. They have already disagreed once
// about a plain date (see course_window.go); a window is more ways to disagree.
//
// The fixture section teaches lecture 09:00–12:00 and lab 13:00–16:00 on Monday,
// so a morning closure is the natural probe: it must take the lecture and leave
// the lab alone.

// addPartialHolidayOnScheduledDay puts a faculty closure covering [start, end)
// on the same Monday addHolidayOnScheduledDay uses.
func (f *fixture) addPartialHolidayOnScheduledDay(start, end string) string {
	d := monthStart()
	for d.Weekday() != time.Monday {
		d = d.AddDate(0, 0, 1)
	}
	iso := d.Format("2006-01-02")
	f.exec(`INSERT INTO public_holidays (id, holiday_date, name_th, source, start_time, end_time)
	        VALUES (gen_random_uuid(), $1::date, 'กีฬาสีคณะ', 'faculty', $2::time, $3::time)`,
		iso, start, end)
	return iso
}

// ---------------------------------------------------------------------------
// The impacts page + the badge that links to it
// ---------------------------------------------------------------------------

// THE BUG, from the lecturer's point of view: a morning ceremony listed the
// afternoon lab as needing a makeup.
func TestPartialHoliday_OnlyOverlappingPeriodsAreAffected(t *testing.T) {
	f := registrarShapeCourse(t)
	holiday := f.addPartialHolidayOnScheduledDay("08:00", "12:00")

	impacts, err := f.holidays().ImpactsForCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatalf("ImpactsForCourse: %v", err)
	}
	kinds := map[string]bool{}
	for _, imp := range impacts.Impacts {
		if imp.OriginalDate != holiday {
			continue
		}
		if imp.HolidayStart == nil || *imp.HolidayStart != "08:00" {
			t.Errorf("impact should carry the closure window, got start=%v end=%v",
				imp.HolidayStart, imp.HolidayEnd)
		}
		for _, sec := range imp.AffectedSections {
			kinds[sec.Kind] = true
		}
	}
	if !kinds["lecture"] {
		t.Error("the 09:00–12:00 lecture overlaps the 08:00–12:00 closure and must be listed")
	}
	if kinds["lab"] {
		t.Error("the 13:00–16:00 lab does not overlap the 08:00–12:00 closure — " +
			"listing it sends the lecturer to reschedule a class that still met")
	}
}

// The sidebar badge counts periods with its own SQL. It must apply the same
// overlap rule as the page, or the pair contradicts itself again.
func TestPartialHoliday_BadgeCountsOnlyOverlappingPeriods(t *testing.T) {
	f := registrarShapeCourse(t)
	f.addPartialHolidayOnScheduledDay("08:00", "12:00")

	tc, err := f.teaching().Get(f.ctx, f.CourseID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	impacts, err := f.holidays().ImpactsForCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatalf("ImpactsForCourse: %v", err)
	}
	periods := 0
	for _, imp := range impacts.Impacts {
		periods += len(imp.AffectedSections)
	}
	if tc.UnresolvedMakeups != periods {
		t.Errorf("badge says %d unresolved periods, page shows %d — the two overlap rules disagree",
			tc.UnresolvedMakeups, periods)
	}
	if periods != 1 {
		t.Errorf("expected exactly the lecture to be affected, got %d periods", periods)
	}
}

// A closure ending exactly when a class begins is not a conflict. Without the
// half-open comparison, an 08:00–09:00 assembly would cancel the 09:00 lecture.
func TestPartialHoliday_TouchingBoundaryIsNotAnOverlap(t *testing.T) {
	f := registrarShapeCourse(t)
	f.addPartialHolidayOnScheduledDay("08:00", "09:00")

	impacts, err := f.holidays().ImpactsForCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatalf("ImpactsForCourse: %v", err)
	}
	for _, imp := range impacts.Impacts {
		if len(imp.AffectedSections) > 0 {
			t.Errorf("a closure ending at 09:00 must not touch the 09:00–12:00 lecture, got %d affected",
				len(imp.AffectedSections))
		}
	}
}

// Regression guard: every existing row is all-day (NULL/NULL), and all-day must
// keep meaning "the whole day is gone".
func TestAllDayHoliday_StillTakesEveryPeriod(t *testing.T) {
	f := registrarShapeCourse(t)
	f.addHolidayOnScheduledDay()

	impacts, err := f.holidays().ImpactsForCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatalf("ImpactsForCourse: %v", err)
	}
	periods := 0
	for _, imp := range impacts.Impacts {
		if imp.HolidayStart != nil {
			t.Errorf("an all-day holiday must report a nil window, got %v", *imp.HolidayStart)
		}
		periods += len(imp.AffectedSections)
	}
	if periods != 2 {
		t.Errorf("an all-day holiday must take both the lecture and the lab, got %d periods", periods)
	}
}

// ---------------------------------------------------------------------------
// The work-log validator
// ---------------------------------------------------------------------------

func partialHolidaySet(name, start, end string) holidaySet {
	sm, _ := parseHM(start)
	em, _ := parseHM(end)
	return holidaySet{"2026-06-08": {{name: name, startMin: sm, endMin: em}}}
}

func validateAgainst(t *testing.T, hs holidaySet, activity, start, end string, hours float64) error {
	t.Helper()
	termStart, termEnd := hardeningBounds()
	return validateWorkLogEntry(
		WorkLog{WorkDate: "2026-06-08", Activity: activity, StartTime: start, EndTime: end, Hours: hours},
		hardeningGate(), termStart, termEnd, examWindow{}, examWindow{},
		hs, makeupIndex{}, time.Time{})
}

// The money case: the afternoon lab actually met, so the TA must be able to log
// it even though the morning was closed.
func TestValidate_PartialHolidayLeavesTheOtherHalfLoggable(t *testing.T) {
	hs := partialHolidaySet("กีฬาสีคณะ", "08:00", "12:00")

	if err := validateAgainst(t, hs, "lab", "13:00", "16:00", 3); err != nil {
		t.Errorf("a 13:00–16:00 lab is outside an 08:00–12:00 closure and must be loggable, got: %v", err)
	}
	if err := validateAgainst(t, hs, "lecture", "09:00", "12:00", 3); err == nil {
		t.Error("a 09:00–12:00 lecture is inside the closure and must be refused")
	}
}

// Both ends half-open, checked from each side.
func TestValidate_PartialHolidayBoundaries(t *testing.T) {
	hs := partialHolidaySet("กีฬาสีคณะ", "09:00", "12:00")

	if err := validateAgainst(t, hs, "lab", "12:00", "14:00", 2); err != nil {
		t.Errorf("starting exactly when the closure ends must be allowed, got: %v", err)
	}
	if err := validateAgainst(t, hs, "lab", "07:00", "09:00", 2); err != nil {
		t.Errorf("ending exactly when the closure starts must be allowed, got: %v", err)
	}
	// One minute of overlap is still overlap.
	if err := validateAgainst(t, hs, "lab", "11:59", "13:59", 2); err == nil {
		t.Error("an entry overlapping the closure by a minute must be refused")
	}
}

// The refusal has to name the hours. Told only "วันหยุด", a TA whose class ran
// normally at 13:00 cannot tell a rule from a bug, and files a ticket instead of
// moving the entry.
func TestValidate_PartialHolidayRefusalNamesTheWindow(t *testing.T) {
	hs := partialHolidaySet("กีฬาสีคณะ", "08:00", "12:00")
	err := validateAgainst(t, hs, "lecture", "09:00", "12:00", 3)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "08:00") || !strings.Contains(err.Error(), "12:00") {
		t.Errorf("refusal must state the closed hours, got: %v", err)
	}
}

// All-day closures keep their original behaviour through the new code path.
func TestValidate_AllDayHolidayStillBlocksEverything(t *testing.T) {
	hs := holidaySet{"2026-06-08": {{name: "วันแม่แห่งชาติ", allDay: true}}}

	if err := validateAgainst(t, hs, "lab", "13:00", "16:00", 3); err == nil {
		t.Error("an all-day holiday must block the afternoon lab too")
	}
	// review is scope-neutral (Q&A rule 7) and stays allowed on any date.
	if err := validateAgainst(t, hs, "review", "13:00", "16:00", 3); err != nil {
		t.Errorf("review must remain allowed on a holiday, got: %v", err)
	}
}

// Two closures on one date compose: the free stretch between them is loggable,
// each closed stretch is not.
func TestValidate_TwoWindowsOnOneDate(t *testing.T) {
	sm1, _ := parseHM("08:00")
	em1, _ := parseHM("10:00")
	sm2, _ := parseHM("15:00")
	em2, _ := parseHM("17:00")
	hs := holidaySet{"2026-06-08": {
		{name: "พิธีเปิด", startMin: sm1, endMin: em1},
		{name: "พิธีปิด", startMin: sm2, endMin: em2},
	}}

	if err := validateAgainst(t, hs, "lab", "10:00", "15:00", 5); err != nil {
		t.Errorf("the stretch between two closures must be loggable, got: %v", err)
	}
	if err := validateAgainst(t, hs, "lab", "16:00", "17:00", 1); err == nil {
		t.Error("the second closure must block too — not just the first one found")
	}
}

// ---------------------------------------------------------------------------
// Filing a makeup
// ---------------------------------------------------------------------------

// The point of the feature: reschedule into the free half of the very same day.
// The old date-level nested-holiday check refused this outright.
func TestAddMakeup_AllowedInTheFreeHalfOfAPartialHoliday(t *testing.T) {
	f := registrarShapeCourse(t)
	holiday := f.addPartialHolidayOnScheduledDay("08:00", "12:00")

	err := f.teaching().AddMakeup(f.ctx, f.LecturerID, f.SectionID, MakeupSchedule{
		OriginalDate: holiday,
		MakeupDate:   holiday, // same day, after the ceremony
		Kind:         "lecture",
		StartTime:    strPtr("13:00"),
		EndTime:      strPtr("16:00"),
	})
	if err != nil {
		t.Fatalf("a makeup outside the closed hours of the same day must be accepted, got: %v", err)
	}
}

func TestAddMakeup_RefusedWhenItOverlapsTheClosure(t *testing.T) {
	f := registrarShapeCourse(t)
	holiday := f.addPartialHolidayOnScheduledDay("08:00", "12:00")

	err := f.teaching().AddMakeup(f.ctx, f.LecturerID, f.SectionID, MakeupSchedule{
		OriginalDate: holiday,
		MakeupDate:   holiday,
		Kind:         "lecture",
		StartTime:    strPtr("10:00"),
		EndTime:      strPtr("13:00"),
	})
	if err == nil {
		t.Fatal("a makeup running through the closed hours must be refused")
	}
	if !strings.Contains(err.Error(), "08:00") {
		t.Errorf("refusal should name the closed hours so the lecturer can shift it, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The auto-generator
// ---------------------------------------------------------------------------

// The generator is where a wrong answer costs the most: it drops rows silently,
// so a TA who never types anything by hand simply gets paid less and has nothing
// to point at. A morning closure must remove the lecture rows for that Monday
// and leave every lab row standing.
func TestGenerate_PartialHolidayDropsOnlyTheOverlappingPeriod(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoOwnClassSchedule: true})
	f.addOwnClassInTerm(3, "08:00", "09:00") // Wednesday — no clash with Monday
	holiday := f.addPartialHolidayOnScheduledDay("08:00", "12:00")

	if _, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	var lectures, labs int
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT COUNT(*) FILTER (WHERE activity = 'lecture'),
		       COUNT(*) FILTER (WHERE activity = 'lab')
		  FROM work_logs WHERE assignment_id = $1 AND work_date = $2::date`,
		f.AssignmentID, holiday).Scan(&lectures, &labs); err != nil {
		t.Fatal(err)
	}
	if lectures != 0 {
		t.Errorf("the 09:00–12:00 lecture overlaps the closure — %d row(s) generated, want 0", lectures)
	}
	if labs != 1 {
		t.Errorf("the 13:00–16:00 lab is outside the closure and must still be generated — got %d row(s), want 1; "+
			"dropping it silently costs the TA a class they actually taught", labs)
	}
}

// A makeup landing on a whole-day holiday is still refused — that guard is what
// keeps the lecturer from pushing the problem to another dead day.
func TestAddMakeup_StillRefusedOnAnAllDayHoliday(t *testing.T) {
	f := registrarShapeCourse(t)
	holiday := f.addHolidayOnScheduledDay()

	// A later Monday, also declared closed for the whole day.
	target := timeutil.Now()
	for target.Weekday() != time.Monday || target.Format("2006-01-02") <= holiday {
		target = target.AddDate(0, 0, 1)
	}
	targetISO := target.Format("2006-01-02")
	f.exec(`INSERT INTO public_holidays (id, holiday_date, name_th, source)
	        VALUES (gen_random_uuid(), $1::date, 'วันหยุดอีกวัน', 'university')
	        ON CONFLICT DO NOTHING`, targetISO)

	err := f.teaching().AddMakeup(f.ctx, f.LecturerID, f.SectionID, MakeupSchedule{
		OriginalDate: holiday,
		MakeupDate:   targetISO,
		Kind:         "lecture",
		StartTime:    strPtr("09:00"),
		EndTime:      strPtr("12:00"),
	})
	if err == nil {
		t.Error("a makeup landing on an all-day holiday must still be refused")
	}
}
