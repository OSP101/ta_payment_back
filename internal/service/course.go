package service

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

type FacultyCourse struct {
	ID   uuid.UUID `json:"id"`
	Code string    `json:"code"`
	// Level is the course's teaching level: "undergrad" (ปริญญาตรี) or
	// "graduate" (บัณฑิตศึกษา). Drives how the appointment order groups courses.
	Level      string  `json:"level"`
	NameTH     string  `json:"name_th"`
	NameEN     *string `json:"name_en,omitempty"`
	Credits    int     `json:"credits"`
	LectureHrs int     `json:"lecture_hrs"`
	LabHrs     int     `json:"lab_hrs"`
	SelfHrs    int     `json:"self_hrs"`
	Department *string `json:"department,omitempty"`
	IsActive   bool    `json:"is_active"`
}

type CourseService struct {
	pool *pgxpool.Pool
	aud  *audit.Auditor
}

func (s *CourseService) List(ctx context.Context, search string) ([]FacultyCourse, error) {
	q := `SELECT id, code, level, name_th, name_en, credits, lecture_hrs, lab_hrs, self_hrs, department, is_active
	      FROM faculty_courses WHERE ($1 = '' OR code ILIKE '%'||$1||'%' OR name_th ILIKE '%'||$1||'%' OR name_en ILIKE '%'||$1||'%')
		  ORDER BY code`
	rows, err := s.pool.Query(ctx, q, search)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FacultyCourse{}
	for rows.Next() {
		var c FacultyCourse
		if err := rows.Scan(&c.ID, &c.Code, &c.Level, &c.NameTH, &c.NameEN, &c.Credits, &c.LectureHrs, &c.LabHrs, &c.SelfHrs, &c.Department, &c.IsActive); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *CourseService) Upsert(ctx context.Context, actor uuid.UUID, in FacultyCourse) (*FacultyCourse, error) {
	in.Code = strings.TrimSpace(in.Code)
	if in.Code == "" {
		return nil, Invalid("กรุณาระบุรหัสวิชา")
	}
	if in.Credits < 0 || in.LectureHrs < 0 || in.LabHrs < 0 || in.SelfHrs < 0 {
		return nil, Invalid("หน่วยกิตและจำนวนชั่วโมงต้องไม่ติดลบ")
	}
	// Level defaults to undergrad; only the two known values are accepted.
	if in.Level == "" {
		in.Level = "undergrad"
	}
	if in.Level != "undergrad" && in.Level != "graduate" {
		return nil, Invalid("ระดับวิชาต้องเป็นปริญญาตรีหรือบัณฑิตศึกษา")
	}
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
		_, err := s.pool.Exec(ctx,
			`INSERT INTO faculty_courses (id, code, level, name_th, name_en, credits, lecture_hrs, lab_hrs, self_hrs, department, is_active)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			in.ID, in.Code, in.Level, in.NameTH, in.NameEN, in.Credits, in.LectureHrs, in.LabHrs, in.SelfHrs, in.Department, in.IsActive)
		if err != nil {
			return nil, err
		}
		s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "faculty_course.create", Entity: "faculty_course", EntityID: in.ID.String(), After: in})
		return &in, nil
	}
	res, err := s.pool.Exec(ctx,
		`UPDATE faculty_courses SET code=$2, level=$3, name_th=$4, name_en=$5, credits=$6, lecture_hrs=$7, lab_hrs=$8, self_hrs=$9, department=$10, is_active=$11, updated_at=NOW()
		 WHERE id=$1`,
		in.ID, in.Code, in.Level, in.NameTH, in.NameEN, in.Credits, in.LectureHrs, in.LabHrs, in.SelfHrs, in.Department, in.IsActive)
	if err != nil {
		return nil, err
	}
	if res.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "faculty_course.update", Entity: "faculty_course", EntityID: in.ID.String(), After: in})
	return &in, nil
}

func (s *CourseService) Delete(ctx context.Context, actor, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE faculty_courses SET is_active=FALSE, updated_at=NOW() WHERE id=$1`, id)
	if err == nil {
		s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "faculty_course.deactivate", Entity: "faculty_course", EntityID: id.String()})
	}
	return err
}

// Settings

