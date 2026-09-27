package service

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// UAT DEF-006: the "เอกสารเบิกจ่ายแล้ว" notice listed months newest first, and
// its fixed title + link meant a second course's export overwrote the TA's
// unread notice about the first (unread in-app notices fold by title and link).
func TestMarkCourseExported_NoticeNamesMonthsInOrderPerCourse(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.addAppointmentOrder()
	m1 := monthStart()
	m2 := m1.AddDate(0, 1, 0)
	p1 := f.addSubmissionPeriod(m1.Format("01"), "2026-12-31", "", false)
	p2 := f.addSubmissionPeriod(m2.Format("01"), "2026-12-31", "", false)
	f.exec(`UPDATE submission_periods SET starts_on = $2, label = 'เดือนแรก' WHERE id = $1`, p1, m1)
	f.exec(`UPDATE submission_periods SET starts_on = $2, label = 'เดือนหลัง' WHERE id = $1`, p2, m2)
	f.mustUpsert(f.entry(m1.AddDate(0, 0, 9).Format("2006-01-02"), "09:00", "11:00", 2))
	f.mustUpsert(f.entry(m2.AddDate(0, 0, 9).Format("2006-01-02"), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1`, f.AssignmentID)
	// p2 first, so insertion order cannot hand the right answer back by luck.
	for _, p := range []uuid.UUID{p2, p1} {
		f.exec(`INSERT INTO submission_period_status (id, submission_period_id, ta_id, teaching_course_id, status)
		        VALUES (gen_random_uuid(), $1, $2, $3, 'staff_reviewed')`, p, f.TAID, f.CourseID)
	}

	if _, err := f.Periods.MarkCourseExported(f.ctx, f.StaffID, f.CourseID,
		[]string{m1.Format("2006-01"), m2.Format("2006-01")}); err != nil {
		t.Fatal(err)
	}

	var title, body, code string
	if err := f.Pool.QueryRow(f.ctx, `SELECT code FROM teaching_courses WHERE id=$1`, f.CourseID).Scan(&code); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT title, body FROM notifications WHERE user_id=$1 AND channel='in_app' AND title LIKE 'เจ้าหน้าที่จัดทำเอกสารเบิกจ่ายแล้ว%'`,
		f.TAID).Scan(&title, &body); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(title, code) {
		t.Errorf("title %q should carry the course code %s", title, code)
	}
	i1, i2 := strings.Index(body, "เดือนแรก"), strings.Index(body, "เดือนหลัง")
	if i1 < 0 || i2 < 0 || i1 > i2 {
		t.Errorf("body %q should list เดือนแรก before เดือนหลัง", body)
	}
}
