package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/timeutil"
)

// worklog_conflict.go enforces the rule the 24/07/2026 meeting put at the
// centre of the TA workflow: a TA must attend their own class, not teach.
//
// Until now `ta_class_schedules` was never consulted when a work log was
// written. The table existed, the request form warned about clashes, and the
// schedule screen drew them as "busy" blocks — but Upsert would happily accept
// an entry for an hour the TA was provably sitting in a lecture of their own,
// and those hours flowed into the payout.
//
// The rule is deliberately blunt: the WHOLE session is refused, never trimmed
// to the surviving minutes. A TA who must leave halfway through a lab cannot
// supervise that lab, so paying for the first half would be wrong.
//
// Scope: this is the per-ENTRY gate. The per-SECTION verdict (which sections a
// TA keeps at all) is decided once at request time — see ta_request_decide.go.
// Both are needed: the section verdict is structural and weekly, while a
// makeup class moves to a different date and time and can clash even when the
// original slot did not.

// ownClassBlock is one weekly slot from the TA's own timetable, in
// minutes-from-midnight so comparisons never touch string ordering.
type ownClassBlock struct {
	Label    string // course_label as the TA typed it; may be empty
	Day      int    // 0=Sunday … 6=Saturday, matching time.Weekday
	StartMin int
	EndMin   int
}

// describe renders the block for an error message. The label is user-supplied
// and may be blank, so fall back to the time alone rather than printing a
// dangling prefix.
func (b ownClassBlock) describe() string {
	when := fmt.Sprintf("%s %s–%s", thaiDayNames[b.Day], hhmm(b.StartMin), hhmm(b.EndMin))
	if b.Label == "" {
		return when
	}
	return fmt.Sprintf("%s (%s)", b.Label, when)
}

func hhmm(min int) string { return fmt.Sprintf("%02d:%02d", min/60, min%60) }

// findOwnClassClash returns the first block overlapping [startMin, endMin) on
// the given weekday, or nil. Touching edges do not overlap: a class ending at
// 12:00 leaves 12:00 onward free.
//
// Pure by design — the interesting cases (edge-touching, containment, partial
// overlap on either side) are worth testing without a database.
func findOwnClassClash(blocks []ownClassBlock, weekday, startMin, endMin int) *ownClassBlock {
	for i := range blocks {
		b := blocks[i]
		if b.Day != weekday {
			continue
		}
		if b.StartMin < endMin && startMin < b.EndMin {
			return &blocks[i]
		}
	}
	return nil
}