type PayRate struct {
	ID                      uuid.UUID `json:"id"`
	EffectiveFrom           string    `json:"effective_from"`
	UndergradRegular        float64   `json:"undergrad_regular"`         // hourly, ประกาศ 731/2565 = 40 ฿/hr
	UndergradSpecial        float64   `json:"undergrad_special"`         // hourly, ประกาศ 1080/2565 = 50 ฿/hr
	// Deprecated: kept for rollback safety. Payment/budget now read GraduateRegularHourly.
	GraduateRegular         float64   `json:"graduate_regular"`
	GraduateSpecialLumpsum  float64   `json:"graduate_special_lumpsum"`  // monthly lump-sum, ประกาศ = 4,000 ฿/เดือน
	// Undergrad budget formula constants (from historical Excel workbook):
	UGLectureHoursPerCredit float64   `json:"ug_lecture_hours_per_credit"`
	UGLabHoursPerCredit     float64   `json:"ug_lab_hours_per_credit"`
	BaselineStudentsLecture int       `json:"baseline_students_lecture"`
	BaselineStudentsLab     int       `json:"baseline_students_lab"`
	// Effective monthly budget rate per weekly-workload-hour.
	// Per Excel formula: default 300 = 50% × 200 (ตรี TA) + 50% × 400 (บัณฑิต TA).
	// Applied to BOTH regular and special sections — tracks differ only in student count.
	UGWorkloadRateRegular   float64   `json:"ug_workload_rate_regular"`
	// Deprecated: kept for backward compatibility. Budget calc no longer reads this.
	UGWorkloadRateSpecial   float64   `json:"ug_workload_rate_special"`
	TermMonths              int       `json:"term_months"`
	// Policy limits (advisory) — from the official regulation notes.
	UGMaxHoursPerDay        int       `json:"ug_max_hours_per_day"`      // undergrad regular: max hrs/day (7)
	MaxCoursesPerStudent    int       `json:"max_courses_per_student"`   // any student: max concurrent TA courses (3)
	// New (migration 0018) — per-track daily caps + graduate hourly rate + term/day money caps.
	GraduateRegularHourly    float64  `json:"graduate_regular_hourly"`   // hourly rate for บัณฑิต regular (50 ฿/hr per ประกาศ)
	GradSpecialTermCap       float64  `json:"grad_special_term_cap"`     // per TA × course × term cap for บัณฑิต special (12,000 ฿)
	DailyPayCapBaht          float64  `json:"daily_pay_cap_baht"`        // อัตราค่าตอบแทน hourly ≤ 300 ฿/วัน
	UGRegularDailyHourCap    float64  `json:"ug_regular_daily_hour_cap"` // ป.ตรี ปกติ ≤ 7 hrs/วัน
	UGSpecialDailyHourCap    float64  `json:"ug_special_daily_hour_cap"` // ป.ตรี พิเศษ ≤ 6 hrs/วัน
	GradRegularDailyHourCap  float64  `json:"grad_regular_daily_hour_cap"` // บัณฑิต ปกติ ≤ 6 hrs/วัน
	Note                    *string   `json:"note,omitempty"`
}

