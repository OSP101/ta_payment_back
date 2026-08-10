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
// trusted at all. Failing that, NULL ta_first_term_id (nobody has stamped it
// yet) defaults to "returning": it is far more often someone who predates the
// system than someone appointed for the very first time in this exact call,
// and RecordTAFirstTermIfUnset is what actually establishes the "new" answer
// going forward once it runs.
func (s *UserService) TASeniority(ctx context.Context, userID, termID uuid.UUID) (string, error) {
	var override *string
	var firstTermID *uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT ta_seniority_override, ta_first_term_id FROM users WHERE id = $1`,
		userID).Scan(&override, &firstTermID)
	if err != nil {
		return "", err
	}
	if override != nil {
		return *override, nil
	}
	if firstTermID != nil && *firstTermID == termID {
		return "new", nil
	}
	return "returning", nil
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
