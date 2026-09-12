package service

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
)

// ta_plan.go feeds the lecturer's TA planner (the calculator on the request
// page) with the facts a good estimate actually depends on — read from the
// course, never typed in.
//
// The number that matters is how many times each section really meets this
// term. Generate walks the calendar with a long list of gates (holidays without
// a makeup, waived periods, exam windows, exam dates); a planner that assumed
// "months × 4" or "17 weeks" was off by a week on every course, which on a
// tight budget is the difference between fitting and not. So the planner walks
// the same calendar with the same gates, minus the per-person ones (own-class
// clashes, daily caps) that cannot be known before a TA is picked.
//
// What is already spoken for comes from the settlement forecast, so the
// planner subtracts the same figure the course page will later report.

type PlanSection struct {
	SectionID   uuid.UUID `json:"section_id"`
	SecNo       string    `json:"sec_no"`
	Track       string    `json:"track"`
	NumStudents int       `json:"num_students"`
	// Weekly contact hours from the timetable.
	LectureHoursWeekly float64 `json:"lecture_hours_weekly"`
	LabHoursWeekly     float64 `json:"lab_hours_weekly"`
	// Periods that survive the calendar gates — what a TA can actually log.
	LecturePeriods int `json:"lecture_periods"`
	LabPeriods     int `json:"lab_periods"`
	// Distinct weeks in which the section meets at all. This is the
	// multiplier for weekly duties (ตรวจงาน, อื่น ๆ) that are logged per week
	// rather than per class period.
	ClassWeeks int `json:"class_weeks"`
	// Periods lost to each gate, so the planner can say why 16 and not 17.
	SkippedHoliday int `json:"skipped_holiday"`
	SkippedExam    int `json:"skipped_exam"`
	// The same periods by calendar month ("2026-06" → counts), because the
	// ป.ตรี ภาคพิเศษ cap is a MONTHLY one and a term-level approximation
	// promised money a five-week month could not pay.
	Months []PlanMonth `json:"months"`
}

type PlanMonth struct {
	YearMonth      string `json:"year_month"`
	LecturePeriods int    `json:"lecture_periods"`
	LabPeriods     int    `json:"lab_periods"`
	ClassWeeks     int    `json:"class_weeks"`
}

type PlanExistingPerson struct {
	TAID   uuid.UUID `json:"ta_id"`
	Name   string    `json:"name"`
	Level  string    `json:"level"`
	Tracks []string  `json:"tracks"`
	// What their logged/forecast work costs, per pool, before any cut.
	RegularBaht float64 `json:"regular_baht"`
	SpecialBaht float64 `json:"special_baht"`
	LumpBaht    float64 `json:"lump_baht"`
	// What the settlement forecast says they will actually be paid, after the
	// proportional cut when a pool is short. Equal to the cost when it is not.
	RegularPaid float64 `json:"regular_paid"`
	SpecialPaid float64 `json:"special_paid"`
}

type PlanTrack struct {
	Track string `json:"track"`
	// The pool, from the budget formula.
	CapBaht float64 `json:"cap_baht"`
	// Already spoken for by approved TAs: hourly work forecast + flat lumps.
	ExistingBaht float64 `json:"existing_baht"`
	ExistingLump float64 `json:"existing_lump_baht"`
	ExistingTAs  int     `json:"existing_tas"`
	NumStudents  int     `json:"num_students"`
}

