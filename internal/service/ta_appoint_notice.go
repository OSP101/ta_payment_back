package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ta_appoint_notice.go is the "you are now a TA of course X" notice. Beyond
// the appointment itself it tells the TA when each month's work log is due,
// because that deadline is what a new TA most needs and was never told: a
// closed month is final, and unsent hours are forfeited.

// appointPeriod is one monthly submission period as the notice lists it.
type appointPeriod struct {
	label, startsOn, dueDate string
}

// notifyTAsAppointed tells every TA who kept at least one section in an
// approved request. A TA whose every section was dropped for a timetable
// clash is left out: the clash notice already told them they are not
// assisting, and an appointment mail would contradict it.
func (s *TARequestService) notifyTAsAppointed(ctx context.Context, reqID, courseID uuid.UUID, code, nameTH string) {
	type appointee struct {
		id          uuid.UUID
		sections    []string
		logsMonthly bool
	}
	rows, err := s.pool.Query(ctx, `
		SELECT a.ta_id,
		       ARRAY_AGG(DISTINCT sec.sec_no ORDER BY sec.sec_no),
		       -- Graduate TAs on a special-track section are paid a lump sum and
		       -- keep no monthly work log (see SweepReminders), so no deadlines.
		       BOOL_OR(a.level::text NOT IN ('master', 'phd') OR sec.track <> 'special')
		  FROM ta_request_assignments a
		  JOIN sections sec ON sec.id = a.section_id
		 WHERE a.request_id = $1 AND a.state <> 'dropped'
		 GROUP BY a.ta_id`, reqID)
	if err != nil {
		return
	}
	var people []appointee
	for rows.Next() {
		var p appointee
		if err := rows.Scan(&p.id, &p.sections, &p.logsMonthly); err != nil {
			rows.Close()
			return
		}
		people = append(people, p)
	}
	rows.Close()
	if rows.Err() != nil || len(people) == 0 {
		return
	}

	periods := s.upcomingPeriods(ctx, courseID)
	label := strings.TrimSpace(code + " " + nameTH)
	for _, p := range people {
		title, body, layout := appointNoticeContent(label, p.sections, p.logsMonthly, periods)
		s.notify.SendLaidOut(ctx, p.id, title+" "+code, body, "/ta", false, layout)
	}
}

// upcomingPeriods are the course term's monthly periods a newly appointed TA
// can still submit to: open, and not yet past their due date.
func (s *TARequestService) upcomingPeriods(ctx context.Context, courseID uuid.UUID) []appointPeriod {
	rows, err := s.pool.Query(ctx, `
		SELECT sp.label, TO_CHAR(sp.starts_on, 'YYYY-MM-DD'), TO_CHAR(sp.due_date, 'YYYY-MM-DD')
		  FROM submission_periods sp
		  JOIN teaching_courses tc ON tc.term_id = sp.term_id
		 WHERE tc.id = $1 AND NOT sp.is_closed AND sp.due_date >= CURRENT_DATE
		 ORDER BY sp.year_month`, courseID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []appointPeriod
	for rows.Next() {
		var p appointPeriod
		if err := rows.Scan(&p.label, &p.startsOn, &p.dueDate); err != nil {
			return nil
		}
		out = append(out, p)
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

// appointNoticeContent builds the notice: the plain body the bell shows, and
// the e-mail layout with the course facts and the deadline table.
func appointNoticeContent(course string, sections []string, logsMonthly bool, periods []appointPeriod) (string, string, MailLayout) {
	title := "ท่านได้รับการแต่งตั้งเป็นผู้ช่วยสอน"
	secText := strings.Join(sections, ", ")
	intro := fmt.Sprintf("ท่านได้รับการอนุมัติให้เป็นผู้ช่วยสอนรายวิชา %s กลุ่มเรียนที่ %s", course, secText)
	if logsMonthly {
		intro += " และสามารถบันทึกเวลาปฏิบัติงานในระบบได้แล้ว"
	}

	layout := MailLayout{
		Intro: intro,
		Facts: []MailFact{
			{"รายวิชา", course},
			{"กลุ่มเรียน", secText},
		},
		ButtonLabel: "บันทึกเวลาปฏิบัติงาน",
	}

	var b strings.Builder
	b.WriteString(intro)
	switch {
	case !logsMonthly:
		// Nothing to schedule; keep the notice to the appointment.
		layout.ButtonLabel = ""
	case len(periods) == 0:
		note := "ขณะนี้เจ้าหน้าที่ยังไม่ได้กำหนดรอบส่งบันทึกเวลาปฏิบัติงานรายเดือนของภาคการศึกษานี้ " +
			"ท่านสามารถตรวจสอบกำหนดส่งได้ที่หน้าแจ้งเตือนกำหนดส่งในระบบเมื่อเจ้าหน้าที่กำหนดแล้ว"
		b.WriteString("\n\n" + note)
		layout.After = note
	default:
		table := &MailTable{
			Title: "กำหนดส่งบันทึกเวลาปฏิบัติงานรายเดือน",
			Head:  []string{"ประจำเดือน", "เริ่มส่งได้ตั้งแต่", "หมดเขตส่ง"},
		}
		lines := make([]string, 0, len(periods))
		for _, p := range periods {
			table.Rows = append(table.Rows, []string{p.label, thaiLongDateISO(p.startsOn), thaiLongDateISO(p.dueDate)})
			lines = append(lines, fmt.Sprintf("ประจำเดือน%s หมดเขตส่งวันที่ %s", p.label, thaiLongDateISO(p.dueDate)))
		}
		layout.Table = table
		b.WriteString("\n\nกำหนดส่งบันทึกเวลาปฏิบัติงานรายเดือน ดังนี้\n" + numberedLines(lines))
		after := "ขอให้ท่านบันทึกเวลาปฏิบัติงานให้ครบถ้วน และส่งให้อาจารย์ผู้สอนพิจารณาอนุมัติภายในกำหนดของแต่ละเดือน " +
			"เมื่อพ้นกำหนดและเจ้าหน้าที่ปิดรอบแล้ว จะไม่สามารถส่งบันทึกเวลาของเดือนนั้นได้อีก " +
			"ทั้งนี้ กำหนดส่งอาจมีการเปลี่ยนแปลง ท่านสามารถตรวจสอบกำหนดล่าสุดได้ในระบบ"
		b.WriteString("\n\n" + after)
		layout.After = after
	}
	return title, b.String(), layout
}
