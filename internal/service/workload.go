package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
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

// oldClassRow is one row of the timetable a save is about to replace, with the
// metadata the save must carry forward (see migration 0132).
type oldClassRow struct {
	ID                 uuid.UUID
	Day, Start, End    int
	Name               string
	IsWBA              bool
	CreatedAt          time.Time
	AddedAfterApproval bool
	Protected          bool // existed when an approved request was decided
}

// termApproval reports whether the TA holds an approved (active/trimmed)
// assignment this term, and the latest time such a request was decided. A
// request decided with no timestamp counts as "always": the protective side.
func termApproval(ctx context.Context, tx pgx.Tx, userID, termID uuid.UUID) (bool, time.Time, error) {
	var approved bool
	var latest *time.Time
	err := tx.QueryRow(ctx, `
		SELECT COUNT(*) > 0,
		       MAX(COALESCE(r.decided_at, 'infinity'::timestamptz))
		  FROM ta_request_assignments a
		  JOIN ta_requests r       ON r.id = a.request_id AND r.status = 'approved'
		  JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		 WHERE a.ta_id = $1 AND tc.term_id = $2 AND a.state IN ('active','trimmed')`,
		userID, termID).Scan(&approved, &latest)
	if err != nil {
		return false, time.Time{}, err
	}
	if latest == nil {
		return approved, time.Time{}, nil
	}
	return approved, *latest, nil
}

