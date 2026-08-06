package service

// The executive analytics behind the 06/08/2026 management-meeting ask: where
// does the TA money go — by month, by curriculum, by course — and how fast.
//
// Every baht figure here comes from ONE source: the settlement (SettleCourse),
// i.e. the same คาบ-level pricing-with-cutoff that prices the printed claim
// documents. The dashboard's older card used BudgetService.Compute's UsedBaht
// (approved hours × rate, no cutoff); showing both on one screen would put two
// disagreeing totals a scroll apart — the exact class of bug this project keeps
// re-finding — so the analytics is settle-based throughout and the UI presents
// it as "เบิกจ่ายจริง". The one exception is the CAP, which is the budget
// formula's PerCourseMaxBaht — a ceiling has no settlement to read.

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
)

// MonthSpend is one month of the term's disbursement, Gregorian "2026-06".
type MonthSpend struct {
	YearMonth string  `json:"year_month"`
	Baht      float64 `json:"baht"`
}

// CurriculumStat groups the term by the programme a section serves.
// Curriculum "" means the sections never carried a ReservedFor token and staff
// have not filled one in — shown as "ยังไม่ระบุ", deliberately not folded into
// OTHER (that means "another faculty", a different statement).
type CurriculumStat struct {
	Curriculum    string  `json:"curriculum"` // CS/IT/GIS/AI/CY/OTHER/""
	CoursesOpen   int     `json:"courses_open"`
	CoursesWithTA int     `json:"courses_with_ta"`
	TAs           int     `json:"tas"`
	SpentBaht     float64 `json:"spent_baht"`
	CapBaht       float64 `json:"cap_baht"`
}

// CourseSpendStat is one row of the per-course drill-down table.
type CourseSpendStat struct {
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	Code             string    `json:"code"`
	NameTH           string    `json:"name_th"`
	Curriculum       string    `json:"curriculum"`
	TAs              int       `json:"tas"`
	ApprovedHours    float64   `json:"approved_hours"`
	SpentBaht        float64   `json:"spent_baht"`
	CapBaht          float64   `json:"cap_baht"`
	OverBudget       bool      `json:"over_budget"`
}

type TermAnalytics struct {
	TermID    uuid.UUID `json:"term_id"`
	TermLabel string    `json:"term_label"`
	// Term boundaries for the pace figure; empty when the term has no dates.
	StartsOn string `json:"starts_on,omitempty"`
	EndsOn   string `json:"ends_on,omitempty"`
	// ElapsedPct is how much of the term has passed today, 0–100. Negative
	// when the term has no dates — the UI hides the pace bar rather than
	// implying a schedule nobody set.
	ElapsedPct float64 `json:"elapsed_pct"`

	CoursesOpen   int `json:"courses_open"`
	CoursesWithTA int `json:"courses_with_ta"`
	TotalTAs      int `json:"total_tas"`

	BudgetAllocated float64 `json:"budget_allocated"`
	BudgetUsed      float64 `json:"budget_used"`
	ApprovedHours   float64 `json:"approved_hours"`

	Monthly   []MonthSpend      `json:"monthly"`
	Curricula []CurriculumStat  `json:"curricula"`
	Courses   []CourseSpendStat `json:"courses"`
}

