package service

import (
	"testing"

	"github.com/google/uuid"
)

// lecturer_pending_worklog tells a lecturer they have TA hours to approve. A
// grad-special (master/phd, track=special) TA no longer logs work_logs at
// all — they are also excluded from ListPending, the lecturer's own approval
// screen — so a leftover 'submitted' row from before that change must not
// target the lecturer: they would be announced work their own screen never
// shows.
func TestAudience_LecturerPendingWorklog_ExcludesGradSpecial(t *testing.T) {
	w := newTargetWorld(t)

	sec := uuid.New()
	w.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track) VALUES ($1,$2,'2','special')`,
		sec, w.courseA)
	req, asg := uuid.New(), uuid.New()
	w.exec(`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at)
	        VALUES ($1,$2,$3,'both','approved',NOW())`, req, w.courseA, w.lecturer)
	w.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
	        VALUES ($1,$2,$3,$4,'master')`, asg, req, sec, w.taReady)
	w.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES (gen_random_uuid(), $1, CURRENT_DATE, '09:00', '11:00', 2, 'review', 'submitted')`, asg)

	got := w.resolve(t, AudienceRule{Filters: []string{"lecturer_pending_worklog"}})
	if got[w.lecturer] {
		t.Error("a grad-special leftover 'submitted' row must not target the lecturer — " +
			"their own approval screen (ListPending) shows nothing for it")
	}
}

// A grad-regular (track=regular) submitted row is a real, approvable row and
// must still reach the lecturer.
func TestAudience_LecturerPendingWorklog_StillIncludesGradRegular(t *testing.T) {
	w := newTargetWorld(t)

	sec := uuid.New()
	w.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track) VALUES ($1,$2,'2','regular')`,
		sec, w.courseA)
	req, asg := uuid.New(), uuid.New()
	w.exec(`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at)
	        VALUES ($1,$2,$3,'both','approved',NOW())`, req, w.courseA, w.lecturer)
	w.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
	        VALUES ($1,$2,$3,$4,'master')`, asg, req, sec, w.taReady)
	w.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES (gen_random_uuid(), $1, CURRENT_DATE, '09:00', '11:00', 2, 'review', 'submitted')`, asg)

	got := w.resolve(t, AudienceRule{Filters: []string{"lecturer_pending_worklog"}})
	if !got[w.lecturer] {
		t.Error("a grad-regular submitted row must still target the lecturer")
	}
}
