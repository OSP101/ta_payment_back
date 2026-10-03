package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// loadBudgetRates reads the formula constants in force plus the course's own
// term months (the source of truth; pay_rates.term_months is only the
// fallback). A missing pay_rates row leaves the zero value, which Compute has
// always treated as "no budget" rather than an error.
//
// Takes a querier so an edit can price the course INSIDE its own transaction —
// see guardBudgetChange, which compares the ceiling before and after a write.
func loadBudgetRates(ctx context.Context, q querier, tcID uuid.UUID) BudgetRates {
	rates := BudgetRates{}
	_ = q.QueryRow(ctx, `
		SELECT ug_lecture_hours_per_credit, ug_lab_hours_per_credit,
		       baseline_students_lecture, baseline_students_lab,
		       ug_workload_rate_regular,
		       graduate_regular, graduate_special_lumpsum, term_months
		FROM `+payRatesInForce+``).Scan(
		&rates.UGLectureHoursPerCredit, &rates.UGLabHoursPerCredit,
		&rates.BaselineStudentsLecture, &rates.BaselineStudentsLab,
		&rates.UGWorkloadRateRegular,
		&rates.GraduateRegularLumpsum, &rates.GraduateSpecialLumpsum, &rates.TermMonths,
	)
	var termMonths int
	_ = q.QueryRow(ctx, `
		SELECT t.months FROM academic_terms t
		JOIN teaching_courses tc ON tc.term_id = t.id WHERE tc.id = $1`, tcID).Scan(&termMonths)
	if termMonths > 0 {
		rates.TermMonths = termMonths
	}
	return rates
}

// ugWeeklyWorkload is the workbook's weekly-hours formula for one track —
// shared by Compute and courseFormulaBudget so the figure an edit is checked
// against is the figure every screen shows.
func ugWeeklyWorkload(lectureCredits, labCredits, students int, rates BudgetRates) float64 {
	var lec, lab float64
	if lectureCredits > 0 && rates.BaselineStudentsLecture > 0 {
		lec = float64(lectureCredits) * rates.UGLectureHoursPerCredit *
			(float64(students) / float64(rates.BaselineStudentsLecture))
	}
	if labCredits > 0 && rates.BaselineStudentsLab > 0 {
		lab = float64(labCredits) * rates.UGLabHoursPerCredit *
			(float64(students) / float64(rates.BaselineStudentsLab))
	}
	return lec + lab
}

// courseBudgetCapSQL is the term's shared เพดานงบรายวิชา for course alias tc
// (migration 0144) — NULL when the term sets none.
const courseBudgetCapSQL = `(SELECT t.course_budget_cap_baht::float8 FROM academic_terms t WHERE t.id = tc.term_id)`

// applyBudgetCap is the เพดานงบรายวิชา rule (migration 0144): nil = no cap,
// otherwise the budget is min(formula, cap). Shared by Compute and
// courseFormulaBudget so the figure an edit is checked against is the one
// every screen shows.
func applyBudgetCap(formula float64, budgetCap *float64) float64 {
	if budgetCap != nil && *budgetCap < formula {
		return *budgetCap
	}
	return formula
}

// courseFormulaBudget is Compute's PerCourseMaxBaht alone — the term ceiling
// the workload formula gives, lowered to the course's เพดานงบ when one is set —
// read through q, so a caller holding a transaction sees its own uncommitted
// edit.
func courseFormulaBudget(ctx context.Context, q querier, tcID uuid.UUID) (float64, error) {
	var total, regular, special, lecHrs, labHrs int
	var budgetCap *float64
	if err := q.QueryRow(ctx, `
		SELECT num_students, num_students_regular, num_students_special, lecture_hrs, lab_hrs,
		       `+courseBudgetCapSQL+`
		FROM teaching_courses tc WHERE id = $1`, tcID).Scan(&total, &regular, &special, &lecHrs, &labHrs, &budgetCap); err != nil {
		return 0, err
	}
	// Same fallbacks as Compute: lab credits are lab hours ÷ 2, and an
	// aggregate with no per-track split counts as regular.
	if regular == 0 && special == 0 && total > 0 {
		regular = total
	}
	rates := loadBudgetRates(ctx, q, tcID)
	if rates.TermMonths <= 0 {
		return 0, nil
	}
	weekly := ugWeeklyWorkload(lecHrs, labHrs/2, regular, rates) + ugWeeklyWorkload(lecHrs, labHrs/2, special, rates)
	return applyBudgetCap(weekly*rates.UGWorkloadRateRegular*float64(rates.TermMonths), budgetCap), nil
}

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
	GraduateRegularLumpsum  float64 `json:"graduate_regular"`
	GraduateSpecialLumpsum  float64 `json:"graduate_special_lumpsum"`
	TermMonths              int     `json:"term_months"`
}

