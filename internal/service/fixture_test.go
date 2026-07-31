package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/testutil"
	"ta-payment-back/internal/timeutil"
)

// fixture_test.go builds the minimum real-database world a work log needs:
//
//	term → course → section → schedule → users → request → assignment → workload
//
// Every money rule in Upsert reads several of those tables, so a shortcut
// (inserting a work_log directly, or stubbing the pool) would test nothing.
//
// Design rule: the defaults put EVERY cap far out of reach. A test that wants
// to prove one rule tightens exactly that rule and leaves the rest slack, so a
// failure names the rule that broke instead of whichever limit happened to be
// lowest. Tests that skip this discipline tend to pass for the wrong reason.

// workloadHours is the lecturer's declared weekly workload. Undergrad and grad
// read disjoint field sets — see loadAssignmentContext.
type workloadHours struct {
	// undergrad
	Attendance float64 // → AllowLecture + weekly lecture cap
	Lab        float64 // → AllowLab + weekly lab cap
	CheckWork  float64 // → AllowReview + weekly review cap
	UGOther    float64 // → AllowOther + weekly other cap
	// graduate
	HelpTeach float64 // → AllowLecture AND AllowLab (shared cap)
	Grade     float64 // → AllowReview
	Other     float64 // → AllowOther (with Prep)
	Prep      float64
}

// rateOverrides mirrors the pay_rates columns the worklog caps consult. Zero
// means "use the fixture default", which is deliberately permissive.
type rateOverrides struct {
	UndergradRegular    float64
	UndergradSpecial    float64
	GradRegularHourly   float64
	DailyPayCapBaht     float64
	UGRegularDailyCap   float64
	UGSpecialDailyCap   float64
	GradRegularDailyCap float64
}

type fixtureOpts struct {
	Level          string // "undergrad" (default) | "master" | "phd"
	Track          string // "regular" (default) | "special"
	ReimburseScope string // "both" (default) | "lecture" | "lab"
	RequestStatus  string // "approved" (default)
	NumStudents    int    // default 40; 0 via NoStudents
	NoStudents     bool
	TermMonths     int    // default 4 → 16 term weeks
	TermStart      string // default "2026-06-01"
	TermEnd        string // default "2026-10-31"
	NoWorkloadForm bool
	Workload       workloadHours
	Rates          rateOverrides
	// NoRequest builds the world up to the section but stops short of creating
	// a TA request. Tests that call TARequestService.Create need this: a
	// pre-existing request for the same (course, TA) would trip the duplicate
	// rule and reject the request under test.
	NoRequest bool
	// NoOwnClassSchedule leaves the TA without a timetable of their own. Work-log
	// writes are refused in that state (see assertOwnClassScheduleFilled), so only
	// tests ABOUT that rule want it — every other test needs the realistic world,
	// where a TA who is logging hours has already filed their timetable.
	NoOwnClassSchedule bool
}

// fixture is one fully wired assignment plus the service under test.
type fixture struct {
	t    *testing.T
	ctx  context.Context
	Pool *pgxpool.Pool
	Svc  *WorkLogService

	Periods *SubmissionPeriodService

	TermID       uuid.UUID
	CourseID     uuid.UUID
	SectionID    uuid.UUID
	LecturerID   uuid.UUID
	TAID         uuid.UUID
	RequestID    uuid.UUID
	AssignmentID uuid.UUID

	// AcademicYear is needed to build submission_periods.year_month, which the
	// month lock derives as academic_year || '-' || MM of the work date.
	AcademicYear int
}

