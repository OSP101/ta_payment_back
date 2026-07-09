package service

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
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
			label += " — บรรยาย"
		}
	case "lab":
		if label != "" {
			label += " — ปฏิบัติการ"
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
	var out []ClassBlock
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
	return out, nil
}

// ReplaceClasses swaps the whole schedule for a term.
func (s *WorkloadService) ReplaceClasses(ctx context.Context, userID, termID uuid.UUID, blocks []ClassBlock) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`DELETE FROM ta_class_schedules WHERE user_id=$1 AND term_id=$2`, userID, termID); err != nil {
		return err
	}
	// RULE C5 (WBA / year-4): WBA rows are excluded from conflict checks and
	// therefore must not be forgeable. Enforce that (a) at most one is_wba row is
	// submitted, and (b) the acting user is a year-4-or-above undergraduate.
	// study_year is now authoritative (migration 0013); it must be set by staff.
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
		if studyLevel != "undergrad" {
			return Invalid("เฉพาะนักศึกษาระดับปริญญาตรีชั้นปีที่ 4 เท่านั้นที่ใช้โหมด WBA ได้")
		}
		if studyYear == nil {
			return Invalid("ยังไม่ได้บันทึกชั้นปีของคุณในระบบ กรุณาติดต่อเจ้าหน้าที่เพื่อใช้โหมด WBA")
		}
		if *studyYear < 4 {
			return Invalid("เฉพาะนักศึกษาชั้นปีที่ 4 ขึ้นไปเท่านั้นที่ใช้โหมด WBA ได้")
		}
	}
	for _, b := range blocks {
		if b.DayOfWeek < 0 || b.DayOfWeek > 6 {
			return errors.New("invalid day_of_week")
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
				return errors.New("เวลาสิ้นสุดของคาบเรียนต้องมากกว่าเวลาเริ่ม")
			}
		}
		if b.Kind != "" && b.Kind != "lecture" && b.Kind != "lab" {
			return errors.New("invalid kind")
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
	return tx.Commit(ctx)
}
