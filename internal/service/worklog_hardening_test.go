package service

import (
	"math"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// round2 (P1.3)
// ---------------------------------------------------------------------------

func TestRound2(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{1234.5600000001, 1234.56},
		{0.005, 0.01},
		{-1.005, -1.0}, // math.Round(-100.49...) — sanity: stays 2dp
		{100, 100},
	}
	for _, c := range cases {
		if got := round2(c.in); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("round2(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// validateWorkLogEntry — 7h per-row cap (P1.1) + existing behaviour intact
// ---------------------------------------------------------------------------

func hardeningGate() activityGate {
	return activityGate{
		Scope: "both", AllowLecture: true, AllowLab: true, AllowReview: true, AllowOther: true,
	}
}

func hardeningBounds() (time.Time, time.Time) {
	start, _ := time.Parse("2006-01-02", "2026-06-01")
	end, _ := time.Parse("2006-01-02", "2026-10-31")
	return start, end
}

func TestValidate_RowHoursCap(t *testing.T) {
	start, end := hardeningBounds()
	base := WorkLog{
		WorkDate: "2026-06-10", Activity: "review",
	}

	// 7.0h exactly — allowed.
	w := base
	w.StartTime, w.EndTime, w.Hours = "08:00", "15:00", 7.0
	if err := validateWorkLogEntry(w, hardeningGate(), start, end, examWindow{}, examWindow{}, holidaySet{}, makeupIndex{}, time.Time{}); err != nil {
		t.Errorf("7.0h row should pass, got: %v", err)
	}

	// 7.5h — rejected by the per-row cap.
	w = base
	w.StartTime, w.EndTime, w.Hours = "08:00", "15:30", 7.5
	err := validateWorkLogEntry(w, hardeningGate(), start, end, examWindow{}, examWindow{}, holidaySet{}, makeupIndex{}, time.Time{})
	if err == nil {
		t.Fatalf("7.5h row must be rejected")
	}
	if !strings.Contains(err.Error(), "7") {
		t.Errorf("error should mention the 7h cap, got: %v", err)
	}
}

func TestValidate_HoursMustMatchSpan(t *testing.T) {
	start, end := hardeningBounds()
	w := WorkLog{
		WorkDate: "2026-06-10", Activity: "review",
		StartTime: "09:00", EndTime: "11:00", Hours: 2.5, // span is 2.0
	}
	if err := validateWorkLogEntry(w, hardeningGate(), start, end, examWindow{}, examWindow{}, holidaySet{}, makeupIndex{}, time.Time{}); err == nil {
		t.Errorf("hours/span mismatch must be rejected")
	}
}

func TestValidate_ExamBlackoutStillEnforced(t *testing.T) {
	start, end := hardeningBounds()
	ms, _ := time.Parse("2006-01-02", "2026-08-01")
	me, _ := time.Parse("2006-01-02", "2026-08-07")
	w := WorkLog{
		WorkDate: "2026-08-03", Activity: "review",
		StartTime: "09:00", EndTime: "11:00", Hours: 2,
	}
	if err := validateWorkLogEntry(w, hardeningGate(), start, end, examWindow{Start: ms, End: me}, examWindow{}, holidaySet{}, makeupIndex{}, time.Time{}); err == nil {
		t.Errorf("exam-window date must be rejected")
	}
}

// No back-dating into a month that already passed. todayRef fixes "now".
func TestValidate_NoBackdatingPastMonth(t *testing.T) {
	start, end := hardeningBounds() // 2026-06-01 .. 2026-10-31
	today, _ := time.Parse("2006-01-02", "2026-08-15")
	mk := func(date string) WorkLog {
		return WorkLog{
			WorkDate: date, Activity: "review",
			StartTime: "09:00", EndTime: "11:00", Hours: 2,
		}
	}
	call := func(date string, ref time.Time) error {
		return validateWorkLogEntry(mk(date), hardeningGate(), start, end,
			examWindow{}, examWindow{}, holidaySet{}, makeupIndex{}, ref)
	}

	// A prior month is rejected.
	if err := call("2026-07-10", today); err == nil {
		t.Errorf("a date in a past month must be rejected")
	}
	// The current month is allowed even for an earlier day of the month.
	if err := call("2026-08-05", today); err != nil {
		t.Errorf("current-month date should pass, got: %v", err)
	}
	// A future month (still within the term) is allowed — TAs may fill ahead.
	if err := call("2026-09-20", today); err != nil {
		t.Errorf("future-month date within term should pass, got: %v", err)
	}
	// Zero todayRef disables the rule (staff override / other callers).
	if err := call("2026-07-10", time.Time{}); err != nil {
		t.Errorf("zero todayRef must disable the back-date rule, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// statusRank — send-back ordering (monthly staff lifecycle)
// ---------------------------------------------------------------------------

func TestStatusRankOrder(t *testing.T) {
	order := []string{"pending", "exported", "finance_sent"}
	for i := 1; i < len(order); i++ {
		if statusRank[order[i-1]] >= statusRank[order[i]] {
			t.Errorf("rank(%s) must be < rank(%s)", order[i-1], order[i])
		}
	}
}
