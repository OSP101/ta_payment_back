package service

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// MyPendingRequest is one course that has asked for this TA and is still
// waiting on its verdict. TA feedback (05/10/2026): a course only appeared on
// the TA's home page once approved, so a TA named on a request had no idea it
// existed — nor that the thing holding it up was their own missing timetable.
//
// A request rests in 'submitted' for one reason only: some TA on it has not
// filed a timetable for the term (decideNew / ReevaluateForTA). WaitingOnMe and
// WaitingOnOthers say which side of that it is; when both are false the
// timetable is in and the next sweep will decide it.
type MyPendingRequest struct {
	ID               uuid.UUID  `json:"id"`
	TeachingCourseID uuid.UUID  `json:"teaching_course_id"`
	Code             string     `json:"code"`
	AltCodes         []string   `json:"alt_codes"`
	NameTH           string     `json:"name_th"`
	TermID           uuid.UUID  `json:"term_id"`
	AcademicYear     int        `json:"academic_year"`
	Semester         int        `json:"semester"`
	LecturerName     string     `json:"lecturer_name"`
	SubmittedAt      *time.Time `json:"submitted_at,omitempty"`
	// Sections THIS TA is named on, in sec_no order. Other TAs' sections on a
	// legacy multi-TA request are none of this TA's business.
	Sections []string `json:"sections"`
	// WaitingOnMe: this TA has no timetable for the request's term.
	WaitingOnMe bool `json:"waiting_on_me"`
	// WaitingOnOthers: other TAs on a pre-0149 shared request still owe one.
	// Counted, not named — their progress is not this TA's to read.
	WaitingOnOthers int `json:"waiting_on_others"`
}

// ListPendingForTA returns the submitted (not yet decided) requests that name
// this TA, oldest first. Lecturer drafts are not here: until the lecturer sends
// it, nothing has been asked of anyone.
func (s *TARequestService) ListPendingForTA(ctx context.Context, taID uuid.UUID) ([]MyPendingRequest, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, tc.id, tc.code, tc.alt_codes, tc.name_th, tc.term_id,
		       at.academic_year, at.semester,
		       u.first_name || ' ' || u.last_name,
		       r.submitted_at,
		       COALESCE(ARRAY(
		           SELECT s.sec_no FROM ta_request_assignments a
		           JOIN sections s ON s.id = a.section_id
		           WHERE a.request_id = r.id AND a.ta_id = $1 AND a.state <> 'dropped'
		           ORDER BY s.sec_no), '{}'),
		       NOT EXISTS (SELECT 1 FROM ta_class_schedules cs
		                   WHERE cs.user_id = $1 AND cs.term_id = tc.term_id),
		       (SELECT COUNT(DISTINCT a.ta_id) FROM ta_request_assignments a
		        WHERE a.request_id = r.id AND a.ta_id <> $1 AND a.state <> 'dropped'
		          AND NOT EXISTS (SELECT 1 FROM ta_class_schedules cs
		                          WHERE cs.user_id = a.ta_id AND cs.term_id = tc.term_id))
		FROM ta_requests r
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		JOIN academic_terms at ON at.id = tc.term_id
		JOIN users u ON u.id = r.lecturer_id
		WHERE r.status = 'submitted'
		  AND EXISTS (SELECT 1 FROM ta_request_assignments a
		              WHERE a.request_id = r.id AND a.ta_id = $1 AND a.state <> 'dropped')
		ORDER BY r.submitted_at NULLS LAST, tc.code`, taID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MyPendingRequest{}
	for rows.Next() {
		var p MyPendingRequest
		if err := rows.Scan(&p.ID, &p.TeachingCourseID, &p.Code, &p.AltCodes, &p.NameTH, &p.TermID,
			&p.AcademicYear, &p.Semester, &p.LecturerName, &p.SubmittedAt,
			&p.Sections, &p.WaitingOnMe, &p.WaitingOnOthers); err != nil {
			return nil, err
		}
		if p.AltCodes == nil {
			p.AltCodes = []string{}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
