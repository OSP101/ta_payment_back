package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

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

// offSlotHours sums the declared duties that are performed OUTSIDE the class
// session — grading and other work, for both undergrad and grad shapes. A
// positive result means a fully-clashing section is still worth keeping.
func offSlotHours(ctx context.Context, q rowQuerier, assignmentID uuid.UUID) (float64, error) {
	rows, err := q.Query(ctx, `
		SELECT COALESCE(wf.check_work_hrs,0) + COALESCE(wf.ug_other_hrs,0)
		     + COALESCE(wf.lab_other_hrs,0)
		     + COALESCE(wf.grade_hrs,0) + COALESCE(wf.other_hrs,0) + COALESCE(wf.prep_hrs,0)
		FROM ta_workload_forms wf WHERE wf.assignment_id = $1`, assignmentID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var total float64
	for rows.Next() {
		var h float64
		if err := rows.Scan(&h); err != nil {
			return 0, err
		}
		total += h
	}
	return total, rows.Err()
}

// kindClash is sectionClash split by session kind.
type kindClash map[string]struct{ Total, Clashing int }

// fullyBlocked reports that EVERY session of this kind collides, which is the
// only case that costs the TA the matching in-class duty. A partial collision
// leaves workable sessions behind, and the meeting was explicit that those
// still count — losing one of three lectures must not cost the whole duty.
//
// A kind with no sessions at all is not "blocked": it is simply absent, and the
// hour caps already refuse hours against it.
func (k kindClash) fullyBlocked(kind string) bool {
	c, ok := k[kind]
	return ok && c.Total > 0 && c.Clashing >= c.Total
}

// sectionClashByKind is sectionClash grouped by section_schedules.kind, so a
// caller can tell "the lectures all clash but the labs are fine" from "the
// whole section is unusable".
//
// The distinction is the whole point of the per-kind rule: เช็คชื่อ must happen
// inside the lecture and สอนปฏิบัติการ inside the lab, but ตรวจงาน and อื่น ๆ
// are done outside the slot and survive any collision.
//
// It deliberately does NOT apply BlockingSessionSQL. That predicate exempts
// lecture periods — a decision from 31/07/2026 that a TA whose own class sits on
// a lecture may still take attendance — and it still governs whether an existing
// duty may be LOGGED. Whether the duty may be DECLARED in the first place is now
// the opposite call: a section whose every lecture collides cannot carry
// เช็คชื่อ, per the 05/08/2026 decision that reversed it for declarations.
//
// Two rules, two questions. Keeping the exemption inside the logging predicate
// and out of this one is what lets both answers stay true at once — but they are
// close enough that changing either without the other will look correct and be
// wrong, so change them together.
func sectionClashByKind(ctx context.Context, q rowQuerier, taID, sectionID uuid.UUID) (kindClash, error) {
	rows, err := q.Query(ctx, `
		SELECT ss.kind, COUNT(*),
		       COUNT(*) FILTER (WHERE EXISTS (
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
		WHERE ss.section_id = $2
		GROUP BY ss.kind`, taID, sectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := kindClash{}
	for rows.Next() {
		var kind string
		var total, clashing int
		if err := rows.Scan(&kind, &total, &clashing); err != nil {
			return nil, err
		}
		out[kind] = struct{ Total, Clashing int }{total, clashing}
	}
	return out, rows.Err()
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
	return s.applyClashOutcomeFor(ctx, tx, reqID, uuid.Nil, false)
}

// applyClashOutcomeFor is applyClashOutcome narrowed to one TA (onlyTA; Nil =
// everyone) and, with recheck set, to assignments whose verdict actually
// changes. recheck is the path for a request that was already decided: the
// timetable page autosaves after every edit, so the verdict was reached on
// whatever the TA had entered by then, and every later save must be able to
// correct it without re-notifying on saves that change nothing.
//
// A recheck never drops an assignment that already has logged hours. Exports
// skip dropped assignments, so dropping it would silently remove those hours
// from pay; it stays 'trimmed' with the new reason for staff to settle.
func (s *TARequestService) applyClashOutcomeFor(ctx context.Context, tx pgx.Tx, reqID, onlyTA uuid.UUID, recheck bool) (map[uuid.UUID][]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT a.id, a.ta_id, a.section_id, sec.sec_no,
		       u.first_name || ' ' || u.last_name, a.level::text,
		       a.state::text, COALESCE(a.state_reason, ''), a.pre_clash IS NOT NULL
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		JOIN users u ON u.id = a.ta_id
		WHERE a.request_id = $1
		  -- A recheck also looks at dropped rows: a drop caused by the TA's own
		  -- timetable is undone when that class goes away (see restoreFromClash).
		  AND (a.state <> 'dropped' OR ($3 AND a.pre_clash IS NOT NULL))
		  AND ($2::uuid = '00000000-0000-0000-0000-000000000000' OR a.ta_id = $2)
		ORDER BY a.ta_id, sec.sec_no`, reqID, onlyTA, recheck)
	if err != nil {
		return nil, err
	}
	type asg struct {
		id, taID, secID      uuid.UUID
		secNo, taName, level string
		oldState, oldReason  string
		hasSnapshot          bool
	}
	var all []asg
	for rows.Next() {
		var a asg
		if err := rows.Scan(&a.id, &a.taID, &a.secID, &a.secNo, &a.taName, &a.level, &a.oldState, &a.oldReason, &a.hasSnapshot); err != nil {
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
			if recheck && a.hasSnapshot && a.oldState != "active" {
				line, err := s.restoreFromClash(ctx, tx, a.id, a.taID, a.secNo)
				if err != nil {
					return nil, err
				}
				if line != "" {
					notices[a.taID] = append(notices[a.taID], line)
				}
			}
			continue
		}
		// Remember the pre-clash shape once, before the first cut, so a later
		// timetable fix can put it back.
		if !a.hasSnapshot {
			if _, err := tx.Exec(ctx, `
				UPDATE ta_request_assignments a
				SET pre_clash = jsonb_build_object(
				      'state', a.state::text,
				      'attendance_hrs', COALESCE(w.attendance_hrs, 0),
				      'lab_hrs', COALESCE(w.lab_hrs, 0))
				FROM (SELECT $1::uuid AS id) x
				LEFT JOIN ta_workload_forms w ON w.assignment_id = x.id
				WHERE a.id = x.id AND a.pre_clash IS NULL`, a.id); err != nil {
				return nil, err
			}
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

		left, err := remainingKinds(ctx, tx, a.taID, a.secID)
		if err != nil {
			return nil, err
		}
		if left != "" {
			reason += fmt.Sprintf("\nยังลงเวลาในคาบ%sได้ตามปกติ", left)
		}

		// Every session colliding no longer costs the whole assignment. Grading
		// and other work happen OUTSIDE the session — that is the entire reason
		// a TA can support a group they cannot stand in the room for — so the
		// assignment survives as long as one such duty was declared.
		//
		// Dropping it outright was the old behaviour and it took the off-slot
		// duties down with the in-class ones.
		if total > 0 && clashing >= total {
			offSlot, err := offSlotHours(ctx, tx, a.id)
			if err != nil {
				return nil, err
			}
			if offSlot > 0 {
				reason = strings.Join(append(
					[]string{fmt.Sprintf("Section %s: ทุกคาบตรงกับตารางเรียนของคุณ ลงเวลาในคาบไม่ได้ แต่ยังทำงานตรวจ/งานอื่นนอกคาบได้ตามที่อาจารย์ระบุ", a.secNo)},
					lines...), "\n")
			} else {
				state = "dropped"
				reason = strings.Join(append(
					[]string{fmt.Sprintf("Section %s: ทุกคาบตรงกับตารางเรียนของคุณ และไม่มีงานนอกคาบที่อาจารย์ระบุไว้ คุณจึงไม่ได้เป็นผู้ช่วยสอนกลุ่มนี้", a.secNo)},
					lines...), "\n")
			}
		}
		if recheck && state == "dropped" {
			var logged bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM work_logs WHERE assignment_id = $1)`, a.id).Scan(&logged); err != nil {
				return nil, err
			}
			if logged {
				state = "trimmed"
			}
		}
		// An in-class duty declared on a kind the TA can never attend is zeroed
		// here rather than refused at Create, so the declared figures match what
		// the form allows whichever order the timetable arrived in. The lines go
		// to the notice only: state_reason must keep matching the wording
		// migration 0048 rebuilds (TestMigration0048_MatchesGoWording).
		var stripped []string
		if state != "dropped" && a.level == "undergrad" {
			stripped, err = stripBlockedInClassHours(ctx, tx, a.id, a.taID, a.secID)
			if err != nil {
				return nil, err
			}
		}
		if recheck && state == a.oldState && reason == a.oldReason && len(stripped) == 0 {
			continue // nothing new since the last verdict
		}
		if _, err := tx.Exec(ctx, `
			UPDATE ta_request_assignments
			SET state = $1::ta_assignment_state, state_reason = $2, state_decided_at = NOW()
			WHERE id = $3`, state, reason, a.id); err != nil {
			return nil, err
		}
		notices[a.taID] = append(notices[a.taID], reason)
		notices[a.taID] = append(notices[a.taID], stripped...)
	}
	return notices, nil
}