// loadOldClassRows reads the timetable about to be replaced. A row is
// protected unless it was added after approval AND no approval has been
// decided since — a class a later request was decided against is part of
// that decision exactly like one that was there from the start.
func loadOldClassRows(ctx context.Context, tx pgx.Tx, userID, termID uuid.UUID, approved bool, latest time.Time) ([]oldClassRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, day_of_week, to_char(start_time,'HH24:MI'), to_char(end_time,'HH24:MI'),
		       COALESCE(NULLIF(course_code,''), course_label, ''), is_wba,
		       created_at, added_after_approval
		  FROM ta_class_schedules
		 WHERE user_id = $1 AND term_id = $2`, userID, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []oldClassRow
	for rows.Next() {
		var r oldClassRow
		var st, en string
		if err := rows.Scan(&r.ID, &r.Day, &st, &en, &r.Name, &r.IsWBA, &r.CreatedAt, &r.AddedAfterApproval); err != nil {
			return nil, err
		}
		r.Start, _ = parseHM(st)
		r.End, _ = parseHM(en)
		r.Protected = approved && (!r.AddedAfterApproval || !r.CreatedAt.After(latest))
		out = append(out, r)
	}
	return out, rows.Err()
}

// assertNoClassRemovedAfterApproval keeps a TA from winning back pay by editing
// their own timetable after a request was decided. The decision trims every
// period that clashes with a class the TA attends; deleting that class
// afterwards made the period loggable again (and regenerate refilled it),
// while the assignment still read "trimmed" and the printed timetable form
// showed no clash for the lecturer to notice.
//
// The rule is one-way on purpose: every PROTECTED class block (one that
// existed when an approved request was decided) must still be covered by the
// new timetable. Adding classes — which can only cost the TA hours — stays
// self-service, and since 01/10/2026 so does taking back a class that was
// added AFTER every approval: it was never part of any decision, and refusing
// it left a TA who mistyped a class stuck until staff stepped in.
func assertNoClassRemovedAfterApproval(old []oldClassRow, blocks []ClassBlock) error {
	for _, o := range old {
		if !o.Protected || o.IsWBA || o.End <= o.Start {
			continue // unprotected, WBA, or malformed legacy row: nothing to keep
		}
		// Minute-level coverage of [start, end) by the new blocks of that day.
		covered := make([]bool, o.End-o.Start)
		for _, b := range blocks {
			if b.IsWBA || b.DayOfWeek != o.Day {
				continue
			}
			bs, ok1 := parseHM(b.StartTime)
			be, ok2 := parseHM(b.EndTime)
			if !ok1 || !ok2 {
				continue
			}
			for m := max(bs, o.Start); m < min(be, o.End); m++ {
				covered[m-o.Start] = true
			}
		}
		for _, c := range covered {
			if !c {
				return Invalid(fmt.Sprintf(
					"มีคำขอ TA ที่อนุมัติแล้วในภาคเรียนนี้ จึงลบหรือลดเวลาคาบเรียนเดิม (%s %s–%s) เองไม่ได้ เพิ่มคาบใหม่ได้ตามปกติ และคาบที่เพิ่มหลังการอนุมัติลบเองได้ หากตารางเรียนเดิมเปลี่ยนจริง กรุณาติดต่อเจ้าหน้าที่",
					o.Name, hmString(o.Start), hmString(o.End)))
			}
		}
	}
	return nil
}

// assertNoOverlappingOwnClasses refuses a timetable in which the TA attends two
// different classes at once (Fri 15:00–16:00 and Fri 15:30–17:00). Every clash
// rule downstream reads this table as the truth about where the TA is; an
// impossible timetable silently trims teaching hours on both sides of a typo.
//
// Two blocks of the SAME course may overlap: sections of one course that meet
// together are entered as separate blocks, and the timetable page has always
// allowed that on purpose.
func assertNoOverlappingOwnClasses(blocks []ClassBlock) error {
	type span struct {
		b          ClassBlock
		start, end int
	}
	byDay := map[int][]span{}
	for _, b := range blocks {
		if b.IsWBA {
			continue
		}
		sm, ok1 := parseHM(b.StartTime)
		em, ok2 := parseHM(b.EndTime)
		if !ok1 || !ok2 || sm >= em {
			continue // the per-block validation reports these
		}
		byDay[b.DayOfWeek] = append(byDay[b.DayOfWeek], span{b, sm, em})
	}
	sameCourse := func(a, b ClassBlock) bool {
		ca, cb := strings.TrimSpace(a.CourseCode), strings.TrimSpace(b.CourseCode)
		if ca != "" || cb != "" {
			return strings.EqualFold(ca, cb)
		}
		na, nb := strings.TrimSpace(a.CourseName+a.CourseLabel), strings.TrimSpace(b.CourseName+b.CourseLabel)
		return na != "" && na == nb
	}
	for day, list := range byDay {
		for i := 0; i < len(list); i++ {
			for j := i + 1; j < len(list); j++ {
				x, y := list[i], list[j]
				if x.start < y.end && y.start < x.end && !sameCourse(x.b, y.b) {
					name := func(b ClassBlock) string {
						if l := strings.TrimSpace(classLabelOf(b)); l != "" {
							return l
						}
						return "คาบเรียน"
					}
					return Invalid(fmt.Sprintf(
						"คาบเรียน %s (%s %s–%s) และ %s (%s %s–%s) เวลาซ้อนกัน เรียนสองวิชาพร้อมกันไม่ได้ กรุณาแก้เวลาให้ตรงกับตารางเรียนจริง",
						name(x.b), dayTH(day), hmString(x.start), hmString(x.end),
						name(y.b), dayTH(day), hmString(y.start), hmString(y.end)))
				}
			}
		}
	}
	return nil
}

// ClassSaveNeedsConfirm is the status a TA's own timetable save returns when it
// would cost an APPROVED assignment teaching sessions. The client shows the
// message and resends with confirm=1. 428 Precondition Required rather than
// 409 so the page can tell "please confirm" from a genuine conflict.
const ClassSaveNeedsConfirm = 428

// approvedSessionImpact lists, in Thai, every teaching session of the TA's
// approved assignments this term that clashes with the NEW timetable but did
// not clash with the old one — the sessions this save would silently trim.
// Empty when the save costs nothing.
func approvedSessionImpact(ctx context.Context, tx pgx.Tx, userID, termID uuid.UUID, old []oldClassRow, blocks []ClassBlock) ([]string, error) {
	var oldBlocks, newBlocks []ownClassBlock
	for _, o := range old {
		if !o.IsWBA && o.Start < o.End {
			oldBlocks = append(oldBlocks, ownClassBlock{Label: o.Name, Day: o.Day, StartMin: o.Start, EndMin: o.End})
		}
	}
	for _, b := range blocks {
		if b.IsWBA {
			continue
		}
		sm, ok1 := parseHM(b.StartTime)
		em, ok2 := parseHM(b.EndTime)
		if ok1 && ok2 && sm < em {
			newBlocks = append(newBlocks, ownClassBlock{Label: strings.TrimSpace(classLabelOf(b)), Day: b.DayOfWeek, StartMin: sm, EndMin: em})
		}
	}
	if len(newBlocks) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT tc.code, sec.sec_no, ss.kind, ss.day_of_week,
		       to_char(ss.start_time,'HH24:MI'), to_char(ss.end_time,'HH24:MI')
		  FROM ta_request_assignments a
		  JOIN ta_requests r       ON r.id = a.request_id AND r.status = 'approved'
		  JOIN sections sec        ON sec.id = a.section_id
		  JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		  JOIN section_schedules ss ON ss.section_id = a.section_id
		 WHERE a.ta_id = $1 AND tc.term_id = $2 AND a.state <> 'dropped'
		 ORDER BY tc.code, sec.sec_no, ss.day_of_week, ss.start_time`, userID, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var code, secNo, kind, st, en string
		var day int
		if err := rows.Scan(&code, &secNo, &kind, &day, &st, &en); err != nil {
			return nil, err
		}
		sm, ok1 := parseHM(st)
		em, ok2 := parseHM(en)
		if !ok1 || !ok2 {
			continue
		}
		hit := findOwnClassClash(newBlocks, day, sm, em)
		if hit == nil || findOwnClassClash(oldBlocks, day, sm, em) != nil {
			continue
		}
		lost := "คาบสอน"
		if k := kindTH(kind); k != "" {
			lost = "คาบ" + k
		}
		lines = append(lines, fmt.Sprintf("%s Sec %s %s %s %s–%s ตรงกับ %s",
			code, secNo, lost, dayTH(day), st, en, hit.describe()))
	}
	return lines, rows.Err()
}