func newFixture(t *testing.T, opts fixtureOpts) *fixture {
	t.Helper()
	pool := testutil.NewPool(t)
	ctx := context.Background()

	applyFixtureDefaults(&opts)

	f := &fixture{
		t: t, ctx: ctx, Pool: pool,
		AcademicYear: 2569,
		Svc: &WorkLogService{
			pool:   pool,
			aud:    audit.New(pool),
			budget: &BudgetService{pool: pool},
			notify: &NotifyService{pool: pool, mailer: mail.New(config.Config{})},
		},
		Periods: &SubmissionPeriodService{
			pool:   pool,
			aud:    audit.New(pool),
			notify: &NotifyService{pool: pool, mailer: mail.New(config.Config{})},
		},
	}

	f.insertPayRates(opts.Rates)
	f.TermID = f.insertTerm(opts)
	f.LecturerID = f.insertUser("lecturer", "lecturer")
	f.TAID = f.insertUser("ta", "ta")
	f.CourseID = f.insertCourse(opts)
	f.SectionID = f.insertSection(opts.Track)
	f.insertSectionSchedule()
	if !opts.NoRequest {
		f.RequestID = f.insertRequest(opts)
		f.AssignmentID = f.insertAssignment(opts)
	}
	if !opts.NoOwnClassSchedule {
		// Sunday 07:00–08:00: satisfies the gate without ever colliding with the
		// hours tests actually use, so no existing case starts failing for a
		// reason it was not written about.
		f.exec(`INSERT INTO ta_class_schedules
		          (id, user_id, term_id, course_code, course_name, sec_no, kind,
		           day_of_week, start_time, end_time)
		        VALUES (gen_random_uuid(), $1, $2, 'ZZ000', 'วิชาของ TA เอง', '1',
		                'lecture', 0, '07:00', '08:00')`, f.TAID, f.TermID)
	}

	return f
}

func applyFixtureDefaults(o *fixtureOpts) {
	if o.Level == "" {
		o.Level = "undergrad"
	}
	if o.Track == "" {
		o.Track = "regular"
	}
	if o.ReimburseScope == "" {
		o.ReimburseScope = "both"
	}
	if o.RequestStatus == "" {
		o.RequestStatus = "approved"
	}
	if o.NumStudents == 0 && !o.NoStudents {
		o.NumStudents = 40
	}
	if o.NoStudents {
		o.NumStudents = 0
	}
	if o.TermMonths == 0 {
		o.TermMonths = 4
	}
	// The term must always contain "now", because Upsert applies the real
	// back-date rule (no writing into a month that has already passed) against
	// timeutil.Now(). Hard-coded dates would make this suite start failing on a
	// future calendar date for reasons having nothing to do with the code.
	if o.TermStart == "" {
		o.TermStart = monthStart().Format("2006-01-02")
	}
	if o.TermEnd == "" {
		o.TermEnd = monthStart().AddDate(0, 3, -1).Format("2006-01-02")
	}
	// Generous workload so the weekly and term ceilings never bind by accident.
	// NUMERIC(4,2) tops out at 99.99, so stay well below.
	if o.Workload == (workloadHours{}) {
		o.Workload = workloadHours{
			Attendance: 20, Lab: 20, CheckWork: 20, UGOther: 20,
			HelpTeach: 20, Grade: 20, Other: 20, Prep: 20,
		}
	}
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.Pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("fixture exec failed: %v\nSQL: %s", err, sql)
	}
}

// insertPayRates writes the single rate row every cap query reads via
// `ORDER BY effective_from DESC LIMIT 1`. Defaults keep each ceiling far away:
// a 50฿/h rate against a 100000฿ daily cap means the baht rule never fires
// unless a test asks it to.
func (f *fixture) insertPayRates(r rateOverrides) {
	orDefault := func(v, def float64) float64 {
		if v == 0 {
			return def
		}
		return v
	}
	f.exec(`
		INSERT INTO pay_rates (
			effective_from, undergrad_regular, undergrad_special,
			graduate_regular, graduate_special_lumpsum, graduate_regular_hourly,
			daily_pay_cap_baht,
			ug_regular_daily_hour_cap, ug_special_daily_hour_cap, grad_regular_daily_hour_cap)
		VALUES ('2020-01-01', $1, $2, $3, 12000, $4, $5, $6, $7, $8)`,
		orDefault(r.UndergradRegular, 50),
		orDefault(r.UndergradSpecial, 50),
		orDefault(r.GradRegularHourly, 50),
		orDefault(r.GradRegularHourly, 50),
		orDefault(r.DailyPayCapBaht, 100000),
		orDefault(r.UGRegularDailyCap, 7),
		orDefault(r.UGSpecialDailyCap, 6),
		orDefault(r.GradRegularDailyCap, 6),
	)
}

func (f *fixture) insertTerm(o fixtureOpts) uuid.UUID {
	id := uuid.New()
	f.exec(`INSERT INTO academic_terms (id, academic_year, semester, starts_on, ends_on, is_active, months)
	        VALUES ($1, $2, 1, $3::date, $4::date, TRUE, $5)`,
		id, f.AcademicYear, o.TermStart, o.TermEnd, o.TermMonths)
	return id
}

