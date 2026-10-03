package service

import (
	"strings"
	"testing"
)

// A newly appointed TA is told each month's work-log deadline, since a closed
// month is final and unsent hours are forfeited. Only months still open and
// not yet due are listed.
func TestAppointNotice_ListsUpcomingMonthlyDeadlines(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := &TARequestService{pool: f.Pool, notify: f.Svc.notify}

	f.clearPeriods()
	f.exec(`INSERT INTO submission_periods (id, term_id, year_month, starts_on, due_date, label, is_closed) VALUES
	        (gen_random_uuid(), $1, '2569-07', CURRENT_DATE - 90, CURRENT_DATE - 60, 'เดือนที่ผ่านไปแล้ว', FALSE),
	        (gen_random_uuid(), $1, '2569-08', CURRENT_DATE - 60, CURRENT_DATE + 1, 'เดือนที่ปิดรอบแล้ว', TRUE),
	        (gen_random_uuid(), $1, '2569-09', CURRENT_DATE - 30, CURRENT_DATE + 5, 'กันยายน 2569', FALSE),
	        (gen_random_uuid(), $1, '2569-10', CURRENT_DATE,      CURRENT_DATE + 35, 'ตุลาคม 2569', FALSE)`,
		f.TermID)

	svc.notifyTAsAppointed(f.ctx, f.RequestID, f.CourseID, "CP999", "วิชาทดสอบ")

	var title, body string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT title, body FROM notifications WHERE user_id = $1 AND channel = 'in_app'`, f.TAID).
		Scan(&title, &body); err != nil {
		t.Fatalf("no appointment notice: %v", err)
	}
	if !strings.Contains(title, "แต่งตั้งเป็นผู้ช่วยสอน") || !strings.Contains(title, "CP999") {
		t.Errorf("title = %q", title)
	}
	for _, want := range []string{"กันยายน 2569", "ตุลาคม 2569", "หมดเขตส่งวันที่", "กลุ่มเรียนที่"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	for _, gone := range []string{"เดือนที่ผ่านไปแล้ว", "เดือนที่ปิดรอบแล้ว"} {
		if strings.Contains(body, gone) {
			t.Errorf("body lists %q, which a new TA can no longer submit to", gone)
		}
	}
	if i, j := strings.Index(body, "กันยายน 2569"), strings.Index(body, "ตุลาคม 2569"); i > j {
		t.Error("months out of order")
	}
}

// A TA whose every section was dropped for a timetable clash is not appointed,
// so must not receive an appointment notice.
func TestAppointNotice_SkipsDroppedTA(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := &TARequestService{pool: f.Pool, notify: f.Svc.notify}
	f.exec(`UPDATE ta_request_assignments SET state = 'dropped' WHERE id = $1`, f.AssignmentID)

	svc.notifyTAsAppointed(f.ctx, f.RequestID, f.CourseID, "CP999", "วิชาทดสอบ")

	var n int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id = $1`, f.TAID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("dropped TA got %d notices, want 0", n)
	}
}

func TestAppointNoticeContent_Cases(t *testing.T) {
	periods := []appointPeriod{{label: "ตุลาคม 2569", startsOn: "2026-10-01", dueDate: "2026-11-05"}}

	_, body, layout := appointNoticeContent("CP1 วิชา", []string{"1", "2"}, true, periods)
	if layout.Table == nil || len(layout.Table.Rows) != 1 || layout.Table.Rows[0][2] != "5 พฤศจิกายน 2569" {
		t.Errorf("deadline table = %+v", layout.Table)
	}
	if !strings.Contains(body, "กลุ่มเรียนที่ 1, 2") {
		t.Errorf("sections missing: %s", body)
	}

	// No periods set yet: say so rather than show an empty table.
	_, body, layout = appointNoticeContent("CP1 วิชา", []string{"1"}, true, nil)
	if layout.Table != nil || !strings.Contains(body, "ยังไม่ได้กำหนดรอบส่ง") {
		t.Errorf("no-period case: table=%v body=%s", layout.Table, body)
	}

	// Graduate special track keeps no monthly log: no deadlines, no "log hours".
	_, body, layout = appointNoticeContent("CP1 วิชา", []string{"1"}, false, periods)
	if layout.Table != nil || strings.Contains(body, "บันทึกเวลา") {
		t.Errorf("grad-special case mentions work logs: %s", body)
	}
}
