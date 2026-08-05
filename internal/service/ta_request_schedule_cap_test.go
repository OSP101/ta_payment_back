package service

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

// The ceiling on declared workload used to come from the credit notation —
// teaching_courses.lecture_hrs / lab_hrs, the figures inside 3(2-2-5). Those do
// not track reality: a course carrying 1 credit-hour of lab can meet for 3 real
// hours, and the TA standing in that room for 3 hours was capped at 1.
//
// These tests pin the ceiling to section_schedules, summed per week per section
// per kind, which is the number the work actually takes.

var capTermSeq = 5000

// capFixture is one course with sections whose timetable we control directly.
type capFixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	ctx  context.Context
	svc  *TARequestService
	tc   uuid.UUID
	lec  uuid.UUID
	ta   uuid.UUID
	secs map[string]uuid.UUID
}

// newCapFixture builds a course whose CREDIT hours deliberately disagree with
// the timetable that gets attached later: credits say 1 lecture / 1 lab, so any
// cap derived from them is 1.0 and any test asking for 3.0 would fail.
func newCapFixture(t *testing.T) *capFixture {
	t.Helper()
	pool := testutil.NewPool(t)
	ctx := context.Background()
	f := &capFixture{
		t: t, pool: pool, ctx: ctx,
		svc:  &TARequestService{pool: pool, aud: audit.New(pool)},
		secs: map[string]uuid.UUID{},
	}
	capTermSeq++
	term := uuid.New()
	f.lec, f.ta, f.tc = uuid.New(), uuid.New(), uuid.New()
	f.exec(`INSERT INTO academic_terms (id, academic_year, semester, is_active)
	        VALUES ($1,$2,1,FALSE)`, term, capTermSeq)
	f.exec(`INSERT INTO users (id,email,first_name,last_name,is_active)
	        VALUES ($1,$2,'อาจารย์','ทดสอบ',TRUE)`, f.lec, "lec-"+f.lec.String()+"@example.test")
	f.exec(`INSERT INTO users (id,email,first_name,last_name,study_level,is_active)
	        VALUES ($1,$2,'ผู้ช่วย','ทดสอบ','undergrad',TRUE)`, f.ta, "ta-"+f.ta.String()+"@example.test")
	f.exec(`INSERT INTO teaching_courses (id,term_id,code,name_th,level,credits,lecture_hrs,lab_hrs,self_hrs,num_students)
	        VALUES ($1,$2,$3,'วิชาทดสอบ','undergrad',3,1,1,5,40)`,
		f.tc, term, "SC"+f.tc.String()[:6])
	f.exec(`INSERT INTO teaching_lecturers (teaching_course_id,lecturer_id,is_primary) VALUES ($1,$2,TRUE)`, f.tc, f.lec)
	// The announced rates. enforceDailyHourFeasibility reads its caps from here
	// and returns "no opinion" when the table is empty, so a fixture without
	// them would let every hour figure through and make the limit tests vacuous.
	f.exec(`INSERT INTO pay_rates (
	          effective_from, undergrad_regular, undergrad_special,
	          graduate_regular, graduate_special_lumpsum, graduate_regular_hourly,
	          ug_special_monthly_cap, daily_pay_cap_baht,
	          ug_regular_daily_hour_cap, ug_special_daily_hour_cap, grad_regular_daily_hour_cap)
	        VALUES ('2020-01-01', 40, 50, 50, 4000, 50, 2000, 300, 7, 6, 6)`)
	return f
}

func (f *capFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("fixture: %v\nSQL: %s", err, sql)
	}
}

