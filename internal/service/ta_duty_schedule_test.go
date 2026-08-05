package service

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

// A TA could nominate weekly grading slots for an assignment whose lecturer
// never asked for grading, then press "สร้างอัตโนมัติ" and get nothing back,
// with no explanation anywhere. And "อื่น ๆ" had no slot table at all, so
// declared other-work could never reach the timetable.
//
// These tests pin the two halves of the fix: a slot is refused unless the
// lecturer declared that duty, and it is bounded by the DECLARED hours rather
// than by the section's timetable.

var dutyTermSeq = 7000

type dutyFixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	ctx    context.Context
	svc    *WorkLogService
	ta     uuid.UUID
	asg    uuid.UUID
	secID  uuid.UUID
	termID uuid.UUID
}

// newDutyFixture builds one approved assignment whose workload form declares
// exactly the hours passed in.
func newDutyFixture(t *testing.T, check, ugOther, labOther float64) *dutyFixture {
	t.Helper()
	pool := testutil.NewPool(t)
	ctx := context.Background()
	f := &dutyFixture{t: t, pool: pool, ctx: ctx,
		svc: &WorkLogService{pool: pool, aud: audit.New(pool)}}

	dutyTermSeq++
	f.termID = uuid.New()
	lec := uuid.New()
	f.ta = uuid.New()
	tc, req := uuid.New(), uuid.New()
	f.secID, f.asg = uuid.New(), uuid.New()

	f.exec(`INSERT INTO academic_terms (id,academic_year,semester,is_active,starts_on,ends_on)
	        VALUES ($1,$2,1,FALSE,'2026-06-22','2026-10-18')`, f.termID, dutyTermSeq)
	f.exec(`INSERT INTO users (id,email,first_name,last_name,is_active)
	        VALUES ($1,$2,'อาจารย์','ทดสอบ',TRUE)`, lec, "lec-"+lec.String()+"@example.test")
	f.exec(`INSERT INTO users (id,email,first_name,last_name,study_level,is_active)
	        VALUES ($1,$2,'ผู้ช่วย','ทดสอบ','undergrad',TRUE)`, f.ta, "ta-"+f.ta.String()+"@example.test")
	f.exec(`INSERT INTO teaching_courses (id,term_id,code,name_th,level,credits,lecture_hrs,lab_hrs,self_hrs,num_students)
	        VALUES ($1,$2,$3,'วิชาทดสอบ','undergrad',3,3,3,5,40)`, tc, f.termID, "SC"+tc.String()[:6])
	f.exec(`INSERT INTO teaching_lecturers (teaching_course_id,lecturer_id,is_primary) VALUES ($1,$2,TRUE)`, tc, lec)
	f.exec(`INSERT INTO ta_requests (id,teaching_course_id,lecturer_id,reimburse_scope,status,submitted_at)
	        VALUES ($1,$2,$3,'both','approved',NOW())`, req, tc, lec)
	f.exec(`INSERT INTO sections (id,teaching_course_id,sec_no,track) VALUES ($1,$2,'1','regular')`, f.secID, tc)
	f.exec(`INSERT INTO section_schedules (section_id,kind,day_of_week,start_time,end_time)
	        VALUES ($1,'lecture',1,'09:00','12:00')`, f.secID)
	f.exec(`INSERT INTO ta_request_assignments (id,request_id,section_id,ta_id,level)
	        VALUES ($1,$2,$3,$4,'undergrad')`, f.asg, req, f.secID, f.ta)
	f.exec(`INSERT INTO ta_workload_forms (assignment_id, check_work_hrs, ug_other_hrs, lab_other_hrs)
	        VALUES ($1,$2,$3,$4)`, f.asg, check, ugOther, labOther)
	return f
}

func (f *dutyFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("fixture: %v\nSQL: %s", err, sql)
	}
}

func dutySlot(kind, start, end string) TAReviewScheduleInput {
	return TAReviewScheduleInput{Kind: kind, DayOfWeek: 3, StartTime: start, EndTime: end}
}

