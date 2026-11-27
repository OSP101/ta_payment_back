package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Check the replacement timetable inside its transaction. Pending requests
// reserve a TA too; neither timetable edits nor filling a WBA blank may turn
// those reservations into overlapping teaching commitments.
func assertSectionRequestSchedules(ctx context.Context, tx pgx.Tx, sectionID uuid.UUID) error {
	var name, code, start, end string
	err := tx.QueryRow(ctx, `
	 SELECT u.first_name || ' ' || u.last_name, otc.code,
	        to_char(oss.start_time,'HH24:MI'), to_char(oss.end_time,'HH24:MI')
	 FROM sections sec
	 JOIN teaching_courses tc ON tc.id=sec.teaching_course_id
	 JOIN section_schedules ss ON ss.section_id=sec.id
	 JOIN ta_request_assignments a ON a.section_id=sec.id AND a.state<>'dropped'
	 JOIN ta_requests r ON r.id=a.request_id AND r.status IN ('submitted','approved')
	 JOIN users u ON u.id=a.ta_id
	 JOIN ta_request_assignments oa ON oa.ta_id=a.ta_id AND oa.state<>'dropped'
	 JOIN ta_requests orq ON orq.id=oa.request_id AND orq.status IN ('submitted','approved')
	 JOIN teaching_courses otc ON otc.id=orq.teaching_course_id
	   AND otc.id<>tc.id AND otc.term_id=tc.term_id
	 JOIN ta_assignment_schedules oss ON oss.assignment_id=oa.id
	   AND oss.day_of_week=ss.day_of_week
	   AND oss.start_time<ss.end_time AND ss.start_time<oss.end_time
	 WHERE sec.id=$1
	 ORDER BY otc.code, oss.day_of_week, oss.start_time LIMIT 1`, sectionID).Scan(&name, &code, &start, &end)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return Conflict(fmt.Sprintf("บันทึกตารางสอนไม่ได้: เวลาทับซ้อนกับรายวิชา %s (%s–%s) ที่มีคำขอผู้ช่วยสอน %s อยู่แล้ว กรุณาแก้ไขคำขอหรือเลือกผู้ช่วยสอนก่อนบันทึกตาราง", code, start, end, name))
}
