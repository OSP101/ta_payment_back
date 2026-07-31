package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// appointment_round.go turns the คำสั่งแต่งตั้ง into a sequence of rounds.
//
// TA requests never close, so at the moment staff produce an order some
// courses are still waiting on a TA's timetable. Those are SKIPPED and marked
// late; staff run the order again later and that round contains only the
// stragglers. The ledger in appointment_order_items is what makes "only the
// stragglers" true — Build() used to be stateless and would happily reprint
// every name in the term.

// AppointmentCandidate is one (TA × course) pair considered for a round.
type AppointmentCandidate struct {
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	CourseCode       string    `json:"course_code"`
	CourseNameTH     string    `json:"course_name_th"`
	TAID             uuid.UUID `json:"ta_id"`
	TAName           string    `json:"ta_name"`
}

// SkippedCourse is a course held back from this round, with the reason staff
// need in order to chase it.
type SkippedCourse struct {
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	CourseCode       string    `json:"course_code"`
	CourseNameTH     string    `json:"course_name_th"`
	Reason           string    `json:"reason"`
	// PendingTAs names who is holding it up, so the reason is actionable
	// rather than a bare "not ready".
	PendingTAs []string `json:"pending_tas,omitempty"`
}

// AppointmentPreview is what staff see BEFORE committing a round: exactly who
// is on it, and what is being left behind and why. Printing an order is a
// paper act that cannot be recalled, so nothing here is issued until it is
// confirmed.
type AppointmentPreview struct {
	TermID     uuid.UUID              `json:"term_id"`
	NextRound  int                    `json:"next_round"`
	IsLate     bool                   `json:"is_late"`
	Include    []AppointmentCandidate `json:"include"`
	Skipped    []SkippedCourse        `json:"skipped"`
	// AlreadyIssued counts pairs printed on an earlier round. Shown so the
	// number of names on this round reads as deliberate rather than truncated.
	AlreadyIssued int `json:"already_issued"`
}

// nextRoundNo returns the round number a new order for this term would take.
// Round 1 is the on-time batch; anything after it is a late batch.
func (s *AppointmentOrderService) nextRoundNo(ctx context.Context, termID uuid.UUID) (int, error) {
	var maxRound *int
	if err := s.pool.QueryRow(ctx,
		`SELECT MAX(round_no) FROM appointment_orders WHERE term_id = $1`, termID).Scan(&maxRound); err != nil {
		return 0, err
	}
	if maxRound == nil {
		return 1, nil
	}
	return *maxRound + 1, nil
}

// Preview computes the membership of the next round without issuing anything.
func (s *AppointmentOrderService) Preview(ctx context.Context, termID uuid.UUID) (*AppointmentPreview, error) {
	round, err := s.nextRoundNo(ctx, termID)
	if err != nil {
		return nil, err
	}
	out := &AppointmentPreview{
		TermID:    termID,
		NextRound: round,
		IsLate:    round > 1,
		Include:   []AppointmentCandidate{},
		Skipped:   []SkippedCourse{},
	}

	// Eligible = approved request, assignment not dropped, and never printed
	// on an earlier round of this term.
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT tc.id, tc.code, tc.name_th, u.id, u.first_name || ' ' || u.last_name
		FROM ta_request_assignments a
		JOIN ta_requests r       ON r.id = a.request_id AND r.status = 'approved'
		JOIN sections sec        ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		JOIN users u             ON u.id = a.ta_id
		WHERE tc.term_id = $1
		  AND a.state <> 'dropped'
		  AND NOT EXISTS (
		      SELECT 1
		      FROM appointment_order_items it
		      JOIN appointment_orders o ON o.id = it.appointment_order_id
		      WHERE o.term_id = $1
		        AND it.teaching_course_id = tc.id
		        AND it.ta_id = a.ta_id)
		-- SELECT DISTINCT requires every ORDER BY expression to be selected;
		-- the columns below are already in the list, so order by them by name.
		ORDER BY tc.code, u.first_name || ' ' || u.last_name`, termID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c AppointmentCandidate
		if err := rows.Scan(&c.TeachingCourseID, &c.CourseCode, &c.CourseNameTH, &c.TAID, &c.TAName); err != nil {
			rows.Close()
			return nil, err
		}
		out.Include = append(out.Include, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM appointment_order_items it
		JOIN appointment_orders o ON o.id = it.appointment_order_id
		WHERE o.term_id = $1`, termID).Scan(&out.AlreadyIssued); err != nil {
		return nil, err
	}

	skipped, err := s.skippedCourses(ctx, termID)
	if err != nil {
		return nil, err
	}
	out.Skipped = skipped
	return out, nil
}

