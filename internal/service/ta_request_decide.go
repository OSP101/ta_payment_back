package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ta_request_decide.go implements the deferred-decision model agreed in the
// 24/07/2026 meeting.
//
// The single invariant, in the lecturers' own words: a TA must go to their own
// class, not teach. So whenever a TA's timetable overlaps a session they were
// assigned to, that WHOLE session is unavailable to them — never the other way
// round, and never partially.
//
// What differs between the two cases is only WHEN the system can know:
//
//	Case 2 — the TA already has a timetable when the lecturer submits.
//	         The clash is knowable now, so it is refused outright with a
//	         message naming the section. The request form blocks the choice
//	         before it gets this far; this is the server-side backstop.
//
//	Case 1 — the TA has no timetable yet.
//	         Nothing can be judged, so the request RESTS in 'submitted'. When
//	         the TA later saves a timetable, ReevaluateForTA finishes the job:
//	         clashing sections are dropped, both parties are told, and the
//	         quota those sections reserved is released.
//
// Refusing in one case and dropping in the other is not two rules. It is one
// rule applied at the only moment each case allows.

// sectionClash counts how many of a section's weekly sessions collide with the
// TA's own timetable. WBA rows (year-4 work-based learning, one sentinel row
// spanning no real time) are excluded from conflict maths by rule C5.
//
// Both tables are weekly-recurring, so a collision repeats every week of the
// term. That makes the verdict structural: decide once per section, not per
// calendar date.
//
// `total` counts every session; `clashing` counts only the BLOCKING ones (see
// BlockingSessionSQL). The asymmetry is the point: a lecture period sitting on
// the TA's own class is not a loss, so it must not inflate the "%d จาก %d"
// message or push clashing past total into a whole-section drop.
func sectionClash(ctx context.Context, q querier, taID, sectionID uuid.UUID) (total, clashing int, err error) {
	err = q.QueryRow(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE `+BlockingSessionSQL("ss")+` AND EXISTS (
		           SELECT 1 FROM ta_class_schedules cs
		           WHERE cs.user_id = $1
		             AND NOT cs.is_wba
		             AND cs.term_id = (
		                 SELECT tc.term_id FROM sections sx
		                 JOIN teaching_courses tc ON tc.id = sx.teaching_course_id
		                 WHERE sx.id = $2)
		             AND cs.day_of_week = ss.day_of_week
		             AND cs.start_time < ss.end_time
		             AND ss.start_time < cs.end_time))
		FROM section_schedules ss
		WHERE ss.section_id = $2`, taID, sectionID).Scan(&total, &clashing)
	return total, clashing, err
}

// clashDetail is one collision, named on both sides: the teaching session the
// TA cannot cover, and the class of their own that takes the slot.
type clashDetail struct {
	SessionKind string // section_schedules.kind — บรรยาย / ปฏิบัติการ
	Day         int
	Start, End  string
	OwnCode     string
	OwnName     string
	OwnKind     string
}

func kindTH(k string) string {
	switch k {
	case "lecture":
		return "บรรยาย"
	case "lab":
		return "ปฏิบัติการ"
	}
	return ""
}

func dayTH(d int) string {
	if d >= 0 && d < len(thaiDayNames) {
		return thaiDayNames[d]
	}
	return ""
}

// sectionClashDetails returns one row per collision so the message can say
// WHICH session is lost and to WHAT.
//
// The type of the lost session is the part that changes what the TA can still
// do, and a bare count hid it: losing a บรรยาย slot costs them the
// attendance-taking for that hour, while losing a ปฏิบัติการ slot means they
// cannot run the lab at all. Same number, different job.
func sectionClashDetails(ctx context.Context, q rowQuerier, taID, sectionID uuid.UUID) ([]clashDetail, error) {
	// DISTINCT ON (ss.id): one bullet per teaching session. A session that
	// collides with two of the TA's own classes is still one session lost, and
	// printing it twice made the list look longer than the damage.
	//
	// course_label is the legacy free-form column; rows written before the
	// structured fields existed carry the name only there, and falling back to
	// it is what ListClasses already does.
	rows, err := q.Query(ctx, `
		SELECT DISTINCT ON (ss.id)
		       ss.kind, ss.day_of_week,
		       TO_CHAR(ss.start_time,'HH24:MI'), TO_CHAR(ss.end_time,'HH24:MI'),
		       COALESCE(NULLIF(cs.course_code,''), ''),
		       COALESCE(NULLIF(cs.course_name,''), NULLIF(cs.course_label,''), ''),
		       COALESCE(cs.kind,'')
		FROM section_schedules ss
		JOIN ta_class_schedules cs
		  ON cs.user_id = $1
		 AND NOT cs.is_wba
		 AND cs.term_id = (SELECT tc.term_id FROM sections sx
		                   JOIN teaching_courses tc ON tc.id = sx.teaching_course_id
		                   WHERE sx.id = $2)
		 AND cs.day_of_week = ss.day_of_week
		 AND cs.start_time < ss.end_time
		 AND ss.start_time < cs.end_time
		WHERE ss.section_id = $2
		  -- Same filter as sectionClash, or the bullet list would name sessions
		  -- the TA has not lost and the count above would not match the lines.
		  AND `+BlockingSessionSQL("ss")+`
		ORDER BY ss.id, ss.day_of_week, ss.start_time`, taID, sectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []clashDetail{}
	for rows.Next() {
		var d clashDetail
		if err := rows.Scan(&d.SessionKind, &d.Day, &d.Start, &d.End,
			&d.OwnCode, &d.OwnName, &d.OwnKind); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// clashLines renders the details as one bullet per collision. Kept separate
// from the query so the wording can be tested without a database.
func clashLines(details []clashDetail) []string {
	out := make([]string, 0, len(details))
	for _, d := range details {
		// Whole phrase, not a suffix: an unknown kind used to yield "คาบคาบสอน".
		lost := "คาบสอน"
		if k := kindTH(d.SessionKind); k != "" {
			lost = "คาบ" + k
		}
		own := strings.TrimSpace(d.OwnCode + " " + d.OwnName)
		if k := kindTH(d.OwnKind); own != "" && k != "" {
			own += " (" + k + ")"
		}
		// Without a name there is nothing to append — "ตรงกับ วิชาที่คุณเรียน
		// ที่คุณเรียน" stutters, so the unnamed case gets its own sentence.
		tail := "ตรงกับคาบเรียนของคุณ"
		if own != "" {
			tail = "ตรงกับ " + own + " ที่คุณเรียน"
		}
		out = append(out, fmt.Sprintf("• %s %s %s–%s %s",
			lost, dayTH(d.Day), d.Start, d.End, tail))
	}
	return out
}

// remainingKinds names the session types the TA can still take in a section,
// so the message ends with what they CAN do rather than only what they lost.
func remainingKinds(ctx context.Context, q rowQuerier, taID, sectionID uuid.UUID) (string, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT ss.kind
		FROM section_schedules ss
		WHERE ss.section_id = $2
		  AND NOT EXISTS (
		      SELECT 1 FROM ta_class_schedules cs
		      WHERE cs.user_id = $1 AND NOT cs.is_wba
		        AND cs.term_id = (SELECT tc.term_id FROM sections sx
		                          JOIN teaching_courses tc ON tc.id = sx.teaching_course_id
		                          WHERE sx.id = $2)
		        AND cs.day_of_week = ss.day_of_week
		        AND cs.start_time < ss.end_time
		        AND ss.start_time < cs.end_time)
		ORDER BY ss.kind`, taID, sectionID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return "", err
		}
		if th := kindTH(k); th != "" {
			kinds = append(kinds, th)
		}
	}
	return strings.Join(kinds, " และ "), rows.Err()
}