// inheritClassMeta decides created_at / added_after_approval for a block being
// (re)inserted. The timetable is replaced wholesale on every save, so without
// this every autosave would make every class look brand new — and a protected
// class could be laundered into a removable one by one save that keeps it and
// a second that deletes it.
//
// A block inherits from every old row it touches: the same row id (an edit
// that moved it), or any old row on that day whose time it overlaps. If ANY
// of those was protected the block is protected (flag false, earliest
// created_at); otherwise it keeps the earliest created_at and stays
// unprotected. A block touching nothing is new: added after approval exactly
// when the TA already holds an approved assignment. nil created_at = NOW().
func inheritClassMeta(old []oldClassRow, b ClassBlock, approved bool) (*time.Time, bool) {
	sm, ok1 := parseHM(b.StartTime)
	em, ok2 := parseHM(b.EndTime)
	id, idErr := uuid.Parse(b.ID)
	var earliest *time.Time
	touched, anyProtected, anyOriginal := false, false, false
	for i := range old {
		o := &old[i]
		match := idErr == nil && o.ID == id
		if !match && o.IsWBA == b.IsWBA {
			if b.IsWBA {
				match = true
			} else if ok1 && ok2 && o.Day == b.DayOfWeek && o.Start < em && sm < o.End {
				match = true
			}
		}
		if !match {
			continue
		}
		touched = true
		if o.Protected {
			anyProtected = true
		}
		if !o.AddedAfterApproval {
			anyOriginal = true
		}
		if earliest == nil || o.CreatedAt.Before(*earliest) {
			t := o.CreatedAt
			earliest = &t
		}
	}
	if !touched {
		return nil, approved
	}
	if anyProtected || anyOriginal {
		return earliest, false
	}
	return earliest, true
}

