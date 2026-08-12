package service

import (
	"math"
	"testing"
	"time"
)

// gradSpecialMonthShares must weight the flat grad-special term lump by the
// COURSE's own regular-track class-schedule hours per calendar month — this
// guards against a regression back to a uniform per-month split, which would
// misprint the flat lump identically every month instead of following where
// the course actually teaches (docs/14. CP363761-บัณฑิต.xls' own example
// splits one course's lump unevenly across its months: 300 + 1,290 + 1,290).
func TestGradSpecialMonthShares_WeightsByRegularTrackScheduleHours(t *testing.T) {
	f := newTCFixture(t)
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP777", Curriculum: "CY", LectureHrs: 100})
	// One 2-hour lecture every Monday for the whole term (fixture's term runs
	// 2026-06-01..2026-10-31).
	f.exec(`INSERT INTO section_schedules (id, section_id, kind, day_of_week, start_time, end_time)
	        VALUES (gen_random_uuid(), $1, 'lecture', 1, '09:00', '11:00')`, regSec)

	weights, err := gradSpecialMonthShares(f.ctx, f.pool, courseID)
	if err != nil {
		t.Fatalf("gradSpecialMonthShares: %v", err)
	}
	if weights == nil {
		t.Fatal("expected non-nil weights, got nil")
	}

	// Independently count Mondays per month across the term range — a
	// different method than the production query (Go's own time.Weekday
	// rather than a day-by-day SQL/loop match), so this isn't just checking
	// the implementation against itself.
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	wantHours := map[string]float64{}
	var total float64
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Monday {
			ym := d.Format("2006-01")
			wantHours[ym] += 2
			total += 2
		}
	}
	if len(weights) != len(wantHours) {
		t.Fatalf("got %d months %+v, want %d months %+v", len(weights), weights, len(wantHours), wantHours)
	}
	var sum float64
	for ym, hrs := range wantHours {
		want := hrs / total
		got, ok := weights[ym]
		if !ok {
			t.Errorf("month %s missing from weights %+v", ym, weights)
			continue
		}
		if math.Abs(got-want) > 0.0001 {
			t.Errorf("month %s: weight = %v, want %v", ym, got, want)
		}
		sum += got
	}
	if math.Abs(sum-1.0) > 0.0001 {
		t.Errorf("weights sum to %v, want 1.0", sum)
	}
}

// A course with no regular-track schedule yet has nothing to weight the
// special-track lump by — callers must fall back to an even split rather
// than gradSpecialMonthShares silently returning an empty, all-zero map.
func TestGradSpecialMonthShares_NilWhenNoRegularSchedule(t *testing.T) {
	f := newTCFixture(t)
	courseID, _, _ := f.insertCourse(tcCourseOpts{Code: "CP778", Curriculum: "CY", LectureHrs: 100})

	weights, err := gradSpecialMonthShares(f.ctx, f.pool, courseID)
	if err != nil {
		t.Fatalf("gradSpecialMonthShares: %v", err)
	}
	if weights != nil {
		t.Errorf("expected nil weights when the course has no regular-track schedule, got %+v", weights)
	}
}

// A full-day holiday landing on a scheduled class day must not count toward
// that month's hours — otherwise a month with more holidays would still get
// full credit for classes that never actually happened.
func TestGradSpecialMonthShares_ExcludesFullDayHolidays(t *testing.T) {
	f := newTCFixture(t)
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP779", Curriculum: "CY", LectureHrs: 100})
	f.exec(`INSERT INTO section_schedules (id, section_id, kind, day_of_week, start_time, end_time)
	        VALUES (gen_random_uuid(), $1, 'lecture', 1, '09:00', '11:00')`, regSec)
	// 2026-06-01 is a Monday — close it as a full-day holiday.
	f.exec(`INSERT INTO public_holidays (id, holiday_date, name_th, source)
	        VALUES (gen_random_uuid(), '2026-06-01', 'ทดสอบ', 'custom')`)

	weights, err := gradSpecialMonthShares(f.ctx, f.pool, courseID)
	if err != nil {
		t.Fatalf("gradSpecialMonthShares: %v", err)
	}

	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	wantHours := map[string]float64{}
	var total float64
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		if d.Weekday() != time.Monday {
			continue
		}
		if d.Format("2006-01-02") == "2026-06-01" {
			continue // closed for the holiday
		}
		ym := d.Format("2006-01")
		wantHours[ym] += 2
		total += 2
	}
	wantJune := wantHours["2026-06"] / total
	if math.Abs(weights["2026-06"]-wantJune) > 0.0001 {
		t.Errorf("June weight = %v, want %v (one Monday closed for the holiday)", weights["2026-06"], wantJune)
	}
}
