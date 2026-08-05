package service

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The faculty announcement, which nothing the system does may exceed:
//
//	ปริญญาตรี  ภาคปกติ   40 ฿/ชม.  ไม่เกิน 7 ชม./วัน  ไม่เกิน 300 ฿/วัน
//	          ภาคพิเศษ  50 ฿/ชม. หรือ 2,000 ฿/เดือน  ไม่เกิน 6 ชม./วัน  ไม่เกิน 300 ฿/วัน
//	บัณฑิต     ภาคปกติ   50 ฿/ชม.  ไม่เกิน 6 ชม./วัน  ไม่เกิน 300 ฿/วัน
//	          ภาคพิเศษ  4,000 ฿/เดือน
//
// The hour caps and the baht cap agree by construction — 7×40 = 280 and 6×50 =
// 300 — so neither alone is redundant, and both have to hold.
//
// The entry gate's side of this is covered in worklog_baseline_test.go. What is
// covered HERE is the request gate, which became load-bearing on 05/08/2026:
// the declared-hours ceiling moved from the credit notation to the section's
// real timetable, and a real timetable can be longer than the credits implied.
// A group meeting 8 hours in one day would now accept an 8-hour declaration on
// the ceiling alone — enforceDailyHourFeasibility is what refuses it, and until
// now nothing tested that function at all.

// A ป.ตรี ภาคปกติ group may not meet more than 7 hours in one day.
func TestAnnouncedLimit_UndergradRegularDailyHoursIs7(t *testing.T) {
	f := newCapFixture(t)
	sec := f.addSection("1", [3]string{"lecture", "08:00", "16:00"}) // 8 hours, one day

	err := f.svc.enforceDailyHourFeasibility(f.ctx, []uuid.UUID{sec}, "undergrad", "ผู้ช่วย ทดสอบ", 8)
	if err == nil {
		t.Fatal("8 hours in one day exceeds the announced 7 ชม./วัน for ป.ตรี ภาคปกติ")
	}
	if !strings.Contains(err.Error(), "7.0") {
		t.Fatalf("the refusal should quote the announced cap, got %q", err)
	}

	// Exactly 7 is allowed — the cap is inclusive.
	f2 := newCapFixture(t)
	sec2 := f2.addSection("1", [3]string{"lecture", "08:00", "15:00"}) // 7 hours
	if err := f2.svc.enforceDailyHourFeasibility(f2.ctx, []uuid.UUID{sec2}, "undergrad", "ผู้ช่วย", 7); err != nil {
		t.Fatalf("exactly 7 hours must be allowed: %v", err)
	}
}

// ภาคพิเศษ is 6, not 7 — the tighter of the two must win for a special group.
func TestAnnouncedLimit_UndergradSpecialDailyHoursIs6(t *testing.T) {
	f := newCapFixture(t)
	sec := f.addSection("1", [3]string{"lecture", "08:00", "14:30"}) // 6.5 hours
	f.exec(`UPDATE sections SET track = 'special' WHERE id = $1`, sec)

	err := f.svc.enforceDailyHourFeasibility(f.ctx, []uuid.UUID{sec}, "undergrad", "ผู้ช่วย", 6.5)
	if err == nil {
		t.Fatal("6.5 hours exceeds the announced 6 ชม./วัน for ภาคพิเศษ")
	}
	if !strings.Contains(err.Error(), "6.0") {
		t.Fatalf("the refusal should quote 6.0, got %q", err)
	}
}

// บัณฑิตศึกษา ภาคปกติ is also 6.
func TestAnnouncedLimit_GraduateRegularDailyHoursIs6(t *testing.T) {
	f := newCapFixture(t)
	sec := f.addSection("1", [3]string{"lecture", "08:00", "15:00"}) // 7 hours

	err := f.svc.enforceDailyHourFeasibility(f.ctx, []uuid.UUID{sec}, "master", "ผู้ช่วย", 7)
	if err == nil {
		t.Fatal("7 hours exceeds the announced 6 ชม./วัน for บัณฑิตศึกษา ภาคปกติ")
	}
}

// The weekly declaration is bounded by the daily cap × working days. This is the
// figure the raised ceiling could otherwise inflate: a section meeting 7 hours a
// day now permits a 7-hour declaration per kind, and the total still has to fit
// inside what a TA can physically log.
func TestAnnouncedLimit_WeeklyDeclarationCannotOutrunTheDailyCap(t *testing.T) {
	f := newCapFixture(t)
	sec := f.addSection("1", [3]string{"lecture", "08:00", "15:00"}) // 7h/day → 35h/week

	if err := f.svc.enforceDailyHourFeasibility(f.ctx, []uuid.UUID{sec}, "undergrad", "ผู้ช่วย", 35); err != nil {
		t.Fatalf("35 ชม./สัปดาห์ is exactly 7 × 5 and must be allowed: %v", err)
	}
	err := f.svc.enforceDailyHourFeasibility(f.ctx, []uuid.UUID{sec}, "undergrad", "ผู้ช่วย", 36)
	if err == nil {
		t.Fatal("36 ชม./สัปดาห์ exceeds 7 ชม./วัน × 5 วันทำการ")
	}
	if !strings.Contains(err.Error(), "35.0") {
		t.Fatalf("the refusal should show the derived weekly ceiling, got %q", err)
	}
}

// The lab-other field must not be a way around the weekly ceiling. It is new,
// and a field left out of weeklyWorkloadTotal would be invisible to this gate.
func TestAnnouncedLimit_LabOtherHoursCountTowardTheWeeklyCeiling(t *testing.T) {
	// 36 hours declared entirely as lab-other still outruns 7 × 5.
	w := WorkloadInput{LabOtherHrs: 36}
	if got := weeklyWorkloadTotal(w, "undergrad"); got != 36 {
		t.Fatalf("lab-other must be visible to the feasibility gate, got %.1f", got)
	}

	f := newCapFixture(t)
	sec := f.addSection("1", [3]string{"lab", "08:00", "15:00"})
	if err := f.svc.enforceDailyHourFeasibility(f.ctx, []uuid.UUID{sec}, "undergrad", "ผู้ช่วย",
		weeklyWorkloadTotal(w, "undergrad")); err == nil {
		t.Fatal("36 hours of lab-other must be refused just like any other 36 hours")
	}
}

// Without pay_rates there is nothing to check the announcement against. The gate
// used to return nil in that state — the same "no opinion" answer it gives a
// course with no timetable — so every hour figure sailed through unexamined.
//
// This is the state the limit tests above were accidentally written in: they all
// passed against a fixture that never seeded pay_rates, asserting nothing.
func TestAnnouncedLimit_RefusesWhenNoRatesAreConfigured(t *testing.T) {
	f := newCapFixture(t)
	sec := f.addSection("1", [3]string{"lecture", "08:00", "16:00"}) // 8h — over any cap
	f.exec(`DELETE FROM pay_rates`)

	err := f.svc.enforceDailyHourFeasibility(f.ctx, []uuid.UUID{sec}, "undergrad", "ผู้ช่วย", 8)
	if err == nil {
		t.Fatal("with no rates configured the caps cannot be checked — the request must be refused, not approved")
	}
	if !strings.Contains(err.Error(), "อัตราค่าตอบแทน") {
		t.Fatalf("the refusal should say the rates are missing, got %q", err)
	}
}