// loadOwnClassBlocks reads the TA's timetable for the term.
//
// WBA rows are excluded (rule C5): a year-4 work-based-learning row is a
// sentinel spanning no real class time, and counting it would block the whole
// term. Rows with malformed times are skipped rather than failing the write —
// a bad row in the TA's own timetable must not make their work log unsavable.
func loadOwnClassBlocks(ctx context.Context, pool *pgxpool.Pool, taID, termID uuid.UUID) ([]ownClassBlock, error) {
	rows, err := pool.Query(ctx, `
		SELECT COALESCE(course_label, ''), day_of_week,
		       TO_CHAR(start_time, 'HH24:MI'), TO_CHAR(end_time, 'HH24:MI')
		FROM ta_class_schedules
		WHERE user_id = $1 AND term_id = $2 AND NOT is_wba
		ORDER BY day_of_week, start_time`, taID, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ownClassBlock
	for rows.Next() {
		var b ownClassBlock
		var start, end string
		if err := rows.Scan(&b.Label, &b.Day, &start, &end); err != nil {
			return nil, err
		}
		sm, ok1 := parseHM(start)
		em, ok2 := parseHM(end)
		if !ok1 || !ok2 || sm >= em {
			continue
		}
		b.StartMin, b.EndMin = sm, em
		out = append(out, b)
	}
	return out, rows.Err()
}

// courseTermID resolves the term a teaching course belongs to. The TA's
// timetable is stored per term, so every conflict lookup needs it.
func courseTermID(ctx context.Context, pool *pgxpool.Pool, tcID uuid.UUID) (uuid.UUID, error) {
	var termID uuid.UUID
	err := pool.QueryRow(ctx,
		`SELECT term_id FROM teaching_courses WHERE id = $1`, tcID).Scan(&termID)
	return termID, err
}

// clashBlockingKind reports whether a period of this kind may NOT overlap the
// TA's own timetable.
//
// LECTURE duty is the exemption, added 31/07/2026. Taking attendance or handing
// out sheets does not require the TA to be in the lecture room for the hour, so a
// TA whose own class sits on a lecture slot can still do the job — and blocking
// it ruled those TAs out of the course entirely.
//
// Everything else keeps its previous behaviour. A lab puts the TA in a room with
// students at a fixed hour and cannot share a slot with anything; grading and
// other work are left blocking because the change asked for was specifically
// about lecture periods, and widening it would quietly authorise hours nobody
// decided to authorise.
//
// `other` follows its parent kind: admin work attached to a lecture is exempt
// with it, admin work attached to a lab is not.
func clashBlockingKind(activity string, parentKind *string) bool {
	if activity == "lecture" {
		return false
	}
	if activity == "other" && parentKind != nil && *parentKind == "lecture" {
		return false
	}
	return true
}

// BlockingSessionSQL is the SAME rule as clashBlockingKind, for the queries that
// ask it of `section_schedules` rows instead of a single work log. Every SQL site
// that decides whether a teaching session may overlap the TA's own timetable must
// call this rather than write the predicate out.
//
// It is a function for one reason: on 31/07/2026 the lecture exemption was added
// to checkOwnClassConflict alone, and sectionClash — which is what Create and the
// deferred re-decide actually run — kept counting lecture overlaps. Both spellings
// existed, both looked deliberate, and the pair of tests covering them passed
// together while contradicting each other, so nothing failed. The exemption was
// therefore dead in the only paths a lecturer ever reaches.
//
// Discovered against a real timetable (จิรายุ, CP351203 ภาคต้น 2569): two of his
// three courses have a LECTURE period sitting on one of his own classes, and both
// requests would have been refused outright.
func BlockingSessionSQL(ssAlias string) string {
	return ssAlias + ".kind = 'lab'"
}

// enforceNoOwnClassConflict rejects a work log whose hours collide with the
// TA's own timetable.
//
// The message names the clashing course and slot, because "you cannot log this"
// with no reason is exactly the dead end the meeting asked to remove — the TA
// should be able to see whether the fix is to move the entry or to correct a
// stale row in their own timetable.
func (s *WorkLogService) enforceNoOwnClassConflict(ctx context.Context, ac *assignmentContext, w WorkLog) error {
	termID, err := courseTermID(ctx, s.pool, ac.TeachingCourseID)
	if err != nil {
		return err
	}
	// Lecture-side work may overlap the TA's own class; only lab work may not.
	// Checked before the lookup so the common case costs nothing. Keeping this in
	// step with checkOwnClassConflict on the request side matters: a TA who can be
	// REQUESTED for a clashing lecture must also be able to LOG those hours, or
	// they are assigned to work the system then refuses to record.
	if !clashBlockingKind(w.Activity, w.ParentKind) {
		return nil
	}
	blocks, err := loadOwnClassBlocks(ctx, s.pool, ac.TAID, termID)
	if err != nil {
		return err
	}
	if len(blocks) == 0 {
		return nil
	}
	d, err := timeutil.ParseDate(w.WorkDate)
	if err != nil {
		// validateWorkLogEntry already rejects malformed dates; reaching here
		// means a caller skipped it, and guessing a weekday would be worse
		// than letting the other gates handle the row.
		return nil
	}
	sm, ok1 := parseHM(w.StartTime)
	em, ok2 := parseHM(w.EndTime)
	if !ok1 || !ok2 {
		return nil
	}
	clash := findOwnClassClash(blocks, int(d.Weekday()), sm, em)
	if clash == nil {
		return nil
	}
	return Invalid(fmt.Sprintf(
		"ลงเวลาช่วงนี้ไม่ได้ — ตรงกับตารางเรียนของคุณ: %s "+
			"(ตารางเรียนของตัวเองมาก่อนเสมอ ถ้าตารางเรียนนี้ไม่ถูกต้อง ให้แก้ที่หน้า “ตารางเรียนของฉัน”)",
		clash.describe()))
}