type PlanFacts struct {
	StartsOn   string  `json:"starts_on"`
	EndsOn     string  `json:"ends_on"`
	WeeksTotal float64 `json:"weeks_total"`
	// Weeks in which any section meets — the term minus exam weeks etc.
	ClassWeeks int           `json:"class_weeks"`
	Sections   []PlanSection `json:"sections"`
	Tracks     struct {
		Regular PlanTrack `json:"regular"`
		Special PlanTrack `json:"special"`
	} `json:"tracks"`
	Existing []PlanExistingPerson `json:"existing"`
	Rates    struct {
		UndergradRegular       float64 `json:"undergrad_regular"`
		UndergradSpecial       float64 `json:"undergrad_special"`
		GraduateRegularHourly  float64 `json:"graduate_regular_hourly"`
		GraduateSpecialLumpsum float64 `json:"graduate_special_lumpsum"`
		UGSpecialMonthlyCap    float64 `json:"ug_special_monthly_cap"`
		TermMonths             int     `json:"term_months"`
		// Weekly ceilings the request form enforces per duty (mirrors the
		// backend caps the planner's default duty template must respect).
		GradReviewHourCap float64 `json:"grad_review_hour_cap"`
		// Planning ratios (0112).
		PlanStudentsPerTA    int `json:"plan_students_per_ta"`
		PlanMinStudentsPerTA int `json:"plan_min_students_per_ta"`
		PlanSuggestedTACap   int `json:"plan_suggested_ta_cap"`
	} `json:"rates"`
}