// restoreFromClash undoes a clash cut once the TA's timetable no longer
// collides with the section: state back to what it was before the first cut,
// and the in-class duty hours stripBlockedInClassHours zeroed back to what the
// lecturer declared. A dropped assignment had released its quota, so it only
// comes back while the TA is still under the per-term course cap; otherwise it
// stays dropped and the notice says why. Returns the notice line ("" = none).
func (s *TARequestService) restoreFromClash(ctx context.Context, tx pgx.Tx, assignmentID, taID uuid.UUID, secNo string) (string, error) {
	var prevState string
	var attendance, lab float64
	var termID, courseID uuid.UUID
	var curState string
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(a.pre_clash->>'state', 'active'),
		       COALESCE((a.pre_clash->>'attendance_hrs')::numeric, 0),
		       COALESCE((a.pre_clash->>'lab_hrs')::numeric, 0),
		       tc.term_id, tc.id, a.state::text
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		WHERE a.id = $1`, assignmentID).Scan(&prevState, &attendance, &lab, &termID, &courseID, &curState); err != nil {
		return "", err
	}
	if prevState == "dropped" || prevState == "trimmed" {
		// The snapshot is of the first cut; anything before it was active.
		prevState = "active"
	}
	if curState == "dropped" {
		count, err := s.reservedCourseCount(ctx, tx, taID, termID, courseID)
		if err != nil {
			return "", err
		}
		courseCap, err := maxCoursesPerTerm(ctx, tx)
		if err != nil {
			return "", err
		}
		if count >= courseCap {
			return fmt.Sprintf("Section %s: ตารางเรียนไม่ทับซ้อนแล้ว แต่คืนสิทธิ์ช่วยสอนไม่ได้ เพราะคุณเป็นผู้ช่วยสอนครบ %d วิชาในภาคการศึกษานี้แล้ว", secNo, courseCap), nil
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ta_workload_forms
		SET attendance_hrs = GREATEST(attendance_hrs, $2),
		    lab_hrs        = GREATEST(lab_hrs, $3)
		WHERE assignment_id = $1`, assignmentID, attendance, lab); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ta_request_assignments
		SET state = $2::ta_assignment_state, state_reason = NULL,
		    state_decided_at = NOW(), pre_clash = NULL
		WHERE id = $1`, assignmentID, prevState); err != nil {
		return "", err
	}
	return fmt.Sprintf("Section %s: ตารางเรียนไม่ชนกับคาบสอนแล้ว ระบบคืนสิทธิ์ช่วยสอนและชั่วโมงตามที่อาจารย์ระบุไว้", secNo), nil
}

// stripBlockedInClassHours zeroes the in-class duty of every session kind whose
// EVERY meeting collides with the TA's own class — เช็คชื่อ for lecture,
// สอนปฏิบัติการ for lab — and returns one line per duty removed, for the notice.
// It is the server-side twin of the request form greying those fields out
// (PreviewConflicts.BlockedKinds), so a call that skips the form ends up with
// the same declaration. Undergrad only: the grad form has no per-kind duty.
func stripBlockedInClassHours(ctx context.Context, tx pgx.Tx, assignmentID, taID, sectionID uuid.UUID) ([]string, error) {
	byKind, err := sectionClashByKind(ctx, tx, taID, sectionID)
	if err != nil {
		return nil, err
	}
	lec, lab := byKind.fullyBlocked("lecture"), byKind.fullyBlocked("lab")
	if !lec && !lab {
		return nil, nil
	}
	var attendance, labHrs float64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(attendance_hrs,0), COALESCE(lab_hrs,0)
		FROM ta_workload_forms WHERE assignment_id = $1`, assignmentID).Scan(&attendance, &labHrs); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var lines []string
	if lec && attendance > 0.001 {
		lines = append(lines, fmt.Sprintf("ตัดชั่วโมงเช็คชื่อ/เก็บใบงาน %.1f ชม./สัปดาห์ออก เพราะคาบบรรยายทุกคาบตรงกับตารางเรียน", attendance))
	} else {
		lec = false
	}
	if lab && labHrs > 0.001 {
		lines = append(lines, fmt.Sprintf("ตัดชั่วโมงสอนปฏิบัติการ %.1f ชม./สัปดาห์ออก เพราะคาบปฏิบัติการทุกคาบตรงกับตารางเรียน", labHrs))
	} else {
		lab = false
	}
	if len(lines) == 0 {
		return nil, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ta_workload_forms
		SET attendance_hrs = CASE WHEN $2 THEN 0 ELSE attendance_hrs END,
		    lab_hrs        = CASE WHEN $3 THEN 0 ELSE lab_hrs END
		WHERE assignment_id = $1`, assignmentID, lec, lab); err != nil {
		return nil, err
	}
	return lines, nil
}

// ReevaluateForTA finishes every request that was waiting on this TA's
// timetable. Called after the TA saves their schedule (see
// WorkloadService.ReplaceClasses) and from the periodic sweep, so a missed
// call self-heals rather than stranding the request forever.
//
// A request filed since 0149 holds one TA, so it waits on that TA alone. An
// older request naming several still waits until every one of them is ready.
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

	// Requests already decided for this TA. The timetable page autosaves after
	// every edit, so a request can be decided on a half-entered timetable — the
	// first class saved finishes it, and the classes added a moment later were
	// never checked. Re-apply the clash rule to this TA's part of each one.
	rows, err = s.pool.Query(ctx, `
		SELECT DISTINCT r.id
		FROM ta_requests r
		JOIN ta_request_assignments a ON a.request_id = r.id
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		WHERE a.ta_id = $1 AND tc.term_id = $2
		  AND (r.status = 'approved'
		       -- auto-rejected because every TA's timetable clashed: a fixed
		       -- timetable may bring it back (recheckApproved)
		       OR (r.status = 'rejected' AND r.decided_by IS NULL AND r.reject_reason IN ($3, $4)))
		  AND (a.state <> 'dropped' OR a.pre_clash IS NOT NULL)`, taID, termID, clashRejectReason, legacyClashRejectReason)
	if err != nil {
		return err
	}
	ids = ids[:0]
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
		if err := s.recheckApproved(ctx, id, taID); err != nil {
			log.Printf("ta_request %s: recheck after timetable change failed: %v", id, err)
		}
	}
	return nil
}

// recheckApproved re-applies the clash rule to one TA's part of an approved
// request after their timetable changed. When that leaves nobody on the request
// able to work, the request is rejected — the verdict it would have had if the
// whole timetable had been there when it was first decided.
func (s *TARequestService) recheckApproved(ctx context.Context, reqID, taID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var status, rejectReason string
	var decidedByNull bool
	if err := tx.QueryRow(ctx,
		`SELECT status::text, COALESCE(reject_reason, ''), decided_by IS NULL
		   FROM ta_requests WHERE id = $1 FOR UPDATE`, reqID).Scan(&status, &rejectReason, &decidedByNull); err != nil {
		return err
	}
	clashRejected := status == "rejected" && decidedByNull && isClashRejectReason(rejectReason)
	if status != "approved" && !clashRejected {
		return nil
	}
	notices, err := s.applyClashOutcomeFor(ctx, tx, reqID, taID, true)
	if err != nil {
		return err
	}
	if len(notices) == 0 {
		return nil
	}

	var surviving int
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM ta_request_assignments WHERE request_id = $1 AND state <> 'dropped'`,
		reqID).Scan(&surviving); err != nil {
		return err
	}
	reason := clashRejectReason
	rejected := surviving == 0 && status == "approved"
	reinstated := surviving > 0 && clashRejected
	if reinstated {
		if _, err := tx.Exec(ctx, `
			UPDATE ta_requests SET
			  status = 'approved', decided_at = NOW(), decided_by = NULL,
			  reject_reason = NULL, updated_at = NOW()
			WHERE id = $1`, reqID); err != nil {
			return err
		}
	}
	if rejected {
		if _, err := tx.Exec(ctx, `
			UPDATE ta_requests SET
			  status = 'rejected', decided_at = NOW(), decided_by = NULL,
			  reject_reason = $1, updated_at = NOW()
			WHERE id = $2`, reason, reqID); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	s.notifyClashOutcome(ctx, reqID, notices)
	if rejected {
		s.notifyDecision(ctx, reqID, "rejected", reason)
	}
	if reinstated {
		s.notifyDecision(ctx, reqID, "approved", "")
	}
	return nil
}