// A duty the lecturer never asked for cannot be scheduled — and says so.
func TestUndeclaredDutyCannotBeScheduled(t *testing.T) {
	f := newDutyFixture(t, 0, 2, 0) // other-work declared, grading NOT

	_, err := f.svc.AddTAReviewSchedule(f.ctx, f.ta, f.asg, dutySlot(DutyReview, "13:00", "14:00"))
	if err == nil {
		t.Fatal("grading slots must be refused when no grading hours were declared")
	}
	if !strings.Contains(err.Error(), "ช่วยตรวจงาน") {
		t.Fatalf("the message should name the duty, got %q", err)
	}

	// The duty that WAS declared goes through.
	if _, err := f.svc.AddTAReviewSchedule(f.ctx, f.ta, f.asg, dutySlot(DutyOtherLecture, "13:00", "14:00")); err != nil {
		t.Fatalf("declared other-work must be schedulable: %v", err)
	}
}

// The ceiling is the lecturer's declared hours, NOT the section's timetable.
// The section here meets for 3 lecture hours but only 1 hour was declared.
func TestSlotsAreBoundedByDeclaredHoursNotTheTimetable(t *testing.T) {
	f := newDutyFixture(t, 1, 0, 0)

	_, err := f.svc.AddTAReviewSchedule(f.ctx, f.ta, f.asg, dutySlot(DutyReview, "13:00", "15:00"))
	if err == nil {
		t.Fatal("2h must not fit inside 1 declared hour, even though the section meets for 3")
	}
	if !strings.Contains(err.Error(), "1.0") {
		t.Fatalf("the message should quote the declared ceiling, got %q", err)
	}
	if _, err := f.svc.AddTAReviewSchedule(f.ctx, f.ta, f.asg, dutySlot(DutyReview, "13:00", "14:00")); err != nil {
		t.Fatalf("1h fits exactly 1 declared hour: %v", err)
	}
}

// Each duty has its own budget; spending one must not consume another.
func TestDutyBudgetsAreSeparate(t *testing.T) {
	f := newDutyFixture(t, 1, 1, 0)

	if _, err := f.svc.AddTAReviewSchedule(f.ctx, f.ta, f.asg, dutySlot(DutyReview, "13:00", "14:00")); err != nil {
		t.Fatalf("grading slot: %v", err)
	}
	// Grading is now full. Other-work has its own untouched hour.
	if _, err := f.svc.AddTAReviewSchedule(f.ctx, f.ta, f.asg, dutySlot(DutyOtherLecture, "15:00", "16:00")); err != nil {
		t.Fatalf("other-work has its own budget and must not be blocked by grading: %v", err)
	}
	// But a second grading hour is over its own ceiling.
	if _, err := f.svc.AddTAReviewSchedule(f.ctx, f.ta, f.asg, dutySlot(DutyReview, "16:00", "17:00")); err == nil {
		t.Fatal("a second grading hour must exceed the 1h grading budget")
	}
}

// The list reports the per-duty budget so the UI can decide which cards exist.
func TestListReportsDeclaredAndUsedPerDuty(t *testing.T) {
	f := newDutyFixture(t, 2, 1, 0)
	if _, err := f.svc.AddTAReviewSchedule(f.ctx, f.ta, f.asg, dutySlot(DutyReview, "13:00", "14:00")); err != nil {
		t.Fatalf("add: %v", err)
	}

	got, err := f.svc.ListTAReviewSchedules(f.ctx, f.ta, f.asg)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got.Declared[DutyReview] != 2 || got.Declared[DutyOtherLecture] != 1 {
		t.Fatalf("declared budgets wrong: %+v", got.Declared)
	}
	if got.Declared[DutyOtherLab] != 0 {
		t.Fatalf("a duty with no declared hours must report 0 so its card is hidden: %+v", got.Declared)
	}
	if got.Used[DutyReview] != 1 {
		t.Fatalf("used hours wrong: %+v", got.Used)
	}
	if len(got.Items) != 1 || got.Items[0].Kind != DutyReview {
		t.Fatalf("the slot should come back carrying its kind: %+v", got.Items)
	}
}

// An unknown kind is refused rather than silently stored.
func TestUnknownDutyKindRejected(t *testing.T) {
	f := newDutyFixture(t, 2, 2, 2)

	if _, err := f.svc.AddTAReviewSchedule(f.ctx, f.ta, f.asg, dutySlot("nonsense", "13:00", "14:00")); err == nil {
		t.Fatal("an unknown duty kind must be refused")
	}
}
