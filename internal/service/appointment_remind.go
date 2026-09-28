package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// appointment_remind.go is the "เตือน" button on the ใบแต่งตั้ง TA page. A TA
// request is decided only once every TA on it has entered a class timetable
// for the term; until then the course is held out of the appointment order.
// Staff used to chase those TAs by hand. This sends each one a notice naming
// the courses waiting on them and linking to the timetable page.

// timetableRemindGap is the least time between two reminders to one TA for
// one term, so repeated clicks cannot flood an inbox.
const timetableRemindGap = 24 * time.Hour

// RemindTimetableResult reports who was reminded and who was not, and why.
type RemindTimetableResult struct {
	Sent    []string        `json:"sent"`
	Skipped []RemindSkipped `json:"skipped"`
}

type RemindSkipped struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// waitingCourse is one course held up by a TA's missing timetable.
type waitingCourse struct{ code, name, lecturer string }

// RemindTimetable reminds TAs who are holding up a pending TA request in the
// term by not having a class timetable. taIDs empty means every such TA.
// Anyone named who is not actually holding anything up is skipped, not
// reminded: a notice telling someone to do what they already did is noise.
func (s *AppointmentOrderService) RemindTimetable(ctx context.Context, actor, termID uuid.UUID, taIDs []uuid.UUID) (*RemindTimetableResult, error) {
	if s.notify == nil {
		return nil, fmt.Errorf("notify service not configured")
	}
	var term string
	if err := s.pool.QueryRow(ctx,
		`SELECT 'ภาคการศึกษาที่ ' || semester::text || ' ปีการศึกษา ' || academic_year::text
		   FROM academic_terms WHERE id = $1`, termID).Scan(&term); err != nil {
		return nil, ErrNotFound
	}

	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT a.ta_id, `+personNameSQL+`,
		       tc.code, tc.name_th,
		       COALESCE((SELECT `+personNameSQL+` FROM users u
		                 LEFT JOIN ta_profiles tp ON tp.user_id = u.id
		                 WHERE u.id = r.lecturer_id), ''),
		       (SELECT MAX(tr.sent_at) FROM timetable_reminders tr WHERE tr.ta_id = a.ta_id AND tr.term_id = $1)
		FROM ta_requests r
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id AND tc.term_id = $1
		JOIN ta_request_assignments a ON a.request_id = r.id AND a.state <> 'dropped'
		JOIN users u ON u.id = a.ta_id
		LEFT JOIN ta_profiles tp ON tp.user_id = u.id
		WHERE r.status = 'submitted'
		  AND NOT EXISTS (SELECT 1 FROM ta_class_schedules cs WHERE cs.user_id = a.ta_id AND cs.term_id = $1)
		  AND ($2::uuid[] IS NULL OR a.ta_id = ANY($2))
		ORDER BY 2, 3`, termID, nilIfEmpty(taIDs))
	if err != nil {
		return nil, err
	}
	type waiting struct {
		name     string
		last     *time.Time
		courses  []waitingCourse
		reminded bool
	}
	byTA := map[uuid.UUID]*waiting{}
	var order []uuid.UUID
	for rows.Next() {
		var (
			id                       uuid.UUID
			name, code, cname, lectr string
			last                     *time.Time
		)
		if err := rows.Scan(&id, &name, &code, &cname, &lectr, &last); err != nil {
			rows.Close()
			return nil, err
		}
		w, ok := byTA[id]
		if !ok {
			w = &waiting{name: name, last: last}
			byTA[id] = w
			order = append(order, id)
		}
		w.courses = append(w.courses, waitingCourse{code, cname, lectr})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	res := &RemindTimetableResult{Sent: []string{}, Skipped: []RemindSkipped{}}
	// Named TAs who hold nothing up (timetable already in, or not on a
	// pending request) are reported so the button's result is honest.
	for _, id := range taIDs {
		if _, ok := byTA[id]; !ok {
			name := personName(ctx, s.pool, id)
			if name == "" {
				name = id.String()
			}
			res.Skipped = append(res.Skipped, RemindSkipped{name, "บันทึกตารางเรียนแล้ว หรือไม่มีคำขอที่รอพิจารณา"})
		}
	}

	now := time.Now()
	for _, id := range order {
		w := byTA[id]
		if w.last != nil && now.Sub(*w.last) < timetableRemindGap {
			res.Skipped = append(res.Skipped, RemindSkipped{w.name,
				"เตือนไปแล้วเมื่อ " + thaiLongDateTime(*w.last) + " เตือนซ้ำได้หลังผ่านไป 24 ชั่วโมง"})
			continue
		}
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO timetable_reminders (ta_id, term_id, sent_by) VALUES ($1, $2, $3)`,
			id, termID, actor); err != nil {
			return res, err
		}
		title, body, layout := timetableReminderContent(term, w.courses)
		s.notify.SendLaidOut(ctx, id, title, body, "/ta/schedule", true, layout)
		res.Sent = append(res.Sent, w.name)
	}

	if err := s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "appointment.remind_timetable",
		Entity: "academic_term", EntityID: termID.String(),
		After: map[string]any{"sent": res.Sent, "skipped": len(res.Skipped)}}); err != nil {
		return res, err
	}
	return res, nil
}

func nilIfEmpty(ids []uuid.UUID) []uuid.UUID {
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// timetableReminderContent is the notice: the plain body for the bell, and the
// e-mail layout with the waiting courses as a table.
func timetableReminderContent(term string, courses []waitingCourse) (string, string, MailLayout) {
	title := "กรุณาบันทึกตารางเรียน " + term
	intro := fmt.Sprintf("ขณะนี้มีคำขอแต่งตั้งผู้ช่วยสอน จำนวน %d รายวิชา ที่ระบุชื่อท่านเป็นผู้ช่วยสอน "+
		"แต่ยังพิจารณาไม่ได้ เนื่องจากท่านยังไม่ได้บันทึกตารางเรียนของ%s ในระบบ", len(courses), term)
	after := "ขอให้ท่านบันทึกตารางเรียนของภาคการศึกษานี้ให้ครบถ้วน เมื่อบันทึกแล้ว ระบบจะพิจารณาคำขอโดยอัตโนมัติ " +
		"ทั้งนี้ จนกว่าคำขอจะได้รับการอนุมัติ ท่านจะยังไม่มีชื่อในคำสั่งแต่งตั้งผู้ช่วยสอน และยังบันทึกเวลาปฏิบัติงานไม่ได้"

	table := &MailTable{
		Title: "รายวิชาที่รอตารางเรียนของท่าน",
		Head:  []string{"รหัสวิชา", "ชื่อรายวิชา", "อาจารย์ผู้ขอ"},
	}
	lines := make([]string, 0, len(courses))
	for _, c := range courses {
		table.Rows = append(table.Rows, []string{c.code, c.name, c.lecturer})
		line := strings.TrimSpace(c.code + " " + c.name)
		if c.lecturer != "" {
			line += " (" + c.lecturer + ")"
		}
		lines = append(lines, line)
	}
	body := intro + " ดังนี้\n" + numberedLines(lines) + "\n\n" + after
	return title, body, MailLayout{
		Intro:       intro + " ดังนี้",
		Table:       table,
		After:       after,
		ButtonLabel: "บันทึกตารางเรียน",
	}
}
