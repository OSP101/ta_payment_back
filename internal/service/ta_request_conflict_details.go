package service

import (
	"context"

	"github.com/google/uuid"
)

// ConflictScheduleSlot is a stored weekly period, not an inferred work hour.
type ConflictScheduleSlot struct {
	Source     string `json:"source"`
	CourseID   string `json:"course_id"`
	CourseCode string `json:"course_code"`
	CourseName string `json:"course_name"`
	SectionID  string `json:"section_id"`
	SectionNo  string `json:"section_no"`
	Kind       string `json:"kind"`
	Day        int    `json:"day_of_week"`
	Start      string `json:"start_time"`
	End        string `json:"end_time"`
	State      string `json:"state"`
}

type ScheduleConflictPair struct {
	Requested    ConflictScheduleSlot `json:"requested"`
	Existing     ConflictScheduleSlot `json:"existing"`
	Blocking     bool                 `json:"blocking"`
	OverlapStart string               `json:"overlap_start"`
	OverlapEnd   string               `json:"overlap_end"`
}

type TAConflictScheduleDetails struct {
	TAName      string                 `json:"ta_name"`
	HasSchedule bool                   `json:"has_schedule"`
	Slots       []ConflictScheduleSlot `json:"slots"`
	Conflicts   []ScheduleConflictPair `json:"conflicts"`
}

// PreviewConflictSchedule shares the course ownership gate of PreviewConflicts.
// Only approved teaching requests in this course's term are shown. Rejected and
// cancelled requests are history, not teaching commitments.
func (s *TARequestService) PreviewConflictSchedule(ctx context.Context, taID, courseID uuid.UUID) (*TAConflictScheduleDetails, error) {
	out := &TAConflictScheduleDetails{Slots: []ConflictScheduleSlot{}, Conflicts: []ScheduleConflictPair{}}
	var termID uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT tc.term_id, u.first_name || ' ' || u.last_name,
	 EXISTS (SELECT 1 FROM ta_class_schedules cs WHERE cs.user_id=u.id AND cs.term_id=tc.term_id)
	 FROM teaching_courses tc CROSS JOIN users u WHERE tc.id=$1 AND u.id=$2`, courseID, taID).Scan(&termID, &out.TAName, &out.HasSchedule); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
	 SELECT 'requested', tc.id::text, tc.code, tc.name_th, sec.id::text, sec.sec_no,
	        ss.kind::text, ss.day_of_week, to_char(ss.start_time,'HH24:MI'), to_char(ss.end_time,'HH24:MI'), ''
	 FROM teaching_courses tc JOIN sections sec ON sec.teaching_course_id=tc.id
	 JOIN section_schedules ss ON ss.section_id=sec.id WHERE tc.id=$1
	 UNION ALL
	 SELECT 'own_class', '', COALESCE(cs.course_label,''), '', '', '', 'own_class',
	        cs.day_of_week, to_char(cs.start_time,'HH24:MI'), to_char(cs.end_time,'HH24:MI'), ''
	 FROM ta_class_schedules cs WHERE cs.user_id=$2 AND cs.term_id=$3 AND NOT cs.is_wba
	 UNION ALL
	 SELECT DISTINCT 'teaching', tc.id::text, tc.code, tc.name_th, sec.id::text, sec.sec_no,
	        ss.kind::text, ss.day_of_week, to_char(ss.start_time,'HH24:MI'), to_char(ss.end_time,'HH24:MI'), a.state::text
	 FROM ta_request_assignments a JOIN ta_requests r ON r.id=a.request_id
	 JOIN teaching_courses tc ON tc.id=r.teaching_course_id
	 JOIN sections sec ON sec.id=a.section_id JOIN ta_assignment_schedules ss ON ss.assignment_id=a.id
	 WHERE a.ta_id=$2 AND r.status='approved' AND tc.term_id=$3 AND a.state<>'dropped'
	 ORDER BY 8,9,1,3,6`, courseID, taID, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b ConflictScheduleSlot
		if err := rows.Scan(&b.Source, &b.CourseID, &b.CourseCode, &b.CourseName, &b.SectionID, &b.SectionNo, &b.Kind, &b.Day, &b.Start, &b.End, &b.State); err != nil {
			return nil, err
		}
		out.Slots = append(out.Slots, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.Conflicts = conflictPairs(out.Slots)
	return out, nil
}

// Mirror checkOwnClassConflict and checkCrossRequestConflict: touching edges
// are free; each overlap excludes that requested meeting, not its section.
func conflictPairs(slots []ConflictScheduleSlot) []ScheduleConflictPair {
	out := []ScheduleConflictPair{}
	for _, requested := range slots {
		if requested.Source != "requested" {
			continue
		}
		for _, existing := range slots {
			if existing.Source == "requested" || requested.Day != existing.Day || requested.Start >= existing.End || existing.Start >= requested.End {
				continue
			}
			blocking := existing.Source == "teaching"
			if blocking && requested.CourseID == existing.CourseID {
				continue
			}
			start, end := requested.Start, requested.End
			if existing.Start > start {
				start = existing.Start
			}
			if existing.End < end {
				end = existing.End
			}
			out = append(out, ScheduleConflictPair{Requested: requested, Existing: existing, Blocking: blocking, OverlapStart: start, OverlapEnd: end})
		}
	}
	return out
}
