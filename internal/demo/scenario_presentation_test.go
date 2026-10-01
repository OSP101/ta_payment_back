package demo

import (
	"context"
	"crypto/rand"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/db"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/pii"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/storage"
	"ta-payment-back/internal/testutil"
	"ta-payment-back/internal/timeutil"
)

// countRows reads one table's row count on a pool, for the "nothing was
// written" assertions.
func countRows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// The dataset writes history by SQL, so it must refuse to run anywhere that
// is not provably a demo slot — and refuse BEFORE writing anything.
func TestPresentationRefusesOutsideDemoSlot(t *testing.T) {
	ctx := context.Background()
	pool := testutil.NewPool(t) // a real, fully migrated database in schema public

	cases := []struct {
		name string
		slot *Slot
	}{
		{"nil slot", nil},
		{"real container (IsDemoSlot=false)", &Slot{SchemaName: "demo_slot_0", Pool: pool,
			Container: &service.Container{Pool: pool, Cfg: config.Config{IsDemoSlot: false}}}},
		{"schema is public", &Slot{SchemaName: "public", Pool: pool,
			Container: &service.Container{Pool: pool, Cfg: config.Config{IsDemoSlot: true}}}},
		{"claims a slot name but the pool writes to public", &Slot{SchemaName: "demo_slot_0", Pool: pool,
			Container: &service.Container{Pool: pool, Cfg: config.Config{IsDemoSlot: true}}}},
		{"container on another pool", &Slot{SchemaName: "demo_slot_0", Pool: pool,
			Container: &service.Container{Pool: nil, Cfg: config.Config{IsDemoSlot: true}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := SeedPresentation(ctx, c.slot); !errors.Is(err, errNotDemoSlot) {
				t.Fatalf("SeedPresentation = %v, want errNotDemoSlot", err)
			}
			if _, _, err := PresentationStatus(ctx, c.slot); !errors.Is(err, errNotDemoSlot) {
				t.Fatalf("PresentationStatus = %v, want errNotDemoSlot", err)
			}
		})
	}
	for _, table := range []string{"users", "academic_terms", "teaching_courses", "work_logs", "ta_profiles"} {
		if n := countRows(t, pool, table); n != 0 {
			t.Errorf("%s has %d rows after refused runs — something was written before the guard", table, n)
		}
	}
}

// newTestSlot builds a real demo slot inside a throwaway database: its own
// schema, migrated through a search_path-pinned pool, a container flagged
// IsDemoSlot, seeded like Bootstrap seeds one.
func newTestSlot(t *testing.T) (*Slot, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	mainPool := testutil.NewPool(t)
	const schema = "demo_slot_7"
	if _, err := mainPool.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	pcfg := mainPool.Config().Copy()
	pcfg.MaxConns = 4
	if pcfg.ConnConfig.RuntimeParams == nil {
		pcfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	pcfg.ConnConfig.RuntimeParams["timezone"] = "Asia/Bangkok"
	pcfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	slotPool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(slotPool.Close)
	if err := db.Migrate(ctx, slotPool, testutil.MigrationsDir(t)); err != nil {
		t.Fatalf("migrate slot: %v", err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cipher, err := pii.New(key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewLocalWithKey(t.TempDir(), key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{IsDemoSlot: true}
	container := service.NewContainer(slotPool, store, mail.New(cfg), audit.New(slotPool), cfg, cipher, cipher)
	if err := seedSlot(ctx, container); err != nil {
		t.Fatalf("seedSlot: %v", err)
	}
	return &Slot{Index: 7, SchemaName: schema, Pool: slotPool, Container: container}, mainPool
}

// The dataset has to show every case the dashboard answers, and its money
// has to add up the same way everywhere, or it is a poor thing to present.
func TestPresentationDatasetCoversEveryCase(t *testing.T) {
	// Seeded at the REAL now, only. The seed takes `now` as a parameter, but
	// the services it drives do not: whether a submission period is closed is
	// decided by Postgres's CURRENT_DATE (periodClosedSQL), which no Go hook
	// can move. Seeding at any other date puts the dataset and the database
	// on different days — that was the flake: the old "3 and 5 days ago"
	// variants and the pinned 2026-10-01 case pass or fail depending on which
	// real day the suite runs (seeding at 2026-09-30 on 2026-10-01 already
	// fails with "ปิดรับบันทึกเวลาแล้ว"; 2027-01-04 empties a bucket).
	// Running at the real date is the only seed/DB pairing the product
	// itself ever uses, so it is the one that must hold — on whatever day,
	// weekday or month boundary the suite happens to run.
	now := timeutil.Now()
	t.Run(now.Format("2006-01-02"), func(t *testing.T) { presentationCoversEveryCase(t, now) })
}

func presentationCoversEveryCase(t *testing.T, now time.Time) {
	ctx := context.Background()
	slot, mainPool := newTestSlot(t)

	msg, err := seedPresentationAt(ctx, slot, now)
	if err != nil {
		t.Fatalf("SeedPresentation: %v", err)
	}
	t.Log(msg)

	// Isolation: the database's own public schema never saw a row.
	for _, table := range []string{"users", "academic_terms", "teaching_courses", "work_logs"} {
		if n := countRows(t, mainPool, "public."+table); n != 0 {
			t.Errorf("public.%s has %d rows — the dataset leaked out of its slot", table, n)
		}
	}

	var termID uuid.UUID
	if err := slot.Pool.QueryRow(ctx, `SELECT id FROM academic_terms WHERE semester=$1`, PresentationSemester).Scan(&termID); err != nil {
		t.Fatal(err)
	}
	var active bool
	_ = slot.Pool.QueryRow(ctx, `SELECT is_active FROM academic_terms WHERE id=$1`, termID).Scan(&active)
	if active {
		t.Error("the presentation term took over as the active term — it must never displace the happy path's")
	}

	a, err := slot.Container.Dashboard.Analytics(ctx, &termID, slot.Container.Budget, slot.Container.Export)
	if err != nil {
		t.Fatalf("Analytics: %v", err)
	}

	// Breadth.
	if a.CoursesOpen < 20 {
		t.Errorf("courses = %d, want 20+", a.CoursesOpen)
	}
	curricula := map[string]bool{}
	for _, c := range a.Curricula {
		curricula[c.Curriculum] = true
	}
	if len(curricula) < 6 {
		t.Errorf("curricula = %v, want all six", curricula)
	}
	if len(a.Monthly) < 3 {
		t.Errorf("months with money = %d, want 3+", len(a.Monthly))
	}

	// Staffing: every verdict appears.
	status := map[string]int{}
	bigNoTA := 0
	for _, r := range a.Staffing {
		status[r.Status]++
		if r.Status == service.StaffingNoRequest && r.Students >= 100 {
			bigNoTA++
		}
	}
	for _, s := range []string{"over_ceiling", "above_guide", "match", "under", "no_students", "no_request"} {
		if status[s] == 0 {
			t.Errorf("no course with staffing status %s (got %v)", s, status)
		}
	}
	if bigNoTA == 0 {
		t.Error("no big course without a TA request")
	}

	// Money: over the cap, and near it (the dashboard's own line).
	over, near := 0, 0
	for _, r := range a.Staffing {
		switch {
		case r.UnfundedBaht > 0:
			over++
		case r.CapBaht > 0 && r.ForecastBaht/r.CapBaht >= service.NearCapRatio:
			near++
		}
		if r.CapBaht > 0 {
			t.Logf("%-9s cap %8.0f forecast %8.0f (%3.0f%%) unfunded %6.0f  %s", r.Code, r.CapBaht, r.ForecastBaht,
				100*r.ForecastBaht/r.CapBaht, r.UnfundedBaht, r.Status)
		}
	}
	if over == 0 || near == 0 {
		t.Errorf("over-cap courses = %d, near-cap courses = %d; want at least one of each", over, near)
	}
	if a.BudgetLump <= 0 {
		t.Error("no graduate lump sum")
	}

	// Work: every queue and every return path has something in it.
	p := a.Pipeline
	stage := map[string]int{}
	for _, s := range p.Stages {
		stage[s.Key] = s.Count
	}
	for _, k := range []string{"requests", "appointments", "payout_review", "export"} {
		if stage[k] == 0 {
			t.Errorf("pipeline stage %s is empty (%v)", k, stage)
		}
	}
	checks := map[string]int{
		"docs returned": p.DocsReturned, "months sent back": p.MonthsSentBack, "work logs rejected": p.WorklogsRejected,
		"requests rejected": p.RequestsRejected, "unresolved makeups": p.UnresolvedMakeups, "missing students": p.MissingStudents,
	}
	for name, n := range checks {
		if n == 0 {
			t.Errorf("%s = 0, want at least one", name)
		}
	}
	buckets := map[string]int{}
	for _, f := range a.Flow {
		buckets["with_ta"] += f.WithTA
		buckets["with_lecturer"] += f.Lecturer
		buckets["await_appointment"] += f.Appoint
		buckets["staff_review"] += f.Review
		buckets["ready_export"] += f.Export
		buckets["exported"] += f.Exported
	}
	for k, n := range buckets {
		if n == 0 {
			t.Errorf("claim-month bucket %s is empty (%v)", k, buckets)
		}
	}
	if a.Deadline == nil {
		t.Error("no open submission deadline")
	}

	// One money truth: KPI = end of the monthly running total = Σ courses.
	var monthSum, courseSum float64
	for _, m := range a.Monthly {
		monthSum += m.Baht
	}
	for _, c := range a.Courses {
		courseSum += c.SpentBaht
	}
	if math.Abs(monthSum+a.BudgetLumpUndated-a.BudgetUsed) > 0.01 || math.Abs(courseSum-a.BudgetUsed) > 0.01 {
		t.Errorf("money disagrees: Σmonthly %.2f + undated %.2f, Σcourses %.2f, BudgetUsed %.2f",
			monthSum, a.BudgetLumpUndated, courseSum, a.BudgetUsed)
	}

	// Running it again is a no-op, not a duplicate.
	before := countRows(t, slot.Pool, "teaching_courses")
	if _, err := SeedPresentation(ctx, slot); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if after := countRows(t, slot.Pool, "teaching_courses"); after != before {
		t.Errorf("second run added courses: %d → %d", before, after)
	}
	if loaded, label, err := PresentationStatus(ctx, slot); err != nil || !loaded || label == "" {
		t.Errorf("PresentationStatus = %v %q %v", loaded, label, err)
	}
}
