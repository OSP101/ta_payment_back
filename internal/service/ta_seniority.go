package service

import (
	"context"

	"github.com/google/uuid"
)

// ta_seniority.go answers "ใหม่"/"เก่า" for the transfer-cover document
// (ปะหน้าจ่ายตรง), per the staff interview's own definition: new = never held
// an account or submitted a document in this system before.
//
// Storing THAT as a boolean would need re-flipping for every TA at the start
// of every term, and reprinting an old term's document after the flip would
// show today's answer instead of the answer that was true when the term ran.
// Storing the first term a person was appointed sidesteps both problems: the
// label for any document is just "was this the term stamped as first?".

// TASeniority answers ใหม่/เก่า for userID as of termID.
// ta_seniority_override wins outright — the escape hatch for anyone whose
// real TA history predates this system, where ta_first_term_id cannot be
// trusted at all. Otherwise use the first-term stamp, falling back to the
// earliest approved assignment when a stamp is missing. If neither exists,
// history is unknown and defaults to returning.
func (s *UserService) TASeniority(ctx context.Context, userID, termID uuid.UUID) (string, error) {
	var result string
	err := s.pool.QueryRow(ctx, `SELECT `+taSenioritySQL("$1::uuid", "$2::uuid"), userID, termID).Scan(&result)
	return result, err
}

// taSenioritySQL is shared by badges, exports and ERP candidates. Expressions
// are internal SQL identifiers/placeholders, never user input. Explicit history
// wins, followed by the first-term stamp and the earliest approved non-dropped
// assignment. Unknown history stays returning.
func taSenioritySQL(userExpr, termExpr string) string {
	return `(SELECT COALESCE(seniority_user.ta_seniority_override,
	  CASE WHEN COALESCE(seniority_user.ta_first_term_id, (
	    SELECT tc.term_id FROM ta_request_assignments a
	    JOIN ta_requests r ON r.id=a.request_id AND r.status='approved'
	    JOIN teaching_courses tc ON tc.id=r.teaching_course_id
	    JOIN academic_terms term ON term.id=tc.term_id
	    WHERE a.ta_id=seniority_user.id AND a.state<>'dropped'
	    ORDER BY term.academic_year, term.semester, tc.term_id LIMIT 1
	  )) = ` + termExpr + ` THEN 'new' ELSE 'returning' END)
	  FROM users seniority_user WHERE seniority_user.id=` + userExpr + `)`
}

// RecordTAFirstTermIfUnset stamps ta_first_term_id the first time a TA's
// appointment for a term is approved. Written once, on purpose: calling this
// again in a later term must NOT move the stamp, or every returning TA would
// look "new" again the moment their next term is recorded.
func (s *UserService) RecordTAFirstTermIfUnset(ctx context.Context, userID, termID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE users SET ta_first_term_id = $2 WHERE id = $1 AND ta_first_term_id IS NULL`,
		userID, termID)
	return err
}
