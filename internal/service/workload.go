package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

// Structured-field limits — kept in sync with the frontend editor. Chosen so
// KKU-style 6–7 digit course codes, section ids like "01"/"A", and free-form
// Thai/English course names all fit comfortably.
const (
	classCourseCodeMax = 16
	classCourseNameMax = 120
	classSecNoMax      = 8
	classNoteMax       = 200
)

var (
	classCourseCodeRe = regexp.MustCompile(`^[A-Za-z0-9-]*$`)
	classSecNoRe      = regexp.MustCompile(`^[0-9]*$`)
)

type WorkloadService struct {
	pool *pgxpool.Pool
	aud  *audit.Auditor
	// requests lets a timetable save finish any TA request that was waiting on
	// it. Optional: nil in tests that only exercise timetable validation.
	requests *TARequestService
}

// TA class schedule (drag-drop UI).
//
// CourseCode / CourseName / Kind / SecNo are the structured display fields.
// CourseLabel is retained on the response as a human-readable summary
// ("<code> <name> (sec <no>) — บรรยาย/ปฏิบัติการ") for older callers; new
// callers should populate the structured fields directly. The DB column of
// the same name is legacy — new writes leave it NULL.
type ClassBlock struct {
	// ID is client-provided for local state matching. The DB always
	// generates a fresh uuid on insert, so the client can send an
	// arbitrary string ("b-1738…") without needing to mint a UUID.
	ID          string    `json:"id"`
	TermID      uuid.UUID `json:"term_id"`
	CourseCode  string    `json:"course_code"`
	CourseName  string    `json:"course_name"`
	CourseLabel string    `json:"course_label"`
	Kind        string    `json:"kind"`
	SecNo       string    `json:"sec_no"`
	DayOfWeek   int       `json:"day_of_week"`
	StartTime   string    `json:"start_time"`
	EndTime     string    `json:"end_time"`
	Note        string    `json:"note"`
	IsWBA       bool      `json:"is_wba"`
}

func classLabelOf(b ClassBlock) string {
	name := b.CourseName
	if name == "" {
		name = b.CourseLabel
	}
	parts := []string{}
	if b.CourseCode != "" {
		parts = append(parts, b.CourseCode)
	}
	if name != "" {
		parts = append(parts, name)
	}
	label := strings.Join(parts, " ")
	if b.SecNo != "" {
		if label == "" {
			label = "sec " + b.SecNo
		} else {
			label = label + " (sec " + b.SecNo + ")"
		}
	}
	switch b.Kind {
	case "lecture":
		if label != "" {
			label += " บรรยาย"
		}
	case "lab":
		if label != "" {
			label += " ปฏิบัติการ"
		}
	}
	return label
}

func (s *WorkloadService) ListClasses(ctx context.Context, userID, termID uuid.UUID) ([]ClassBlock, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, term_id,
		        COALESCE(course_code,''), COALESCE(course_name,''), COALESCE(kind,''), COALESCE(sec_no,''),
		        COALESCE(course_label,''),
		        day_of_week, start_time::text, end_time::text, COALESCE(note,''), is_wba
		 FROM ta_class_schedules WHERE user_id=$1 AND term_id=$2 ORDER BY day_of_week, start_time`,
		userID, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ClassBlock{}
	for rows.Next() {
		var b ClassBlock
		var rowID uuid.UUID
		var legacyLabel string
		if err := rows.Scan(&rowID, &b.TermID,
			&b.CourseCode, &b.CourseName, &b.Kind, &b.SecNo,
			&legacyLabel,
			&b.DayOfWeek, &b.StartTime, &b.EndTime, &b.Note, &b.IsWBA); err != nil {
			return nil, err
		}
		b.ID = rowID.String()
		// If no structured fields were ever set, surface the legacy free-form
		// label as the course name so the UI shows something to edit rather
		// than an empty row.
		if b.CourseName == "" && b.CourseCode == "" && legacyLabel != "" {
			b.CourseName = legacyLabel
		}
		b.CourseLabel = classLabelOf(b)
		out = append(out, b)
	}
	return out, rows.Err()
}

// ScheduleLockedReason returns a non-empty Thai reason when the TA may no
// longer edit their class schedule for this term, or "" when editing is open.
//
// The schedule is an input to the clash rules that decide which work logs are
// payable. Once staff have exported a payout document for this TA in this term,
// those decisions are on paper and in the finance office — letting the TA move
// a class afterwards would silently contradict a document that has already been
// sent. Past terms therefore stay readable but frozen.
//
// 'exported' and 'finance_sent' are the same two states submission_period.go
// already treats as locked for work logs; the schedule now follows the same
// line rather than inventing a second notion of "too late".
func (s *WorkloadService) ScheduleLockedReason(ctx context.Context, userID, termID uuid.UUID) (string, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM submission_period_status st
		JOIN submission_periods sp ON sp.id = st.submission_period_id
		WHERE st.ta_id = $1 AND sp.term_id = $2
		  AND st.status IN ('exported','finance_sent')`, userID, termID).Scan(&n); err != nil {
		return "", err
	}
	if n > 0 {
		return "เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายของภาคเรียนนี้แล้ว ดูได้แต่แก้ไขไม่ได้", nil
	}
	return "", nil
}

