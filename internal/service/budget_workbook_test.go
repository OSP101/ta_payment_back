package service

import (
	"context"
	"math"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/testutil"
)

// The per-course budget ceiling follows the faculty's own workbook
// (docs/ค่า-TA-ภาคต้น-ปี-2560, ชีต "2_59 ป.ตรี"), confirmed with the user on
// 2026-07-29:
//
//	ภาระงาน/สัปดาห์ = นก.บรรยาย × 3 × (นศ./60) + นก.แล็บ × 4.5 × (นศ./30)
//	ค่า TA/เดือน     = ภาระงาน × (50% × 200 ตรี + 50% × 400 บัณฑิต) = ภาระงาน × 300
//	วงเงินสูงสุด      = ค่า TA/เดือน × 4 เดือน
//
// The cases below are real rows from that workbook, so a future edit to the
// formula or the rate has to disagree with the department's own paperwork
// before it can pass.
//
// This matters beyond arithmetic: BudgetService sets PerCourseMaxBaht =
// TermPay and ExportService prorata-caps payouts against it, so an understated
// ceiling quietly cuts money the faculty had budgeted.

type workbookCase struct {
	code        string
	credits     string // as printed, for the failure message
	lectureCr   int
	labCr       int
	students    int
	wantCeiling float64 // วงเงินสูงสุด from the workbook
}

func TestBudget_MatchesFacultyWorkbook(t *testing.T) {
	cases := []workbookCase{
		{"322238", "3(3-0-6)", 3, 0, 69, 12420},
		{"322339", "1(0-2-2)", 0, 1, 76, 13680},
		{"322391", "3(3-0-6)", 3, 0, 66, 11880},
	}

	pool := testutil.NewPool(t)
	ctx := context.Background()
	svc := &BudgetService{pool: pool}
	seedWorkbookRates(t, pool)

	termID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO academic_terms (id, academic_year, semester, is_active, months)
		 VALUES ($1, 2560, 1, TRUE, 4)`, termID); err != nil {
		t.Fatalf("insert term: %v", err)
	}

	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			tcID := uuid.New()
			if _, err := pool.Exec(ctx, `
				INSERT INTO teaching_courses
				    (id, term_id, code, name_th, level, credits, lecture_hrs, lab_hrs,
				     num_students, num_students_regular)
				VALUES ($1,$2,$3,'วิชาจากสมุดงานคณะ','undergrad',$4,$5,$6,$7,$7)`,
				tcID, termID, c.code+"-"+tcID.String()[:4],
				c.lectureCr+c.labCr, c.lectureCr, c.labCr, c.students); err != nil {
				t.Fatalf("insert course: %v", err)
			}

			snap, err := svc.Compute(ctx, tcID)
			if err != nil {
				t.Fatalf("Compute: %v", err)
			}
			if math.Abs(snap.PerCourseMaxBaht-c.wantCeiling) > 1 {
				t.Errorf("วงเงินสูงสุด %s %s, นศ. %d = %.0f฿, want %.0f฿ (ต่างกัน %.0f฿)",
					c.code, c.credits, c.students,
					snap.PerCourseMaxBaht, c.wantCeiling,
					c.wantCeiling-snap.PerCourseMaxBaht)
			}
		})
	}
}

// The rate is the single number that made every ceiling a third too low.
func TestBudget_WorkloadRateIs300(t *testing.T) {
	pool := testutil.NewPool(t)
	seedWorkbookRates(t, pool)

	var rate float64
	if err := pool.QueryRow(context.Background(),
		`SELECT ug_workload_rate_regular FROM pay_rates ORDER BY effective_from DESC LIMIT 1`,
	).Scan(&rate); err != nil {
		t.Fatalf("read rate: %v", err)
	}
	if rate != 300 {
		t.Fatalf("ug_workload_rate_regular = %.0f, want 300 "+
			"(50%%×200 ตรี + 50%%×400 บัณฑิต — see ชีต \"2_59 ป.ตรี\")", rate)
	}
}

// seedWorkbookRates inserts a rate row naming only the columns that have no
// usable schema default, so everything else — including
// ug_workload_rate_regular — comes from the migrated DEFAULT. That way this
// test fails if the schema default ever drifts away from the workbook, which
// is exactly the failure that shipped: migration 0005 fixed the default while
// cmd/seed kept writing 200 over the top of it.
func seedWorkbookRates(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO pay_rates
		    (effective_from, undergrad_regular, undergrad_special,
		     graduate_regular, graduate_special_lumpsum)
		VALUES ('2020-01-01', 40, 50, 3000, 4000)`); err != nil {
		t.Fatalf("seed pay_rates: %v", err)
	}
}
