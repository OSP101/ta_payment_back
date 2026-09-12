package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

// CourseService now holds only pay-rate + budget-cap settings. The faculty
// course catalog was removed — course identity lives per-term on
// teaching_courses, populated from the imported registrar file.
type CourseService struct {
	pool *pgxpool.Pool
	aud  *audit.Auditor
}

// Settings

// Every numeric field below carries validate:"gte=0" as a first-line mirror
// of UpsertPayRate's own blanket "no negatives" check — that function still
// owns the sharper rules (some fields treat 0 as "use default", actual pay
// rates must be > 0, TermMonths has an upper bound too), which a struct tag
// cannot express without duplicating business logic, so those stay exactly
// where they are.
type PayRate struct {
	ID               uuid.UUID `json:"id"`
	EffectiveFrom    string    `json:"effective_from" validate:"required"`
	UndergradRegular float64   `json:"undergrad_regular" validate:"gte=0"` // hourly, ประกาศ 731/2565 = 40 ฿/hr
	UndergradSpecial float64   `json:"undergrad_special" validate:"gte=0"` // hourly, ประกาศ 1080/2565 = 50 ฿/hr
	// Deprecated: kept for rollback safety. Payment/budget now read GraduateRegularHourly.
	GraduateRegular        float64 `json:"graduate_regular" validate:"gte=0"`
	GraduateSpecialLumpsum float64 `json:"graduate_special_lumpsum" validate:"gte=0"` // monthly lump-sum, ประกาศ = 4,000 ฿/เดือน
	// Undergrad budget formula constants (from historical Excel workbook):
	UGLectureHoursPerCredit float64 `json:"ug_lecture_hours_per_credit" validate:"gte=0"`
	UGLabHoursPerCredit     float64 `json:"ug_lab_hours_per_credit" validate:"gte=0"`
	BaselineStudentsLecture int     `json:"baseline_students_lecture" validate:"gte=0"`
	BaselineStudentsLab     int     `json:"baseline_students_lab" validate:"gte=0"`
	// Effective monthly budget rate per weekly-workload-hour.
	// Per Excel formula: default 300 = 50% × 200 (ตรี TA) + 50% × 400 (บัณฑิต TA).
	// Applied to BOTH regular and special sections — tracks differ only in student count.
	UGWorkloadRateRegular float64 `json:"ug_workload_rate_regular" validate:"gte=0"`
	TermMonths            int     `json:"term_months" validate:"gte=0"`
	// Policy limits (advisory) — from the official regulation notes.
	UGMaxHoursPerDay     int `json:"ug_max_hours_per_day" validate:"gte=0"`    // undergrad regular: max hrs/day (7)
	MaxCoursesPerStudent int `json:"max_courses_per_student" validate:"gte=0"` // any student: max concurrent TA courses (3)
	// New (migration 0018) — per-track daily caps + graduate hourly rate + term/day money caps.
	GraduateRegularHourly   float64 `json:"graduate_regular_hourly" validate:"gte=0"`     // hourly rate for บัณฑิต regular (50 ฿/hr per ประกาศ)
	GradSpecialTermCap      float64 `json:"grad_special_term_cap" validate:"gte=0"`       // per TA × course × term cap for บัณฑิต special (12,000 ฿)
	DailyPayCapBaht         float64 `json:"daily_pay_cap_baht" validate:"gte=0"`          // อัตราค่าตอบแทน hourly ≤ 300 ฿/วัน
	UGRegularDailyHourCap   float64 `json:"ug_regular_daily_hour_cap" validate:"gte=0"`   // ป.ตรี ปกติ ≤ 7 hrs/วัน
	UGSpecialDailyHourCap   float64 `json:"ug_special_daily_hour_cap" validate:"gte=0"`   // ป.ตรี พิเศษ ≤ 6 hrs/วัน
	GradRegularDailyHourCap float64 `json:"grad_regular_daily_hour_cap" validate:"gte=0"` // บัณฑิต ปกติ ≤ 6 hrs/วัน
	// New (migration 0040) — ประกาศระบุ ป.ตรี ภาคพิเศษ "50 ฿/ชม. หรือ 2,000 ฿/เดือน":
	// จ่ายรายชั่วโมงตามจริง แต่ไม่เกินเพดานนี้ต่อเดือน/คน/วิชา.
	UGSpecialMonthlyCap float64 `json:"ug_special_monthly_cap" validate:"gte=0"`
	// TA planning ratios (migration 0112) — the planner's guideline (one TA per
	// N students), its density ceiling (never more than one per M), and the
	// guideline's headcount cap (0 = none). Staff-editable so the college can
	// move them without a release.
	PlanStudentsPerTA    int     `json:"plan_students_per_ta" validate:"gte=0"`
	PlanMinStudentsPerTA int     `json:"plan_min_students_per_ta" validate:"gte=0"`
	PlanSuggestedTACap   int     `json:"plan_suggested_ta_cap" validate:"gte=0"`
	Note                 *string `json:"note,omitempty" validate:"omitempty,max=500"`
}

