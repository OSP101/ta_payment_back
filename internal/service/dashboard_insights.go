package service

// dashboard_insights.go — the question-led dashboard (26/09/2026, TOR 3.13).
//
// The analytics used to answer one question: where did the money go. The
// redesign asks three, and this file supplies the other two:
//
//	ขอ TA สมเหตุสมผลไหม — per course: students, the recommended and ceiling
//	    TA counts (ta_recommend.go, the planner's rule), how many were asked
//	    for, and a verdict.
//	งานค้างอยู่ขั้นไหน — the five TOR queues, what was sent back, the claim
//	    months broken down by who is holding each one, and the next deadline.
//
// Everything is scoped to one term and computed in a fixed handful of
// queries whatever the course count — the Settle/Forecast loop in Analytics
// is already the slow part and nothing here adds to it per course.

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Staffing verdicts, in increasing order of concern for a reviewer.
const (
	StaffingNoRequest   = "no_request"   // no live request
	StaffingNoStudents  = "no_students"  // requested, but no student count to judge by
	StaffingUnder       = "under"        // fewer than recommended
	StaffingMatch       = "match"        // exactly the recommendation
	StaffingAboveGuide  = "above_guide"  // more than recommended, within the ceiling
	StaffingOverCeiling = "over_ceiling" // more than one TA per plan_min_students_per_ta
)

// CourseStaffing is one row of the "ขอ TA เกินไหม" table — every course of the
// term, requested or not.
type CourseStaffing struct {
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	Code             string    `json:"code"`
	NameTH           string    `json:"name_th"`
	Curriculum       string    `json:"curriculum"`
	Level            string    `json:"level"`
	Lecturers        []string  `json:"lecturers"`

	Students        int  `json:"students"`
	StudentsMissing bool `json:"students_missing"`
	Sittings        int  `json:"sittings"`
	Recommended     int  `json:"recommended"`
	Ceiling         int  `json:"ceiling"`

	// Requested is distinct named TAs on the course's live requests
	// (submitted or approved), not dropped. Approved is the approved subset.
	Requested      int  `json:"requested"`
	Approved       int  `json:"approved"`
	PendingRequest bool `json:"pending_request"`
	// StudentsPerTA is Students ÷ Requested; 0 when either is zero.
	StudentsPerTA float64 `json:"students_per_ta"`
	Status        string  `json:"status"`

	SpentBaht    float64 `json:"spent_baht"`
	CapBaht      float64 `json:"cap_baht"`
	ForecastBaht float64 `json:"forecast_baht"`
	UnfundedBaht float64 `json:"unfunded_baht"`

	UnresolvedMakeups int `json:"unresolved_makeups"`
}

// PipelineStage is one of the TOR's five queues.
type PipelineStage struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
	// OldestDays is how long the oldest item has waited, when knowable.
	OldestDays *int `json:"oldest_days,omitempty"`
}

// PipelineSummary is "งานค้างอยู่ขั้นไหน" plus what went backwards.
type PipelineSummary struct {
	Stages []PipelineStage `json:"stages"`
	// Returned items — the TOR's "การแก้ไขเอกสาร".
	DocsReturned      int `json:"docs_returned"`      // TA documents needs_fix / rejected
	MonthsSentBack    int `json:"months_sent_back"`   // claim months staff sent back, not yet resent
	WorklogsRejected  int `json:"worklogs_rejected"`  // lecturer-rejected rows not yet fixed
	RequestsRejected  int `json:"requests_rejected"`  // TA requests staff rejected this term
	MissingStudents   int `json:"missing_students"`   // courses blocking export
	UnresolvedMakeups int `json:"unresolved_makeups"` // holiday periods without a makeup date
	CoursesNoTA       int `json:"courses_no_ta"`      // courses with students and no live request
	PayoutCourses     int `json:"payout_courses"`     // courses an officer can move now
}