func hmString(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

// maxClassBlocksPerTerm bounds the self-service timetable a TA may submit.
const maxClassBlocksPerTerm = 200

// ReplaceClasses swaps the whole schedule for a term as the TA, with the
// impact already accepted — for programmatic callers (demo seeding, tests).
// A person saving from the timetable page goes through SaveOwnClasses, which
// asks first when the save would cost an approved assignment sessions.
func (s *WorkloadService) ReplaceClasses(ctx context.Context, userID, termID uuid.UUID, blocks []ClassBlock) error {
	return s.replaceClasses(ctx, userID, userID, termID, blocks, true)
}

// SaveOwnClasses is the TA's own save from the timetable page. Unless
// confirmed, a timetable that newly clashes with sessions of an APPROVED
// assignment is refused with status ClassSaveNeedsConfirm and a Thai list of
// what would be lost. It used to save silently: the re-check that follows a
// save trims those sessions (and can zero a declared duty), so a TA who typed
// a class on the wrong day lost approved teaching hours without being told.
func (s *WorkloadService) SaveOwnClasses(ctx context.Context, userID, termID uuid.UUID, blocks []ClassBlock, confirmed bool) error {
	return s.replaceClasses(ctx, userID, userID, termID, blocks, confirmed)
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
	return s.replaceClasses(ctx, actor, taID, termID, blocks, true)
}

func (s *WorkloadService) replaceClasses(ctx context.Context, actor, userID, termID uuid.UUID, blocks []ClassBlock, confirmed bool) error {
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
	// Applies to staff corrections too: an impossible timetable is wrong
	// whoever types it.
	if err := assertNoOverlappingOwnClasses(blocks); err != nil {
		return err
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
	approved, latestApproval, err := termApproval(ctx, tx, userID, termID)
	if err != nil {
		return err
	}
	old, err := loadOldClassRows(ctx, tx, userID, termID, approved, latestApproval)
	if err != nil {
		return err
	}
	if actor == userID {
		if err := assertNoClassRemovedAfterApproval(old, blocks); err != nil {
			return err
		}
		if !confirmed && approved {
			lines, err := approvedSessionImpact(ctx, tx, userID, termID, old, blocks)
			if err != nil {
				return err
			}
			if len(lines) > 0 {
				return &UserError{Status: ClassSaveNeedsConfirm, Msg: "ตารางเรียนใหม่ตรงกับคาบสอนที่ได้รับอนุมัติแล้ว " +
					"หากบันทึก คาบเหล่านี้จะลงเวลาไม่ได้ และชั่วโมงที่อาจารย์กำหนดอาจถูกตัดลง:\n- " +
					strings.Join(lines, "\n- ") +
					"\nหากเพิ่มคาบผิด ลบคาบที่เพิ่มหลังการอนุมัติออกเองได้ ระบบจะคืนสิทธิ์และชั่วโมงที่ถูกตัดให้อัตโนมัติ"}
			}
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
		var studentID *string
		var termYearBE int
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(u.study_level::text, ''), u.study_year, u.student_id,
			        COALESCE((SELECT academic_year FROM academic_terms WHERE id = $2), 0)
			   FROM users u WHERE u.id=$1`, userID, termID).Scan(&studyLevel, &studyYear, &studentID, &termYearBE); err != nil {
			return err
		}
		// The year comes from the student id against THIS term's academic
		// year, the same derivation the users screen shows — the stored
		// column is only a fallback for ids that do not parse.
		if studentID != nil {
			if yr := deriveStudyYear(*studentID, termYearBE); yr > 0 {
				studyYear = &yr
			}
		}
		switch studyLevel {
		case "master", "phd":
			// No year requirement for graduate students.
		case "undergrad":
			if studyYear == nil {
				return Invalid("ระบบยังคำนวณชั้นปีของคุณไม่ได้ กรุณากรอกรหัสนักศึกษาในแบบฟอร์มข้อมูลส่วนตัวก่อนใช้โหมด WBA")
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
		createdAt, addedAfter := inheritClassMeta(old, b, approved)
		if _, err := tx.Exec(ctx,
			`INSERT INTO ta_class_schedules
			 (id, user_id, term_id, course_code, course_name, kind, sec_no, day_of_week, start_time, end_time, note, is_wba,
			  created_at, added_after_approval)
			 VALUES ($1,$2,$3, NULLIF($4,''), NULLIF($5,''), NULLIF($6,''), NULLIF($7,''), $8,$9::time,$10::time,$11,$12,
			         COALESCE($13, NOW()), $14)`,
			uuid.New(), userID, termID,
			b.CourseCode, b.CourseName, b.Kind, b.SecNo,
			b.DayOfWeek, b.StartTime, b.EndTime, b.Note, b.IsWBA,
			createdAt, addedAfter); err != nil {
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
