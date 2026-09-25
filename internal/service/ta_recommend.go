package service

// ta_recommend.go is the ONE place the system decides how many TAs a course
// should have. Until 26/09/2026 there were two answers: BudgetService.Compute
// said ceil(course total ÷ 25) capped at 3, while the lecturer's planner
// (TaPlanner.tsx buildModel) worked per SITTING — the sections that meet in
// one room at one time — and gave every sitting at least one TA. A course of
// two 30-student sections at different times was "2" to one and "2" to the
// other only by luck; three 20-student sections were "3" in the planner and
// "3" on the server, but one 60-student section plus a 10-student special one
// was "3" vs "4". The dashboard's "ขอ TA เกินที่แนะนำไหม" question cannot be
// asked against two different yardsticks, so both now read this file and the
// planner is the rule it copies:
//
//	sitting  = sections whose timetables overlap (same day, overlapping time)
//	guide    = max(1, min(cap, ceil(students ÷ plan_students_per_ta)))
//	ceiling  = max(guide, ceil(students ÷ plan_min_students_per_ta))
//	course   = Σ over sittings
//
// Keep it in step with sittingGroups/guideFor/ceilingFor in TaPlanner.tsx.

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TARecommendation is the staffing yardstick for one course.
type TARecommendation struct {
	Students    int `json:"students"`
	Sittings    int `json:"sittings"`
	Recommended int `json:"recommended"`
	Ceiling     int `json:"ceiling"`
}

// PlanRatios are the staff-editable planning ratios on pay_rates (0112).
type PlanRatios struct {
	StudentsPerTA    int `json:"students_per_ta"`
	MinStudentsPerTA int `json:"min_students_per_ta"`
	SuggestedTACap   int `json:"suggested_ta_cap"`
}

func (s *DashboardService) planRatios(ctx context.Context) PlanRatios {
	return loadPlanRatios(ctx, s.pool)
}

func loadPlanRatios(ctx context.Context, q querier) PlanRatios {
	var r PlanRatios
	_ = q.QueryRow(ctx, `SELECT plan_students_per_ta, plan_min_students_per_ta, plan_suggested_ta_cap
	                     FROM `+payRatesInForce).Scan(&r.StudentsPerTA, &r.MinStudentsPerTA, &r.SuggestedTACap)
	if r.StudentsPerTA <= 0 {
		r.StudentsPerTA = 25
	}
	if r.MinStudentsPerTA <= 0 {
		r.MinStudentsPerTA = 15
	}
	return r
}

type recSection struct {
	id       uuid.UUID
	students int
	slots    []recSlot
}

type recSlot struct {
	day        int
	start, end string // "HH:MM", compared as strings exactly like the planner
}

func (a recSection) overlaps(b recSection) bool {
	for _, x := range a.slots {
		for _, y := range b.slots {
			if x.day == y.day && x.start < y.end && y.start < x.end {
				return true
			}
		}
	}
	return false
}

// recommendFromSections groups sections into sittings (first-come, one pass —
// the planner's exact order) and applies the ratios. courseTotal is the
// course-level student count, used only when no section carries a count yet:
// the budget already runs on that figure, and a recommendation of "1 per
// sitting" for a 90-student course whose sections are still blank would read
// as a verdict rather than as missing data.
func recommendFromSections(secs []recSection, courseTotal int, r PlanRatios) TARecommendation {
	out := TARecommendation{}
	sectionSum := 0
	for _, s := range secs {
		sectionSum += s.students
	}
	if sectionSum == 0 && courseTotal > 0 {
		secs = []recSection{{students: courseTotal}}
		sectionSum = courseTotal
	}
	out.Students = sectionSum
	if len(secs) == 0 {
		return out
	}
	group := make([]int, len(secs))
	next := 0
	for i := range secs {
		if group[i] != 0 {
			continue
		}
		next++
		group[i] = next
		for j := range secs {
			if j == i || group[j] != 0 {
				continue
			}
			if secs[i].overlaps(secs[j]) {
				group[j] = next
			}
		}
	}
	students := make([]int, next+1)
	for i, s := range secs {
		students[group[i]] += s.students
	}
	out.Sittings = next
	for g := 1; g <= next; g++ {
		n := students[g]
		guide := 0
		if n > 0 {
			guide = (n + r.StudentsPerTA - 1) / r.StudentsPerTA
		}
		if r.SuggestedTACap > 0 && guide > r.SuggestedTACap {
			guide = r.SuggestedTACap
		}
		if guide < 1 {
			guide = 1
		}
		ceiling := 0
		if n > 0 {
			ceiling = (n + r.MinStudentsPerTA - 1) / r.MinStudentsPerTA
		}
		if ceiling < guide {
			ceiling = guide
		}
		out.Recommended += guide
		out.Ceiling += ceiling
	}
	return out
}