func (s *CourseService) LatestPayRate(ctx context.Context) (*PayRate, error) {
	pr := &PayRate{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, TO_CHAR(effective_from,'YYYY-MM-DD'),
		       undergrad_regular, undergrad_special, graduate_regular, graduate_special_lumpsum,
		       ug_lecture_hours_per_credit, ug_lab_hours_per_credit,
		       baseline_students_lecture, baseline_students_lab,
		       ug_workload_rate_regular, term_months,
		       ug_max_hours_per_day, max_courses_per_student,
		       graduate_regular_hourly, grad_special_term_cap, daily_pay_cap_baht,
		       ug_regular_daily_hour_cap, ug_special_daily_hour_cap, grad_regular_daily_hour_cap,
		       ug_special_monthly_cap,
		       plan_students_per_ta, plan_min_students_per_ta, plan_suggested_ta_cap, note
		FROM pay_rates ORDER BY effective_from DESC LIMIT 1`).Scan(
		&pr.ID, &pr.EffectiveFrom, &pr.UndergradRegular, &pr.UndergradSpecial,
		&pr.GraduateRegular, &pr.GraduateSpecialLumpsum,
		&pr.UGLectureHoursPerCredit, &pr.UGLabHoursPerCredit,
		&pr.BaselineStudentsLecture, &pr.BaselineStudentsLab,
		&pr.UGWorkloadRateRegular, &pr.TermMonths,
		&pr.UGMaxHoursPerDay, &pr.MaxCoursesPerStudent,
		&pr.GraduateRegularHourly, &pr.GradSpecialTermCap, &pr.DailyPayCapBaht,
		&pr.UGRegularDailyHourCap, &pr.UGSpecialDailyHourCap, &pr.GradRegularDailyHourCap,
		&pr.UGSpecialMonthlyCap,
		&pr.PlanStudentsPerTA, &pr.PlanMinStudentsPerTA, &pr.PlanSuggestedTACap, &pr.Note)
	if err != nil {
		return nil, err
	}
	return pr, nil
}

func (s *CourseService) UpsertPayRate(ctx context.Context, actor uuid.UUID, in PayRate) (*PayRate, error) {
	in.ID = uuid.New()
	// Reject negatives on every numeric field. 0 is allowed only for the fields
	// below that treat it as "use default"; the defaults block then fills them in.
	if in.UndergradRegular < 0 || in.UndergradSpecial < 0 ||
		in.GraduateRegular < 0 || in.GraduateSpecialLumpsum < 0 ||
		in.UGLectureHoursPerCredit < 0 || in.UGLabHoursPerCredit < 0 ||
		in.BaselineStudentsLecture < 0 || in.BaselineStudentsLab < 0 ||
		in.UGWorkloadRateRegular < 0 ||
		in.TermMonths < 0 || in.UGMaxHoursPerDay < 0 || in.MaxCoursesPerStudent < 0 ||
		in.GraduateRegularHourly < 0 || in.GradSpecialTermCap < 0 || in.DailyPayCapBaht < 0 ||
		in.UGRegularDailyHourCap < 0 || in.UGSpecialDailyHourCap < 0 || in.GradRegularDailyHourCap < 0 ||
		in.UGSpecialMonthlyCap < 0 ||
		in.PlanStudentsPerTA < 0 || in.PlanMinStudentsPerTA < 0 || in.PlanSuggestedTACap < 0 {
		return nil, Invalid("ค่าตัวเลขต้องไม่ติดลบ")
	}
	// Planning ratios: 0 means "use the default" for the two divisors (a zero
	// divisor is not a policy); the cap's 0 genuinely means uncapped.
	if in.PlanStudentsPerTA == 0 {
		in.PlanStudentsPerTA = 25
	}
	if in.PlanMinStudentsPerTA == 0 {
		in.PlanMinStudentsPerTA = 15
	}
	if in.PlanMinStudentsPerTA > in.PlanStudentsPerTA {
		return nil, Invalid("เพดานความหนาแน่น TA (นศ. ขั้นต่ำต่อ TA) ต้องไม่มากกว่าเกณฑ์ นศ. ต่อ TA")
	}
	// Actual payment rates have no default — a 0 rate breaks payroll.
	if in.UndergradRegular <= 0 || in.UndergradSpecial <= 0 ||
		in.GraduateRegular <= 0 || in.GraduateSpecialLumpsum <= 0 {
		return nil, Invalid("อัตราจ่ายต้องเป็นค่ามากกว่า 0")
	}
	// Term length must be within a sane academic range (0 = use default).
	if in.TermMonths > 12 {
		return nil, Invalid("จำนวนเดือนต่อภาคเรียนต้องอยู่ระหว่าง 1 ถึง 12")
	}
	// Fill defaults if the client didn't send the new fields
	if in.UGLectureHoursPerCredit == 0 {
		in.UGLectureHoursPerCredit = 3
	}
	if in.UGLabHoursPerCredit == 0 {
		in.UGLabHoursPerCredit = 4.5
	}
	if in.BaselineStudentsLecture == 0 {
		in.BaselineStudentsLecture = 60
	}
	if in.BaselineStudentsLab == 0 {
		in.BaselineStudentsLab = 30
	}
	if in.UGWorkloadRateRegular == 0 {
		// 300 = 50%×200 (ตรี) + 50%×400 (บัณฑิต), per ชีต "2_59 ป.ตรี".
		// This defaulted to 200 and would have quietly re-introduced the
		// one-third-low course ceiling every time staff saved the rate form
		// with the field left blank.
		in.UGWorkloadRateRegular = 300
	}
	if in.TermMonths == 0 {
		in.TermMonths = 4
	}
	if in.UGMaxHoursPerDay == 0 {
		in.UGMaxHoursPerDay = 7
	}
	if in.MaxCoursesPerStudent == 0 {
		in.MaxCoursesPerStudent = 3
	}
	// ประกาศ 731/2565 + 1080/2565 defaults for the new caps.
	if in.GraduateRegularHourly == 0 {
		in.GraduateRegularHourly = 50
	}
	if in.GradSpecialTermCap == 0 {
		in.GradSpecialTermCap = 12000
	}
	if in.DailyPayCapBaht == 0 {
		in.DailyPayCapBaht = 300
	}
	if in.UGRegularDailyHourCap == 0 {
		in.UGRegularDailyHourCap = 7
	}
	if in.UGSpecialDailyHourCap == 0 {
		in.UGSpecialDailyHourCap = 6
	}
	if in.GradRegularDailyHourCap == 0 {
		in.GradRegularDailyHourCap = 6
	}
	if in.UGSpecialMonthlyCap == 0 {
		in.UGSpecialMonthlyCap = 2000
	}
	// A new pay_rates row supersedes the one that was in force, so the rate it
	// REPLACED is the before-image — without it the trail says "the graduate
	// rate is 60" and never that it used to be 50. Read inside the transaction
	// so a concurrent insert cannot slip between the two.
	prev, err := s.latestPayRateSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	entry := audit.Entry{ActorID: &actor, Action: "pay_rate.create", Entity: "pay_rate",
		EntityID: in.ID.String(), After: in}
	if prev != nil {
		entry.Before = prev
	}
	if err := writeAudited(ctx, s.pool, s.aud, entry,
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
		INSERT INTO pay_rates (id, effective_from, undergrad_regular, undergrad_special,
		    graduate_regular, graduate_special_lumpsum,
		    ug_lecture_hours_per_credit, ug_lab_hours_per_credit,
		    baseline_students_lecture, baseline_students_lab,
		    ug_workload_rate_regular, term_months,
		    ug_max_hours_per_day, max_courses_per_student,
		    graduate_regular_hourly, grad_special_term_cap, daily_pay_cap_baht,
		    ug_regular_daily_hour_cap, ug_special_daily_hour_cap, grad_regular_daily_hour_cap,
		    ug_special_monthly_cap, plan_students_per_ta, plan_min_students_per_ta, plan_suggested_ta_cap, note)
		 VALUES ($1,$2::date,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)`,
				in.ID, in.EffectiveFrom, in.UndergradRegular, in.UndergradSpecial,
				in.GraduateRegular, in.GraduateSpecialLumpsum,
				in.UGLectureHoursPerCredit, in.UGLabHoursPerCredit,
				in.BaselineStudentsLecture, in.BaselineStudentsLab,
				in.UGWorkloadRateRegular, in.TermMonths,
				in.UGMaxHoursPerDay, in.MaxCoursesPerStudent,
				in.GraduateRegularHourly, in.GradSpecialTermCap, in.DailyPayCapBaht,
				in.UGRegularDailyHourCap, in.UGSpecialDailyHourCap, in.GradRegularDailyHourCap,
				in.UGSpecialMonthlyCap, in.PlanStudentsPerTA, in.PlanMinStudentsPerTA, in.PlanSuggestedTACap, in.Note)
			return err
		}); err != nil {
		return nil, err
	}
	return &in, nil
}

