package service

import (
	"strings"
	"testing"
	"time"

	"ta-payment-back/internal/timeutil"
)

// A makeup replaces ONE period, not a whole day.
//
// The reported failure: SC363001 sec 1 teaches บรรยาย 10:30–12:30 and ปฏิบัติการ
// 13:00–15:00 on the same Tuesday. Filing a makeup for the lab also marked the
// lecture as made up, and showed the lab's replacement slot on the lecture row —
// asserting two different classes would run in the same 2-hour window. The unique
// constraint then refused a second makeup, so the lecture's real slot could not be
// filed at all.

// twoPeriodFixture gives a course whose section teaches both a lecture and a lab
// on the same weekday, with that weekday declared a public holiday. This is the
// exact shape from the report.
func twoPeriodFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := registrarShapeCourse(t)
	holiday := f.addHolidayOnScheduledDay() // Monday: fixture has lecture + lab
	return f, holiday
}

// nextMonday returns a Monday `weeks` weeks out — a legal makeup target (future
// month rules aside) that is not itself the holiday.
func nextMonday(weeks int) string {
	d := timeutil.Now().AddDate(0, 0, 7*weeks)
	for d.Weekday() != time.Monday {
		d = d.AddDate(0, 0, 1)
	}
	return d.Format("2006-01-02")
}

// THE BUG, from the lecturer's point of view.
func TestMakeup_FilingOnePeriodLeavesTheOtherOutstanding(t *testing.T) {
	f, holiday := twoPeriodFixture(t)

	if err := f.teaching().AddMakeup(f.ctx, f.LecturerID, f.SectionID, MakeupSchedule{
		OriginalDate: holiday,
		MakeupDate:   nextMonday(1),
		Kind:         "lab",
		StartTime:    strPtr("13:00"),
		EndTime:      strPtr("15:00"),
	}); err != nil {
		t.Fatalf("AddMakeup(lab): %v", err)
	}

	impacts, err := f.holidays().ImpactsForCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatalf("ImpactsForCourse: %v", err)
	}
	var lecture, lab *HolidayImpactSection
	for i := range impacts.Impacts {
		if impacts.Impacts[i].OriginalDate != holiday {
			continue
		}
		for j := range impacts.Impacts[i].AffectedSections {
			s := &impacts.Impacts[i].AffectedSections[j]
			switch s.Kind {
			case "lecture":
				lecture = s
			case "lab":
				lab = s
			}
		}
	}
	if lecture == nil || lab == nil {
		t.Fatalf("expected both periods on %s, got %+v", holiday, impacts.Impacts)
	}
	if lab.Makeup == nil {
		t.Error("the lab makeup that was just filed is not shown on the lab period")
	}
	if lecture.Makeup != nil {
		t.Errorf("the lecture was marked as made up on %s, but no makeup was filed for it — "+
			"and its slot would be the lab's %s", lecture.Makeup.MakeupDate, lab.Makeup.MakeupDate)
	}
	if impacts.UnresolvedCount != 1 {
		t.Errorf("unresolved = %d, want 1 (the lecture) — the badge would say the day is done",
			impacts.UnresolvedCount)
	}
}

// ...and the lecture's own makeup must be fileable afterwards, at its own time.
// The old unique constraint refused this outright.
func TestMakeup_SecondPeriodCanBeFiledAtADifferentTime(t *testing.T) {
	f, holiday := twoPeriodFixture(t)
	svc := f.teaching()

	if err := svc.AddMakeup(f.ctx, f.LecturerID, f.SectionID, MakeupSchedule{
		OriginalDate: holiday, MakeupDate: nextMonday(1), Kind: "lab",
		StartTime: strPtr("13:00"), EndTime: strPtr("15:00"),
	}); err != nil {
		t.Fatalf("AddMakeup(lab): %v", err)
	}
	// Different day AND different time — the two periods are independent.
	if err := svc.AddMakeup(f.ctx, f.LecturerID, f.SectionID, MakeupSchedule{
		OriginalDate: holiday, MakeupDate: nextMonday(2), Kind: "lecture",
		StartTime: strPtr("09:00"), EndTime: strPtr("11:00"),
	}); err != nil {
		t.Fatalf("AddMakeup(lecture) was refused — the lecturer cannot reschedule it at all: %v", err)
	}

	impacts, err := f.holidays().ImpactsForCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if impacts.UnresolvedCount != 0 {
		t.Errorf("unresolved = %d after filing both periods, want 0", impacts.UnresolvedCount)
	}
	seen := map[string]string{}
	for _, im := range impacts.Impacts {
		if im.OriginalDate != holiday {
			continue
		}
		for _, s := range im.AffectedSections {
			if s.Makeup == nil {
				t.Fatalf("%s period has no makeup after both were filed", s.Kind)
			}
			seen[s.Kind] = s.Makeup.MakeupDate + " " + derefOr(s.Makeup.StartTime, "-")
		}
	}
	if seen["lecture"] == seen["lab"] {
		t.Errorf("both periods report the same replacement slot (%q) — they were filed differently",
			seen["lecture"])
	}
}

// Re-filing the SAME period is still refused, and the message has to name the
// period so the lecturer knows it is not the other one blocking them.
func TestMakeup_SamePeriodTwiceIsRefusedByName(t *testing.T) {
	f, holiday := twoPeriodFixture(t)
	svc := f.teaching()
	m := MakeupSchedule{OriginalDate: holiday, MakeupDate: nextMonday(1), Kind: "lab"}

	if err := svc.AddMakeup(f.ctx, f.LecturerID, f.SectionID, m); err != nil {
		t.Fatalf("first AddMakeup: %v", err)
	}
	err := svc.AddMakeup(f.ctx, f.LecturerID, f.SectionID, m)
	if err == nil {
		t.Fatal("filing the same period twice must be refused")
	}
	if !strings.Contains(err.Error(), "ปฏิบัติการ") {
		t.Errorf("refusal should name the period (ปฏิบัติการ), got: %v", err)
	}
}

// A makeup for a period the section does not teach that day can never be matched
// by any reader, so it must be refused at the door rather than filed and lost.
func TestMakeup_RefusesAPeriodTheSectionDoesNotTeachThatDay(t *testing.T) {
	f, holiday := twoPeriodFixture(t)
	// The fixture schedules nothing on Tuesday; move the section's lab off Monday
	// so a lab makeup for that Monday has no period to replace.
	f.exec(`DELETE FROM section_schedules WHERE section_id=$1 AND kind='lab'`, f.SectionID)

	err := f.teaching().AddMakeup(f.ctx, f.LecturerID, f.SectionID, MakeupSchedule{
		OriginalDate: holiday, MakeupDate: nextMonday(1), Kind: "lab",
	})
	if err == nil {
		t.Fatal("a makeup for a period that does not exist must be refused")
	}
	if !strings.Contains(err.Error(), "ไม่มีคาบ") {
		t.Errorf("refusal should say the period does not exist, got: %v", err)
	}
}

func TestMakeup_RejectsAnUnknownKind(t *testing.T) {
	f, holiday := twoPeriodFixture(t)
	err := f.teaching().AddMakeup(f.ctx, f.LecturerID, f.SectionID, MakeupSchedule{
		OriginalDate: holiday, MakeupDate: nextMonday(1), Kind: "seminar",
	})
	if err == nil {
		t.Fatal("an unknown period kind must be refused")
	}
}

func strPtr(s string) *string { return &s }

func derefOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}