// recommendTAsForTerm answers for every course of a term in two queries.
func (s *DashboardService) recommendTAsForTerm(ctx context.Context, termID uuid.UUID, r PlanRatios) (map[uuid.UUID]TARecommendation, error) {
	return recommendTAs(ctx, s.pool, `tc.term_id = $1`, termID, r)
}

// recommendTAsForCourse is the single-course form, used by BudgetService.
func recommendTAsForCourse(ctx context.Context, q *pgxpool.Pool, courseID uuid.UUID, r PlanRatios) (TARecommendation, error) {
	m, err := recommendTAs(ctx, q, `tc.id = $1`, courseID, r)
	if err != nil {
		return TARecommendation{}, err
	}
	return m[courseID], nil
}

func recommendTAs(ctx context.Context, q *pgxpool.Pool, where string, arg any, r PlanRatios) (map[uuid.UUID]TARecommendation, error) {
	type course struct {
		total int
		order []uuid.UUID
		secs  map[uuid.UUID]*recSection
	}
	courses := map[uuid.UUID]*course{}
	rows, err := q.Query(ctx, `
		SELECT tc.id,
		       CASE WHEN tc.num_students_regular + tc.num_students_special > 0
		            THEN tc.num_students_regular + tc.num_students_special
		            ELSE tc.num_students END,
		       sec.id, COALESCE(sec.num_students, 0)
		FROM teaching_courses tc
		LEFT JOIN sections sec ON sec.teaching_course_id = tc.id
		WHERE `+where+`
		ORDER BY tc.id, sec.sec_no`, arg)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var tcID uuid.UUID
		var total, students int
		var secID *uuid.UUID
		if err := rows.Scan(&tcID, &total, &secID, &students); err != nil {
			rows.Close()
			return nil, err
		}
		c, ok := courses[tcID]
		if !ok {
			c = &course{total: total, secs: map[uuid.UUID]*recSection{}}
			courses[tcID] = c
		}
		if secID != nil {
			c.order = append(c.order, *secID)
			c.secs[*secID] = &recSection{id: *secID, students: students}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sch, err := q.Query(ctx, `
		SELECT tc.id, sch.section_id, sch.day_of_week,
		       TO_CHAR(sch.start_time,'HH24:MI'), TO_CHAR(sch.end_time,'HH24:MI')
		FROM section_schedules sch
		JOIN sections sec ON sec.id = sch.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		WHERE `+where, arg)
	if err != nil {
		return nil, err
	}
	for sch.Next() {
		var tcID, secID uuid.UUID
		var slot recSlot
		if err := sch.Scan(&tcID, &secID, &slot.day, &slot.start, &slot.end); err != nil {
			sch.Close()
			return nil, err
		}
		if c, ok := courses[tcID]; ok {
			if sec, ok := c.secs[secID]; ok {
				sec.slots = append(sec.slots, slot)
			}
		}
	}
	sch.Close()
	if err := sch.Err(); err != nil {
		return nil, err
	}

	out := make(map[uuid.UUID]TARecommendation, len(courses))
	for id, c := range courses {
		secs := make([]recSection, 0, len(c.order))
		for _, sid := range c.order {
			secs = append(secs, *c.secs[sid])
		}
		out[id] = recommendFromSections(secs, c.total, r)
	}
	return out, nil
}