func (f *fixture) insertUser(role, tag string) uuid.UUID {
	id := uuid.New()
	// Email must be unique across the database; the UUID guarantees it.
	f.exec(`INSERT INTO users (id, email, first_name, last_name, is_active, profile_completed, study_level)
	        VALUES ($1, $2, $3, 'Test', TRUE, TRUE, 'undergrad')`,
		id, fmt.Sprintf("%s-%s@example.test", tag, id), tag)
	f.exec(`INSERT INTO user_roles (user_id, role) VALUES ($1, $2::role_code)`, id, role)
	return id
}

func (f *fixture) insertCourse(o fixtureOpts) uuid.UUID {
	id := uuid.New()
	// level is constrained to undergrad|graduate — it describes the COURSE, not
	// the TA, so a master's TA can still assist an undergrad course.
	courseLevel := "undergrad"
	f.exec(`INSERT INTO teaching_courses
	          (id, term_id, code, name_th, level, credits, lecture_hrs, lab_hrs,
	           num_students, num_students_regular, starts_on, ends_on)
	        VALUES ($1, $2, $3, 'วิชาทดสอบ', $4, 3, 3, 3, $5, $5, $6::date, $7::date)`,
		id, f.TermID, "CP"+id.String()[:6], courseLevel, o.NumStudents, o.TermStart, o.TermEnd)
	f.exec(`INSERT INTO teaching_lecturers (teaching_course_id, lecturer_id, is_primary)
	        VALUES ($1, $2, TRUE)`, id, f.LecturerID)
	return id
}

