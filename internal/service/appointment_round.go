package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/docxgen"
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
	// rather than a bare "not ready". Same people as Waiting, names only.
	PendingTAs []string `json:"pending_tas,omitempty"`
	// Waiting are the TAs on the request with no class timetable for the
	// term, the one thing a pending request waits on, with when staff last
	// reminded each of them.
	Waiting []WaitingTA `json:"waiting"`
}

// WaitingTA is one TA holding up a course's TA request.
type WaitingTA struct {
	TAID       uuid.UUID  `json:"ta_id"`
	Name       string     `json:"name"`
	RemindedAt *time.Time `json:"reminded_at,omitempty"`
}

// AppointmentPreview is what staff see BEFORE committing a round: exactly who
// is on it, and what is being left behind and why. Printing an order is a
// paper act that cannot be recalled, so nothing here is issued until it is
// confirmed.
type AppointmentPreview struct {
	TermID    uuid.UUID              `json:"term_id"`
	NextRound int                    `json:"next_round"`
	IsLate    bool                   `json:"is_late"`
	Include   []AppointmentCandidate `json:"include"`
	Skipped   []SkippedCourse        `json:"skipped"`
	// AlreadyIssued counts pairs printed on an earlier round. Shown so the
	// number of names on this round reads as deliberate rather than truncated.
	AlreadyIssued int `json:"already_issued"`
	// SkippedTAs are (TA × course) pairs that would otherwise be on this round
	// but are left off, each with the reason ("ข้าม: ..."), so a missing name
	// reads as a decision rather than a bug.
	SkippedTAs []SkippedAppointee `json:"skipped_tas"`
}