// addSection creates a section and attaches weekly sessions to it.
func (f *capFixture) addSection(secNo string, sessions ...[3]string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO sections (id,teaching_course_id,sec_no,track) VALUES ($1,$2,$3,'regular')`, id, f.tc, secNo)
	for _, sc := range sessions {
		f.exec(`INSERT INTO section_schedules (section_id, kind, day_of_week, start_time, end_time)
		        VALUES ($1,$2,1,$3::time,$4::time)`, id, sc[0], sc[1], sc[2])
	}
	f.secs[secNo] = id
	return id
}

// weekly reads the ceiling the service will apply to a section.
func (f *capFixture) weekly(secID uuid.UUID) sectionWeekly {
	f.t.Helper()
	m, err := f.svc.sectionWeeklyHours(f.ctx, []uuid.UUID{secID})
	if err != nil {
		f.t.Fatalf("sectionWeeklyHours: %v", err)
	}
	return m[secID]
}

func ugWorkload(check, attendance, ugOther, lab, labOther float64) WorkloadInput {
	return WorkloadInput{
		CheckWorkHrs: check, AttendanceHrs: attendance,
		UGOtherHrs: ugOther, LabHrs: lab, LabOtherHrs: labOther,
	}
}

// The case that prompted the change: 1 credit-hour of lab, 3 real hours taught.
func TestCapFollowsTheTimetableNotTheCredits(t *testing.T) {
	f := newCapFixture(t)
	sec := f.addSection("1", [3]string{"lab", "09:00", "12:00"}) // 3 real hours

	got := f.weekly(sec)
	if got.Lab != 3 {
		t.Fatalf("the ceiling must be the 3 hours actually taught, got %.2f", got.Lab)
	}

	// 3.0 fits the real session; the old credit-derived ceiling was 1.0.
	if err := validateUndergradSectionCaps(ugWorkload(0, 0, 0, 3, 0), "ผู้ช่วย ทดสอบ", "1", got); err != nil {
		t.Fatalf("3 hours must be allowed on a 3-hour lab: %v", err)
	}
	// A hair over is still refused — the cap is real, just correctly sourced.
	err := validateUndergradSectionCaps(ugWorkload(0, 0, 0, 3.5, 0), "ผู้ช่วย ทดสอบ", "1", got)
	if err == nil {
		t.Fatal("3.5 hours must exceed a 3-hour lab")
	}
	if !strings.Contains(err.Error(), "3.00") {
		t.Fatalf("the message should quote the real ceiling, got %q", err)
	}
}

// Two sections of one course can meet for different lengths; each carries its
// own ceiling rather than a single course-wide figure.
func TestCapIsPerSection(t *testing.T) {
	f := newCapFixture(t)
	short := f.addSection("1", [3]string{"lecture", "09:00", "10:00"})
	long := f.addSection("2", [3]string{"lecture", "09:00", "13:00"})

	if got := f.weekly(short).Lecture; got != 1 {
		t.Fatalf("sec 1 ceiling: want 1, got %.2f", got)
	}
	if got := f.weekly(long).Lecture; got != 4 {
		t.Fatalf("sec 2 ceiling: want 4, got %.2f", got)
	}
	w := ugWorkload(0, 3, 0, 0, 0)
	if err := validateUndergradSectionCaps(w, "ผู้ช่วย", "2", f.weekly(long)); err != nil {
		t.Fatalf("3h fits the 4h section: %v", err)
	}
	if err := validateUndergradSectionCaps(w, "ผู้ช่วย", "1", f.weekly(short)); err == nil {
		t.Fatal("3h must not fit the 1h section")
	}
}

// Several meetings a week add up — the ceiling is the weekly total, which is
// what the form asks the lecturer for.
func TestCapSumsEverySessionInTheWeek(t *testing.T) {
	f := newCapFixture(t)
	sec := f.addSection("1",
		[3]string{"lecture", "09:00", "10:30"},
		[3]string{"lecture", "13:00", "14:30"})

	if got := f.weekly(sec).Lecture; got != 3 {
		t.Fatalf("two 1.5h lectures should give a 3h weekly ceiling, got %.2f", got)
	}
}

// A kind the section never meets for admits no hours at all, and says why.
func TestKindAbsentFromTheTimetableAdmitsNothing(t *testing.T) {
	f := newCapFixture(t)
	sec := f.addSection("1", [3]string{"lecture", "09:00", "12:00"})

	err := validateUndergradSectionCaps(ugWorkload(0, 0, 0, 1, 0), "ผู้ช่วย", "1", f.weekly(sec))
	if err == nil {
		t.Fatal("a section with no lab sessions must refuse lab hours")
	}
	if !strings.Contains(err.Error(), "ไม่มีคาบประเภทนี้ในตารางสอน") {
		t.Fatalf("the message should point at the timetable, got %q", err)
	}
	_ = fmt.Sprint(sec)
}