func (f *fixture) insertSection(track string) uuid.UUID {
	id := uuid.New()
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track)
	        VALUES ($1, $2, '01', $3::section_track)`, id, f.CourseID, track)
	return id
}

// insertSectionSchedule gives the section a Monday lecture and a Monday lab.
// courseDateRange and the holiday/makeup logic tolerate a section with no
// schedule, but a realistic fixture keeps later tests honest.
func (f *fixture) insertSectionSchedule() {
	f.exec(`INSERT INTO section_schedules (section_id, kind, day_of_week, start_time, end_time)
	        VALUES ($1, 'lecture', 1, '09:00', '12:00'), ($1, 'lab', 1, '13:00', '16:00')`,
		f.SectionID)
}

func (f *fixture) insertRequest(o fixtureOpts) uuid.UUID {
	id := uuid.New()
	f.exec(`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at, decided_at)
	        VALUES ($1, $2, $3, $4::reimburse_scope, $5::ta_request_status, NOW(), NOW())`,
		id, f.CourseID, f.LecturerID, o.ReimburseScope, o.RequestStatus)
	return id
}

func (f *fixture) insertAssignment(o fixtureOpts) uuid.UUID {
	id := uuid.New()
	f.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
	        VALUES ($1, $2, $3, $4, $5::study_level)`,
		id, f.RequestID, f.SectionID, f.TAID, o.Level)
	if o.NoWorkloadForm {
		return id
	}
	w := o.Workload
	f.exec(`INSERT INTO ta_workload_forms
	          (id, assignment_id, help_teach_hrs, prep_hrs, grade_hrs, other_hrs,
	           check_work_hrs, attendance_hrs, ug_other_hrs, lab_hrs)
	        VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		id, w.HelpTeach, w.Prep, w.Grade, w.Other,
		w.CheckWork, w.Attendance, w.UGOther, w.Lab)
	return id
}

// ---------------------------------------------------------------------------
// Calendar helpers
//
// Every date a test uses must sit inside the CURRENT Bangkok month. Upsert
// rejects writes into a month that has already passed, so a literal like
// "2026-08-15" would silently start failing once that month goes by. Deriving
// the dates from the clock keeps the suite stable forever.
// ---------------------------------------------------------------------------

// monthStart is midnight on the 1st of the current Bangkok month.
func monthStart() time.Time {
	y, m, _ := timeutil.Now().Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, timeutil.Bangkok)
}

// day returns the nth day of the current month as "YYYY-MM-DD". n is 1-based
// and must be ≤ 28 so it exists in February too.
func day(n int) string {
	if n < 1 || n > 28 {
		panic("fixture: day(n) must be within 1..28 so it exists in every month")
	}
	return monthStart().AddDate(0, 0, n-1).Format("2006-01-02")
}

// firstMondayOffset is the 0-based offset from the 1st to the first Monday.
// Used to build two dates guaranteed to fall in the same Monday–Sunday week,
// which is the window PostgreSQL's date_trunc('week', …) uses for the weekly
// activity cap.
func firstMondayOffset() int {
	// time.Weekday: Sunday=0 … Monday=1.
	wd := int(monthStart().Weekday())
	return (8 - wd) % 7
}

// sameWeekDays returns two dates in the same ISO week and the same month.
func sameWeekDays() (string, string) {
	off := firstMondayOffset()
	mon := monthStart().AddDate(0, 0, off)
	return mon.Format("2006-01-02"), mon.AddDate(0, 0, 1).Format("2006-01-02")
}

// currentMonthMM is the two-digit month used to key submission_periods.
func currentMonthMM() string { return timeutil.Now().Format("01") }

// openDueDate is a due date that has NOT passed, for tests that need a period
// which is open on the merits rather than by the is_closed flag.
//
// day(28) used to be used here, which made those tests fail on the 30th and
// 31st of every month: assertWorklogWritable closes a period once
// CURRENT_DATE > due_date + 1 day, so a due date pinned inside the month
// silently expires near month end. Anchoring to "today + a week" keeps the
// period open on every calendar day.
func openDueDate() string { return timeutil.Now().AddDate(0, 0, 7).Format("2006-01-02") }

// ---------------------------------------------------------------------------
// Helpers used by the behaviour tests
// ---------------------------------------------------------------------------

// entry builds a work log for this fixture's assignment. Defaults to a review
// activity, which is date-flexible (no holiday or makeup interaction), so tests
// about hour and money caps aren't derailed by the calendar rules.
func (f *fixture) entry(date, start, end string, hours float64) WorkLog {
	return WorkLog{
		AssignmentID: f.AssignmentID,
		WorkDate:     date,
		StartTime:    start,
		EndTime:      end,
		Hours:        hours,
		Activity:     "review",
	}
}

// upsert runs the real Upsert as the fixture's TA.
func (f *fixture) upsert(w WorkLog) (uuid.UUID, error) {
	return f.Svc.Upsert(f.ctx, f.TAID, w)
}

// mustUpsert fails the test if the write is rejected — used to establish
// preconditions, so a broken fixture surfaces immediately rather than as a
// confusing assertion failure later.
func (f *fixture) mustUpsert(w WorkLog) uuid.UUID {
	f.t.Helper()
	id, err := f.upsert(w)
	if err != nil {
		f.t.Fatalf("precondition write should have succeeded: %v", err)
	}
	return id
}

// addSubmissionPeriod registers a monthly period and, when status is non-empty,
// the per-(TA, course) status row the write lock consults. month is "MM".
func (f *fixture) addSubmissionPeriod(month, dueDate, status string, closed bool) uuid.UUID {
	id := uuid.New()
	ym := fmt.Sprintf("%d-%s", f.AcademicYear, month)
	f.exec(`INSERT INTO submission_periods (id, term_id, year_month, due_date, label, starts_on, is_closed)
	        VALUES ($1, $2, $3, $4::date, $5, $6::date, $7)`,
		id, f.TermID, ym, dueDate, "รอบ "+ym, dueDate, closed)
	if status != "" {
		f.exec(`INSERT INTO submission_period_status
		          (id, submission_period_id, ta_id, teaching_course_id, status)
		        VALUES (gen_random_uuid(), $1, $2, $3, $4)`,
			id, f.TAID, f.CourseID, status)
	}
	return id
}

// addAppointmentOrder records a printed appointment order (คำสั่งแต่งตั้ง)
// covering this fixture's (course, TA) pair.
//
// Needed by anything that reads the payout-review queue or the exports
// dashboard: both are gated on the order existing, because the TA's work is not
// payable until they are officially appointed (see AppointedSQL). A fixture
// without it is a course whose TA was requested but never appointed, which is a
// real state and a useful one to test — just not the default one.
func (f *fixture) addAppointmentOrder() uuid.UUID {
	orderID := uuid.New()
	// round_no is unique per term, so derive one that will not collide if a test
	// records two orders.
	var round int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COALESCE(MAX(round_no), 0) + 1 FROM appointment_orders WHERE term_id = $1`,
		f.TermID).Scan(&round); err != nil {
		f.t.Fatalf("next round_no: %v", err)
	}
	f.exec(`INSERT INTO appointment_orders
	          (id, term_id, round_no, order_no, order_date, effective_date, ta_count)
	        VALUES ($1, $2, $3, $4, '2569-01-01', '2569-01-01', 1)`,
		orderID, f.TermID, round, fmt.Sprintf("TEST-%d", round))
	f.exec(`INSERT INTO appointment_order_items
	          (id, appointment_order_id, teaching_course_id, ta_id)
	        VALUES (gen_random_uuid(), $1, $2, $3)`,
		orderID, f.CourseID, f.TAID)
	return orderID
}

