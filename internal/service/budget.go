package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BudgetService encapsulates budget & hour cap calculations for a teaching course.
type BudgetService struct {
	pool *pgxpool.Pool
}

type BudgetRates struct {
	UGLectureHoursPerCredit float64 `json:"ug_lecture_hours_per_credit"`
	UGLabHoursPerCredit     float64 `json:"ug_lab_hours_per_credit"`
	BaselineStudentsLecture int     `json:"baseline_students_lecture"`
	BaselineStudentsLab     int     `json:"baseline_students_lab"`
	UGWorkloadRateRegular   float64 `json:"ug_workload_rate_regular"`
	UGWorkloadRateSpecial   float64 `json:"ug_workload_rate_special"`
	GraduateRegularLumpsum  float64 `json:"graduate_regular"`
	GraduateSpecialLumpsum  float64 `json:"graduate_special_lumpsum"`
	TermMonths              int     `json:"term_months"`
}

type BudgetSnapshot struct {
	TeachingCourseID    uuid.UUID `json:"teaching_course_id"`
	NumStudents         int       `json:"num_students"`          // = regular + special (aggregate)
	NumStudentsRegular  int       `json:"num_students_regular"`
	NumStudentsSpecial  int       `json:"num_students_special"`
	Credits             int       `json:"credits"`
	LectureCredits      int       `json:"lecture_credits"`
	LabCredits          int       `json:"lab_credits"`
	PerCourseMaxBaht    float64   `json:"per_course_max"` // from budget_caps
	// Aggregate (regular + special)
	WeeklyWorkload      float64   `json:"weekly_workload_hours"`
	MonthlyPay          float64   `json:"monthly_pay_baht"`
	TermPay             float64   `json:"term_pay_baht"`
	// Breakdown by track (per Excel: same rate, different student count)
	WeeklyWorkloadRegular float64 `json:"weekly_workload_regular"`
	MonthlyPayRegular     float64 `json:"monthly_pay_regular"`
	TermPayRegular        float64 `json:"term_pay_regular"`
	WeeklyWorkloadSpecial float64 `json:"weekly_workload_special"`
	MonthlyPaySpecial     float64 `json:"monthly_pay_special"`
	TermPaySpecial        float64 `json:"term_pay_special"`
	Rates               BudgetRates `json:"rates"`
	SuggestedTAs        struct {
		Undergrad int `json:"undergrad"`
		Graduate  int `json:"graduate"`
	} `json:"suggested_tas"`
	OverBudget    bool    `json:"over_budget"`
	UsedBaht      float64 `json:"used_baht"`
	RemainingBaht float64 `json:"remaining_baht"`
}

