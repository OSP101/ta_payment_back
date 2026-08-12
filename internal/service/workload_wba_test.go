package service

import (
	"context"
	"testing"

	"ta-payment-back/internal/testutil"
)

// RULE C5 (WBA / year-4 / grad): a WBA row skips the schedule-clash checks
// entirely, so eligibility must be enforced server-side, not just hidden in
// the UI. Undergrad requires study_year >= 4; graduate students (master/phd)
// are allowed regardless of study_year — they may genuinely have no class
// schedule of their own.

func TestReplaceClasses_WBA_UndergradYear4Allowed(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	svc := &WorkloadService{pool: pool}
	term := insertTerm(t, pool, 2569, 1, true)
	ta := insertDashUser(t, pool, "ta")
	if _, err := pool.Exec(ctx, `UPDATE users SET study_year = 4 WHERE id = $1`, ta); err != nil {
		t.Fatalf("set study_year: %v", err)
	}

	if err := svc.ReplaceClasses(ctx, ta, term, []ClassBlock{{IsWBA: true, DayOfWeek: 0, StartTime: "00:00", EndTime: "00:00", Note: "ไม่มีตารางเรียนปกติ"}}); err != nil {
		t.Fatalf("year-4 undergrad must be allowed to use WBA: %v", err)
	}
}

func TestReplaceClasses_WBA_UndergradBelowYear4Rejected(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	svc := &WorkloadService{pool: pool}
	term := insertTerm(t, pool, 2569, 1, true)
	ta := insertDashUser(t, pool, "ta")
	if _, err := pool.Exec(ctx, `UPDATE users SET study_year = 2 WHERE id = $1`, ta); err != nil {
		t.Fatalf("set study_year: %v", err)
	}

	if err := svc.ReplaceClasses(ctx, ta, term, []ClassBlock{{IsWBA: true, DayOfWeek: 0, StartTime: "00:00", EndTime: "00:00", Note: "ไม่มีตารางเรียนปกติ"}}); err == nil {
		t.Fatal("an undergrad below year 4 must be refused WBA")
	}
}

func TestReplaceClasses_WBA_UndergradWithNoYearOnFileRejected(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	svc := &WorkloadService{pool: pool}
	term := insertTerm(t, pool, 2569, 1, true)
	ta := insertDashUser(t, pool, "ta") // study_year left NULL

	if err := svc.ReplaceClasses(ctx, ta, term, []ClassBlock{{IsWBA: true, DayOfWeek: 0, StartTime: "00:00", EndTime: "00:00", Note: "ไม่มีตารางเรียนปกติ"}}); err == nil {
		t.Fatal("an undergrad with no study_year on file must be refused WBA, not silently allowed")
	}
}

// Graduate students may use the no-schedule option regardless of study_year
// — this is the 2026 correction: some grad TAs (research-only, or grad-special
// whose pay no longer depends on logged hours) genuinely have no class
// schedule of their own.
func TestReplaceClasses_WBA_GraduateAllowedRegardlessOfYear(t *testing.T) {
	for _, level := range []string{"master", "phd"} {
		t.Run(level, func(t *testing.T) {
			pool := testutil.NewPool(t)
			ctx := context.Background()
			svc := &WorkloadService{pool: pool}
			term := insertTerm(t, pool, 2569, 1, true)
			ta := insertDashUser(t, pool, "ta")
			if _, err := pool.Exec(ctx,
				`UPDATE users SET study_level = $2::study_level, study_year = NULL WHERE id = $1`,
				ta, level); err != nil {
				t.Fatalf("set study_level: %v", err)
			}

			if err := svc.ReplaceClasses(ctx, ta, term, []ClassBlock{{IsWBA: true, DayOfWeek: 0, StartTime: "00:00", EndTime: "00:00", Note: "ไม่มีตารางเรียนปกติ"}}); err != nil {
				t.Fatalf("a %s student must be allowed to use the no-schedule option: %v", level, err)
			}
		})
	}
}

func TestReplaceClasses_WBA_AtMostOneRowAllowed(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	svc := &WorkloadService{pool: pool}
	term := insertTerm(t, pool, 2569, 1, true)
	ta := insertDashUser(t, pool, "ta")
	if _, err := pool.Exec(ctx,
		`UPDATE users SET study_level = 'master'::study_level WHERE id = $1`, ta); err != nil {
		t.Fatalf("set study_level: %v", err)
	}

	err := svc.ReplaceClasses(ctx, ta, term, []ClassBlock{
		{IsWBA: true, DayOfWeek: 0, StartTime: "00:00", EndTime: "00:00", Note: "a"}, {IsWBA: true, DayOfWeek: 0, StartTime: "00:00", EndTime: "00:00", Note: "b"},
	})
	if err == nil {
		t.Fatal("more than one WBA row must be refused")
	}
}