// skippedCourses lists courses with a TA request still waiting on a decision.
// Those TAs are not approved yet, so they cannot be appointed — and staff need
// to know the course exists rather than wonder why it is missing.
func (s *AppointmentOrderService) skippedCourses(ctx context.Context, termID uuid.UUID) ([]SkippedCourse, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tc.id, tc.code, tc.name_th,
		       ARRAY_AGG(DISTINCT u.first_name || ' ' || u.last_name)
		FROM ta_requests r
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		JOIN ta_request_assignments a ON a.request_id = r.id AND a.state <> 'dropped'
		JOIN users u ON u.id = a.ta_id
		WHERE tc.term_id = $1
		  AND r.status = 'submitted'
		GROUP BY tc.id, tc.code, tc.name_th
		ORDER BY tc.code`, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []SkippedCourse{}
	for rows.Next() {
		var c SkippedCourse
		if err := rows.Scan(&c.TeachingCourseID, &c.CourseCode, &c.CourseNameTH, &c.PendingTAs); err != nil {
			return nil, err
		}
		c.Reason = "คำขอ TA ยังไม่ได้รับการตัดสิน — รอตารางเรียนของผู้ช่วยสอน"
		out = append(out, c)
	}
	return out, rows.Err()
}

// recordRound writes the ledger entry for an order that was just produced.
// Runs in the same call as Build so a generated document is never left
// unrecorded — an unrecorded round would be reprinted next time.
func (s *AppointmentOrderService) recordRound(
	ctx context.Context, actor uuid.UUID, in AppointmentOrderInput,
	round int, pairs []AppointmentCandidate,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	orderID := uuid.New()
	var signer *uuid.UUID
	if in.SignerOfficerID != uuid.Nil {
		signer = &in.SignerOfficerID
	}
	// generated_by is a FK to users. The handler always supplies an
	// authenticated actor, but a nil one must not turn a successfully rendered
	// document into a 500 — store NULL and keep the round recorded, because an
	// unrecorded round is reprinted next time.
	var by *uuid.UUID
	if actor != uuid.Nil {
		by = &actor
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO appointment_orders
		    (id, term_id, round_no, order_no, order_date, effective_date,
		     signer_officer_id, ta_count, generated_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		orderID, in.TermID, round, in.OrderNo, in.OrderDate, in.EffectiveDate,
		signer, len(pairs), by); err != nil {
		return err
	}
	for _, p := range pairs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO appointment_order_items (id, appointment_order_id, teaching_course_id, ta_id)
			VALUES (gen_random_uuid(), $1, $2, $3)
			ON CONFLICT DO NOTHING`, orderID, p.TeachingCourseID, p.TAID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// AppointmentRound summarises one issued order for the history list.
type AppointmentRound struct {
	ID          uuid.UUID `json:"id"`
	RoundNo     int       `json:"round_no"`
	OrderNo     string    `json:"order_no"`
	OrderDate   string    `json:"order_date"`
	TACount     int       `json:"ta_count"`
	GeneratedAt string    `json:"generated_at"`
	GeneratedBy string    `json:"generated_by,omitempty"`
	IsLate      bool      `json:"is_late"`
}

// ListRounds returns the orders already issued for a term, newest first.
func (s *AppointmentOrderService) ListRounds(ctx context.Context, termID uuid.UUID) ([]AppointmentRound, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT o.id, o.round_no, o.order_no, o.order_date, o.ta_count,
		       TO_CHAR(o.generated_at, 'YYYY-MM-DD"T"HH24:MI:SSTZ'),
		       COALESCE(u.first_name || ' ' || u.last_name, '')
		FROM appointment_orders o
		LEFT JOIN users u ON u.id = o.generated_by
		WHERE o.term_id = $1
		ORDER BY o.round_no DESC`, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AppointmentRound{}
	for rows.Next() {
		var r AppointmentRound
		if err := rows.Scan(&r.ID, &r.RoundNo, &r.OrderNo, &r.OrderDate, &r.TACount,
			&r.GeneratedAt, &r.GeneratedBy); err != nil {
			return nil, err
		}
		r.IsLate = r.RoundNo > 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// appointmentRoundSuffix labels the generated file so an on-time and a late
// bundle do not land in the downloads folder with the same name.
func appointmentRoundSuffix(round int) string {
	if round <= 1 {
		return ""
	}
	return fmt.Sprintf("_late%d", round)
}