// clashRejectReason is the reject_reason written when the TA's own timetable
// leaves no section they can work. It doubles as the marker that a rejection
// was the system's, not a decision anyone made, so a fixed timetable may
// reinstate it.
const clashRejectReason = "ผู้ช่วยสอนติดตารางเรียนทุกคาบในทุกกลุ่มที่ขอ จึงช่วยสอนไม่ได้"

// legacyClashRejectReason is the same marker from before requests were filed
// one per TA (0149), when one request could hold several. Rows carrying it
// must still be reinstatable.
const legacyClashRejectReason = "ผู้ช่วยสอนทุกคนในคำขอนี้ติดตารางเรียนทุกคาบ จึงไม่มีใครสอนได้"

func isClashRejectReason(r string) bool {
	return r == clashRejectReason || r == legacyClashRejectReason
}

// tryFinalize decides one pending request if every TA on it now has a
// timetable. No-op otherwise.
func (s *TARequestService) tryFinalize(ctx context.Context, reqID, termID uuid.UUID) error {
	return s.finalize(ctx, reqID, termID, false)
}

// finalize is tryFinalize with a switch for re-decided old requests
// (SplitLegacyRequests): onlyApprovals keeps every notice back unless the
// verdict is an approval, so a TA rejected once is not told again.
func (s *TARequestService) finalize(ctx context.Context, reqID, termID uuid.UUID, onlyApprovals bool) error {
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
		reason = clashRejectReason
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

	if onlyApprovals && verdict != "approved" {
		// Already told under the old request; keep it out of the digest too.
		if _, err := s.pool.Exec(ctx, `
			UPDATE ta_requests SET lecturer_notified_at = NOW(), lecturer_note = NULL
			WHERE id = $1`, reqID); err != nil {
			log.Printf("ta_request %s: mark lecturer told: %v", reqID, err)
		}
		return nil
	}
	s.notifyClashOutcome(ctx, reqID, notices)
	s.notifyDecision(ctx, reqID, verdict, reason)
	return nil
}

