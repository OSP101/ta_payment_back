package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Count the union of own-class and cross-course clashes per session, not per
// source: a session hit by both sources must count only once.
func sectionAvailabilityByKind(ctx context.Context, q rowQuerier, taID, sectionID uuid.UUID, cross map[uuid.UUID][]string) (kindClash, map[string]float64, error) {
	rows, err := q.Query(ctx, `SELECT ss.id,ss.kind,EXTRACT(EPOCH FROM(ss.end_time-ss.start_time))/3600,EXISTS(
 SELECT 1 FROM ta_class_schedules cs JOIN teaching_courses tc ON tc.term_id=cs.term_id JOIN sections sec ON sec.teaching_course_id=tc.id
 WHERE sec.id=$2 AND cs.user_id=$1 AND NOT cs.is_wba AND cs.day_of_week=ss.day_of_week AND cs.start_time<ss.end_time AND ss.start_time<cs.end_time)
 FROM section_schedules ss WHERE ss.section_id=$2`, taID, sectionID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := kindClash{}
	hours := map[string]float64{"lecture": 0, "lab": 0}
	for rows.Next() {
		var id uuid.UUID
		var kind string
		var own bool
		var h float64
		if err := rows.Scan(&id, &kind, &h, &own); err != nil {
			return nil, nil, err
		}
		c := out[kind]
		c.Total++
		if own || len(cross[id]) > 0 {
			c.Clashing++
		} else {
			hours[kind] += h
		}
		out[kind] = c
	}
	return out, hours, rows.Err()
}

// crossSessionClashes always compares against the other assignment's effective
// meetings. A meeting already excluded from that assignment reserves no time.
func crossSessionClashes(ctx context.Context, q rowQuerier, taID, sectionID, excludeRequestID uuid.UUID) (map[uuid.UUID][]string, error) {
	rows, err := q.Query(ctx, `
 SELECT DISTINCT ss.id, ss.kind, ss.day_of_week,
 to_char(ss.start_time,'HH24:MI'),to_char(ss.end_time,'HH24:MI'),
 otc.code, os.sec_no, oss.kind,
 to_char(oss.start_time,'HH24:MI'),to_char(oss.end_time,'HH24:MI'),
 to_char(GREATEST(ss.start_time,oss.start_time),'HH24:MI'),
 to_char(LEAST(ss.end_time,oss.end_time),'HH24:MI')
 FROM sections sec JOIN teaching_courses tc ON tc.id=sec.teaching_course_id
 JOIN section_schedules ss ON ss.section_id=sec.id
 JOIN ta_request_assignments oa ON oa.ta_id=$1 AND oa.state<>'dropped'
 JOIN ta_requests r ON r.id=oa.request_id AND r.status='approved' AND r.id<>$3
 JOIN teaching_courses otc ON otc.id=r.teaching_course_id AND otc.id<>tc.id AND otc.term_id=tc.term_id
 JOIN sections os ON os.id=oa.section_id
 JOIN ta_assignment_schedules oss ON oss.assignment_id=oa.id
 AND oss.day_of_week=ss.day_of_week AND oss.start_time<ss.end_time AND ss.start_time<oss.end_time
 WHERE sec.id=$2 ORDER BY ss.id,otc.code,os.sec_no`, taID, sectionID, excludeRequestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]string{}
	for rows.Next() {
		var id uuid.UUID
		var kind, start, end, code, sec, otherKind, otherStart, otherEnd, overlapStart, overlapEnd string
		var day int
		if err := rows.Scan(&id, &kind, &day, &start, &end, &code, &sec, &otherKind, &otherStart, &otherEnd, &overlapStart, &overlapEnd); err != nil {
			return nil, err
		}
		out[id] = append(out[id], fmt.Sprintf("คาบ%s %s %s–%s ชนกับ %s Sec %s คาบ%s %s–%s ช่วงที่ทับ %s–%s จึงตัดคาบ %s–%s ทั้งคาบออก คาบอื่นและงานนอกคาบยังขอได้", kindTH(kind), dayTH(day), start, end, code, sec, kindTH(otherKind), otherStart, otherEnd, overlapStart, overlapEnd, start, end))
	}
	return out, rows.Err()
}

// Staff workload corrections must not reintroduce hours on excluded meetings.
func checkExcludedSessionWorkload(ctx context.Context, q rowQuerier, assignmentID, taID, sectionID uuid.UUID, w WorkloadInput) error {
	rows, err := q.Query(ctx, `SELECT schedule_id FROM ta_assignment_session_exclusions WHERE assignment_id=$1`, assignmentID)
	if err != nil {
		return err
	}
	excluded := map[uuid.UUID][]string{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		excluded[id] = []string{"excluded"}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(excluded) == 0 {
		return nil
	}
	_, hours, err := sectionAvailabilityByKind(ctx, q, taID, sectionID, excluded)
	if err != nil {
		return err
	}
	if w.AttendanceHrs > hours["lecture"]+0.001 || w.LabHrs > hours["lab"]+0.001 || w.HelpTeachHrs > hours["lecture"]+hours["lab"]+0.001 {
		return fmt.Errorf("ระบุงานในคาบเกินคาบที่ไม่ชน: บรรยายเหลือ %.1f ชม./สัปดาห์ ปฏิบัติการเหลือ %.1f ชม./สัปดาห์ คาบที่ถูกตัดออกยังลงเวลาไม่ได้ แต่งานตรวจและงานอื่นนอกคาบยังระบุได้", hours["lecture"], hours["lab"])
	}
	return nil
}

// Run under the same per-TA transaction lock as Create/AddSections/finalize.
// Only the new request loses meetings; an earlier approval is never edited.
func (s *TARequestService) applyCrossSessionOutcome(ctx context.Context, tx pgx.Tx, reqID, onlyTA uuid.UUID) (map[uuid.UUID][]string, error) {
	rows, err := tx.Query(ctx, `SELECT id,ta_id,section_id FROM ta_request_assignments WHERE request_id=$1 AND state<>'dropped' AND ($2::uuid='00000000-0000-0000-0000-000000000000' OR ta_id=$2) ORDER BY ta_id,section_id`, reqID, onlyTA)
	if err != nil {
		return nil, err
	}
	type assignment struct{ id, ta, sec uuid.UUID }
	var all []assignment
	for rows.Next() {
		var a assignment
		if err := rows.Scan(&a.id, &a.ta, &a.sec); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	notices := map[uuid.UUID][]string{}
	for _, a := range all {
		clashes, err := crossSessionClashes(ctx, tx, a.ta, a.sec, reqID)
		if err != nil {
			return nil, err
		}
		for id, lines := range clashes {
			reason := strings.Join(lines, "\n")
			tag, err := tx.Exec(ctx, `INSERT INTO ta_assignment_session_exclusions(assignment_id,schedule_id,reason) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, a.id, id, reason)
			if err != nil {
				return nil, err
			}
			if tag.RowsAffected() > 0 {
				notices[a.ta] = append(notices[a.ta], reason)
			}
		}
		var excluded bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ta_assignment_session_exclusions WHERE assignment_id=$1)`, a.id).Scan(&excluded); err != nil {
			return nil, err
		}
		if !excluded {
			continue
		}
		excludedRows, err := tx.Query(ctx, `SELECT schedule_id FROM ta_assignment_session_exclusions WHERE assignment_id=$1`, a.id)
		if err != nil {
			return nil, err
		}
		for excludedRows.Next() {
			var id uuid.UUID
			if err := excludedRows.Scan(&id); err != nil {
				excludedRows.Close()
				return nil, err
			}
			if len(clashes[id]) == 0 {
				clashes[id] = []string{"excluded"}
			}
		}
		excludedRows.Close()
		if err := excludedRows.Err(); err != nil {
			return nil, err
		}
		_, available, err := sectionAvailabilityByKind(ctx, tx, a.ta, a.sec, clashes)
		if err != nil {
			return nil, err
		}
		// Clamp only duties performed inside a meeting. Grading and other
		// off-slot work keep the declared hours even when all meetings clash.
		if _, err := tx.Exec(ctx, `UPDATE ta_workload_forms SET
          attendance_hrs=LEAST(attendance_hrs,$2), lab_hrs=LEAST(lab_hrs,$3),
          help_teach_hrs=LEAST(help_teach_hrs,$2+$3)
          WHERE assignment_id=$1`, a.id, available["lecture"], available["lab"]); err != nil {
			return nil, err
		}

		var remaining float64
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT attendance_hrs+lab_hrs+check_work_hrs+ug_other_hrs+lab_other_hrs+grade_hrs+other_hrs+prep_hrs+help_teach_hrs FROM ta_workload_forms WHERE assignment_id=$1),0)`, a.id).Scan(&remaining); err != nil {
			return nil, err
		}
		state := "trimmed"
		if remaining <= 0.001 {
			state = "dropped"
		}
		// Preserve own-class reasons separately: otherwise every timetable
		// autosave sees a changed reason and sends the same notice again.
		_, ownClashing, err := sectionClash(ctx, tx, a.ta, a.sec)
		if err != nil {
			return nil, err
		}
		// Never hide existing pay during a later reevaluation.
		if _, err := tx.Exec(ctx, `UPDATE ta_request_assignments SET state=CASE WHEN $2='dropped' AND EXISTS(SELECT 1 FROM work_logs WHERE assignment_id=$1) THEN 'trimmed'::ta_assignment_state ELSE $2::ta_assignment_state END,
 state_reason=CASE WHEN $3 THEN state_reason ELSE (SELECT string_agg(reason,E'\n' ORDER BY schedule_id) FROM ta_assignment_session_exclusions WHERE assignment_id=$1) END,state_decided_at=NOW() WHERE id=$1`, a.id, state, ownClashing > 0); err != nil {
			return nil, err
		}
	}
	return notices, nil
}

// A request with no surviving declared work must say whether the cause was
// another TA course, rather than incorrectly claiming every class was busy.
func clashRejectionReason(ctx context.Context, q querier, reqID uuid.UUID) (string, error) {
	var cross bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ta_assignment_session_exclusions e JOIN ta_request_assignments a ON a.id=e.assignment_id WHERE a.request_id=$1)`, reqID).Scan(&cross)
	if err != nil {
		return "", err
	}
	if cross {
		return "ไม่มีภาระงานที่ทำได้เหลือหลังตัดคาบที่ชนกับวิชาอื่น กรุณาเลือกคาบที่ไม่ชน หรือระบุงานตรวจ/งานอื่นนอกคาบ", nil
	}
	return clashRejectReason, nil
}