type BudgetSnapshot struct {
	TeachingCourseID   uuid.UUID `json:"teaching_course_id"`
	NumStudents        int       `json:"num_students"` // = regular + special (aggregate)
	NumStudentsRegular int       `json:"num_students_regular"`
	NumStudentsSpecial int       `json:"num_students_special"`
	Credits            int       `json:"credits"`
	// LectureHrs / LabHrs are the weekly CONTACT HOURS from the registrar's
	// "3 (2-2-5)" — what teaching_courses actually stores. LectureCredits /
	// LabCredits are the CREDITS the budget formula wants, derived below. The
	// two were treated as the same number until 04/08/2026, which inflated the
	// ceiling of every course that has a lab.
	LectureHrs       int     `json:"lecture_hrs"`
	LabHrs           int     `json:"lab_hrs"`
	LectureCredits   int     `json:"lecture_credits"`
	LabCredits       int     `json:"lab_credits"`
	PerCourseMaxBaht float64 `json:"per_course_max"` // min(workload formula, BudgetCapBaht) — see Compute below
	// BudgetCapBaht is the term's shared เพดานงบรายวิชา (nil = ไม่กำหนด);
	// FormulaBaht is what the workload formula alone gives. CapApplied says the
	// cap is the lower of the two, so every money figure below was scaled to it.
	BudgetCapBaht *float64 `json:"budget_cap_baht"`
	FormulaBaht   float64  `json:"formula_baht"`
	CapApplied    bool     `json:"cap_applied"`
	// Aggregate (regular + special)
	WeeklyWorkload float64 `json:"weekly_workload_hours"`
	MonthlyPay     float64 `json:"monthly_pay_baht"`
	TermPay        float64 `json:"term_pay_baht"`
	// Breakdown by track (per Excel: same rate, different student count)
	WeeklyWorkloadRegular float64     `json:"weekly_workload_regular"`
	MonthlyPayRegular     float64     `json:"monthly_pay_regular"`
	TermPayRegular        float64     `json:"term_pay_regular"`
	WeeklyWorkloadSpecial float64     `json:"weekly_workload_special"`
	MonthlyPaySpecial     float64     `json:"monthly_pay_special"`
	TermPaySpecial        float64     `json:"term_pay_special"`
	Rates                 BudgetRates `json:"rates"`
	SuggestedTAs          struct {
		Undergrad int `json:"undergrad"`
		Graduate  int `json:"graduate"`
	} `json:"suggested_tas"`
	OverBudget bool    `json:"over_budget"`
	UsedBaht   float64 `json:"used_baht"`
	// UsedBaht split by track — same billing rules as UsedBaht (undergrad
	// hourly + grad regular hourly + grad special lumpsum), just kept apart
	// by which pool actually pays each part. Drives the dashboard card's
	// segmented usage bar (see LecturerOverview) so a lecturer glancing at
	// the card sees not just "how full" but "full with which pool's money".
	UsedBahtRegular float64 `json:"used_baht_regular"`
	UsedBahtSpecial float64 `json:"used_baht_special"`
	RemainingBaht   float64 `json:"remaining_baht"`
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
		       tc.credits, tc.lecture_hrs, tc.lab_hrs, `+courseBudgetCapSQL+`
		FROM teaching_courses tc
		WHERE tc.id = $1`, tcID).Scan(&snap.NumStudents, &snap.NumStudentsRegular, &snap.NumStudentsSpecial,
		&snap.Credits, &snap.LectureHrs, &snap.LabHrs, &snap.BudgetCapBaht)
	if err != nil {
		return nil, err
	}
	// Contact hours → credits, exactly as the faculty workbook derives them from
	// the course code (ชีต "2_59 ป.ตรี", cells D21/E21):
	//
	//	D21 = ROUNDDOWN((C-…)/100, 0)                → นก.บรรยาย = ชม.บรรยาย
	//	E21 = ROUNDDOWN(MOD(…,10) × 0.5, 0)          → นก.แล็บ  = ⌊ชม.แล็บ ÷ 2⌋
	//
	// One lecture hour a week is one credit; a lab credit is two hours. Feeding
	// lab HOURS in where the formula wants lab CREDITS doubled the lab term and
	// put SC362102's regular ceiling at 14,400 instead of the 9,000 the faculty
	// budgeted (30 นศ. × 300). Both of the college's own files — Ngamnij.xlsx
	// for 2569 and the 2560 workbook — reproduce exactly once this is right.
	snap.LectureCredits = snap.LectureHrs
	snap.LabCredits = snap.LabHrs / 2
	// If regular/special not filled yet, treat aggregate num_students as regular
	// so budget stays computable during migration.
	if snap.NumStudentsRegular == 0 && snap.NumStudentsSpecial == 0 && snap.NumStudents > 0 {
		snap.NumStudentsRegular = snap.NumStudents
	}
	// per_course_max is derived from the formula (weekly workload × rate × months)
	// — set below after workload is computed — and then lowered to the term's
	// เพดานงบรายวิชา when staff set one (migration 0144).

	rates := loadBudgetRates(ctx, s.pool, tcID)
	snap.Rates = rates

	// Weekly workload per Excel — identical formula for regular vs special,
	// only the student count differs.
	workload := func(students int) float64 {
		return ugWeeklyWorkload(snap.LectureCredits, snap.LabCredits, students, rates)
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
	// The ceiling is the formula (weekly workload × rate × months), lowered to
	// the เพดานงบ when one is set and smaller. Every money figure is scaled by
	// the same ratio so the regular/special pools still add up to the ceiling —
	// the export's two-pool cap and the TA planner read the per-track figures.
	snap.FormulaBaht = snap.TermPay
	if capped := applyBudgetCap(snap.TermPay, snap.BudgetCapBaht); capped < snap.TermPay {
		ratio := capped / snap.TermPay
		snap.TermPayRegular *= ratio
		snap.TermPaySpecial *= ratio
		snap.MonthlyPayRegular *= ratio
		snap.MonthlyPaySpecial *= ratio
		snap.MonthlyPay *= ratio
		snap.TermPay = capped
		snap.CapApplied = true
	}
	snap.PerCourseMaxBaht = snap.TermPay

	// Suggested TAs: informational only. Since 26/09/2026 this is the planner's
	// per-sitting rule (ta_recommend.go) rather than a course-wide ceil(n/25),
	// so the budget page, the planner and the dashboard give one answer.
	totalStudents := snap.NumStudentsRegular + snap.NumStudentsSpecial
	if totalStudents == 0 {
		totalStudents = snap.NumStudents
	}
	if rec, err := recommendTAsForCourse(ctx, s.pool, tcID, loadPlanRatios(ctx, s.pool)); err == nil {
		snap.SuggestedTAs.Undergrad = rec.Recommended
	}
	snap.SuggestedTAs.Graduate = min(2, totalStudents/60)

	// Used baht — reflects post-2026 payment model (ประกาศ 731/2565 + 1080/2565):
	//   Undergrad: SUM(hours × per-track hourly rate) from approved work_logs.
	//   Grad regular: SUM(hours × graduate_regular_hourly) — now hourly (was lump-sum).
	//   Grad special: flat term amount per assignment/course — graduate_special_lumpsum
	//                 IS the whole-term figure (4,000฿/TA/course, per the 2026 meeting
	//                 correction), NOT a monthly rate multiplied by term months. Capped
	//                 at grad_special_term_cap as a safety ceiling. Counted per assignment,
	//                 independently per course — a TA on 3 special-track courses gets up
	//                 to 3 × 4,000, there is no cross-course aggregate cap.
	//
	// Hourly work is priced by the SAME per-sitting ledger the settlement and
	// the payout use (claimCostByTASlot over merged approved sittings): a
	// co-taught sitting counts once, time on both tracks at once counts once
	// (B2), and the undergrad-special monthly cap applies. Summing raw
	// work_log hours here double-counted co-taught sections, so the lecturer
	// card and the executive dashboard disagreed with the settlement.
	var pr PayRate
	if err := s.pool.QueryRow(ctx, `
		SELECT undergrad_regular, undergrad_special, graduate_regular_hourly,
		       ug_special_monthly_cap, graduate_special_lumpsum, grad_special_term_cap
		FROM `+payRatesInForce).Scan(
		&pr.UndergradRegular, &pr.UndergradSpecial, &pr.GraduateRegularHourly,
		&pr.UGSpecialMonthlyCap, &pr.GraduateSpecialLumpsum, &pr.GradSpecialTermCap); err == nil {
		if costs, err := (&ExportService{pool: s.pool}).claimCostByTASlot(ctx, tcID, pr, mergedSittingsCTE); err == nil {
			for _, c := range costs {
				if c.Track == "regular" {
					snap.UsedBahtRegular += c.Baht
				} else {
					snap.UsedBahtSpecial += c.Baht
				}
			}
		}
		// ONE lump per TA per course. ta_request_assignments holds a row per
		// SECTION, so summing rows billed a PhD covering special sec 3 and
		// sec 4 twice. A holder dropped from the section gets no lump.
		var holders int
		if err := s.pool.QueryRow(ctx, `
			SELECT COUNT(DISTINCT a.ta_id)
			FROM ta_request_assignments a
			JOIN ta_requests r  ON r.id = a.request_id AND r.status = 'approved'
			JOIN sections sec   ON sec.id = a.section_id
			WHERE r.teaching_course_id = $1 AND a.level IN ('master','phd')
			  AND sec.track = 'special' AND a.state <> 'dropped'`, tcID).Scan(&holders); err == nil {
			lump := pr.GraduateSpecialLumpsum
			if pr.GradSpecialTermCap > 0 && lump > pr.GradSpecialTermCap {
				lump = pr.GradSpecialTermCap
			}
			snap.UsedBahtSpecial += float64(holders) * lump
		}
	}
	snap.UsedBaht = snap.UsedBahtRegular + snap.UsedBahtSpecial

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