// secondTAOnSameCourse adds a DIFFERENT TA to this fixture's course, with their
// own approved month of work and no appointment order.
//
// This is the shape that separates a per-pair rule from a per-course one: the
// course is on an order, this person is not. Inserted with SQL rather than
// through Upsert because the work-log service writes as the fixture's own TA.
func (f *fixture) secondTAOnSameCourse() uuid.UUID {
	ta := f.insertUser("ta", "ta2")
	reqID := uuid.New()
	f.exec(`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at, decided_at)
	        VALUES ($1, $2, $3, 'both'::reimburse_scope, 'approved'::ta_request_status, NOW(), NOW())`,
		reqID, f.CourseID, f.LecturerID)
	assignID := uuid.New()
	f.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
	        VALUES ($1, $2, $3, $4, 'undergrad'::study_level)`,
		assignID, reqID, f.SectionID, ta)
	f.exec(`INSERT INTO work_logs
	          (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES (gen_random_uuid(), $1, $2::date, '09:00', '11:00', 2, 'review', 'approved')`,
		assignID, day(11))
	return ta
}

// secondCourseAssignment wires the same TA into a DIFFERENT course so the
// cross-course rules (clock overlap, daily baht cap) have something to collide
// with. Returns the new assignment id.
func (f *fixture) secondCourseAssignment(o fixtureOpts) uuid.UUID {
	applyFixtureDefaults(&o)

	courseID := uuid.New()
	f.exec(`INSERT INTO teaching_courses
	          (id, term_id, code, name_th, level, credits, lecture_hrs, lab_hrs,
	           num_students, num_students_regular, starts_on, ends_on)
	        VALUES ($1, $2, $3, 'วิชาทดสอบสอง', 'undergrad', 3, 3, 3, $4, $4, $5::date, $6::date)`,
		courseID, f.TermID, "CP"+courseID.String()[:6], o.NumStudents, o.TermStart, o.TermEnd)
	f.exec(`INSERT INTO teaching_lecturers (teaching_course_id, lecturer_id, is_primary)
	        VALUES ($1, $2, TRUE)`, courseID, f.LecturerID)

	sectionID := uuid.New()
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track)
	        VALUES ($1, $2, '01', $3::section_track)`, sectionID, courseID, o.Track)

	requestID := uuid.New()
	f.exec(`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at, decided_at)
	        VALUES ($1, $2, $3, $4::reimburse_scope, 'approved', NOW(), NOW())`,
		requestID, courseID, f.LecturerID, o.ReimburseScope)

	assignmentID := uuid.New()
	f.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
	        VALUES ($1, $2, $3, $4, $5::study_level)`,
		assignmentID, requestID, sectionID, f.TAID, o.Level)
	w := o.Workload
	f.exec(`INSERT INTO ta_workload_forms
	          (id, assignment_id, help_teach_hrs, prep_hrs, grade_hrs, other_hrs,
	           check_work_hrs, attendance_hrs, ug_other_hrs, lab_hrs)
	        VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		assignmentID, w.HelpTeach, w.Prep, w.Grade, w.Other,
		w.CheckWork, w.Attendance, w.UGOther, w.Lab)
	return assignmentID
}

// cotaughtSiblingAssignment adds a SECOND section of the SAME course, on the
// same request, sharing a cotaught_group with the fixture's own assignment —
// the sec 1 ภาคปกติ / sec 2 โครงการพิเศษ pair that meets in one room at one hour.
//
// Both assignments get the group marker: the exemption compares the two rows'
// groups, so tagging only the newcomer would leave the pair unmatched.
func (f *fixture) cotaughtSiblingAssignment(track string) uuid.UUID {
	return f.siblingAssignment(track, 1)
}

