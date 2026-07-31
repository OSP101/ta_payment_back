package service

import (
	"testing"
	"time"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/timeutil"
)

// The sidebar badge and the holidays page must agree about whether a course has
// unresolved makeups. They didn't: the badge said 8 and the page it linked to said
// "ไม่มีวันหยุดที่ตรงกับคาบเรียน", because three copies of the same query
// disagreed about the date window to search.
//
// These tests pin the agreement rather than any one number. A count can change
// for good reasons — a new public holiday, a filed makeup — but "one screen says
// there is work and another says there is none" is never right, and it is the
// failure a lecturer actually hit.

// registrarShapeCourse is a course as the registrar import leaves it: its own
// starts_on / ends_on NULL, inheriting the academic term's dates. Every course in
// the real database looks like this, which is why the fallback decided everything.
func registrarShapeCourse(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, fixtureOpts{})
	f.exec(`UPDATE teaching_courses SET starts_on = NULL, ends_on = NULL WHERE id = $1`, f.CourseID)
	return f
}

// addHolidayOnScheduledDay puts a public holiday on the next Monday inside the
// term — Monday is when the fixture's section holds both its lecture and its lab,
// so one holiday produces two affected periods.
func (f *fixture) addHolidayOnScheduledDay() string {
	// Find a Monday inside the term window. The term starts at the beginning of
	// the current Bangkok month and runs three months (see applyFixtureDefaults).
	d := monthStart()
	for d.Weekday() != time.Monday {
		d = d.AddDate(0, 0, 1)
	}
	iso := d.Format("2006-01-02")
	f.exec(`INSERT INTO public_holidays (id, holiday_date, name_th, source)
	        VALUES (gen_random_uuid(), $1::date, 'วันหยุดทดสอบ', 'custom')
	        ON CONFLICT DO NOTHING`, iso)
	return iso
}

func (f *fixture) teaching() *TeachingService {
	return &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
}

func (f *fixture) holidays() *HolidayService {
	return &HolidayService{pool: f.Pool, aud: audit.New(f.Pool)}
}

// THE BUG. Badge non-zero, page empty, same course, same moment.
func TestUnresolvedMakeups_BadgeAndPageAgree(t *testing.T) {
	f := registrarShapeCourse(t)
	f.addHolidayOnScheduledDay()

	tc, err := f.teaching().Get(f.ctx, f.CourseID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	impacts, err := f.holidays().ImpactsForCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatalf("ImpactsForCourse: %v", err)
	}

	if tc.UnresolvedMakeups == 0 {
		t.Fatal("the badge count is 0 on a course whose class day is a holiday")
	}
	if impacts.UnresolvedCount != tc.UnresolvedMakeups {
		t.Errorf("badge says %d unresolved, page says %d — the lecturer clicks a badge "+
			"and lands on a screen that tells them there is nothing to do",
			tc.UnresolvedMakeups, impacts.UnresolvedCount)
	}
	if len(impacts.Impacts) == 0 {
		t.Error("the page has no rows to act on, so the makeup can never be filed")
	}
}

// The course LIST is a third reader of the same formula, and it had the same
// CURRENT_DATE fallback. A lecturer comparing their course list to the course page
// must not see two different answers either.
func TestUnresolvedMakeups_ListAgreesWithGet(t *testing.T) {
	f := registrarShapeCourse(t)
	f.addHolidayOnScheduledDay()

	svc := f.teaching()
	tc, err := svc.Get(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	list, err := svc.List(f.ctx, &f.TermID, nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, row := range list {
		if row.ID != f.CourseID {
			continue
		}
		if row.UnresolvedMakeups != tc.UnresolvedMakeups {
			t.Errorf("list says %d unresolved, course page says %d",
				row.UnresolvedMakeups, tc.UnresolvedMakeups)
		}
		return
	}
	t.Fatal("course missing from List")
}

// Filing the makeup must clear it everywhere at once — otherwise the badge nags
// forever about work that is done, which is the same class of defect from the
// other direction.
func TestUnresolvedMakeups_FilingAMakeupClearsBothReaders(t *testing.T) {
	f := registrarShapeCourse(t)
	holiday := f.addHolidayOnScheduledDay()

	// One makeup per PERIOD (see migration 0055) — the fixture's Monday holds both
	// a lecture and a lab, so clearing the day takes two.
	for _, kind := range []string{"lecture", "lab"} {
		f.exec(`INSERT INTO makeup_schedules (id, section_id, original_date, makeup_date, note, kind)
		        VALUES (gen_random_uuid(), $1, $2::date, $3::date, 'ชดเชยทดสอบ', $4)`,
			f.SectionID, holiday, timeutil.Now().AddDate(0, 0, 7).Format("2006-01-02"), kind)
	}

	tc, err := f.teaching().Get(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	impacts, err := f.holidays().ImpactsForCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if tc.UnresolvedMakeups != 0 {
		t.Errorf("badge still shows %d unresolved after the makeup was filed", tc.UnresolvedMakeups)
	}
	if impacts.UnresolvedCount != 0 {
		t.Errorf("page still shows %d unresolved after the makeup was filed", impacts.UnresolvedCount)
	}
	// The date itself stays listed — the lecturer needs to see what they filed.
	if len(impacts.Impacts) == 0 {
		t.Error("the resolved holiday vanished from the page; the makeup is no longer reviewable")
	}
}

// A holiday outside the course's term is not this course's problem, and the
// window must still exclude it — the fix widened the window, so this guards
// against widening it to "everything".
func TestUnresolvedMakeups_IgnoresHolidaysOutsideTheTerm(t *testing.T) {
	f := registrarShapeCourse(t)

	// A Monday well after the term ends (term is 3 months from the month start).
	d := monthStart().AddDate(1, 0, 0)
	for d.Weekday() != time.Monday {
		d = d.AddDate(0, 0, 1)
	}
	f.exec(`INSERT INTO public_holidays (id, holiday_date, name_th, source)
	        VALUES (gen_random_uuid(), $1::date, 'วันหยุดนอกเทอม', 'custom')`,
		d.Format("2006-01-02"))

	tc, err := f.teaching().Get(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if tc.UnresolvedMakeups != 0 {
		t.Errorf("counted %d unresolved from a holiday outside the term window", tc.UnresolvedMakeups)
	}
	impacts, err := f.holidays().ImpactsForCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if impacts.UnresolvedCount != 0 {
		t.Errorf("page counted %d unresolved outside the term window", impacts.UnresolvedCount)
	}
}