// Compute a budget snapshot using the undergrad formula from the historical
// planning workbook (ชีต "2_59 ป.ตรี"):
//
//	weekly_workload_track = (lecture_credits × ug_lec_hrs_per_credit × students_track/60)
//	                      + (lab_credits     × ug_lab_hrs_per_credit × students_track/30)
//	monthly_pay_track     = weekly_workload_track × ug_workload_rate_regular
//	term_pay_track        = monthly_pay_track × term_months
//
// where "track" is regular OR special — the formula is identical, only the
// student count differs (num_students_regular vs num_students_special).
// The effective rate (default 300 = 50% × 200 + 50% × 400) already blends
// undergrad/graduate TA rates per the Excel weighting.
//
// Graduate TA compensation is FLAT (lump-sum), not computed by this formula.
func (s *BudgetService) Compute(ctx context.Context, tcID uuid.UUID) (*BudgetSnapshot, error) {
	snap := &BudgetSnapshot{TeachingCourseID: tcID}
	err := s.pool.QueryRow(ctx, `
		SELECT tc.num_students, tc.num_students_regular, tc.num_students_special,
		       fc.credits, fc.lecture_hrs, fc.lab_hrs
		FROM teaching_courses tc JOIN faculty_courses fc ON fc.id = tc.faculty_course_id
		WHERE tc.id = $1`, tcID).Scan(&snap.NumStudents, &snap.NumStudentsRegular, &snap.NumStudentsSpecial,
		&snap.Credits, &snap.LectureCredits, &snap.LabCredits)
	if err != nil {
		return nil, err
	}
	// If regular/special not filled yet, treat aggregate num_students as regular
	// so budget stays computable during migration.
	if snap.NumStudentsRegular == 0 && snap.NumStudentsSpecial == 0 && snap.NumStudents > 0 {
		snap.NumStudentsRegular = snap.NumStudents
	}
	// per_course_max is derived from the formula (weekly workload × rate × months)
	// — set below after workload is computed. No more manual budget_caps.

	rates := BudgetRates{}
	if err := s.pool.QueryRow(ctx, `
		SELECT ug_lecture_hours_per_credit, ug_lab_hours_per_credit,
		       baseline_students_lecture, baseline_students_lab,
		       ug_workload_rate_regular, ug_workload_rate_special,
		       graduate_regular, graduate_special_lumpsum, term_months
		FROM pay_rates ORDER BY effective_from DESC LIMIT 1`).Scan(
		&rates.UGLectureHoursPerCredit, &rates.UGLabHoursPerCredit,
		&rates.BaselineStudentsLecture, &rates.BaselineStudentsLab,
		&rates.UGWorkloadRateRegular, &rates.UGWorkloadRateSpecial,
		&rates.GraduateRegularLumpsum, &rates.GraduateSpecialLumpsum, &rates.TermMonths,
	); err == nil {
		snap.Rates = rates
	}
	// Prefer per-term months (source of truth); fall back to pay_rates.term_months.
	var termMonths int
	_ = s.pool.QueryRow(ctx, `
		SELECT t.months FROM academic_terms t
		JOIN teaching_courses tc ON tc.term_id = t.id WHERE tc.id = $1`, tcID).Scan(&termMonths)
	if termMonths > 0 {
		rates.TermMonths = termMonths
		snap.Rates.TermMonths = termMonths
	}

	// Weekly workload per Excel — identical formula for regular vs special,
	// only the student count differs.
	workload := func(students int) float64 {
		var lec, lab float64
		if snap.LectureCredits > 0 && rates.BaselineStudentsLecture > 0 {
			lec = float64(snap.LectureCredits) * rates.UGLectureHoursPerCredit *
				(float64(students) / float64(rates.BaselineStudentsLecture))
		}
		if snap.LabCredits > 0 && rates.BaselineStudentsLab > 0 {
			lab = float64(snap.LabCredits) * rates.UGLabHoursPerCredit *
				(float64(students) / float64(rates.BaselineStudentsLab))
		}
		return lec + lab
	}
	snap.WeeklyWorkloadRegular = workload(snap.NumStudentsRegular)
	snap.WeeklyWorkloadSpecial = workload(snap.NumStudentsSpecial)
	snap.WeeklyWorkload = snap.WeeklyWorkloadRegular + snap.WeeklyWorkloadSpecial

	// Same effective rate for both tracks (default 300 = weighted 50/50 of 200+400).
	rate := rates.UGWorkloadRateRegular
	snap.MonthlyPayRegular = snap.WeeklyWorkloadRegular * rate
	snap.MonthlyPaySpecial = snap.WeeklyWorkloadSpecial * rate
	snap.MonthlyPay = snap.MonthlyPayRegular + snap.MonthlyPaySpecial
	if rates.TermMonths > 0 {
		m := float64(rates.TermMonths)
		snap.TermPayRegular = snap.MonthlyPayRegular * m
		snap.TermPaySpecial = snap.MonthlyPaySpecial * m
		snap.TermPay = snap.MonthlyPay * m
	}
	// Per-course budget cap is DERIVED from the formula (weekly workload × rate × months)
	// — no more manual budget_caps setting. Total term pay across both tracks = the cap.
	snap.PerCourseMaxBaht = snap.TermPay

	// Suggested TAs: informational only. Use aggregate student count.
	totalStudents := snap.NumStudentsRegular + snap.NumStudentsSpecial
	if totalStudents == 0 {
		totalStudents = snap.NumStudents
	}
	snap.SuggestedTAs.Undergrad = min(3, (totalStudents+24)/25)
	snap.SuggestedTAs.Graduate = min(2, totalStudents/60)

	// Used baht — reflects post-2026 payment model (ประกาศ 731/2565 + 1080/2565):
	//   Undergrad: SUM(hours × per-track hourly rate) from approved work_logs.
	//   Grad regular: SUM(hours × graduate_regular_hourly) — now hourly (was lump-sum).
	//   Grad special: flat 4,000฿/month × term months, capped at grad_special_term_cap
	//                 (12,000฿/TA/course/term). Counted per assignment.
	// Months come from academic_terms (per-term); fall back to pay_rates.term_months.
	_ = s.pool.QueryRow(ctx, `
        WITH latest AS (SELECT * FROM pay_rates ORDER BY effective_from DESC LIMIT 1),
             tm AS (
                SELECT COALESCE(NULLIF(t.months,0), (SELECT term_months FROM latest)) AS months
                FROM academic_terms t
                JOIN teaching_courses tc ON tc.term_id = t.id
                WHERE tc.id = $1
             )
        SELECT COALESCE(
            (SELECT COALESCE(SUM(wl.hours *
                CASE WHEN sec.track = 'regular' THEN pr.undergrad_regular
                     ELSE pr.undergrad_special END
             ), 0)
             FROM work_logs wl
             JOIN ta_request_assignments a ON a.id = wl.assignment_id
             JOIN ta_requests r  ON r.id = a.request_id
             JOIN sections sec   ON sec.id = a.section_id
             JOIN users u        ON u.id = a.ta_id
             CROSS JOIN latest pr
             WHERE r.teaching_course_id = $1 AND wl.status = 'approved' AND u.study_level = 'undergrad')
          +
            (SELECT COALESCE(SUM(wl.hours * pr.graduate_regular_hourly), 0)
             FROM work_logs wl
             JOIN ta_request_assignments a ON a.id = wl.assignment_id
             JOIN ta_requests r  ON r.id = a.request_id
             JOIN sections sec   ON sec.id = a.section_id
             JOIN users u        ON u.id = a.ta_id
             CROSS JOIN latest pr
             WHERE r.teaching_course_id = $1 AND wl.status = 'approved'
               AND u.study_level IN ('master','phd') AND sec.track = 'regular')
          +
            (SELECT COALESCE(SUM(
                LEAST(pr.graduate_special_lumpsum * (SELECT months FROM tm),
                      pr.grad_special_term_cap)
             ), 0)
             FROM ta_request_assignments a
             JOIN ta_requests r  ON r.id = a.request_id AND r.status = 'approved'
             JOIN sections sec   ON sec.id = a.section_id
             JOIN users u        ON u.id = a.ta_id
             CROSS JOIN latest pr
             WHERE r.teaching_course_id = $1 AND u.study_level IN ('master','phd')
               AND sec.track = 'special')
        , 0)`, tcID).Scan(&snap.UsedBaht)

	snap.RemainingBaht = snap.PerCourseMaxBaht - snap.UsedBaht
	snap.OverBudget = snap.PerCourseMaxBaht > 0 && snap.RemainingBaht < 0
	return snap, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
