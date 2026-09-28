package service

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// A pending TA request waits only on its TAs' class timetables. The page must
// name only the TAs who have none, and the reminder must reach only them.
func remindFixture(t *testing.T) (*fixture, *AppointmentOrderService, uuid.UUID) {
	t.Helper()
	f := newFixture(t, fixtureOpts{RequestStatus: "submitted", NoOwnClassSchedule: true})
	svc := &AppointmentOrderService{pool: f.Pool, aud: audit.New(f.Pool), notify: f.Svc.notify}

	// A second TA on the same request who HAS entered a timetable.
	ready := f.insertUser("ta", "ta-ready")
	f.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
	        VALUES (gen_random_uuid(), $1, $2, $3, 'undergrad'::study_level)`, f.RequestID, f.SectionID, ready)
	f.exec(`INSERT INTO ta_class_schedules (id, user_id, term_id, course_code, course_name, sec_no, kind, day_of_week, start_time, end_time)
	        VALUES (gen_random_uuid(), $1, $2, 'ZZ000', 'วิชาของ TA เอง', '1', 'lecture', 0, '07:00', '08:00')`, ready, f.TermID)
	return f, svc, ready
}

func TestSkippedCourses_NameOnlyTAsWithoutTimetable(t *testing.T) {
	f, svc, ready := remindFixture(t)
	skipped, err := svc.skippedCourses(f.ctx, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %+v", skipped)
	}
	w := skipped[0].Waiting
	if len(w) != 1 || w[0].TAID != f.TAID {
		t.Fatalf("waiting = %+v, want only the TA without a timetable", w)
	}
	for _, x := range w {
		if x.TAID == ready {
			t.Error("a TA who entered a timetable is listed as holding the course up")
		}
	}

	// Once every timetable is in, the course is still held (the sweep has not
	// run) but nobody is named and the reason says so.
	f.exec(`INSERT INTO ta_class_schedules (id, user_id, term_id, course_code, course_name, sec_no, kind, day_of_week, start_time, end_time)
	        VALUES (gen_random_uuid(), $1, $2, 'ZZ001', 'x', '1', 'lecture', 0, '07:00', '08:00')`, f.TAID, f.TermID)
	skipped, _ = svc.skippedCourses(f.ctx, f.TermID)
	if len(skipped) != 1 || len(skipped[0].Waiting) != 0 || !strings.Contains(skipped[0].Reason, "ภายใน 1 ชั่วโมง") {
		t.Errorf("all timetables in: %+v", skipped)
	}
}

func TestRemindTimetable_OnlyWaitingTAs_OncePerDay(t *testing.T) {
	f, svc, ready := remindFixture(t)
	actor := f.StaffID

	res, err := svc.RemindTimetable(f.ctx, actor, f.TermID, []uuid.UUID{f.TAID, ready})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sent) != 1 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "บันทึกตารางเรียนแล้ว") {
		t.Fatalf("res = %+v, want the waiting TA reminded and the ready one skipped", res)
	}

	var title, body, link string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT title, body, link FROM notifications WHERE user_id = $1 AND channel = 'in_app'`, f.TAID).
		Scan(&title, &body, &link); err != nil {
		t.Fatalf("no notice: %v", err)
	}
	if !strings.Contains(title, "ตารางเรียน") || !strings.Contains(title, "ปีการศึกษา") || link != "/ta/schedule" {
		t.Errorf("title=%q link=%q", title, link)
	}
	if !strings.Contains(body, "ยังไม่ได้บันทึกตารางเรียน") {
		t.Errorf("body = %s", body)
	}
	var n int
	_ = f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM notifications WHERE user_id = $1`, ready).Scan(&n)
	if n != 0 {
		t.Errorf("TA with a timetable got %d notices", n)
	}

	// A second click the same day reminds nobody.
	res, err = svc.RemindTimetable(f.ctx, actor, f.TermID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sent) != 0 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "24 ชั่วโมง") {
		t.Errorf("second reminder: %+v", res)
	}

	// The page shows when the TA was last reminded.
	skipped, _ := svc.skippedCourses(f.ctx, f.TermID)
	if len(skipped) != 1 || skipped[0].Waiting[0].RemindedAt == nil {
		t.Errorf("reminded_at not reported: %+v", skipped)
	}

	var audited int
	_ = f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM audit_logs WHERE action = 'appointment.remind_timetable'`).Scan(&audited)
	if audited != 2 {
		t.Errorf("audit rows = %d, want 2", audited)
	}
}