type BudgetCap struct {
	ID            uuid.UUID `json:"id"`
	EffectiveFrom string    `json:"effective_from" validate:"required"`
	PerCourseMax  float64   `json:"per_course_max" validate:"gte=0"`
	Note          *string   `json:"note,omitempty" validate:"omitempty,max=500"`
}

func (s *CourseService) LatestBudgetCap(ctx context.Context) (*BudgetCap, error) {
	b := &BudgetCap{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, TO_CHAR(effective_from,'YYYY-MM-DD'), per_course_max, note
		 FROM budget_caps ORDER BY effective_from DESC LIMIT 1`).Scan(&b.ID, &b.EffectiveFrom, &b.PerCourseMax, &b.Note)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (s *CourseService) UpsertBudgetCap(ctx context.Context, actor uuid.UUID, in BudgetCap) (*BudgetCap, error) {
	if in.PerCourseMax < 0 {
		return nil, Invalid("จำนวนเงินต้องไม่ติดลบ")
	}
	in.ID = uuid.New()
	prevCap, err := s.latestBudgetCapSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	capEntry := audit.Entry{ActorID: &actor, Action: "budget_cap.create", Entity: "budget_cap",
		EntityID: in.ID.String(), After: in}
	if prevCap != nil {
		capEntry.Before = prevCap
	}
	if err := writeAudited(ctx, s.pool, s.aud, capEntry,
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO budget_caps (id, effective_from, per_course_max, note) VALUES ($1,$2::date,$3,$4)`,
				in.ID, in.EffectiveFrom, in.PerCourseMax, in.Note)
			return err
		}); err != nil {
		return nil, err
	}
	return &in, nil
}

// latestPayRateSnapshot returns the rate currently in force, as a plain map, or
// nil when this is the first one ever set.
//
// Read as a whole row rather than a chosen list of columns: pay_rates has
// grown a dozen caps and ceilings over the project's life, and a hand-picked
// list would go stale the next time one is added — silently, and only visible
// years later when somebody asks what a cap used to be.
func (s *CourseService) latestPayRateSnapshot(ctx context.Context) (map[string]any, error) {
	return latestRowSnapshot(ctx, s.pool, "pay_rates")
}

func (s *CourseService) latestBudgetCapSnapshot(ctx context.Context) (map[string]any, error) {
	return latestRowSnapshot(ctx, s.pool, "budget_caps")
}