// Analytics builds the executive view for one term (nil = active/newest, same
// resolution as Executive). Read-only.
func (s *DashboardService) Analytics(ctx context.Context, termID *uuid.UUID, budget *BudgetService, export *ExportService) (*TermAnalytics, error) {
	out := &TermAnalytics{ElapsedPct: -1}

	var (
		tid       uuid.UUID
		year, sem int
		starts    *time.Time
		ends      *time.Time
		resolve   = `SELECT id, academic_year, semester, starts_on, ends_on FROM academic_terms
		              ORDER BY is_active DESC, academic_year DESC, semester DESC LIMIT 1`
		resolveArg = []any{}
	)
	if termID != nil {
		resolve = `SELECT id, academic_year, semester, starts_on, ends_on FROM academic_terms WHERE id = $1`
		resolveArg = append(resolveArg, *termID)
	}
	if err := s.pool.QueryRow(ctx, resolve, resolveArg...).Scan(&tid, &year, &sem, &starts, &ends); err != nil {
		return out, nil // fresh install: an empty view is the honest answer
	}
	out.TermID = tid
	out.TermLabel = itoa(year) + "/" + itoa(sem)
	if starts != nil && ends != nil && ends.After(*starts) {
		out.StartsOn = starts.Format("2006-01-02")
		out.EndsOn = ends.Format("2006-01-02")
		elapsed := time.Since(*starts).Hours() / ends.Sub(*starts).Hours() * 100
		switch {
		case elapsed < 0:
			elapsed = 0
		case elapsed > 100:
			elapsed = 100
		}
		out.ElapsedPct = elapsed
	}

	// Every course in the term, with its curriculum: the most common non-null
	// value across its sections (ties break alphabetically so the answer is
	// stable). A course serving several programmes counts under its main one —
	// splitting one course's money across groups would make the group totals
	// stop adding up to the term total.
	type courseRow struct {
		id         uuid.UUID
		code, name string
		curriculum string
		withTA     bool
	}
	rows, err := s.pool.Query(ctx, `
		SELECT tc.id, tc.code, tc.name_th,
		       COALESCE((SELECT sx.curriculum FROM sections sx
		                  WHERE sx.teaching_course_id = tc.id AND sx.curriculum IS NOT NULL
		                  GROUP BY sx.curriculum
		                  ORDER BY COUNT(*) DESC, sx.curriculum LIMIT 1), ''),
		       EXISTS (SELECT 1 FROM ta_requests r
		               WHERE r.teaching_course_id = tc.id
		                 AND r.status IN ('submitted','approved'))
		FROM teaching_courses tc
		WHERE tc.term_id = $1
		ORDER BY tc.code`, tid)
	if err != nil {
		return nil, err
	}
	var courses []courseRow
	for rows.Next() {
		var c courseRow
		if err := rows.Scan(&c.id, &c.code, &c.name, &c.curriculum, &c.withTA); err != nil {
			rows.Close()
			return nil, err
		}
		courses = append(courses, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.CoursesOpen = len(courses)

	_ = s.pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT a.ta_id)
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		WHERE tc.term_id = $1 AND a.state <> 'dropped'`, tid).Scan(&out.TotalTAs)

	monthly := map[string]float64{}
	curStats := map[string]*CurriculumStat{}
	curStat := func(key string) *CurriculumStat {
		st, ok := curStats[key]
		if !ok {
			st = &CurriculumStat{Curriculum: key}
			curStats[key] = st
		}
		return st
	}
	// Distinct TAs per curriculum — a TA on two IT courses is one IT person.
	curTAs := map[string]map[uuid.UUID]struct{}{}

	for _, c := range courses {
		curStat(c.curriculum).CoursesOpen++
		if !c.withTA {
			continue
		}
		out.CoursesWithTA++
		st := curStat(c.curriculum)
		st.CoursesWithTA++

		settle, err := export.SettleCourse(ctx, c.id)
		if err != nil {
			return nil, err
		}
		// Committed is the graduate-special lump sum — real money taken off the
		// top with no month of its own, so it joins the totals but not the
		// monthly series.
		spent := settle.Regular.PaidBaht + settle.Regular.Committed +
			settle.Special.PaidBaht + settle.Special.Committed
		for _, tr := range []TrackSettlement{settle.Regular, settle.Special} {
			for _, m := range tr.Months {
				monthly[m.YearMonth] += m.PaidBaht
			}
		}

		snap, err := budget.Compute(ctx, c.id)
		if err != nil {
			return nil, err
		}

		var hours float64
		_ = s.pool.QueryRow(ctx, `
			SELECT COALESCE(SUM(wl.hours), 0)
			FROM work_logs wl
			JOIN ta_request_assignments a ON a.id = wl.assignment_id
			JOIN ta_requests r ON r.id = a.request_id
			WHERE r.teaching_course_id = $1 AND wl.status = 'approved'`, c.id).Scan(&hours)

		var tas int
		taRows, err := s.pool.Query(ctx, `
			SELECT DISTINCT a.ta_id
			FROM ta_request_assignments a
			JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
			WHERE r.teaching_course_id = $1 AND a.state <> 'dropped'`, c.id)
		if err != nil {
			return nil, err
		}
		for taRows.Next() {
			var taID uuid.UUID
			if err := taRows.Scan(&taID); err != nil {
				taRows.Close()
				return nil, err
			}
			tas++
			set, ok := curTAs[c.curriculum]
			if !ok {
				set = map[uuid.UUID]struct{}{}
				curTAs[c.curriculum] = set
			}
			set[taID] = struct{}{}
		}
		taRows.Close()

		out.Courses = append(out.Courses, CourseSpendStat{
			TeachingCourseID: c.id,
			Code:             c.code,
			NameTH:           c.name,
			Curriculum:       c.curriculum,
			TAs:              tas,
			ApprovedHours:    round2(hours),
			SpentBaht:        round2(spent),
			CapBaht:          round2(snap.PerCourseMaxBaht),
			OverBudget:       settle.OverBudget,
		})
		st.SpentBaht += spent
		st.CapBaht += snap.PerCourseMaxBaht
		out.BudgetUsed += spent
		out.BudgetAllocated += snap.PerCourseMaxBaht
		out.ApprovedHours += hours
	}

	for ym, baht := range monthly {
		out.Monthly = append(out.Monthly, MonthSpend{YearMonth: ym, Baht: round2(baht)})
	}
	sort.Slice(out.Monthly, func(i, j int) bool { return out.Monthly[i].YearMonth < out.Monthly[j].YearMonth })

	for key, st := range curStats {
		st.TAs = len(curTAs[key])
		st.SpentBaht = round2(st.SpentBaht)
		st.CapBaht = round2(st.CapBaht)
		out.Curricula = append(out.Curricula, *st)
	}
	// Biggest spender first; ties (the all-zero groups) by open-course count
	// then name so the order never jitters between loads.
	sort.Slice(out.Curricula, func(i, j int) bool {
		a, b := out.Curricula[i], out.Curricula[j]
		if a.SpentBaht != b.SpentBaht {
			return a.SpentBaht > b.SpentBaht
		}
		if a.CoursesOpen != b.CoursesOpen {
			return a.CoursesOpen > b.CoursesOpen
		}
		return a.Curriculum < b.Curriculum
	})
	sort.Slice(out.Courses, func(i, j int) bool {
		if out.Courses[i].SpentBaht != out.Courses[j].SpentBaht {
			return out.Courses[i].SpentBaht > out.Courses[j].SpentBaht
		}
		return out.Courses[i].Code < out.Courses[j].Code
	})

	out.BudgetUsed = round2(out.BudgetUsed)
	out.BudgetAllocated = round2(out.BudgetAllocated)
	out.ApprovedHours = round2(out.ApprovedHours)
	return out, nil
}