// SkippedAppointee is one TA held off the order for a reason about the person
// rather than the course (SkippedCourse covers the course-level holds).
type SkippedAppointee struct {
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	CourseCode       string    `json:"course_code"`
	TAID             uuid.UUID `json:"ta_id"`
	TAName           string    `json:"ta_name"`
	Reason           string    `json:"reason"`
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

// appointmentEligibleSQL is the one definition of "this (TA × course) pair
// still needs an appointment order": approved request, assignment not dropped,
// active account, never printed on an earlier round of this term. Preview's
// name list and the dashboard badge both read it, so the two can never show
// different numbers. Build's own roster query repeats the same predicates.
const appointmentEligibleSQL = appointmentPendingFromSQL + `
	  AND u.is_active`

// appointmentPendingFromSQL is appointmentEligibleSQL without the per-person
// readiness checks — what Preview diffs against to name who was skipped.
const appointmentPendingFromSQL = `
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
	        AND it.ta_id = a.ta_id)`

// PendingCount reports how many รายชื่อ (TA × course pairs) the next round
// would print — the figure behind the sidebar badge. Same unit as the
// appointments page's "จะออกคำสั่งให้ N รายชื่อ".
func (s *AppointmentOrderService) PendingCount(ctx context.Context, termID uuid.UUID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM (SELECT DISTINCT tc.id, u.id `+appointmentEligibleSQL+`) q`,
		termID).Scan(&n)
	return n, err
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
		// A deactivated account (left the programme, disabled by staff) must
		// not be appointed on paper. Listed rather than silently dropped.
		SkippedTAs: []SkippedAppointee{},
	}
	srows, err := s.pool.Query(ctx, `
		SELECT DISTINCT tc.id, `+CourseCodesSQL("tc")+` AS codes, u.id, u.first_name || ' ' || u.last_name
		`+appointmentPendingFromSQL+`
		  AND NOT u.is_active
		ORDER BY codes, u.first_name || ' ' || u.last_name`, termID)
	if err != nil {
		return nil, err
	}
	for srows.Next() {
		var sk SkippedAppointee
		if err := srows.Scan(&sk.TeachingCourseID, &sk.CourseCode, &sk.TAID, &sk.TAName); err != nil {
			srows.Close()
			return nil, err
		}
		sk.Reason = "ข้าม: บัญชีผู้ใช้ของ TA ถูกปิดใช้งาน"
		out.SkippedTAs = append(out.SkippedTAs, sk)
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return nil, err
	}

	// Eligible = approved request, assignment not dropped, and never printed
	// on an earlier round of this term.
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT tc.id, `+CourseCodesSQL("tc")+` AS codes, tc.name_th, u.id, u.first_name || ' ' || u.last_name
		`+appointmentEligibleSQL+`
		-- SELECT DISTINCT requires every ORDER BY expression to be selected;
		-- the columns below are already in the list, so order by them by name.
		-- Every registrar code of a merged course ("CP245201/SC363001"), as
		-- the printed order shows it.
		ORDER BY codes, u.first_name || ' ' || u.last_name`, termID)
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
//
// A pending request waits only for its TAs' class timetables, so the people
// named are the TAs with no timetable for the term. It used to name every TA
// on the request, sending staff to chase people who had already done their
// part.
func (s *AppointmentOrderService) skippedCourses(ctx context.Context, termID uuid.UUID) ([]SkippedCourse, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tc.id, `+CourseCodesSQL("tc")+`, tc.name_th, w.ta_id, w.name, w.reminded_at
		FROM teaching_courses tc
		LEFT JOIN LATERAL (
			SELECT DISTINCT a.ta_id, `+personNameSQL+` AS name,
			       (SELECT MAX(tr.sent_at) FROM timetable_reminders tr
			         WHERE tr.ta_id = a.ta_id AND tr.term_id = $1) AS reminded_at
			FROM ta_requests r
			JOIN ta_request_assignments a ON a.request_id = r.id AND a.state <> 'dropped'
			JOIN users u ON u.id = a.ta_id
			LEFT JOIN ta_profiles tp ON tp.user_id = u.id
			WHERE r.teaching_course_id = tc.id AND r.status = 'submitted'
			  AND NOT EXISTS (SELECT 1 FROM ta_class_schedules cs
			                   WHERE cs.user_id = a.ta_id AND cs.term_id = $1)
		) w ON TRUE
		WHERE tc.term_id = $1
		  AND EXISTS (SELECT 1 FROM ta_requests r
		               WHERE r.teaching_course_id = tc.id AND r.status = 'submitted')
		ORDER BY tc.code, w.name`, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []SkippedCourse{}
	for rows.Next() {
		var (
			id         uuid.UUID
			code, name string
			taID       *uuid.UUID
			taName     *string
			remindedAt *time.Time
		)
		if err := rows.Scan(&id, &code, &name, &taID, &taName, &remindedAt); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].TeachingCourseID != id {
			out = append(out, SkippedCourse{TeachingCourseID: id, CourseCode: code, CourseNameTH: name,
				Waiting: []WaitingTA{}})
		}
		c := &out[len(out)-1]
		if taID != nil && taName != nil {
			c.Waiting = append(c.Waiting, WaitingTA{TAID: *taID, Name: *taName, RemindedAt: remindedAt})
			c.PendingTAs = append(c.PendingTAs, *taName)
		}
	}
	for i := range out {
		if len(out[i].Waiting) > 0 {
			out[i].Reason = "คำขอ TA ยังไม่ได้รับการตัดสิน รอตารางเรียนของผู้ช่วยสอน"
		} else {
			// Every timetable is in; the hourly sweep decides the request.
			out[i].Reason = "ผู้ช่วยสอนบันทึกตารางเรียนครบแล้ว ระบบจะพิจารณาคำขอภายใน 1 ชั่วโมง"
		}
	}
	return out, rows.Err()
}

// recordRound writes the ledger entry for an order that was just produced.
// Runs in the same call as Build so a generated document is never left
// unrecorded — an unrecorded round would be reprinted next time.
// The composed document is frozen in the same transaction as the ledger row:
// an order recorded without its snapshot could never be re-issued, and one
// stored without its ledger row would be reprinted in the next round.
func (s *AppointmentOrderService) recordRound(
	ctx context.Context, actor uuid.UUID, in AppointmentOrderInput,
	round int, pairs []AppointmentCandidate, doc docxgen.AppointmentOrderData,
) error {
	snapshot, err := json.Marshal(doc)
	if err != nil {
		return err
	}
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
		     signer_officer_id, ta_count, generated_by, document)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		orderID, in.TermID, round, in.OrderNo, in.OrderDate, in.EffectiveDate,
		signer, len(pairs), by, snapshot); err != nil {
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
	// CanReprint is false for orders issued before the document snapshot column
	// existed. The button is hidden rather than shown-and-failing, because the
	// refusal is permanent — nothing will ever make those reprintable.
	CanReprint bool `json:"can_reprint"`
}

// ListRounds returns the orders already issued for a term, newest first.
func (s *AppointmentOrderService) ListRounds(ctx context.Context, termID uuid.UUID) ([]AppointmentRound, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT o.id, o.round_no, o.order_no, o.order_date, o.ta_count,
		       TO_CHAR(o.generated_at, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'),
		       COALESCE(u.first_name || ' ' || u.last_name, ''),
		       o.document IS NOT NULL
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
			&r.GeneratedAt, &r.GeneratedBy, &r.CanReprint); err != nil {
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
