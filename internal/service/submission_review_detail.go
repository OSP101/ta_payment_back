package service

import (
	"context"

	"github.com/google/uuid"
)

// The rows behind ONE cell of the payout-review queue: what a staff member is
// actually approving when they press "ผ่าน" on (month, TA, course).
//
// Until now there was nothing behind that button. The screen offered a total —
// "52.0 ชม. ฿2,340" — and an "แก้ไข" link that led to the whole course workspace,
// so the only way to see the days being approved was to leave the queue, find the
// TA among everyone else on the course, and read their entire term. In practice
// that means the total was approved unread.
//
// What it returns is deliberately narrow: the day rows of that month, and the
// section's weekly timetable to read them against. Anything else the reviewer
// might want is already one click away on the course page.

// ReviewDay is one work-log row as the reviewer sees it.
type ReviewDay struct {
	ID        uuid.UUID `json:"id"`
	WorkDate  string    `json:"work_date"`
	StartTime string    `json:"start_time"`
	EndTime   string    `json:"end_time"`
	Hours     float64   `json:"hours"`
	Activity  string    `json:"activity"`
	Note      *string   `json:"note,omitempty"`
	Room      *string   `json:"room,omitempty"`
	SecNo     string    `json:"sec_no"`
	// Track is ภาคปกติ / ภาคพิเศษ. It rides along with the section number because
	// the two are read together and never mean anything apart: the track decides
	// the hourly rate, so "sec 2" alone does not tell a reviewer what the row is
	// worth.
	Track string `json:"track"`
	// Source is 'auto' or 'manual' (migration 0057). The reviewer's attention
	// belongs on the manual rows: an auto row reproduces class times the lecturer
	// entered, so there is nothing in it that a second person can verify.
	Source string `json:"source"`
	// OnTimetable is whether this row lands on one of the section's scheduled
	// periods. Computed rather than stored because a schedule can be re-imported
	// after the row was written — the answer is only true "as of now", which is
	// exactly what a reviewer looking at it now wants to know.
	OnTimetable bool `json:"on_timetable"`
}

// ReviewSlot is one period of the section's weekly timetable — the backdrop the
// day rows are read against.
type ReviewSlot struct {
	SecNo     string  `json:"sec_no"`
	Track     string  `json:"track"`
	Kind      string  `json:"kind"`
	DayOfWeek int     `json:"day_of_week"`
	StartTime string  `json:"start_time"`
	EndTime   string  `json:"end_time"`
	Room      *string `json:"room,omitempty"`
}

// ReviewMonthDetail is everything behind one queue row.
type ReviewMonthDetail struct {
	Days  []ReviewDay  `json:"days"`
	Slots []ReviewSlot `json:"slots"`
	// Totals split by origin, so the header can say "41 of 52 hours came from the
	// timetable" without the client re-deriving it from the rows.
	AutoHours   float64 `json:"auto_hours"`
	ManualHours float64 `json:"manual_hours"`
	AutoCount   int     `json:"auto_count"`
	ManualCount int     `json:"manual_count"`
	// DaysWorked is distinct work dates. "52 hours over 6 days" and "52 hours
	// over 20 days" describe very different months and the totals hide it.
	DaysWorked int `json:"days_worked"`
}

// MonthDetailForReview loads the rows behind one (period, course, TA) cell.
//
// Scoped by the period's year_month rather than by a date range so it matches
// exactly what StaffReview will act on — the queue, the detail and the approval
// must all describe the same set of rows, or the reviewer approves something
// other than what they read.
func (s *SubmissionPeriodService) MonthDetailForReview(
	ctx context.Context, periodID, tcID, taID uuid.UUID,
) (*ReviewMonthDetail, error) {
	out := &ReviewMonthDetail{Days: []ReviewDay{}, Slots: []ReviewSlot{}}

	rows, err := s.pool.Query(ctx, `
		SELECT wl.id, TO_CHAR(wl.work_date,'YYYY-MM-DD'),
		       wl.start_time::text, wl.end_time::text, wl.hours, wl.activity,
		       wl.note, wl.room, sec.sec_no, sec.track::text, wl.source,
		       EXISTS (
		         SELECT 1 FROM section_schedules sch
		          WHERE sch.section_id = sec.id
		            AND sch.kind = wl.activity
		            AND sch.start_time = wl.start_time
		            AND sch.end_time = wl.end_time
		            AND sch.day_of_week = EXTRACT(DOW FROM wl.work_date)::int
		       )
		  FROM work_logs wl
		  JOIN ta_request_assignments a ON a.id = wl.assignment_id
		  JOIN sections sec ON sec.id = a.section_id
		  JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		  JOIN academic_terms trm ON trm.id = tc.term_id
		  JOIN submission_periods sp ON sp.id = $1
		 WHERE tc.id = $2
		   AND a.ta_id = $3
		   AND a.state <> 'dropped'
		   -- Same month key the queue and StaffReview use: the term's academic
		   -- year joined to the work date's month.
		   AND trm.academic_year::text || '-' || to_char(wl.work_date,'MM') = sp.year_month
		 ORDER BY wl.work_date, wl.start_time`, periodID, tcID, taID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seenDates := map[string]bool{}
	for rows.Next() {
		var d ReviewDay
		if err := rows.Scan(&d.ID, &d.WorkDate, &d.StartTime, &d.EndTime, &d.Hours,
			&d.Activity, &d.Note, &d.Room, &d.SecNo, &d.Track, &d.Source, &d.OnTimetable); err != nil {
			return nil, err
		}
		if d.Source == "auto" {
			out.AutoHours += d.Hours
			out.AutoCount++
		} else {
			out.ManualHours += d.Hours
			out.ManualCount++
		}
		seenDates[d.WorkDate] = true
		out.Days = append(out.Days, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.DaysWorked = len(seenDates)

	slotRows, err := s.pool.Query(ctx, `
		SELECT sec.sec_no, sec.track::text, sch.kind, sch.day_of_week,
		       sch.start_time::text, sch.end_time::text, sch.room
		  FROM section_schedules sch
		  JOIN sections sec ON sec.id = sch.section_id
		  JOIN ta_request_assignments a ON a.section_id = sec.id
		 WHERE sec.teaching_course_id = $1
		   AND a.ta_id = $2
		   AND a.state <> 'dropped'
		 GROUP BY sec.sec_no, sec.track, sch.kind, sch.day_of_week, sch.start_time, sch.end_time, sch.room
		 ORDER BY sch.day_of_week, sch.start_time`, tcID, taID)
	if err != nil {
		return nil, err
	}
	defer slotRows.Close()
	for slotRows.Next() {
		var sl ReviewSlot
		if err := slotRows.Scan(&sl.SecNo, &sl.Track, &sl.Kind, &sl.DayOfWeek,
			&sl.StartTime, &sl.EndTime, &sl.Room); err != nil {
			return nil, err
		}
		out.Slots = append(out.Slots, sl)
	}
	return out, slotRows.Err()
}