// MonthFlow is one claim month split by who is holding each (TA × course).
type MonthFlow struct {
	YearMonth string  `json:"year_month"` // BE "2569-06", as submission_periods stores it
	Label     string  `json:"label"`
	DueDate   string  `json:"due_date"`
	IsClosed  bool    `json:"is_closed"`
	WithTA    int     `json:"with_ta"`       // drafts or lecturer-rejected rows
	Lecturer  int     `json:"with_lecturer"` // sent, awaiting the lecturer
	Appoint   int     `json:"await_appointment"`
	Review    int     `json:"staff_review"`
	Export    int     `json:"ready_export"`
	Exported  int     `json:"exported"`
	Finance   int     `json:"finance_sent"`
	Skipped   int     `json:"skipped"`
	SentBack  int     `json:"sent_back"`
	Total     int     `json:"total"`
	BahtPaid  float64 `json:"baht"`
}

// DeadlineInfo is the next open submission deadline.
type DeadlineInfo struct {
	DueDate    string   `json:"due_date"`
	DaysLeft   int      `json:"days_left"`
	Months     []string `json:"months"` // BE year_month of every period due that day
	Labels     []string `json:"labels"`
	RemindDays int      `json:"remind_days"` // reminders start this many days before
	// NotSent counts (TA × course × month) still holding a draft or a
	// rejected row for those months — who the reminder is chasing.
	NotSent int `json:"not_sent"`
}

// DocStatusCounts is the TOR's "สถานะการส่งเอกสาร การตรวจ การแก้ไข" for the
// TAs of this term (distinct people on live requests).
type DocStatusCounts struct {
	NotSubmitted int `json:"not_submitted"`
	Submitted    int `json:"submitted"` // waiting for staff
	NeedsFix     int `json:"needs_fix"`
	Rejected     int `json:"rejected"`
	Approved     int `json:"approved"`
	Total        int `json:"total"`
}

func (s *DashboardService) addInsights(ctx context.Context, out *TermAnalytics, tid uuid.UUID) error {
	out.Plan = s.planRatios(ctx)

	if err := s.staffing(ctx, out, tid); err != nil {
		return err
	}
	if err := s.taCounts(ctx, out, tid); err != nil {
		return err
	}
	if err := s.pipeline(ctx, out, tid); err != nil {
		return err
	}
	if err := s.monthFlow(ctx, out, tid); err != nil {
		return err
	}
	if err := s.docStatus(ctx, out, tid); err != nil {
		return err
	}
	return nil
}

