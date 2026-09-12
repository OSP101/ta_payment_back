package service

import "testing"

// The planner's estimate stands or falls on how many times a section really
// meets. The fixture teaches Mondays, 1 Jun – 31 Oct 2026: 22 Mondays. Take one
// away for a holiday with no makeup, keep one that was moved to a Saturday,
// and drop the midterm week — the count must follow the calendar, not months×4.
func TestPlanFacts_CountsPeriodsFromTheCalendar(t *testing.T) {
	f := newFixture(t, fixtureOpts{TermStart: "2026-06-01", TermEnd: "2026-10-31"})
	svc := exportSvcFor(f)

	// Holiday on a Monday, no makeup filed → that lecture and lab are gone.
	f.exec(`INSERT INTO public_holidays (id, holiday_date, name_th, source)
	        VALUES (gen_random_uuid(), '2026-06-08', 'วันหยุดทดสอบ', 'custom') ON CONFLICT DO NOTHING`)
	// Holiday on another Monday, both periods moved to the Saturday → still count.
	f.exec(`INSERT INTO public_holidays (id, holiday_date, name_th, source)
	        VALUES (gen_random_uuid(), '2026-07-13', 'วันหยุดทดสอบ 2', 'custom') ON CONFLICT DO NOTHING`)
	f.exec(`INSERT INTO makeup_schedules (id, section_id, original_date, makeup_date, kind)
	        VALUES (gen_random_uuid(), $1, '2026-07-13', '2026-07-18', 'lecture'),
	               (gen_random_uuid(), $1, '2026-07-13', '2026-07-18', 'lab')`, f.SectionID)
	// Midterm week swallows one Monday.
	f.exec(`UPDATE academic_terms SET midterm_starts_on = '2026-08-03', midterm_ends_on = '2026-08-07' WHERE id = $1`, f.TermID)

	facts, err := svc.PlanFacts(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Sections) != 1 {
		t.Fatalf("sections = %d, want 1", len(facts.Sections))
	}
	s := facts.Sections[0]
	// 22 Mondays − 1 holiday − 1 midterm = 20 lectures and 20 labs.
	if s.LecturePeriods != 20 || s.LabPeriods != 20 {
		t.Fatalf("periods = %d/%d, want 20/20 (holiday %d, exam %d)", s.LecturePeriods, s.LabPeriods, s.SkippedHoliday, s.SkippedExam)
	}
	if s.SkippedHoliday != 2 || s.SkippedExam != 2 {
		t.Fatalf("skipped = holiday %d exam %d, want 2/2 (one period of each kind)", s.SkippedHoliday, s.SkippedExam)
	}
	if s.ClassWeeks != 20 || facts.ClassWeeks != 20 {
		t.Fatalf("class weeks = %d/%d, want 20", s.ClassWeeks, facts.ClassWeeks)
	}
	if s.LectureHoursWeekly != 3 || s.LabHoursWeekly != 3 {
		t.Fatalf("weekly hours = %v/%v, want 3/3", s.LectureHoursWeekly, s.LabHoursWeekly)
	}
	// The fixture's approved TA holds a regular seat and the pool comes from
	// the budget formula, so the planner starts from what is left.
	if facts.Tracks.Regular.ExistingTAs != 1 {
		t.Fatalf("existing regular TAs = %d, want 1", facts.Tracks.Regular.ExistingTAs)
	}
	if facts.Tracks.Regular.CapBaht <= 0 {
		t.Fatalf("regular cap = %v, want the budget formula's figure", facts.Tracks.Regular.CapBaht)
	}
	if facts.Rates.UndergradRegular <= 0 || facts.Rates.GraduateSpecialLumpsum <= 0 {
		t.Fatalf("rates missing: %+v", facts.Rates)
	}
}

// A lecturer who does not teach the course gets nothing; staff always can.
func TestPlanFacts_ViewerGate(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := exportSvcFor(f)
	if _, err := svc.PlanFactsForViewer(f.ctx, f.LecturerID, f.CourseID, false); err != nil {
		t.Fatalf("own lecturer: %v", err)
	}
	stranger := f.insertUser("lecturer", "other")
	if _, err := svc.PlanFactsForViewer(f.ctx, stranger, f.CourseID, false); err != ErrForbidden {
		t.Fatalf("stranger: err = %v, want ErrForbidden", err)
	}
	if _, err := svc.PlanFactsForViewer(f.ctx, stranger, f.CourseID, true); err != nil {
		t.Fatalf("staff: %v", err)
	}
}
