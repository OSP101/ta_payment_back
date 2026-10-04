package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// SplitLegacyRequests brings requests filed before 0149 — one request holding
// several TAs, judged as a unit — under the one-request-per-TA model, and
// decides each TA again on their own.
//
// Asked for on 03/10/2026, after the per-TA change shipped: requests sent
// before it still showed some TAs passing every check while the request stayed
// rejected (or waiting on someone else's timetable), so the course never
// reached those TAs' home page.
//
// Only what is still undecided for anyone is touched: 'submitted', or rejected
// by the system (decided_by NULL — a cancel always records who). Approved
// requests already let every TA work, and anything cancelled was a person's
// choice. Terms that have ended are left alone — approving now would appoint
// TAs for a term that is over — unless staff still have the term active.
//
// Idempotent: a split request gets a batch_id, which takes it out of the
// query, so running it from every sweep is safe.
func (s *TARequestService) SplitLegacyRequests(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, tc.term_id
		FROM ta_requests r
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		JOIN academic_terms at ON at.id = tc.term_id
		WHERE r.batch_id IS NULL
		  AND (r.status = 'submitted' OR (r.status = 'rejected' AND r.decided_by IS NULL))
		  AND (at.is_active OR at.ends_on IS NULL OR at.ends_on >= CURRENT_DATE)
		  AND (SELECT COUNT(DISTINCT a.ta_id) FROM ta_request_assignments a
		       WHERE a.request_id = r.id) > 1
		ORDER BY r.submitted_at`)
	if err != nil {
		return 0, err
	}
	type legacy struct{ reqID, termID uuid.UUID }
	var list []legacy
	for rows.Next() {
		var l legacy
		if err := rows.Scan(&l.reqID, &l.termID); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	split := 0
	for _, l := range list {
		ids, err := s.splitLegacy(ctx, l.reqID, l.termID)
		if err != nil {
			log.Printf("ta_request %s: split into per-TA requests failed: %v", l.reqID, err)
			continue
		}
		if len(ids) == 0 {
			continue // changed under us; the next sweep sees it as it is now
		}
		split++
		for _, id := range ids {
			if err := s.finalize(ctx, id, l.termID, true); err != nil {
				log.Printf("ta_request %s: decide after split failed: %v", id, err)
			}
		}
	}
	return split, nil
}

// splitLegacy moves every TA but the first onto a request of their own, all
// tied by batch_id = the original id, and puts each back to 'submitted' for
// finalize to judge. Returns the request ids, or nil when the request no
// longer qualifies.
func (s *TARequestService) splitLegacy(ctx context.Context, reqID, termID uuid.UUID) ([]uuid.UUID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var status string
	var decidedByNull, hasBatch bool
	if err := tx.QueryRow(ctx, `
		SELECT status::text, decided_by IS NULL, batch_id IS NOT NULL
		FROM ta_requests WHERE id = $1 FOR UPDATE`, reqID).Scan(&status, &decidedByNull, &hasBatch); err != nil {
		return nil, err
	}
	if hasBatch || !(status == "submitted" || (status == "rejected" && decidedByNull)) {
		return nil, nil
	}

	trows, err := tx.Query(ctx, `
		SELECT DISTINCT a.ta_id FROM ta_request_assignments a
		WHERE a.request_id = $1 ORDER BY a.ta_id`, reqID)
	if err != nil {
		return nil, err
	}
	var tas []uuid.UUID
	for trows.Next() {
		var id uuid.UUID
		if err := trows.Scan(&id); err != nil {
			trows.Close()
			return nil, err
		}
		tas = append(tas, id)
	}
	trows.Close()
	if err := trows.Err(); err != nil {
		return nil, err
	}
	if len(tas) < 2 {
		return nil, nil
	}

	// The original keeps the first TA and every field it had; the others get
	// copies of it, so submitted_at, is_late, window and sender stay true to
	// when and how it was really sent.
	ids := []uuid.UUID{reqID}
	for _, ta := range tas[1:] {
		nid := uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO ta_requests (id, teaching_course_id, window_id, lecturer_id, reimburse_scope,
			                         status, submitted_at, created_at, is_late, submitted_by, batch_id)
			SELECT $2, teaching_course_id, window_id, lecturer_id, reimburse_scope,
			       'submitted', submitted_at, created_at, is_late, submitted_by, $1
			FROM ta_requests WHERE id = $1`, reqID, nid); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE ta_request_assignments SET request_id = $1 WHERE request_id = $2 AND ta_id = $3`,
			nid, reqID, ta); err != nil {
			return nil, err
		}
		ids = append(ids, nid)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ta_requests SET
		  batch_id = id, status = 'submitted', decided_at = NULL, decided_by = NULL,
		  reject_reason = NULL, decision_checks = '[]'::jsonb, updated_at = NOW(),
		  lecturer_notified_at = NULL, lecturer_note = NULL
		WHERE id = $1`, reqID); err != nil {
		return nil, err
	}

	// Counts follow the people: each request counts its own TA on each of
	// their sections, as Create now writes them.
	if _, err := tx.Exec(ctx, `DELETE FROM ta_request_counts WHERE request_id = ANY($1)`, ids); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO ta_request_counts (request_id, section_id, undergrad_count, graduate_count)
		SELECT request_id, section_id,
		       COUNT(*) FILTER (WHERE level = 'undergrad'),
		       COUNT(*) FILTER (WHERE level <> 'undergrad')
		FROM ta_request_assignments WHERE request_id = ANY($1)
		GROUP BY request_id, section_id`, ids); err != nil {
		return nil, err
	}

	// A TA still without a timetable rests, as a fresh request would; say so
	// on the row, since finalize leaves it untouched.
	for _, id := range ids {
		waiting, err := tasMissingSchedule(ctx, tx, id, termID)
		if err != nil {
			return nil, err
		}
		if len(waiting) == 0 {
			continue
		}
		checks, err := json.Marshal([]DecisionCheck{{
			Rule: "schedule", Passed: false, Warning: true,
			Message: fmt.Sprintf(
				"รอตารางเรียนของ %s ระบบจะตัดสินให้อัตโนมัติเมื่อบันทึกตารางเรียนแล้ว "+
					"หากคาบใดตรงกับตารางเรียน คาบนั้นจะถูกตัดออก",
				strings.Join(waiting, ", ")),
		}})
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE ta_requests SET decision_checks = $1::jsonb WHERE id = $2`, checks, id); err != nil {
			return nil, err
		}
	}

	idStrs := make([]string, len(ids))
	for i, id := range ids {
		idStrs[i] = id.String()
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{
		Action: "ta_request.split_per_ta", Entity: "ta_request", EntityID: reqID.String(),
		Note:  fmt.Sprintf("แยกคำขอเดิม (สถานะ %s) เป็นรายบุคคล %d คน เพื่อตัดสินแยกกัน", statusLabelTH[status], len(ids)),
		After: map[string]any{"requests": idStrs},
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return ids, nil
}