// staffing builds the per-course rows. Money columns are copied from the
// settle-priced Courses rows Analytics already built, so the two tables on
// one screen can never disagree.
func (s *DashboardService) staffing(ctx context.Context, out *TermAnalytics, tid uuid.UUID) error {
	recs, err := s.recommendTAsForTerm(ctx, tid, out.Plan)
	if err != nil {
		return err
	}
	money := map[uuid.UUID]CourseSpendStat{}
	for _, c := range out.Courses {
		money[c.TeachingCourseID] = c
	}

	rows, err := s.pool.Query(ctx, `
		WITH live AS (
		    SELECT r.teaching_course_id AS tc_id,
		           COUNT(DISTINCT a.ta_id) FILTER (WHERE r.status IN ('submitted','approved')) AS requested,
		           COUNT(DISTINCT a.ta_id) FILTER (WHERE r.status = 'approved')                AS approved,
		           BOOL_OR(r.status = 'submitted')                                              AS pending
		    FROM ta_requests r
		    JOIN teaching_courses tc ON tc.id = r.teaching_course_id AND tc.term_id = $1
		    LEFT JOIN ta_request_assignments a ON a.request_id = r.id AND a.state <> 'dropped'
		    WHERE r.status IN ('submitted','approved')
		    GROUP BY r.teaching_course_id
		)
		SELECT tc.id, tc.code, tc.name_th, COALESCE(tc.level, ''),
		       COALESCE((SELECT sx.curriculum FROM sections sx
		                  WHERE sx.teaching_course_id = tc.id AND sx.curriculum IS NOT NULL
		                  GROUP BY sx.curriculum
		                  ORDER BY COUNT(*) DESC, sx.curriculum LIMIT 1), ''),
		       COALESCE((SELECT ARRAY_AGG(TRIM(COALESCE(u.title,'') || u.first_name || ' ' || u.last_name)
		                                  ORDER BY tl.is_primary DESC, u.first_name)
		                  FROM teaching_lecturers tl JOIN users u ON u.id = tl.lecturer_id
		                  WHERE tl.teaching_course_id = tc.id), '{}'),
		       (tc.num_students_regular = 0
		        OR (tc.num_students_special = 0
		            AND EXISTS (SELECT 1 FROM sections sx
		                        WHERE sx.teaching_course_id = tc.id AND sx.track = 'special'))),
		       COALESCE(live.requested, 0), COALESCE(live.approved, 0), COALESCE(live.pending, FALSE),
		       `+UnresolvedMakeupsSQL("tc")+`
		FROM teaching_courses tc
		LEFT JOIN live ON live.tc_id = tc.id
		WHERE tc.term_id = $1
		ORDER BY tc.code`, tid)
	if err != nil {
		return err
	}
	defer rows.Close()
	out.Staffing = []CourseStaffing{}
	for rows.Next() {
		var c CourseStaffing
		var makeups int64
		if err := rows.Scan(&c.TeachingCourseID, &c.Code, &c.NameTH, &c.Level, &c.Curriculum,
			&c.Lecturers, &c.StudentsMissing, &c.Requested, &c.Approved, &c.PendingRequest, &makeups); err != nil {
			return err
		}
		c.UnresolvedMakeups = int(makeups)
		rec := recs[c.TeachingCourseID]
		c.Students, c.Sittings, c.Recommended, c.Ceiling = rec.Students, rec.Sittings, rec.Recommended, rec.Ceiling
		if m, ok := money[c.TeachingCourseID]; ok {
			c.SpentBaht, c.CapBaht, c.ForecastBaht, c.UnfundedBaht = m.SpentBaht, m.CapBaht, m.ForecastBaht, m.UnfundedBaht
		}
		if c.Requested > 0 && c.Students > 0 {
			c.StudentsPerTA = math.Round(float64(c.Students)/float64(c.Requested)*10) / 10
		}
		c.Status = staffingStatus(c)
		if c.Lecturers == nil {
			c.Lecturers = []string{}
		}
		out.Staffing = append(out.Staffing, c)
	}
	return rows.Err()
}

func staffingStatus(c CourseStaffing) string {
	switch {
	case c.Requested == 0:
		return StaffingNoRequest
	case c.Students == 0:
		return StaffingNoStudents
	case c.Requested > c.Ceiling:
		return StaffingOverCeiling
	case c.Requested > c.Recommended:
		return StaffingAboveGuide
	case c.Requested == c.Recommended:
		return StaffingMatch
	default:
		return StaffingUnder
	}
}

// taCounts: "TA ทั้งหมด" vs "TA ที่ปฏิบัติงานจริง" (≥ 1 approved work log this
// term — the definition agreed on 26/09/2026), plus the level split.
func (s *DashboardService) taCounts(ctx context.Context, out *TermAnalytics, tid uuid.UUID) error {
	return s.pool.QueryRow(ctx, `
		WITH seats AS (
		    SELECT a.ta_id, a.id AS assignment_id, a.level::text AS level
		    FROM ta_request_assignments a
		    JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		    JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		    WHERE tc.term_id = $1 AND a.state <> 'dropped'
		)
		SELECT
		  (SELECT COUNT(DISTINCT s.ta_id) FROM seats s
		    WHERE EXISTS (SELECT 1 FROM work_logs wl
		                  WHERE wl.assignment_id = s.assignment_id AND wl.status = 'approved')),
		  (SELECT COUNT(DISTINCT ta_id) FROM seats WHERE level = 'undergrad'),
		  (SELECT COUNT(DISTINCT ta_id) FROM seats WHERE level IN ('master','phd'))`, tid).
		Scan(&out.ActiveTAs, &out.TAsUndergrad, &out.TAsGraduate)
}