// assertNoClassRemovedAfterApproval keeps a TA from winning back pay by editing
// their own timetable after a request was decided. The decision trims every
// period that clashes with a class the TA attends; deleting that class
// afterwards made the period loggable again (and regenerate refilled it),
// while the assignment still read "trimmed" and the printed timetable form
// showed no clash for the lecturer to notice.
//
// The rule is one-way on purpose: once any assignment of this term is
// approved (active/trimmed), every existing non-WBA class block must still be
// covered by the new timetable. Adding classes — which can only cost the TA
// hours — stays self-service; removing or shortening one needs staff.
func assertNoClassRemovedAfterApproval(ctx context.Context, tx pgx.Tx, userID, termID uuid.UUID, blocks []ClassBlock) error {
	var approved bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM ta_request_assignments a
		    JOIN ta_requests r      ON r.id = a.request_id AND r.status = 'approved'
		    JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		   WHERE a.ta_id = $1 AND tc.term_id = $2 AND a.state IN ('active','trimmed'))`,
		userID, termID).Scan(&approved); err != nil {
		return err
	}
	if !approved {
		return nil
	}
	rows, err := tx.Query(ctx, `
		SELECT day_of_week, to_char(start_time,'HH24:MI'), to_char(end_time,'HH24:MI'),
		       COALESCE(NULLIF(course_code,''), course_label, '')
		  FROM ta_class_schedules
		 WHERE user_id = $1 AND term_id = $2 AND NOT is_wba`, userID, termID)
	if err != nil {
		return err
	}
	type span struct{ day, start, end int }
	var old []span
	var names []string
	for rows.Next() {
		var d int
		var st, en, name string
		if err := rows.Scan(&d, &st, &en, &name); err != nil {
			rows.Close()
			return err
		}
		sm, _ := parseHM(st)
		em, _ := parseHM(en)
		old = append(old, span{d, sm, em})
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i, o := range old {
		if o.end <= o.start {
			continue // malformed legacy row; nothing to protect
		}
		// Minute-level coverage of [start, end) by the new blocks of that day.
		covered := make([]bool, o.end-o.start)
		for _, b := range blocks {
			if b.IsWBA || b.DayOfWeek != o.day {
				continue
			}
			bs, ok1 := parseHM(b.StartTime)
			be, ok2 := parseHM(b.EndTime)
			if !ok1 || !ok2 {
				continue
			}
			for m := max(bs, o.start); m < min(be, o.end); m++ {
				covered[m-o.start] = true
			}
		}
		for _, c := range covered {
			if !c {
				return Invalid(fmt.Sprintf(
					"มีคำขอ TA ที่อนุมัติแล้วในภาคเรียนนี้ จึงลบหรือลดเวลาคาบเรียนเดิม (%s %s–%s) เองไม่ได้ เพิ่มคาบใหม่ได้ตามปกติ หากตารางเรียนเปลี่ยนจริง กรุณาติดต่อเจ้าหน้าที่",
					names[i], hmString(o.start), hmString(o.end)))
			}
		}
	}
	return nil
}

func hmString(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

// maxClassBlocksPerTerm bounds the self-service timetable a TA may submit.
const maxClassBlocksPerTerm = 200

// ReplaceClasses swaps the whole schedule for a term — the TA's own save.
func (s *WorkloadService) ReplaceClasses(ctx context.Context, userID, termID uuid.UUID, blocks []ClassBlock) error {
	return s.replaceClasses(ctx, userID, userID, termID, blocks)
}

// ReplaceClassesForTA is staff correcting a TA's timetable — the path the
// TA-facing lock message points at when a real class was dropped or moved
// after a request was approved. Staff may remove or shorten blocks; the save
// is audited under the staff member's id, and requests are re-evaluated the
// same way as on a TA save.
func (s *WorkloadService) ReplaceClassesForTA(ctx context.Context, actor, taID, termID uuid.UUID, blocks []ClassBlock) error {
	var isTA bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM user_roles WHERE user_id = $1 AND role = 'ta')`, taID).Scan(&isTA); err != nil {
		return err
	}
	if !isTA {
		return Invalid("บัญชีนี้ไม่ใช่ผู้ช่วยสอน")
	}
	return s.replaceClasses(ctx, actor, taID, termID, blocks)
}

