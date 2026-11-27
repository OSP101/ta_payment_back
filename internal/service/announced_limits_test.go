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
// Request declarations are weekly. Long class timetables do not establish
// how many hours a TA actually works on one day; worklogs enforce daily limits.
func TestAnnouncedLimit_LongTimetableAllowsPartialDeclaredWork(t *testing.T) {
	for _, tc := range []struct {
		level, track string
		limit        float64
	}{{"undergrad", "regular", 35}, {"undergrad", "special", 30}, {"master", "regular", 30}} {
		t.Run(tc.level+tc.track, func(t *testing.T) {
			f := newCapFixture(t)
			sec := f.addSection("1", [3]string{"lecture", "08:00", "16:00"})
			f.exec(`UPDATE sections SET track=$1 WHERE id=$2`, tc.track, sec)
			if err := f.svc.enforceDailyHourFeasibility(f.ctx, []uuid.UUID{sec}, tc.level, "ผู้ช่วย", 4); err != nil {
				t.Fatal(err)
			}
			if err := f.svc.enforceDailyHourFeasibility(f.ctx, []uuid.UUID{sec}, tc.level, "ผู้ช่วย", tc.limit+1); err == nil {
				t.Fatal("weekly ceiling was bypassed")
			}
		})
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