// pipeline reuses Executive's counts — the same predicates the sidebar badges
// and the pages they link to use — and adds what went backwards.
func (s *DashboardService) pipeline(ctx context.Context, out *TermAnalytics, tid uuid.UUID) error {
	sum, err := s.Executive(ctx, &tid, nil, s.appointments)
	if err != nil {
		return err
	}
	p := &PipelineSummary{
		MissingStudents: sum.MissingStudentCounts,
		PayoutCourses:   sum.PayoutCoursesActionable,
	}
	var oldestReq *float64
	_ = s.pool.QueryRow(ctx, `
		SELECT EXTRACT(EPOCH FROM NOW() - MIN(r.submitted_at)) / 86400
		FROM ta_requests r JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		WHERE tc.term_id = $1 AND r.status = 'submitted'`, tid).Scan(&oldestReq)
	var oldestDoc *float64
	_ = s.pool.QueryRow(ctx, `
		SELECT EXTRACT(EPOCH FROM NOW() - MIN(p.completed_at)) / 86400
		FROM ta_profiles p
		WHERE p.status IN ('submitted','needs_fix') AND `+ProfileDocsInSQL("p.user_id")+` = 3`).Scan(&oldestDoc)
	days := func(v *float64) *int {
		if v == nil {
			return nil
		}
		d := int(math.Floor(*v))
		return &d
	}
	p.Stages = []PipelineStage{
		{Key: "requests", Count: sum.PendingTARequests, OldestDays: days(oldestReq)},
		{Key: "documents", Count: sum.PendingReviews, OldestDays: days(oldestDoc)},
		{Key: "appointments", Count: sum.PendingAppointments},
		{Key: "payout_review", Count: sum.PendingPayoutReviews},
		{Key: "export", Count: sum.ReadyToExport},
	}

	_ = s.pool.QueryRow(ctx, `
		WITH term_tas AS (
		    SELECT DISTINCT a.ta_id
		    FROM ta_request_assignments a
		    JOIN ta_requests r ON r.id = a.request_id AND r.status IN ('submitted','approved')
		    JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		    WHERE tc.term_id = $1 AND a.state <> 'dropped'
		)
		SELECT
		  (SELECT COUNT(*) FROM ta_profiles p JOIN term_tas t ON t.ta_id = p.user_id
		    WHERE p.status IN ('needs_fix','rejected')),
		  (SELECT COUNT(*) FROM submission_period_status st
		     JOIN submission_periods sp ON sp.id = st.submission_period_id
		    WHERE sp.term_id = $1 AND st.status = 'pending' AND st.sent_back_at IS NOT NULL),
		  (SELECT COUNT(*) FROM work_logs wl
		     JOIN ta_request_assignments a ON a.id = wl.assignment_id AND a.state <> 'dropped'
		     JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		     JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		    WHERE tc.term_id = $1 AND wl.status = 'rejected'),
		  (SELECT COUNT(*) FROM ta_requests r JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		    WHERE tc.term_id = $1 AND r.status = 'rejected')`, tid).
		Scan(&p.DocsReturned, &p.MonthsSentBack, &p.WorklogsRejected, &p.RequestsRejected)

	for _, c := range out.Staffing {
		p.UnresolvedMakeups += c.UnresolvedMakeups
		if c.Status == StaffingNoRequest && c.Students > 0 {
			p.CoursesNoTA++
		}
	}
	out.Pipeline = p
	return nil
}