// tasMissingSchedule lists the TAs on a request who have not recorded a
// timetable for the term yet, in a stable order so the message the lecturer
// sees does not shuffle between reloads.
func tasMissingSchedule(ctx context.Context, q rowQuerier, reqID, termID uuid.UUID) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT u.first_name || ' ' || u.last_name AS name
		FROM ta_request_assignments a
		JOIN users u ON u.id = a.ta_id
		WHERE a.request_id = $1
		  AND a.state <> 'dropped'
		  AND NOT EXISTS (
		      SELECT 1 FROM ta_class_schedules cs
		      WHERE cs.user_id = a.ta_id AND cs.term_id = $2)
		ORDER BY name`, reqID, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// rowQuerier is the multi-row counterpart of querier — satisfied by both
// *pgxpool.Pool and pgx.Tx, so the helpers work inside or outside a decision
// transaction.
type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// assertNoKnownClash is GONE (31/07/2026). It refused the whole submission when
// a clash was already knowable, which meant a TA who had filed their timetable
// was judged more harshly than one who had not — the first lost the section, the
// second only lost the clashing session. Create now calls applyClashOutcome like
// the deferred path, so both orders reach the same verdict.
//
// The real case that exposed it: จิรายุ assists SC362004 sec 2-3 doing grading
// only. One of that section's labs sits on a class of his, so the old gate
// refused him outright — for a lab he was never going to run.

// applyClashOutcome writes the per-assignment verdict for every TA on the
// request who now has a timetable. Returns the human-readable notices, keyed by
// TA id, describing what they lost and why.
//
// Sessions are dropped whole (never trimmed to the surviving minutes): the
// meeting was explicit that a TA who must be in class for part of a session
// cannot cover that session at all.
func (s *TARequestService) applyClashOutcome(ctx context.Context, tx pgx.Tx, reqID uuid.UUID) (map[uuid.UUID][]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT a.id, a.ta_id, a.section_id, sec.sec_no,
		       u.first_name || ' ' || u.last_name
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		JOIN users u ON u.id = a.ta_id
		WHERE a.request_id = $1 AND a.state <> 'dropped'
		ORDER BY a.ta_id, sec.sec_no`, reqID)
	if err != nil {
		return nil, err
	}
	type asg struct {
		id, taID, secID uuid.UUID
		secNo, taName   string
	}
	var all []asg
	for rows.Next() {
		var a asg
		if err := rows.Scan(&a.id, &a.taID, &a.secID, &a.secNo, &a.taName); err != nil {
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
		total, clashing, err := sectionClash(ctx, tx, a.taID, a.secID)
		if err != nil {
			return nil, err
		}
		if clashing == 0 {
			continue
		}
		details, err := sectionClashDetails(ctx, tx, a.taID, a.secID)
		if err != nil {
			return nil, err
		}
		lines := clashLines(details)

		state := "trimmed"
		// Name the collisions, then say what is left. A count alone ("1 จาก 2
		// คาบ") told the TA how much they lost but not which job — and losing
		// the lecture hour vs the lab hour leaves them able to do different
		// things.
		head := fmt.Sprintf("Section %s: ตรงกับตารางเรียนของคุณ %d จาก %d คาบ",
			a.secNo, clashing, total)
		reason := strings.Join(append([]string{head}, lines...), "\n")
		if total > 0 && clashing >= total {
			state = "dropped"
			reason = strings.Join(append(
				[]string{fmt.Sprintf("Section %s: ทุกคาบตรงกับตารางเรียนของคุณ — คุณจึงไม่ได้เป็นผู้ช่วยสอนกลุ่มนี้", a.secNo)},
				lines...), "\n")
		} else {
			left, err := remainingKinds(ctx, tx, a.taID, a.secID)
			if err != nil {
				return nil, err
			}
			if left != "" {
				reason += fmt.Sprintf("\nยังลงเวลาในคาบ%sได้ตามปกติ", left)
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE ta_request_assignments
			SET state = $1::ta_assignment_state, state_reason = $2, state_decided_at = NOW()
			WHERE id = $3`, state, reason, a.id); err != nil {
			return nil, err
		}
		notices[a.taID] = append(notices[a.taID], reason)
	}
	return notices, nil
}

// ReevaluateForTA finishes every request that was waiting on this TA's
// timetable. Called after the TA saves their schedule (see
// WorkloadService.ReplaceClasses) and from the periodic sweep, so a missed
// call self-heals rather than stranding the request forever.
//
// Requests where some OTHER TA still has no timetable are left alone — the
// meeting asked for the verdict to wait until everyone on the request is ready.
func (s *TARequestService) ReevaluateForTA(ctx context.Context, taID, termID uuid.UUID) error {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT r.id
		FROM ta_requests r
		JOIN ta_request_assignments a ON a.request_id = r.id
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		WHERE a.ta_id = $1 AND tc.term_id = $2 AND r.status = 'submitted'`, taID, termID)
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, id := range ids {
		if err := s.tryFinalize(ctx, id, termID); err != nil {
			// One stuck request must not block the others, and the TA's
			// schedule save (the caller) must still succeed.
			log.Printf("ta_request %s: reevaluate failed: %v", id, err)
		}
	}
	return nil
}

// tryFinalize decides one pending request if every TA on it now has a
// timetable. No-op otherwise.
func (s *TARequestService) tryFinalize(ctx context.Context, reqID, termID uuid.UUID) error {
	missing, err := tasMissingSchedule(ctx, s.pool, reqID, termID)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return nil // still waiting on someone else
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Re-read status inside the tx so two concurrent finalisers (the TA's save
	// and the sweep) cannot both decide the same request.
	var status string
	if err := tx.QueryRow(ctx,
		`SELECT status::text FROM ta_requests WHERE id = $1 FOR UPDATE`, reqID).Scan(&status); err != nil {
		return err
	}
	if status != "submitted" {
		return nil
	}

	notices, err := s.applyClashOutcome(ctx, tx, reqID)
	if err != nil {
		return err
	}

	checks, passed, err := s.autoDecide(ctx, tx, reqID)
	if err != nil {
		return err
	}

	// A TA whose every section was dropped is no longer on this request, so the
	// request can still be approved for whoever remains.
	var surviving int
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM ta_request_assignments WHERE request_id = $1 AND state <> 'dropped'`,
		reqID).Scan(&surviving); err != nil {
		return err
	}
	verdict := "rejected"
	var reason string
	switch {
	case surviving == 0:
		reason = "ผู้ช่วยสอนทุกคนในคำขอนี้ติดตารางเรียนทุกคาบ จึงไม่มีใครสอนได้"
	case passed:
		verdict = "approved"
	default:
		reason = joinRejectMessages(checks)
	}

	checksJSON, err := json.Marshal(checks)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ta_requests SET
		  status = $1::ta_request_status, decided_at = NOW(), decided_by = NULL,
		  reject_reason = NULLIF($2, ''), decision_checks = $3::jsonb, updated_at = NOW()
		WHERE id = $4`, verdict, reason, checksJSON, reqID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	s.notifyClashOutcome(ctx, reqID, notices)
	s.notifyDecision(ctx, reqID, verdict, reason)
	return nil
}

// notifyClashOutcome tells each affected TA exactly which sessions they lost
// and why, and gives the lecturer one combined summary. Without this the TA
// would discover the loss only when the work-log screen silently refused a
// session.
func (s *TARequestService) notifyClashOutcome(ctx context.Context, reqID uuid.UUID, notices map[uuid.UUID][]string) {
	if len(notices) == 0 {
		return
	}
	var courseID, lecturerID uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT teaching_course_id, lecturer_id FROM ta_requests WHERE id = $1`,
		reqID).Scan(&courseID, &lecturerID); err != nil {
		return
	}
	code, nameTH := s.courseLabel(ctx, courseID)
	label := strings.TrimSpace(code + " " + nameTH)

	var summary []string
	for taID, lines := range notices {
		s.notify.Send(ctx, taID,
			"ตารางเรียนของคุณทับกับคาบสอน",
			fmt.Sprintf("วิชา %s\n%s", label, strings.Join(lines, "\n")),
			"/ta/courses")
		summary = append(summary, fmt.Sprintf("%s — %s", s.taName(ctx, taID), strings.Join(lines, "; ")))
	}
	s.notify.Send(ctx, lecturerID,
		"ผู้ช่วยสอนบางคนติดตารางเรียน",
		fmt.Sprintf("วิชา %s\n%s", label, strings.Join(summary, "\n")),
		"/lecturer/courses")
}

// SweepPendingRequests finalises any request whose TAs have all filed their
// timetables since the last pass. The trigger on the TA's save is the fast
// path; this is the safety net that keeps a dropped call from stranding a
// request — and, with it, the course quota those assignments reserve.
func (s *TARequestService) SweepPendingRequests(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, tc.term_id
		FROM ta_requests r
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		WHERE r.status = 'submitted'`)
	if err != nil {
		return 0, err
	}
	type pending struct{ reqID, termID uuid.UUID }
	var list []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.reqID, &p.termID); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	decided := 0
	for _, p := range list {
		before, err := s.requestStatus(ctx, p.reqID)
		if err != nil {
			continue
		}
		if err := s.tryFinalize(ctx, p.reqID, p.termID); err != nil {
			log.Printf("ta_request %s: sweep failed: %v", p.reqID, err)
			continue
		}
		if after, err := s.requestStatus(ctx, p.reqID); err == nil && after != before {
			decided++
		}
	}
	return decided, nil
}

func (s *TARequestService) requestStatus(ctx context.Context, reqID uuid.UUID) (string, error) {
	var st string
	err := s.pool.QueryRow(ctx, `SELECT status::text FROM ta_requests WHERE id = $1`, reqID).Scan(&st)
	return st, err
}