func (s *WorkloadService) replaceClasses(ctx context.Context, actor, userID, termID uuid.UUID, blocks []ClassBlock) error {
	// A term's real timetable is a couple of dozen blocks. The cap is here
	// because everything downstream scales with this count and none of it is
	// bounded on its own: the insert below is one round trip per row inside a
	// held transaction, and the timetable PDF lays blocks out by comparing each
	// against those already placed, which is quadratic when they all overlap.
	// Checked before any query so an oversized payload costs nothing.
	if len(blocks) > maxClassBlocksPerTerm {
		return Invalid(fmt.Sprintf("ตารางเรียนมีได้ไม่เกิน %d ช่วงต่อภาคการศึกษา", maxClassBlocksPerTerm))
	}
	// Checked server-side, not just hidden in the UI: the client can be stale
	// by a whole term, and an export that lands between page load and save
	// must still win.
	if reason, err := s.ScheduleLockedReason(ctx, userID, termID); err != nil {
		return err
	} else if reason != "" {
		return Invalid(reason)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Serialise saves of one TA's term timetable so the before-image recorded
	// below is exactly what this save replaced.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 7))`,
		"ta_class_schedules/"+userID.String()+"/"+termID.String()); err != nil {
		return err
	}
	before, err := classScheduleSnapshot(ctx, tx, userID, termID)
	if err != nil {
		return err
	}
	if actor == userID {
		if err := assertNoClassRemovedAfterApproval(ctx, tx, userID, termID, blocks); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM ta_class_schedules WHERE user_id=$1 AND term_id=$2`, userID, termID); err != nil {
		return err
	}
	// RULE C5 (WBA / year-4 / grad): WBA rows are excluded from conflict checks
	// and therefore must not be forgeable. Enforce that (a) at most one is_wba
	// row is submitted, and (b) the acting user is either a year-4-or-above
	// undergraduate OR a graduate student (master/phd — they may genuinely have
	// no class schedule of their own). study_year is authoritative for
	// undergrads (migration 0013) and is not required for grad students.
	wbaCount := 0
	for _, b := range blocks {
		if b.IsWBA {
			wbaCount++
		}
	}
	if wbaCount > 1 {
		return Invalid("มีแถว WBA ได้เพียงรายการเดียว")
	}
	if wbaCount > 0 {
		var studyLevel string
		var studyYear *int
		if err := tx.QueryRow(ctx,
			`SELECT study_level::text, study_year FROM users WHERE id=$1`, userID).Scan(&studyLevel, &studyYear); err != nil {
			return err
		}
		switch studyLevel {
		case "master", "phd":
			// No year requirement for graduate students.
		case "undergrad":
			if studyYear == nil {
				return Invalid("ยังไม่ได้บันทึกชั้นปีของคุณในระบบ กรุณาติดต่อเจ้าหน้าที่เพื่อใช้โหมด WBA")
			}
			if *studyYear < 4 {
				return Invalid("เฉพาะนักศึกษาชั้นปีที่ 4 ขึ้นไปเท่านั้นที่ใช้โหมด WBA ได้")
			}
		default:
			return Invalid("เฉพาะนักศึกษาระดับปริญญาตรีชั้นปีที่ 4 ขึ้นไป หรือนักศึกษาระดับบัณฑิตศึกษาเท่านั้นที่ใช้โหมดนี้ได้")
		}
	}
	for _, b := range blocks {
		if b.DayOfWeek < 0 || b.DayOfWeek > 6 {
			return Invalid("วันในสัปดาห์ของคาบเรียนไม่ถูกต้อง")
		}
		// BUG B12: times must be compared as minutes-from-midnight, not as raw
		// strings ("9:00" > "10:00" lexicographically). Validating here also
		// stops malformed strings from reaching the ::time cast (a 500). WBA
		// rows use the 00:00–00:00 sentinel and skip time validation entirely.
		if !b.IsWBA {
			startMin, okS := parseHM(b.StartTime)
			endMin, okE := parseHM(b.EndTime)
			if !okS || !okE {
				return Invalid("รูปแบบเวลาไม่ถูกต้อง (HH:MM)")
			}
			// M2: parseHM already bounds each value to a valid 00:00–23:59
			// (0..1439 min) range; reject anything where start is not strictly
			// before end.
			if startMin >= endMin {
				return Invalid("เวลาสิ้นสุดของคาบเรียนต้องมากกว่าเวลาเริ่ม")
			}
		}
		if b.Kind != "" && b.Kind != "lecture" && b.Kind != "lab" {
			return Invalid("ประเภทคาบเรียนต้องเป็นบรรยายหรือปฏิบัติการ")
		}
		// Legacy callers that only send course_label are still accepted:
		// treat the label as course_name so it isn't silently dropped.
		if b.CourseName == "" && b.CourseCode == "" && b.CourseLabel != "" {
			b.CourseName = b.CourseLabel
		}
		// Normalize + validate structured fields. WBA rows carry a fixed
		// system-generated label so they bypass the charset check but still
		// respect the length ceilings.
		b.CourseCode = strings.TrimSpace(b.CourseCode)
		b.CourseName = strings.TrimSpace(b.CourseName)
		b.SecNo = strings.TrimSpace(b.SecNo)
		b.Note = strings.TrimSpace(b.Note)
		if !b.IsWBA {
			if b.CourseCode != "" && !classCourseCodeRe.MatchString(b.CourseCode) {
				return errors.New("รหัสวิชาใช้ได้เฉพาะตัวอักษร A–Z, ตัวเลข และเครื่องหมาย -")
			}
			if b.SecNo != "" && !classSecNoRe.MatchString(b.SecNo) {
				return errors.New("Section ใช้ได้เฉพาะตัวเลข 0–9")
			}
		}
		if utf8.RuneCountInString(b.CourseCode) > classCourseCodeMax {
			return errors.New("รหัสวิชายาวเกินกำหนด")
		}
		if utf8.RuneCountInString(b.CourseName) > classCourseNameMax {
			return errors.New("ชื่อวิชายาวเกินกำหนด")
		}
		if utf8.RuneCountInString(b.SecNo) > classSecNoMax {
			return errors.New("section ยาวเกินกำหนด")
		}
		if utf8.RuneCountInString(b.Note) > classNoteMax {
			return errors.New("หมายเหตุยาวเกินกำหนด")
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO ta_class_schedules
			 (id, user_id, term_id, course_code, course_name, kind, sec_no, day_of_week, start_time, end_time, note, is_wba)
			 VALUES ($1,$2,$3, NULLIF($4,''), NULLIF($5,''), NULLIF($6,''), NULLIF($7,''), $8,$9::time,$10::time,$11,$12)`,
			uuid.New(), userID, termID,
			b.CourseCode, b.CourseName, b.Kind, b.SecNo,
			b.DayOfWeek, b.StartTime, b.EndTime, b.Note, b.IsWBA); err != nil {
			return err
		}
	}
	// The timetable decides which worked hours are payable (the own-class clash
	// rule), and a TA can edit it freely until the term is exported — so removing
	// a class, logging hours in its slot and putting the class back used to leave
	// no trace at all. Recorded on the same transaction as the save.
	after, err := classScheduleSnapshot(ctx, tx, userID, termID)
	if err != nil {
		return err
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{
		ActorID: &actor, Action: "ta_class_schedule.replace",
		Entity: "ta_class_schedule", EntityID: termID.String(),
		Note:   "ta=" + userID.String(),
		Before: map[string]any{"blocks": before},
		After:  map[string]any{"blocks": after},
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	// The timetable this TA just saved may be the last one a pending TA request
	// was waiting for. Finish those requests now so the lecturer sees a verdict
	// immediately rather than at the next sweep — and so any session that
	// clashes is dropped (and its quota released) before the TA can try to log
	// time against it.
	//
	// Deliberately not inside the transaction above and deliberately not fatal:
	// saving a timetable is the TA's action and must succeed on its own terms.
	// SweepPendingRequests re-runs anything missed here.
	if s.requests != nil {
		if err := s.requests.ReevaluateForTA(ctx, userID, termID); err != nil {
			log.Printf("worklist: reevaluate requests for TA %s: %v", userID, err)
		}
	}
	return nil
}

// classScheduleSnapshot is a TA's whole timetable for one term as JSON, for the
// audit trail. Whole rows minus identifiers, so a column added later is recorded
// without anyone remembering to list it here.
func classScheduleSnapshot(ctx context.Context, tx pgx.Tx, userID, termID uuid.UUID) (json.RawMessage, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(jsonb_agg(
		         to_jsonb(t) - 'id' - 'user_id' - 'term_id' - 'created_at' - 'updated_at'
		         ORDER BY t.day_of_week, t.start_time), '[]'::jsonb)
		FROM ta_class_schedules t
		WHERE t.user_id = $1 AND t.term_id = $2`, userID, termID).Scan(&raw)
	return json.RawMessage(raw), err
}