// monthFlow places every (month × TA × course) that has logged work in the
// bucket of whoever must act next, and finds the next deadline.
func (s *DashboardService) monthFlow(ctx context.Context, out *TermAnalytics, tid uuid.UUID) error {
	paid := map[string]float64{}
	for _, m := range out.Monthly {
		paid[m.YearMonth] = m.Baht // Gregorian "2026-06"
	}

	periods := []*MonthFlow{}
	byID := map[uuid.UUID]*MonthFlow{}
	type periodMeta struct {
		due    time.Time
		remind int
		closed bool
	}
	meta := map[uuid.UUID]periodMeta{}
	prow, err := s.pool.Query(ctx, `
		SELECT id, year_month, COALESCE(label, ''), due_date, remind_days_before, is_closed
		FROM submission_periods WHERE term_id = $1 ORDER BY year_month`, tid)
	if err != nil {
		return err
	}
	for prow.Next() {
		var id uuid.UUID
		var ym, label string
		var due time.Time
		var remind int
		var closed bool
		if err := prow.Scan(&id, &ym, &label, &due, &remind, &closed); err != nil {
			prow.Close()
			return err
		}
		f := &MonthFlow{YearMonth: ym, Label: label, DueDate: due.Format("2006-01-02"), IsClosed: closed}
		f.BahtPaid = round2(paid[gregorianYM(ym)])
		periods = append(periods, f)
		byID[id] = f
		meta[id] = periodMeta{due: due, remind: remind, closed: closed}
	}
	prow.Close()
	if err := prow.Err(); err != nil {
		return err
	}

	rows, err := s.pool.Query(ctx, `
		WITH m AS (
		    SELECT sp.id AS period_id, a.ta_id, tc.id AS tc_id,
		           COALESCE(st.status, 'pending') AS status,
		           BOOL_OR(st.sent_back_at IS NOT NULL) AS sent_back,
		           COUNT(*) FILTER (WHERE wl.status IN ('draft','rejected')) AS with_ta,
		           COUNT(*) FILTER (WHERE wl.status = 'submitted')           AS with_lecturer,
		           BOOL_OR(`+AppointedSQL("tc.id", "a.ta_id")+`)            AS appointed
		    FROM teaching_courses tc
		    JOIN academic_terms trm       ON trm.id = tc.term_id
		    JOIN submission_periods sp    ON sp.term_id = tc.term_id
		    JOIN sections sec             ON sec.teaching_course_id = tc.id
		    JOIN ta_request_assignments a ON a.section_id = sec.id AND a.state <> 'dropped'
		    JOIN ta_requests r            ON r.id = a.request_id AND r.status = 'approved'
		    JOIN work_logs wl             ON wl.assignment_id = a.id
		                                 AND `+workLogInPeriodSQL("wl", "trm", "sp")+`
		    LEFT JOIN submission_period_status st
		           ON st.submission_period_id = sp.id
		          AND st.ta_id = a.ta_id
		          AND st.teaching_course_id = tc.id
		    WHERE tc.term_id = $1
		      -- Grad-special holders no longer log; leftovers are no one's queue.
		      AND (a.level::text NOT IN ('master','phd') OR sec.track <> 'special')
		    GROUP BY sp.id, a.ta_id, tc.id, COALESCE(st.status, 'pending')
		)
		SELECT period_id,
		       CASE
		         WHEN status = 'finance_sent'   THEN 'finance'
		         WHEN status = 'exported'       THEN 'exported'
		         WHEN status = 'staff_reviewed' THEN 'export'
		         WHEN status = 'skipped'        THEN 'skipped'
		         WHEN with_ta > 0               THEN 'ta'
		         WHEN with_lecturer > 0         THEN 'lecturer'
		         WHEN NOT appointed             THEN 'appoint'
		         ELSE 'review'
		       END AS bucket,
		       COUNT(*),
		       COUNT(*) FILTER (WHERE sent_back AND status = 'pending')
		FROM m GROUP BY 1, 2`, tid)
	if err != nil {
		return err
	}
	for rows.Next() {
		var pid uuid.UUID
		var bucket string
		var n, sentBack int
		if err := rows.Scan(&pid, &bucket, &n, &sentBack); err != nil {
			rows.Close()
			return err
		}
		f, ok := byID[pid]
		if !ok {
			continue
		}
		switch bucket {
		case "finance":
			f.Finance += n
		case "exported":
			f.Exported += n
		case "export":
			f.Export += n
		case "skipped":
			f.Skipped += n
		case "ta":
			f.WithTA += n
		case "lecturer":
			f.Lecturer += n
		case "appoint":
			f.Appoint += n
		default:
			f.Review += n
		}
		f.SentBack += sentBack
		f.Total += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	out.Flow = make([]MonthFlow, 0, len(periods))
	for _, f := range periods {
		out.Flow = append(out.Flow, *f)
	}

	// Next deadline: the earliest open period due today or later; every period
	// sharing that date is named (June–August 2569 were all due 30/09).
	// "Today" is the database's CURRENT_DATE — the pool pins Asia/Bangkok, and
	// due_date is a DATE compared the same way everywhere else.
	var today time.Time
	if err := s.pool.QueryRow(ctx, `SELECT CURRENT_DATE`).Scan(&today); err != nil {
		return err
	}
	var next *time.Time
	for _, m := range meta {
		if m.closed || m.due.Before(today) {
			continue
		}
		if next == nil || m.due.Before(*next) {
			d := m.due
			next = &d
		}
	}
	if next != nil {
		dl := &DeadlineInfo{DueDate: next.Format("2006-01-02"), Months: []string{}, Labels: []string{}}
		dl.DaysLeft = int(math.Round(next.Sub(today).Hours() / 24))
		for id, m := range meta {
			if m.closed || !m.due.Equal(*next) {
				continue
			}
			f := byID[id]
			dl.Months = append(dl.Months, f.YearMonth)
			dl.NotSent += f.WithTA
			if m.remind > dl.RemindDays {
				dl.RemindDays = m.remind
			}
		}
		sort.Strings(dl.Months)
		for _, ym := range dl.Months {
			for _, f := range periods {
				if f.YearMonth == ym {
					dl.Labels = append(dl.Labels, f.Label)
				}
			}
		}
		out.Deadline = dl
	}
	return nil
}

func (s *DashboardService) docStatus(ctx context.Context, out *TermAnalytics, tid uuid.UUID) error {
	d := &out.Docs
	return s.pool.QueryRow(ctx, `
		WITH term_tas AS (
		    SELECT DISTINCT a.ta_id
		    FROM ta_request_assignments a
		    JOIN ta_requests r ON r.id = a.request_id AND r.status IN ('submitted','approved')
		    JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		    WHERE tc.term_id = $1 AND a.state <> 'dropped'
		)
		SELECT
		  COUNT(*) FILTER (WHERE p.user_id IS NULL OR p.status = 'pending'
		                   OR (p.status = 'submitted' AND `+ProfileDocsInSQL("t.ta_id")+` < 3)),
		  COUNT(*) FILTER (WHERE p.status = 'submitted' AND `+ProfileDocsInSQL("t.ta_id")+` = 3),
		  COUNT(*) FILTER (WHERE p.status = 'needs_fix'),
		  COUNT(*) FILTER (WHERE p.status = 'rejected'),
		  COUNT(*) FILTER (WHERE p.status = 'approved'),
		  COUNT(*)
		FROM term_tas t LEFT JOIN ta_profiles p ON p.user_id = t.ta_id`, tid).
		Scan(&d.NotSubmitted, &d.Submitted, &d.NeedsFix, &d.Rejected, &d.Approved, &d.Total)
}

// gregorianYM turns the periods' BE "2569-06" into the settlement's "2026-06".
func gregorianYM(be string) string {
	if len(be) != 7 {
		return be
	}
	y := 0
	for _, ch := range be[:4] {
		if ch < '0' || ch > '9' {
			return be
		}
		y = y*10 + int(ch-'0')
	}
	if y > 2400 {
		y -= 543
	}
	return itoa(y) + be[4:]
}