// siblingAssignment adds a SECOND section of the SAME course on the SAME request.
// Pass group=1 for the co-taught pair (sec 1 ภาคปกติ / sec 2 โครงการพิเศษ meeting
// in one room), or nil for two sections that merely belong to the same request
// and are NOT taught together — the case that proves the exemption's NULL guard
// is doing work.
func (f *fixture) siblingAssignment(track string, group any) uuid.UUID {
	sectionID := uuid.New()
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track)
	        VALUES ($1, $2, '02', $3::section_track)`, sectionID, f.CourseID, track)
	// Same slots as the fixture section — that is what makes them co-taught.
	f.exec(`INSERT INTO section_schedules (section_id, kind, day_of_week, start_time, end_time)
	        VALUES ($1, 'lecture', 1, '09:00', '12:00'), ($1, 'lab', 1, '13:00', '16:00')`,
		sectionID)

	assignmentID := uuid.New()
	f.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level, cotaught_group)
	        VALUES ($1, $2, $3, $4, 'undergrad', $5)`,
		assignmentID, f.RequestID, sectionID, f.TAID, group)
	// Both sides carry the marker: the exemption compares the two rows' groups,
	// so tagging only the newcomer would leave the pair unmatched.
	f.exec(`UPDATE ta_request_assignments SET cotaught_group = $1 WHERE id = $2`,
		group, f.AssignmentID)
	f.exec(`INSERT INTO ta_workload_forms
	          (id, assignment_id, help_teach_hrs, prep_hrs, grade_hrs, other_hrs,
	           check_work_hrs, attendance_hrs, ug_other_hrs, lab_hrs)
	        VALUES (gen_random_uuid(), $1, 0, 0, 0, 0, 20, 20, 20, 20)`, assignmentID)
	return assignmentID
}

// groupSecondCourse tags an assignment from ANOTHER course with a co-taught
// group number. Two courses can independently number a group "1"; the exemption
// must not treat that coincidence as co-teaching.
func (f *fixture) groupSecondCourse(assignmentID uuid.UUID, group int) {
	f.exec(`UPDATE ta_request_assignments SET cotaught_group = $1 WHERE id = $2`,
		group, assignmentID)
	f.exec(`UPDATE ta_request_assignments SET cotaught_group = $1 WHERE id = $2`,
		group, f.AssignmentID)
}

// addOwnClass gives the fixture's TA a weekly class of their own, which the
// class-timetable rule must then treat as untouchable.
func (f *fixture) addOwnClass(label string, dayOfWeek int, start, end string) {
	f.exec(`INSERT INTO ta_class_schedules (user_id, term_id, course_label, day_of_week, start_time, end_time, is_wba)
	        VALUES ($1, $2, $3, $4, $5::time, $6::time, FALSE)`,
		f.TAID, f.TermID, label, dayOfWeek, start, end)
}

// addWBARow inserts the year-4 work-based-learning sentinel. It must be
// ignored by conflict checks (rule C5) — counting it would block the term.
func (f *fixture) addWBARow() {
	f.exec(`INSERT INTO ta_class_schedules (user_id, term_id, course_label, day_of_week, start_time, end_time, is_wba)
	        VALUES ($1, $2, 'WBA', 1, '00:00'::time, '00:00'::time, TRUE)`,
		f.TAID, f.TermID)
}

// weekdayOf returns the Go weekday for a "YYYY-MM-DD" date produced by day().
// nextWeekday returns the first date in the current Bangkok month falling on the
// given weekday (0=Sunday). Derived from the clock rather than hard-coded so the
// suite does not start failing on a future calendar date.
func nextWeekday(dow int) string {
	d := monthStart()
	for int(d.Weekday()) != dow {
		d = d.AddDate(0, 0, 1)
	}
	return d.Format("2006-01-02")
}

func weekdayOf(t *testing.T, date string) int {
	t.Helper()
	d, err := timeutil.ParseDate(date)
	if err != nil {
		t.Fatalf("parse %s: %v", date, err)
	}
	return int(d.Weekday())
}

// countLogs returns how many work_log rows exist for the fixture's assignment.
func (f *fixture) countLogs() int {
	f.t.Helper()
	var n int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM work_logs WHERE assignment_id = $1`, f.AssignmentID).Scan(&n); err != nil {
		f.t.Fatalf("count logs: %v", err)
	}
	return n
}