func (s *CourseService) LatestPayRate(ctx context.Context) (*PayRate, error) {
	pr := &PayRate{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, TO_CHAR(effective_from,'YYYY-MM-DD'),
		       undergrad_regular, undergrad_special, graduate_regular, graduate_special_lumpsum,
		       ug_lecture_hours_per_credit, ug_lab_hours_per_credit,
		       baseline_students_lecture, baseline_students_lab,
		       ug_workload_rate_regular, ug_workload_rate_special, term_months,
		       ug_max_hours_per_day, max_courses_per_student,
		       graduate_regular_hourly, grad_special_term_cap, daily_pay_cap_baht,
		       ug_regular_daily_hour_cap, ug_special_daily_hour_cap, grad_regular_daily_hour_cap,
		       note
		FROM pay_rates ORDER BY effective_from DESC LIMIT 1`).Scan(
		&pr.ID, &pr.EffectiveFrom, &pr.UndergradRegular, &pr.UndergradSpecial,
		&pr.GraduateRegular, &pr.GraduateSpecialLumpsum,
		&pr.UGLectureHoursPerCredit, &pr.UGLabHoursPerCredit,
		&pr.BaselineStudentsLecture, &pr.BaselineStudentsLab,
		&pr.UGWorkloadRateRegular, &pr.UGWorkloadRateSpecial, &pr.TermMonths,
		&pr.UGMaxHoursPerDay, &pr.MaxCoursesPerStudent,
		&pr.GraduateRegularHourly, &pr.GradSpecialTermCap, &pr.DailyPayCapBaht,
		&pr.UGRegularDailyHourCap, &pr.UGSpecialDailyHourCap, &pr.GradRegularDailyHourCap,
		&pr.Note)
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
		in.UGWorkloadRateRegular < 0 || in.UGWorkloadRateSpecial < 0 ||
		in.TermMonths < 0 || in.UGMaxHoursPerDay < 0 || in.MaxCoursesPerStudent < 0 ||
		in.GraduateRegularHourly < 0 || in.GradSpecialTermCap < 0 || in.DailyPayCapBaht < 0 ||
		in.UGRegularDailyHourCap < 0 || in.UGSpecialDailyHourCap < 0 || in.GradRegularDailyHourCap < 0 {
		return nil, Invalid("ค่าตัวเลขต้องไม่ติดลบ")
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
	if in.UGLectureHoursPerCredit == 0 { in.UGLectureHoursPerCredit = 3 }
	if in.UGLabHoursPerCredit == 0     { in.UGLabHoursPerCredit = 4.5 }
	if in.BaselineStudentsLecture == 0 { in.BaselineStudentsLecture = 60 }
	if in.BaselineStudentsLab == 0     { in.BaselineStudentsLab = 30 }
	if in.UGWorkloadRateRegular == 0   { in.UGWorkloadRateRegular = 200 }
	if in.UGWorkloadRateSpecial == 0   { in.UGWorkloadRateSpecial = 250 }
	if in.TermMonths == 0              { in.TermMonths = 4 }
	if in.UGMaxHoursPerDay == 0        { in.UGMaxHoursPerDay = 7 }
	if in.MaxCoursesPerStudent == 0    { in.MaxCoursesPerStudent = 3 }
	// ประกาศ 731/2565 + 1080/2565 defaults for the new caps.
	if in.GraduateRegularHourly == 0    { in.GraduateRegularHourly = 50 }
	if in.GradSpecialTermCap == 0       { in.GradSpecialTermCap = 12000 }
	if in.DailyPayCapBaht == 0          { in.DailyPayCapBaht = 300 }
	if in.UGRegularDailyHourCap == 0    { in.UGRegularDailyHourCap = 7 }
	if in.UGSpecialDailyHourCap == 0    { in.UGSpecialDailyHourCap = 6 }
	if in.GradRegularDailyHourCap == 0  { in.GradRegularDailyHourCap = 6 }
	_, err := s.pool.Exec(ctx, `
		INSERT INTO pay_rates (id, effective_from, undergrad_regular, undergrad_special,
		    graduate_regular, graduate_special_lumpsum,
		    ug_lecture_hours_per_credit, ug_lab_hours_per_credit,
		    baseline_students_lecture, baseline_students_lab,
		    ug_workload_rate_regular, ug_workload_rate_special, term_months,
		    ug_max_hours_per_day, max_courses_per_student,
		    graduate_regular_hourly, grad_special_term_cap, daily_pay_cap_baht,
		    ug_regular_daily_hour_cap, ug_special_daily_hour_cap, grad_regular_daily_hour_cap,
		    note)
		 VALUES ($1,$2::date,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`,
		in.ID, in.EffectiveFrom, in.UndergradRegular, in.UndergradSpecial,
		in.GraduateRegular, in.GraduateSpecialLumpsum,
		in.UGLectureHoursPerCredit, in.UGLabHoursPerCredit,
		in.BaselineStudentsLecture, in.BaselineStudentsLab,
		in.UGWorkloadRateRegular, in.UGWorkloadRateSpecial, in.TermMonths,
		in.UGMaxHoursPerDay, in.MaxCoursesPerStudent,
		in.GraduateRegularHourly, in.GradSpecialTermCap, in.DailyPayCapBaht,
		in.UGRegularDailyHourCap, in.UGSpecialDailyHourCap, in.GradRegularDailyHourCap,
		in.Note)
	if err != nil {
		return nil, err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "pay_rate.create", Entity: "pay_rate", EntityID: in.ID.String(), After: in})
	return &in, nil
}

type BudgetCap struct {
	ID            uuid.UUID `json:"id"`
	EffectiveFrom string    `json:"effective_from"`
	PerCourseMax  float64   `json:"per_course_max"`
	Note          *string   `json:"note,omitempty"`
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
	_, err := s.pool.Exec(ctx,
		`INSERT INTO budget_caps (id, effective_from, per_course_max, note) VALUES ($1,$2::date,$3,$4)`,
		in.ID, in.EffectiveFrom, in.PerCourseMax, in.Note)
	if err != nil {
		return nil, err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "budget_cap.create", Entity: "budget_cap", EntityID: in.ID.String(), After: in})
	return &in, nil
}