// notifyClashOutcome tells each affected TA exactly which sessions they lost
// and why, and gives the lecturer one combined summary. Without this the TA
// would discover the loss only when the work-log screen silently refused a
// session.
// restoreLineMarker is in every line restoreFromClash writes.
const restoreLineMarker = "ตารางเรียนไม่ชน"

func allRestoreLines(lines []string) bool {
	if len(lines) == 0 {
		return false
	}
	for _, l := range lines {
		if !strings.Contains(l, restoreLineMarker) {
			return false
		}
	}
	return true
}

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

	var summary, names []string
	for taID, lines := range notices {
		// A notice made only of restorations (restoreFromClash) is good news
		// and must not arrive under a "ทับซ้อน" headline.
		title := "ตารางเรียนของท่านทับซ้อนกับคาบสอน " + code
		body := fmt.Sprintf("ระบบพบว่าตารางเรียนของท่านทับซ้อนกับคาบสอนของรายวิชา %s ดังนี้\n%s", label, strings.Join(lines, "\n"))
		if allRestoreLines(lines) {
			title = "คืนสิทธิ์ผู้ช่วยสอน " + code
			body = fmt.Sprintf("ตารางเรียนของท่านไม่ทับซ้อนกับคาบสอนของรายวิชา %s แล้ว\n%s", label, strings.Join(lines, "\n"))
		}
		s.notify.Send(ctx, taID,
			// Code in the title: unread notices fold by (title, link), and a
			// clash on a second course would otherwise overwrite this one.
			title,
			body,
			// /ta/courses is not a route — the TA's course list is the home page.
			"/ta")
		names = append(names, s.taName(ctx, taID))
		summary = append(summary, fmt.Sprintf("%s %s", names[len(names)-1], strings.Join(lines, " ")))
	}
	// Until the lecturer has the submission's verdicts, the clash lines go
	// into that one notice instead of one of their own.
	if s.lecturerAwaitsDigest(ctx, reqID) {
		var note []string
		for _, lines := range notices {
			note = append(note, strings.Join(lines, " "))
		}
		s.holdLecturerNote(ctx, reqID, strings.Join(note, " "))
		return
	}
	// The TA is in the title too: a course holds one request per TA (0149),
	// and the code alone would fold one TA's notice into another's.
	who := " (" + strings.Join(names, ", ") + ")"
	lecTitle := "ผู้ช่วยสอนบางรายมีตารางเรียนทับซ้อนกับคาบสอน " + code + who
	lecBody := fmt.Sprintf("ระบบพบว่าผู้ช่วยสอนในคำขอรายวิชา %s มีตารางเรียนทับซ้อนกับคาบสอน ดังนี้\n%s", label, numberedLines(summary))
	if allRestoreLines(summary) {
		lecTitle = "คืนสิทธิ์ผู้ช่วยสอน " + code + who
		lecBody = fmt.Sprintf("ผู้ช่วยสอนในคำขอรายวิชา %s แก้ตารางเรียนแล้ว ระบบคืนสิทธิ์ให้ ดังนี้\n%s", label, numberedLines(summary))
	}
	s.notify.Send(ctx, lecturerID,
		lecTitle,
		lecBody,
		// Likewise: the lecturer's course list is their home page.
		"/lecturer")
}

// SweepPendingRequests finalises any request whose TAs have all filed their
// timetables since the last pass. The trigger on the TA's save is the fast
// path; this is the safety net that keeps a dropped call from stranding a
// request — and, with it, the course quota those assignments reserve.
func (s *TARequestService) SweepPendingRequests(ctx context.Context) (int, error) {
	// Old multi-TA requests first, so the TAs among them who pass are decided
	// in this same pass (see SplitLegacyRequests).
	if n, err := s.SplitLegacyRequests(ctx); err != nil {
		log.Printf("ta_request: split legacy requests: %v", err)
	} else if n > 0 {
		log.Printf("ta_request: split %d legacy request(s) into per-TA requests", n)
	}

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

	defer func() {
		// After the pass, so a submission whose last TA it just decided is
		// sent too; this is also where the 24-hour summaries go out.
		if n, err := s.SweepLecturerDigests(ctx, time.Now()); err != nil {
			log.Printf("ta_request: lecturer digests: %v", err)
		} else if n > 0 {
			log.Printf("ta_request: sent %d lecturer digest(s)", n)
		}
	}()

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