// PlanFactsForViewer is the lecturer's (or staff's) view. Same gate as the
// settlement: you must teach the course, or be privileged.
func (s *ExportService) PlanFactsForViewer(ctx context.Context, actor, courseID uuid.UUID, privileged bool) (*PlanFacts, error) {
	if !privileged {
		var allowed bool
		if err := s.pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM teaching_lecturers tl
			                WHERE tl.teaching_course_id = $1 AND tl.lecturer_id = $2)`,
			courseID, actor).Scan(&allowed); err != nil {
			return nil, err
		}
		if !allowed {
			return nil, ErrForbidden
		}
	}
	return s.PlanFacts(ctx, courseID)
}

func (s *ExportService) PlanFacts(ctx context.Context, courseID uuid.UUID) (*PlanFacts, error) {
	out := &PlanFacts{Sections: []PlanSection{}, Existing: []PlanExistingPerson{}}
	wl := &WorkLogService{pool: s.pool}

	start, end, err := wl.courseDateRange(ctx, courseID)
	if err != nil {
		return nil, err
	}
	out.StartsOn, out.EndsOn = start.Format("2006-01-02"), end.Format("2006-01-02")
	out.WeeksTotal = WeeksInTerm(ctx, s.pool, courseID)

	midterm, final, err := wl.courseExamWindows(ctx, courseID)
	if err != nil {
		return nil, err
	}
	holidays, err := wl.loadHolidaysInRange(ctx, start, end)
	if err != nil {
		return nil, err
	}

	// Rates.
	if err := s.pool.QueryRow(ctx, `
		SELECT undergrad_regular, undergrad_special, graduate_regular_hourly,
		       graduate_special_lumpsum, ug_special_monthly_cap, term_months,
		       plan_students_per_ta, plan_min_students_per_ta, plan_suggested_ta_cap
		FROM pay_rates ORDER BY effective_from DESC LIMIT 1`).Scan(
		&out.Rates.UndergradRegular, &out.Rates.UndergradSpecial, &out.Rates.GraduateRegularHourly,
		&out.Rates.GraduateSpecialLumpsum, &out.Rates.UGSpecialMonthlyCap, &out.Rates.TermMonths,
		&out.Rates.PlanStudentsPerTA, &out.Rates.PlanMinStudentsPerTA, &out.Rates.PlanSuggestedTACap); err != nil {
		return nil, err
	}
	if out.Rates.PlanStudentsPerTA <= 0 {
		out.Rates.PlanStudentsPerTA = 25
	}
	if out.Rates.PlanMinStudentsPerTA <= 0 {
		out.Rates.PlanMinStudentsPerTA = 15
	}
	out.Rates.GradReviewHourCap = gradReviewHourCap

	// Pools.
	if s.budget != nil {
		snap, err := s.budget.Compute(ctx, courseID)
		if err != nil {
			return nil, err
		}
		out.Tracks.Regular = PlanTrack{Track: "regular", CapBaht: snap.TermPayRegular, NumStudents: snap.NumStudentsRegular}
		out.Tracks.Special = PlanTrack{Track: "special", CapBaht: snap.TermPaySpecial, NumStudents: snap.NumStudentsSpecial}
		if out.Rates.TermMonths == 0 {
			out.Rates.TermMonths = snap.Rates.TermMonths
		}
	}

	// Sections + their calendar walk.
	rows, err := s.pool.Query(ctx, `
		SELECT sec.id, sec.sec_no, sec.track::text, sec.num_students
		FROM sections sec WHERE sec.teaching_course_id = $1
		ORDER BY sec.sec_no`, courseID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ps PlanSection
		if err := rows.Scan(&ps.SectionID, &ps.SecNo, &ps.Track, &ps.NumStudents); err != nil {
			rows.Close()
			return nil, err
		}
		out.Sections = append(out.Sections, ps)
	}
	rows.Close()

	allWeeks := map[string]bool{}
	for i := range out.Sections {
		if err := s.walkSection(ctx, &out.Sections[i], start, end, holidays, midterm, final, allWeeks); err != nil {
			return nil, err
		}
	}
	out.ClassWeeks = len(allWeeks)

	// What is already committed, per pool, from the same forecast the course
	// page shows. People are listed so the planner can name them.
	fc, err := s.ForecastCourse(ctx, courseID)
	if err != nil {
		return nil, err
	}
	people := map[uuid.UUID]*PlanExistingPerson{}
	get := func(p PersonSettlement) *PlanExistingPerson {
		if e, ok := people[p.TAID]; ok {
			return e
		}
		e := &PlanExistingPerson{TAID: p.TAID, Name: p.Name, Level: p.Level, Tracks: []string{}}
		people[p.TAID] = e
		return e
	}
	for _, p := range fc.Regular.People {
		e := get(p)
		e.RegularBaht += p.Baht
		e.RegularPaid += p.PaidBaht
		e.Tracks = append(e.Tracks, "regular")
		out.Tracks.Regular.ExistingBaht += p.Baht
	}
	for _, p := range fc.Special.People {
		e := get(p)
		e.SpecialBaht += p.Baht
		e.SpecialPaid += p.PaidBaht
		e.LumpBaht += p.LumpBaht
		e.Tracks = append(e.Tracks, "special")
		out.Tracks.Special.ExistingBaht += p.Baht
	}
	out.Tracks.Special.ExistingLump = fc.Special.Committed
	// TAs with an approved assignment but nothing logged yet still hold a seat
	// — count them per pool from the assignments themselves.
	seat, err := s.pool.Query(ctx, `
		SELECT DISTINCT a.ta_id, sec.track::text, a.level::text,
		       COALESCE(u.title,'') || u.first_name || ' ' || u.last_name
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		JOIN sections sec ON sec.id = a.section_id
		JOIN users u ON u.id = a.ta_id
		WHERE sec.teaching_course_id = $1 AND a.state <> 'dropped'`, courseID)
	if err != nil {
		return nil, err
	}
	seatRegular, seatSpecial := map[uuid.UUID]bool{}, map[uuid.UUID]bool{}
	for seat.Next() {
		var ta uuid.UUID
		var track, level, name string
		if err := seat.Scan(&ta, &track, &level, &name); err != nil {
			seat.Close()
			return nil, err
		}
		e, ok := people[ta]
		if !ok {
			e = &PlanExistingPerson{TAID: ta, Name: name, Level: level, Tracks: []string{}}
			people[ta] = e
		}
		if track == "special" {
			seatSpecial[ta] = true
		} else {
			seatRegular[ta] = true
		}
		has := false
		for _, t := range e.Tracks {
			if t == track {
				has = true
			}
		}
		if !has {
			e.Tracks = append(e.Tracks, track)
		}
	}
	seat.Close()
	out.Tracks.Regular.ExistingTAs = len(seatRegular)
	out.Tracks.Special.ExistingTAs = len(seatSpecial)
	for _, e := range people {
		out.Existing = append(out.Existing, *e)
	}
	sort.Slice(out.Existing, func(i, j int) bool { return out.Existing[i].Name < out.Existing[j].Name })
	return out, nil
}

// walkSection counts the periods of one section that survive the calendar —
// the same gates Generate applies before any per-TA rule.
func (s *ExportService) walkSection(
	ctx context.Context, ps *PlanSection, start, end time.Time,
	holidays holidaySet, midterm, final examWindow, allWeeks map[string]bool,
) error {
	type sch struct {
		kind       string
		day        int
		start, end string
		hours      float64
	}
	var schs []sch
	rows, err := s.pool.Query(ctx, `
		SELECT kind, day_of_week, TO_CHAR(start_time,'HH24:MI'), TO_CHAR(end_time,'HH24:MI'),
		       EXTRACT(EPOCH FROM (end_time - start_time))/3600
		FROM section_schedules WHERE section_id = $1`, ps.SectionID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var x sch
		if err := rows.Scan(&x.kind, &x.day, &x.start, &x.end, &x.hours); err != nil {
			rows.Close()
			return err
		}
		schs = append(schs, x)
		if x.kind == "lab" {
			ps.LabHoursWeekly += x.hours
		} else {
			ps.LectureHoursWeekly += x.hours
		}
	}
	rows.Close()
	if len(schs) == 0 {
		return nil
	}

	type key struct{ date, kind string }
	makeup := map[key]time.Time{}
	waived := map[key]bool{}
	mk, err := s.pool.Query(ctx,
		`SELECT original_date, makeup_date, kind FROM makeup_schedules WHERE section_id = $1`, ps.SectionID)
	if err != nil {
		return err
	}
	for mk.Next() {
		var orig time.Time
		var to *time.Time
		var kind string
		if err := mk.Scan(&orig, &to, &kind); err == nil {
			k := key{orig.Format("2006-01-02"), kind}
			if to == nil {
				waived[k] = true
			} else {
				makeup[k] = *to
			}
		}
	}
	mk.Close()
	exam := map[string]bool{}
	ex, err := s.pool.Query(ctx, `SELECT exam_date FROM exam_schedules WHERE section_id = $1`, ps.SectionID)
	if err != nil {
		return err
	}
	for ex.Next() {
		var d time.Time
		if err := ex.Scan(&d); err == nil {
			exam[d.Format("2006-01-02")] = true
		}
	}
	ex.Close()

	weeks := map[string]bool{}
	type monthAcc struct {
		lec, lab int
		weeks    map[string]bool
	}
	months := map[string]*monthAcc{}
	monthOf := func(ym string) *monthAcc {
		m, ok := months[ym]
		if !ok {
			m = &monthAcc{weeks: map[string]bool{}}
			months[ym] = m
		}
		return m
	}
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		wd := int(d.Weekday())
		if wd == 0 || wd == 6 {
			continue
		}
		dkey := d.Format("2006-01-02")
		for _, sc := range schs {
			if sc.day != wd {
				continue
			}
			if waived[key{dkey, sc.kind}] {
				ps.SkippedHoliday++
				continue
			}
			use := d
			if to, ok := makeup[key{dkey, sc.kind}]; ok {
				use = to
			} else {
				st, okS := parseHM(sc.start)
				en, okE := parseHM(sc.end)
				if !okS || !okE {
					st, en = 0, 24*60
				}
				if _, closed := holidays.overlapping(dkey, st, en); closed {
					ps.SkippedHoliday++
					continue
				}
			}
			ukey := use.Format("2006-01-02")
			if exam[ukey] || midterm.contains(use) || final.contains(use) {
				ps.SkippedExam++
				continue
			}
			m := monthOf(use.Format("2006-01"))
			if sc.kind == "lab" {
				ps.LabPeriods++
				m.lab++
			} else {
				ps.LecturePeriods++
				m.lec++
			}
			wk := weekStart(use).Format("2006-01-02")
			weeks[wk] = true
			allWeeks[wk] = true
			m.weeks[wk] = true
		}
	}
	ps.ClassWeeks = len(weeks)
	ps.Months = []PlanMonth{}
	for ym, m := range months {
		ps.Months = append(ps.Months, PlanMonth{YearMonth: ym, LecturePeriods: m.lec, LabPeriods: m.lab, ClassWeeks: len(m.weeks)})
	}
	sort.Slice(ps.Months, func(i, j int) bool { return ps.Months[i].YearMonth < ps.Months[j].YearMonth })
	return nil
}
