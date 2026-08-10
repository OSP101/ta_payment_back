package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/timeutil"
)

type TeachingCourse struct {
	ID     uuid.UUID `json:"id"`
	TermID uuid.UUID `json:"term_id"`
	// Course identity — denormalized per-term from the imported registrar file
	// (no central catalog). A course code is unique within a term.
	Code    string  `json:"code"`
	NameTH  string  `json:"name_th"`
	NameEN  *string `json:"name_en,omitempty"`
	Level   string  `json:"level"` // "undergrad" | "graduate"
	Credits int     `json:"credits"`
	// Credit hours — surfaced so the request-form UI can derive its default
	// reimburse_scope (Q&A rule 3): lecture-only → "lecture", lab-only → "lab",
	// both → "both" (user can still override).
	LectureHrs         int     `json:"lecture_hrs"`
	LabHrs             int     `json:"lab_hrs"`
	SelfHrs            int     `json:"self_hrs"`
	Department         *string `json:"department,omitempty"`
	StartsOn           *string `json:"starts_on,omitempty"`
	EndsOn             *string `json:"ends_on,omitempty"`
	NumStudents        int     `json:"num_students"` // aggregate (regular + special)
	NumStudentsRegular int     `json:"num_students_regular"`
	NumStudentsSpecial int     `json:"num_students_special"`
	// HasSpecial is true when the course has at least one special-track section
	// (i.e. it runs a special program). When false the "นศ. พิเศษ" count is not
	// applicable — the UI disables that input to prevent stray data entry.
	HasSpecial bool `json:"has_special"`
	// HasMissingSchedule is true when at least one section carries no class
	// schedule — typical of courses the registrar file marks WBA ("will be
	// arranged"). Such a course cannot accept a TA request until the lecturer
	// or staff fills the schedule in; both UIs surface a warning off this flag.
	HasMissingSchedule bool `json:"has_missing_schedule"`
	// UnresolvedMakeups counts class occurrences killed by a public holiday that
	// still have no makeup filed. Each one is a day the TA physically cannot log
	// (the generator skips it) = money the TA loses, so every lecturer surface
	// warns off this number. 0 = nothing to do.
	UnresolvedMakeups int `json:"unresolved_makeups"`
	// List-only aggregates for the staff course table: section counts per
	// track and a pre-joined lecturer name string (primary first).
	NumSectionsRegular int    `json:"num_sections_regular"`
	NumSectionsSpecial int    `json:"num_sections_special"`
	LecturerNames      string `json:"lecturer_names,omitempty"`
	// ExportedAt is set the first time staff builds the export zip for this
	// course. Once set, section list and per-section student counts are
	// frozen — the export file is considered the source of truth.
	ExportedAt *string   `json:"exported_at,omitempty"`
	Sections   []Section `json:"sections,omitempty"`
	Lecturers  []struct {
		ID        uuid.UUID `json:"id"`
		FirstName string    `json:"first_name"`
		LastName  string    `json:"last_name"`
		IsPrimary bool      `json:"is_primary"`
	} `json:"lecturers,omitempty"`
}

type Section struct {
	ID               uuid.UUID `json:"id"`
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	SecNo            string    `json:"sec_no"`
	Track            string    `json:"track"`
	Room             *string   `json:"room,omitempty"`
	NumStudents      int       `json:"num_students"`
	// Programme group served (CS/IT/GIS/AI/CY, OTHER = another faculty),
	// derived from the import file's ReservedFor; nil = not yet known.
	Curriculum *string `json:"curriculum,omitempty"`
	// Set when a lecturer filled in a missing timetable — they get one such
	// write per section and the UI uses this to explain why the row is now
	// read-only to them. See ReplaceSectionSchedules.
	ScheduleSetByLecturerAt *string           `json:"schedule_set_by_lecturer_at,omitempty"`
	Schedules               []SectionSchedule `json:"schedules,omitempty"`
	Exams                   []ExamSchedule    `json:"exams,omitempty"`
	Makeups                 []MakeupSchedule  `json:"makeups,omitempty"`
	Reviews                 []LectureReview   `json:"reviews,omitempty"`
}

type SectionSchedule struct {
	ID        uuid.UUID `json:"id"`
	Kind      string    `json:"kind"`
	DayOfWeek int       `json:"day_of_week"`
	StartTime string    `json:"start_time"`
	EndTime   string    `json:"end_time"`
	Room      *string   `json:"room,omitempty"`
}

type ExamSchedule struct {
	ID        uuid.UUID `json:"id"`
	Kind      string    `json:"kind"`
	ExamDate  string    `json:"exam_date"`
	StartTime *string   `json:"start_time,omitempty"`
	EndTime   *string   `json:"end_time,omitempty"`
	Room      *string   `json:"room,omitempty"`
}

type MakeupSchedule struct {
	ID           uuid.UUID `json:"id"`
	OriginalDate string    `json:"original_date"`
	MakeupDate   string    `json:"makeup_date"`
	// Kind is the period this makeup replaces: "lecture" or "lab". Required —
	// a section that teaches both on the cancelled day needs two independent
	// makeups, at different times, and a day-level makeup could express only one
	// of them while silently claiming to cover the other.
	Kind      string  `json:"kind"`
	StartTime *string `json:"start_time,omitempty"`
	EndTime   *string `json:"end_time,omitempty"`
	Note      *string `json:"note,omitempty"`
}

type LectureReview struct {
	ID         uuid.UUID `json:"id"`
	ReviewDate string    `json:"review_date"`
	StartTime  *string   `json:"start_time,omitempty"`
	EndTime    *string   `json:"end_time,omitempty"`
	Hours      float64   `json:"hours"`
	Note       *string   `json:"note,omitempty"`
}

type TeachingService struct {
	pool   *pgxpool.Pool
	aud    *audit.Auditor
	notify *NotifyService
	// fontDir carries the Sarabun TTFs the timetable-form PDF needs. Empty
	// means "no PDF" rather than a broken one — same convention as ExportService.
	fontDir string
}

// Create a teaching course with sections + schedules in one transaction.
// Course identity is supplied inline (no central catalog): the manual
// "add course" form and the Excel import both carry it.
type CreateTeachingCourseInput struct {
	TermID      uuid.UUID   `json:"term_id"`
	Code        string      `json:"code"`
	NameTH      string      `json:"name_th"`
	NameEN      *string     `json:"name_en,omitempty"`
	Level       string      `json:"level"`
	Credits     int         `json:"credits"`
	LectureHrs  int         `json:"lecture_hrs"`
	LabHrs      int         `json:"lab_hrs"`
	SelfHrs     int         `json:"self_hrs"`
	StartsOn    *string     `json:"starts_on,omitempty"`
	EndsOn      *string     `json:"ends_on,omitempty"`
	NumStudents int         `json:"num_students"`
	LecturerIDs []uuid.UUID `json:"lecturer_ids"`
	Sections    []struct {
		SecNo       string            `json:"sec_no"`
		Track       string            `json:"track"`
		Room        *string           `json:"room,omitempty"`
		NumStudents int               `json:"num_students"`
		Schedules   []SectionSchedule `json:"schedules,omitempty"`
		Exams       []ExamSchedule    `json:"exams,omitempty"`
	} `json:"sections"`
}

// Sanitize + validate the manually-typed course code. Accepted forms follow
// KKU numbering: legacy = 6 digits ("342233"); current = 2 uppercase letters
// + 6 digits ("CP353201", "SC363001"). Anything else (odd symbols, Thai
// characters, wrong length) is rejected outright so junk codes can't seed
// courses. All whitespace — including internal — is stripped first.
var courseCodeRe = regexp.MustCompile(`^(?:[A-Z]{2}[0-9]{6}|[0-9]{6})$`)

func (s *TeachingService) Create(ctx context.Context, actor uuid.UUID, in CreateTeachingCourseInput) (uuid.UUID, error) {
	// Staff-only, for the same reason the section roster is (see
	// errSectionsAreStaffOnly): a course carries a section list, and that list
	// belongs to the registrar file. There is no ownership check to fall back
	// on here — the course does not exist yet — so this is a plain role gate.
	priv, err := isPrivileged(ctx, s.pool, actor)
	if err != nil {
		return uuid.Nil, err
	}
	if !priv {
		return uuid.Nil, Forbidden("การเปิดรายวิชาต้องให้เจ้าหน้าที่ดำเนินการ รายวิชามาจากไฟล์ทะเบียน")
	}
	in.Code = strings.ToUpper(strings.Join(strings.Fields(in.Code), ""))
	in.NameTH = strings.TrimSpace(in.NameTH)
	if in.Code == "" || in.TermID == uuid.Nil {
		return uuid.Nil, ErrInvalidInput
	}
	if !courseCodeRe.MatchString(in.Code) {
		return uuid.Nil, Invalid("รูปแบบรหัสวิชาไม่ถูกต้อง ต้องเป็นตัวเลข 6 หลัก (เช่น 342233) หรือตัวอักษรพิมพ์ใหญ่ 2 ตัวตามด้วยตัวเลข 6 หลัก (เช่น CP353201)")
	}
	if in.NameTH == "" {
		// English-only policy: the display name falls back to the English name,
		// then the code, so the NOT NULL column is always satisfied.
		if in.NameEN != nil && strings.TrimSpace(*in.NameEN) != "" {
			in.NameTH = strings.TrimSpace(*in.NameEN)
		} else {
			in.NameTH = in.Code
		}
	}
	if in.Level == "" {
		in.Level = "undergrad"
	}
	if in.Level != "undergrad" && in.Level != "graduate" {
		return uuid.Nil, Invalid("ระดับวิชาต้องเป็นปริญญาตรีหรือบัณฑิตศึกษา")
	}
	// Credit hours come straight from the input now (no catalog lookup); they
	// gate which schedule kinds a section may carry.
	lecHrs, labHrs := in.LectureHrs, in.LabHrs
	for _, sec := range in.Sections {
		if err := validateSectionSchedules(sec.Schedules, lecHrs, labHrs); err != nil {
			return uuid.Nil, err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)
	id := uuid.New()
	_, err = tx.Exec(ctx,
		`INSERT INTO teaching_courses (
			id, term_id, code, name_th, name_en, level,
			credits, lecture_hrs, lab_hrs, self_hrs,
			starts_on, ends_on, num_students, created_by
		 ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::date,$12::date,$13,$14)`,
		id, in.TermID, in.Code, in.NameTH, in.NameEN, in.Level,
		in.Credits, in.LectureHrs, in.LabHrs, in.SelfHrs,
		nilStr(in.StartsOn), nilStr(in.EndsOn),
		in.NumStudents, actor)
	if err != nil {
		return uuid.Nil, err
	}
	// Auto-add the actor as primary lecturer only on the "lecturer opens their
	// own course" path. When a non-lecturer (staff/admin) opens a course they
	// MUST name the actual teaching lecturer(s) — otherwise the course would be
	// wrongly attributed to the staff account and every downstream lecturer
	// action (approvals, sign-off, notifications) would target the wrong person.
	lecturerIDs := in.LecturerIDs
	if len(lecturerIDs) == 0 {
		isLecturer, lerr := hasRole(ctx, s.pool, actor, "lecturer")
		if lerr != nil {
			return uuid.Nil, lerr
		}
		if !isLecturer {
			return uuid.Nil, Invalid("ต้องระบุอาจารย์ผู้สอนอย่างน้อย 1 คน")
		}
		lecturerIDs = []uuid.UUID{actor}
	}
	for i, lid := range lecturerIDs {
		primary := i == 0
		if _, err := tx.Exec(ctx,
			`INSERT INTO teaching_lecturers (teaching_course_id, lecturer_id, is_primary) VALUES ($1,$2,$3)
			 ON CONFLICT DO NOTHING`, id, lid, primary); err != nil {
			return uuid.Nil, err
		}
	}
	// schedule_set_by_lecturer_at stays NULL: only staff reach this method, so
	// a timetable typed in here is staff's, not a lecturer's one-shot write.
	var sumRegular, sumSpecial int
	for _, sec := range in.Sections {
		secID := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO sections (id, teaching_course_id, sec_no, track, room, num_students)
			 VALUES ($1,$2,$3,$4::section_track,$5,$6)`,
			secID, id, sec.SecNo, sec.Track, sec.Room, sec.NumStudents); err != nil {
			return uuid.Nil, err
		}
		if sec.Track == "special" {
			sumSpecial += sec.NumStudents
		} else {
			sumRegular += sec.NumStudents
		}
		for _, sch := range sec.Schedules {
			if _, err := tx.Exec(ctx,
				`INSERT INTO section_schedules (id, section_id, kind, day_of_week, start_time, end_time, room)
				 VALUES ($1,$2,$3,$4,$5::time,$6::time,$7)`,
				uuid.New(), secID, sch.Kind, sch.DayOfWeek, sch.StartTime, sch.EndTime, sch.Room); err != nil {
				return uuid.Nil, err
			}
		}
		for _, e := range sec.Exams {
			if _, err := tx.Exec(ctx,
				`INSERT INTO exam_schedules (id, section_id, kind, exam_date, start_time, end_time, room)
				 VALUES ($1,$2,$3::exam_kind,$4::date,$5,$6,$7)`,
				uuid.New(), secID, e.Kind, e.ExamDate, e.StartTime, e.EndTime, e.Room); err != nil {
				return uuid.Nil, err
			}
		}
	}
	// If any section-level counts were provided, they win over the aggregate
	// in the input body — sections are the source of truth going forward.
	if sumRegular+sumSpecial > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE teaching_courses SET num_students=$1, num_students_regular=$2, num_students_special=$3
			 WHERE id=$4`, sumRegular+sumSpecial, sumRegular, sumSpecial, id); err != nil {
			return uuid.Nil, err
		}
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "teaching_course.create", Entity: "teaching_course", EntityID: id.String(), After: in}); err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// Delete removes a teaching course ONLY when it carries no downstream records —
// i.e. it was opened by mistake. It refuses (with a clear reason) once the
// course has been exported, has any worklog / payout status, has assigned TAs,
// or has any TA request. This protects the payroll + government-document audit
// trail. A clean delete cascades sections/schedules/lecturers via FK. Staff/admin.
func (s *TeachingService) Delete(ctx context.Context, actor, id uuid.UUID) error {
	priv, err := isPrivileged(ctx, s.pool, actor)
	if err != nil {
		return err
	}
	if !priv {
		return ErrForbidden
	}
	var (
		found                                                                bool
		exported, hasWL, hasStatus, hasAssign, hasReq, hasCounts, hasHoliday bool
	)
	err = s.pool.QueryRow(ctx, `
		SELECT TRUE,
		       (tc.exported_at IS NOT NULL),
		       EXISTS(SELECT 1 FROM work_logs wl
		                JOIN ta_request_assignments a ON a.id = wl.assignment_id
		                JOIN sections s ON s.id = a.section_id
		               WHERE s.teaching_course_id = tc.id),
		       EXISTS(SELECT 1 FROM submission_period_status st WHERE st.teaching_course_id = tc.id),
		       EXISTS(SELECT 1 FROM ta_request_assignments a
		                JOIN sections s ON s.id = a.section_id
		               WHERE s.teaching_course_id = tc.id),
		       EXISTS(SELECT 1 FROM ta_requests r WHERE r.teaching_course_id = tc.id),
		       EXISTS(SELECT 1 FROM ta_request_counts c
		                JOIN sections s ON s.id = c.section_id
		               WHERE s.teaching_course_id = tc.id),
		       EXISTS(SELECT 1 FROM holiday_remind_log h WHERE h.teaching_course_id = tc.id)
		FROM teaching_courses tc WHERE tc.id = $1`, id).Scan(
		&found, &exported, &hasWL, &hasStatus, &hasAssign, &hasReq, &hasCounts, &hasHoliday)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invalid("ไม่พบรายวิชานี้")
	}
	if err != nil {
		return err
	}
	switch {
	case exported:
		return Conflict("ลบไม่ได้ วิชานี้ถูกส่งออกเอกสารแล้ว")
	case hasWL:
		return Conflict("ลบไม่ได้ วิชานี้มีบันทึกเวลาแล้ว")
	case hasStatus:
		return Conflict("ลบไม่ได้ วิชานี้มีสถานะการเบิกจ่ายแล้ว")
	case hasAssign:
		return Conflict("ลบไม่ได้ วิชานี้มี TA ที่ได้รับมอบหมายแล้ว")
	case hasReq, hasCounts:
		return Conflict("ลบไม่ได้ วิชานี้มีคำขอ TA อยู่")
	case hasHoliday:
		return Conflict("ลบไม่ได้ วิชานี้มีข้อมูลที่เกี่ยวข้องอยู่")
	}
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "teaching_course.delete",
			Entity: "teaching_course", EntityID: id.String()},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM teaching_courses WHERE id=$1`, id)
			return err
		})
}

func (s *TeachingService) Get(ctx context.Context, id uuid.UUID) (*TeachingCourse, error) {
	tc := &TeachingCourse{}
	err := s.pool.QueryRow(ctx,
		// Effective term dates fall back to the parent academic_term when the
		// course itself doesn't override them — the UI (month grouper,
		// auto-generator) treats these as the canonical range for the course.
		`SELECT tc.id, tc.term_id, tc.code, tc.name_th, tc.name_en, tc.level,
		        tc.credits, tc.lecture_hrs, tc.lab_hrs, tc.self_hrs,
		        TO_CHAR(COALESCE(tc.starts_on, at.starts_on), 'YYYY-MM-DD'),
		        TO_CHAR(COALESCE(tc.ends_on,   at.ends_on),   'YYYY-MM-DD'),
		        tc.num_students, tc.num_students_regular, tc.num_students_special,
		        TO_CHAR(tc.exported_at,'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'),
		        -- คาบที่ตรงวันหยุดและยังไม่กำหนดวันชดเชย see UnresolvedMakeupsSQL.
		        `+UnresolvedMakeupsSQL("tc")+`,
		        -- ≥1 section ยังไม่มีตารางเรียน (WBA จากทะเบียน) บล็อกการส่งคำขอ TA.
		        -- List() คำนวณค่านี้อยู่แล้ว; Get() ไม่เคยส่งมา ทำให้หน้าเดียวเปิดวิชา
		        -- เดียวกันแล้วเห็นสถานะไม่ตรงกับหน้ารายการ
		        EXISTS (SELECT 1 FROM sections sx
		                 WHERE sx.teaching_course_id = tc.id
		                   AND NOT EXISTS (SELECT 1 FROM section_schedules sch
		                                    WHERE sch.section_id = sx.id))
		 FROM teaching_courses tc
		 JOIN academic_terms  at ON at.id = tc.term_id
		 WHERE tc.id = $1`, id).Scan(&tc.ID, &tc.TermID, &tc.Code, &tc.NameTH, &tc.NameEN, &tc.Level,
		&tc.Credits, &tc.LectureHrs, &tc.LabHrs, &tc.SelfHrs,
		&tc.StartsOn, &tc.EndsOn,
		&tc.NumStudents, &tc.NumStudentsRegular, &tc.NumStudentsSpecial,
		&tc.ExportedAt, &tc.UnresolvedMakeups, &tc.HasMissingSchedule)
	if err != nil {
		return nil, err
	}
	// Sections
	rows, err := s.pool.Query(ctx,
		`SELECT id, sec_no, track::text, room, num_students, curriculum,
		        TO_CHAR(schedule_set_by_lecturer_at,'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM')
		   FROM sections WHERE teaching_course_id=$1 ORDER BY sec_no`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sec Section
		if err := rows.Scan(&sec.ID, &sec.SecNo, &sec.Track, &sec.Room, &sec.NumStudents,
			&sec.Curriculum, &sec.ScheduleSetByLecturerAt); err != nil {
			rows.Close()
			return nil, err
		}
		sec.TeachingCourseID = id
		tc.Sections = append(tc.Sections, sec)
	}
	rows.Close()
	for i := range tc.Sections {
		sid := tc.Sections[i].ID
		schRows, _ := s.pool.Query(ctx,
			`SELECT id, kind, day_of_week, start_time::text, end_time::text, room
			 FROM section_schedules WHERE section_id=$1 ORDER BY day_of_week, start_time`, sid)
		for schRows.Next() {
			var sch SectionSchedule
			if err := schRows.Scan(&sch.ID, &sch.Kind, &sch.DayOfWeek, &sch.StartTime, &sch.EndTime, &sch.Room); err == nil {
				tc.Sections[i].Schedules = append(tc.Sections[i].Schedules, sch)
			}
		}
		schRows.Close()
		exRows, _ := s.pool.Query(ctx,
			`SELECT id, kind::text, TO_CHAR(exam_date,'YYYY-MM-DD'), start_time::text, end_time::text, room
			 FROM exam_schedules WHERE section_id=$1 ORDER BY exam_date`, sid)
		for exRows.Next() {
			var e ExamSchedule
			if err := exRows.Scan(&e.ID, &e.Kind, &e.ExamDate, &e.StartTime, &e.EndTime, &e.Room); err == nil {
				tc.Sections[i].Exams = append(tc.Sections[i].Exams, e)
			}
		}
		exRows.Close()
	}
	// WBA detection: a section with no schedule rows blocks TA requests until
	// someone fills the timetable in. Derived here (sections are already
	// loaded) so Get and List agree on the flag.
	for i := range tc.Sections {
		if len(tc.Sections[i].Schedules) == 0 {
			tc.HasMissingSchedule = true
			break
		}
	}
	// HasSpecial mirrors List's computed column for single-course readers.
	for i := range tc.Sections {
		if tc.Sections[i].Track == "special" {
			tc.HasSpecial = true
			break
		}
	}
	return tc, nil
}

// ClassKindRow is one weekly class slot of a term, flattened to the fields a
// client needs to tell บรรยาย from ปฏิบัติการ. The KKU REG .ics export carries
// NO lecture/lab marker (SUMMARY is just "code (credits) sec"), so the TA's
// schedule import resolves the kind by matching each imported slot against
// this table — which came from the registrar Excel where Lec/Lab IS labelled.
type ClassKindRow struct {
	Code      string `json:"code"`
	SecNo     string `json:"sec_no"`
	Kind      string `json:"kind"` // "lecture" | "lab"
	DayOfWeek int    `json:"day_of_week"`
	StartTime string `json:"start_time"` // "HH:MM"
	EndTime   string `json:"end_time"`   // "HH:MM"
	Room      string `json:"room,omitempty"`
}

// ClassKinds returns every scheduled class slot of a term. Read-only and not
// sensitive (it is the published timetable), so any signed-in user may read it.
func (s *TeachingService) ClassKinds(ctx context.Context, termID uuid.UUID) ([]ClassKindRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tc.code, sec.sec_no, sch.kind, sch.day_of_week,
		       TO_CHAR(sch.start_time, 'HH24:MI'), TO_CHAR(sch.end_time, 'HH24:MI'),
		       COALESCE(sch.room, '')
		FROM section_schedules sch
		JOIN sections sec ON sec.id = sch.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		WHERE tc.term_id = $1
		ORDER BY tc.code, sec.sec_no, sch.day_of_week, sch.start_time`, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ClassKindRow{}
	for rows.Next() {
		var r ClassKindRow
		if err := rows.Scan(&r.Code, &r.SecNo, &r.Kind, &r.DayOfWeek,
			&r.StartTime, &r.EndTime, &r.Room); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *TeachingService) List(ctx context.Context, termID *uuid.UUID, lecturerID *uuid.UUID) ([]TeachingCourse, error) {
	q := `SELECT tc.id, tc.term_id, tc.code, tc.name_th, tc.level,
	             tc.credits, tc.lecture_hrs, tc.lab_hrs, tc.self_hrs,
	             tc.num_students, tc.num_students_regular, tc.num_students_special,
	             EXISTS(SELECT 1 FROM sections sx WHERE sx.teaching_course_id=tc.id AND sx.track='special') AS has_special,
	             EXISTS(SELECT 1 FROM sections sx WHERE sx.teaching_course_id=tc.id
	                    AND NOT EXISTS(SELECT 1 FROM section_schedules ss WHERE ss.section_id=sx.id)) AS has_missing_schedule,
	             (SELECT COUNT(*) FROM sections sx WHERE sx.teaching_course_id=tc.id AND sx.track='regular') AS n_sec_regular,
	             (SELECT COUNT(*) FROM sections sx WHERE sx.teaching_course_id=tc.id AND sx.track='special') AS n_sec_special,
	             COALESCE((SELECT string_agg(u.first_name || ' ' || u.last_name, ', ' ORDER BY tl.is_primary DESC, u.first_name)
	                       FROM teaching_lecturers tl JOIN users u ON u.id = tl.lecturer_id
	                       WHERE tl.teaching_course_id = tc.id), '') AS lecturer_names,
	             -- คาบที่ตรงวันหยุดและยังไม่มีวันชดเชย see UnresolvedMakeupsSQL.
	             -- Previously inlined here with a CURRENT_DATE fallback, which made
	             -- this list disagree with both Get() and the holidays page.
	             ` + UnresolvedMakeupsSQL("tc") + ` AS unresolved_makeups
	      FROM teaching_courses tc`
	where := []string{}
	args := []any{}
	i := 1
	if termID != nil {
		where = append(where, "tc.term_id=$"+strconv.Itoa(i))
		args = append(args, *termID)
		i++
	}
	if lecturerID != nil {
		where = append(where, "EXISTS (SELECT 1 FROM teaching_lecturers tl WHERE tl.teaching_course_id=tc.id AND tl.lecturer_id=$"+strconv.Itoa(i)+")")
		args = append(args, *lecturerID)
		i++
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY tc.code"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Zero-length slice (not nil) so empty result sets marshal as `[]` in JSON
	// instead of `null` — SWR + downstream `.length` checks treat the two
	// differently and null crashes the render.
	out := []TeachingCourse{}
	for rows.Next() {
		var tc TeachingCourse
		if err := rows.Scan(&tc.ID, &tc.TermID, &tc.Code, &tc.NameTH, &tc.Level,
			&tc.Credits, &tc.LectureHrs, &tc.LabHrs, &tc.SelfHrs,
			&tc.NumStudents, &tc.NumStudentsRegular, &tc.NumStudentsSpecial,
			&tc.HasSpecial, &tc.HasMissingSchedule,
			&tc.NumSectionsRegular, &tc.NumSectionsSpecial, &tc.LecturerNames,
			&tc.UnresolvedMakeups); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, nil
}

// ListForTA returns teaching courses where the given TA is assigned via an
// approved TA request. Optionally filtered by term.
func (s *TeachingService) ListForTA(ctx context.Context, taID uuid.UUID, termID *uuid.UUID) ([]TeachingCourse, error) {
	q := `SELECT DISTINCT tc.id, tc.term_id, tc.code, tc.name_th,
	             tc.num_students, tc.num_students_regular, tc.num_students_special
	      FROM ta_request_assignments a
	      JOIN sections s ON s.id = a.section_id
	      JOIN teaching_courses tc ON tc.id = s.teaching_course_id
	      JOIN ta_requests r ON r.id = a.request_id
	      WHERE a.ta_id = $1 AND r.status = 'approved'`
	args := []any{taID}
	if termID != nil {
		q += " AND tc.term_id = $2"
		args = append(args, *termID)
	}
	q += " ORDER BY tc.code"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Zero-length slice (not nil) so empty result sets marshal as `[]` in JSON
	// instead of `null` — SWR + downstream `.length` checks treat the two
	// differently and null crashes the render.
	out := []TeachingCourse{}
	for rows.Next() {
		var tc TeachingCourse
		if err := rows.Scan(&tc.ID, &tc.TermID, &tc.Code, &tc.NameTH,
			&tc.NumStudents, &tc.NumStudentsRegular, &tc.NumStudentsSpecial); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, nil
}

// TAAssignment is one section-level slot the TA holds on an approved TA request.
// Surfaced so the TA-facing course page can resolve the assignment_id needed by
// /assignments/:id/worklog without having to peek at the /ta-requests API.
type TAAssignment struct {
	ID               uuid.UUID `json:"id"`
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	CourseCode       string    `json:"course_code"`
	CourseName       string    `json:"course_name"`
	SectionID        uuid.UUID `json:"section_id"`
	SecNo            string    `json:"sec_no"`
	Track            string    `json:"track"`
	Level            string    `json:"level"`
	// ReimburseScope is copied from the parent ta_request so the TA-facing
	// worklog UI can restrict the activity dropdown to what the lecturer
	// actually requested — "lecture" | "lab" | "both". Only guides UX; the
	// server-side hour caps stay authoritative.
	ReimburseScope string `json:"reimburse_scope"`
	// HasSchedule tells the UI whether the section has any rows in
	// section_schedules yet. Auto-generation reads that table to produce class
	// occurrences, so an empty schedule silently produces zero rows — the UI
	// blocks the button and tells the TA to ask the lecturer to add class
	// times instead of letting them wonder why nothing happened.
	HasSchedule bool `json:"has_schedule"`
	// Weekly hour caps per activity, derived from ta_workload_forms so the
	// TA-worklog modal can preflight-limit hour entry and the server can
	// reject overages before they hit the approver's queue. Mapping:
	//   undergrad: attendance_hrs → lecture, lab_hrs → lab,
	//              check_work_hrs → review, ug_other_hrs → other
	//   grad:      help_teach_hrs → shared cap for lecture+lab combined,
	//              grade_hrs      → review,
	//              other_hrs+prep_hrs → other (billable prep collapses in)
	// WeeklyCapsSet=false means the lecturer hasn't filled the workload form
	// yet — treat all zeros as "no cap known" and skip enforcement.
	WeeklyCapLecture       float64 `json:"weekly_cap_lecture"`
	WeeklyCapLab           float64 `json:"weekly_cap_lab"`
	WeeklyCapReview        float64 `json:"weekly_cap_review"`
	WeeklyCapOther         float64 `json:"weekly_cap_other"`
	WeeklyLectureLabShared bool    `json:"weekly_lecture_lab_shared"`
	WeeklyCapsSet          bool    `json:"weekly_caps_set"`
	// TermHourCeiling caps the TOTAL hours loggable for this assignment across
	// the whole term = (sum of declared weekly workload hours) × weeks-in-term.
	// Zero when no workload form is filed (no ceiling enforced).
	TermHourCeiling float64 `json:"term_hour_ceiling"`
	// State is 'active' or 'trimmed'. Dropped rows never reach the TA — they
	// are no longer assisting that section at all — so this is the only place
	// they learn that SOME sessions of a section they DO hold are unavailable
	// because their own class runs at the same time. Without it the work-log
	// screen would simply refuse those entries with no explanation of why.
	State       string  `json:"state"`
	StateReason *string `json:"state_reason,omitempty"`

	// Work-log tallies for THIS assignment. Every action on the worklog screen —
	// add, auto-generate, submit — is scoped to one assignment, so a TA holding
	// several sections of one course has several separate piles of work. These
	// let the UI show each pile's state side by side instead of one at a time
	// behind a dropdown, which is how a section got forgotten unsent.
	//
	// UnsentCount is draft + rejected: exactly what pressing "ส่งอนุมัติ" would
	// send (see WorkLogService.Submit), so the number on the button and the number
	// it acts on cannot disagree.
	UnsentCount int `json:"unsent_count"`
	// SubmittableCount is the part of UnsentCount that pressing the button would
	// actually send — rows whose month is still open. The difference is stranded:
	// the TA missed the deadline and only staff can move those now. Counted with
	// the same predicate Submit skips by (unsubmittableMonthSQL), so the button
	// can never offer to send something the server will refuse.
	SubmittableCount int `json:"submittable_count"`
	// MonthsInReview lists the "YYYY-MM" months of this assignment that have
	// entered review — anything submitted or approved. Upsert refuses a NEW row
	// in exactly these, so the screen hides its "+ เพิ่ม" affordance there rather
	// than offering a button the server will reject. Server-derived on purpose: a
	// second copy of the rule in the client is a copy that drifts.
	MonthsInReview []string `json:"months_in_review"`
	SubmittedCount int      `json:"submitted_count"`
	ApprovedCount  int      `json:"approved_count"`
	// HoursLogged counts everything not rejected, matching the term-ceiling
	// arithmetic the worklog screen already shows.
	HoursLogged float64 `json:"hours_logged"`
}

// ListAssignmentsForTA returns every approved TA-request assignment belonging to
// the given TA, optionally narrowed to a single teaching course. One TA can hold
// multiple assignments on the same course (one per section).
func (s *TeachingService) ListAssignmentsForTA(ctx context.Context, taID uuid.UUID, tcID *uuid.UUID) ([]TAAssignment, error) {
	q := `SELECT a.id, tc.id, tc.code, tc.name_th,
	             sec.id, sec.sec_no, sec.track, a.level::text, r.reimburse_scope::text,
	             EXISTS (SELECT 1 FROM section_schedules ss WHERE ss.section_id = sec.id),
	             CASE WHEN a.level::text = 'undergrad' THEN COALESCE(wf.attendance_hrs, 0)
	                  ELSE COALESCE(wf.help_teach_hrs, 0) END,
	             CASE WHEN a.level::text = 'undergrad' THEN COALESCE(wf.lab_hrs, 0)
	                  ELSE COALESCE(wf.help_teach_hrs, 0) END,
	             CASE WHEN a.level::text = 'undergrad' THEN COALESCE(wf.check_work_hrs, 0)
	                  ELSE COALESCE(wf.grade_hrs, 0) END,
	             CASE WHEN a.level::text = 'undergrad' THEN COALESCE(wf.ug_other_hrs, 0)
	                  ELSE COALESCE(wf.other_hrs, 0) + COALESCE(wf.prep_hrs, 0) END,
	             (a.level::text != 'undergrad'),
	             (wf.id IS NOT NULL),
	             a.state::text, a.state_reason,
	             (SELECT COUNT(*) FROM work_logs wl
	               WHERE wl.assignment_id = a.id AND wl.status IN ('draft','rejected')),
	             (SELECT COUNT(*) FROM work_logs wl
	               WHERE wl.assignment_id = a.id AND wl.status IN ('draft','rejected')
	                 AND NOT ` + unsubmittableMonthSQL("wl") + `),
	             COALESCE((SELECT ARRAY_AGG(DISTINCT to_char(wl.work_date,'YYYY-MM'))
	               FROM work_logs wl
	               WHERE wl.assignment_id = a.id
	                 AND wl.status IN ('submitted','approved')), '{}'),
	             (SELECT COUNT(*) FROM work_logs wl
	               WHERE wl.assignment_id = a.id AND wl.status = 'submitted'),
	             (SELECT COUNT(*) FROM work_logs wl
	               WHERE wl.assignment_id = a.id AND wl.status = 'approved'),
	             (SELECT COALESCE(SUM(wl.hours), 0) FROM work_logs wl
	               WHERE wl.assignment_id = a.id AND wl.status <> 'rejected')
	      FROM ta_request_assignments a
	      JOIN sections sec ON sec.id = a.section_id
	      JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
	      JOIN ta_requests r ON r.id = a.request_id
	      LEFT JOIN ta_workload_forms wf ON wf.assignment_id = a.id
	      -- 'dropped' means every session of that section clashed with the TA's
	      -- own timetable, so they are not assisting it at all. Showing it here
	      -- would offer a work-log target that can never accept an entry.
	      WHERE a.ta_id = $1 AND r.status = 'approved' AND a.state <> 'dropped'`
	args := []any{taID}
	if tcID != nil {
		q += " AND tc.id = $2"
		args = append(args, *tcID)
	}
	q += " ORDER BY tc.code, sec.sec_no"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TAAssignment{}
	for rows.Next() {
		var a TAAssignment
		if err := rows.Scan(&a.ID, &a.TeachingCourseID, &a.CourseCode, &a.CourseName,
			&a.SectionID, &a.SecNo, &a.Track, &a.Level, &a.ReimburseScope, &a.HasSchedule,
			&a.WeeklyCapLecture, &a.WeeklyCapLab, &a.WeeklyCapReview, &a.WeeklyCapOther,
			&a.WeeklyLectureLabShared, &a.WeeklyCapsSet, &a.State, &a.StateReason,
			&a.UnsentCount, &a.SubmittableCount, &a.MonthsInReview, &a.SubmittedCount, &a.ApprovedCount, &a.HoursLogged); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	// Term-hour ceiling per row = weekly workload total × weeks-in-term. For
	// grad, help_teach is a shared lecture+lab figure so it must not be counted
	// twice. Term months are cached per course.
	weeksCache := map[uuid.UUID]float64{}
	for i := range out {
		a := &out[i]
		if !a.WeeklyCapsSet {
			continue
		}
		weeklyTotal := a.WeeklyCapReview + a.WeeklyCapOther
		if a.WeeklyLectureLabShared {
			weeklyTotal += a.WeeklyCapLecture // help covers lecture+lab once
		} else {
			weeklyTotal += a.WeeklyCapLecture + a.WeeklyCapLab
		}
		weeks, ok := weeksCache[a.TeachingCourseID]
		if !ok {
			// Same helper the ENFORCEMENT uses. These were two copies of
			// `months × 4`, so the number shown to the TA and the number the
			// server refuses at could drift apart without anyone noticing.
			weeks = WeeksInTerm(ctx, s.pool, a.TeachingCourseID)
			weeksCache[a.TeachingCourseID] = weeks
		}
		a.TermHourCeiling = weeklyTotal * weeks
	}
	return out, nil
}

// SetNumStudents updates aggregate + per-track counts. Callers may pass -1 for
// a field they don't want to change (current value is kept).
func (s *TeachingService) SetNumStudents(ctx context.Context, actor, id uuid.UUID, total, regular, special int) error {
	// Staff-only, like the per-section headcount it aggregates: these numbers
	// come off the registrar file and drive the budget and the TA hour ceiling.
	priv, err := courseAccess(ctx, s.pool, actor, id)
	if err != nil {
		return err
	}
	if !priv {
		return Forbidden("จำนวนนักศึกษาต้องให้เจ้าหน้าที่กรอก ข้อมูลมาจากไฟล์ทะเบียน")
	}
	// Fetch current values so we can preserve untouched fields.
	var curTotal, curRegular, curSpecial int
	if err := s.pool.QueryRow(ctx,
		`SELECT num_students, num_students_regular, num_students_special
		 FROM teaching_courses WHERE id = $1`, id).Scan(&curTotal, &curRegular, &curSpecial); err != nil {
		return err
	}
	if regular < 0 {
		regular = curRegular
	}
	if special < 0 {
		special = curSpecial
	}
	// Per-track is source of truth for the aggregate; ignore an explicit total
	// unless neither track was provided.
	if regular != curRegular || special != curSpecial {
		total = regular + special
	} else if total < 0 {
		total = curTotal
	}
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "teaching_course.num_students",
			Entity: "teaching_course", EntityID: id.String(),
			After: map[string]int{"num_students": total, "regular": regular, "special": special}},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE teaching_courses
				SET num_students = $1,
				    num_students_regular = $2,
				    num_students_special = $3,
				    updated_at = NOW()
				WHERE id = $4`, total, regular, special, id)
			return err
		})
}

// UpdateSettingsInput overrides the course's own date range, which otherwise
// falls back to the parent academic_term. Course code / name / credits are set
// at import and are not editable at all. Pointer semantics: nil = leave alone;
// an empty string clears the DB column (sets to NULL, i.e. back to the term's
// dates).
type UpdateSettingsInput struct {
	StartsOn *string `json:"starts_on,omitempty"`
	EndsOn   *string `json:"ends_on,omitempty"`
}

func (s *TeachingService) UpdateSettings(ctx context.Context, actor, id uuid.UUID, in UpdateSettingsInput) error {
	// Staff-only. The course date range decides which months a TA may log work
	// into and how the term hour ceiling is scaled, so it is not a per-course
	// preference a lecturer sets — it follows the academic term.
	priv, err := courseAccess(ctx, s.pool, actor, id)
	if err != nil {
		return err
	}
	if !priv {
		return Forbidden("ช่วงวันที่ของรายวิชาต้องให้เจ้าหน้าที่กำหนด อ้างอิงตามภาคการศึกษา")
	}
	sets := []string{}
	args := []any{}
	i := 1
	add := func(col string, v *string) {
		if v == nil {
			return
		}
		sets = append(sets, fmt.Sprintf("%s = $%d::date", col, i))
		if *v == "" {
			args = append(args, nil)
		} else {
			args = append(args, *v)
		}
		i++
	}
	add("starts_on", in.StartsOn)
	add("ends_on", in.EndsOn)
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = NOW()")
	args = append(args, id)
	q := fmt.Sprintf("UPDATE teaching_courses SET %s WHERE id = $%d", strings.Join(sets, ", "), i)
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{
			ActorID: &actor, Action: "teaching_course.update_settings",
			Entity: "teaching_course", EntityID: id.String(), After: in,
		},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q, args...)
			return err
		})
}

// ErrCourseLocked is returned when a mutation would change a course whose
// export snapshot has been taken. Frontend surfaces this as a lock icon.
var ErrCourseLocked = errors.New("course is locked after export")

// assertNotExported returns ErrCourseLocked if the course has been exported.
// All section CRUD mutations gate on this so the export file stays the source
// of truth after staff has generated it.
func (s *TeachingService) assertNotExported(ctx context.Context, tx pgx.Tx, tcID uuid.UUID) error {
	var exported *time.Time
	q := "SELECT exported_at FROM teaching_courses WHERE id = $1"
	var err error
	if tx != nil {
		err = tx.QueryRow(ctx, q, tcID).Scan(&exported)
	} else {
		err = s.pool.QueryRow(ctx, q, tcID).Scan(&exported)
	}
	if err != nil {
		return err
	}
	if exported != nil {
		return ErrCourseLocked
	}
	return nil
}

// recomputeAggregate refreshes teaching_courses.num_students{,_regular,_special}
// as the sum of sections.num_students grouped by track. Callers pass their
// enclosing tx so aggregate updates are atomic with the row change.
func (s *TeachingService) recomputeAggregate(ctx context.Context, tx pgx.Tx, tcID uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		UPDATE teaching_courses tc SET
		  num_students_regular = COALESCE((SELECT SUM(num_students) FROM sections WHERE teaching_course_id=tc.id AND track='regular'), 0),
		  num_students_special = COALESCE((SELECT SUM(num_students) FROM sections WHERE teaching_course_id=tc.id AND track='special'), 0),
		  num_students = COALESCE((SELECT SUM(num_students) FROM sections WHERE teaching_course_id=tc.id), 0),
		  updated_at = NOW()
		WHERE tc.id = $1`, tcID)
	return err
}

type AddSectionInput struct {
	SecNo       string            `json:"sec_no"`
	Track       string            `json:"track"`
	Room        *string           `json:"room,omitempty"`
	NumStudents int               `json:"num_students"`
	Schedules   []SectionSchedule `json:"schedules,omitempty"`
}

// errSectionsAreStaffOnly explains the roster half of the lecturer rules: which
// sections a course has, what they are numbered and how many students sit in
// them all come from the registrar file that staff import. A lecturer editing
// that list would put the system out of step with the registrar, so the whole
// of section add/rename/delete is staff-only.
func errSectionsAreStaffOnly(verb string) error {
	return Forbidden(verb + " section ต้องให้เจ้าหน้าที่ดำเนินการ รายชื่อ section มาจากไฟล์ทะเบียน")
}

// AddSection adds a new section to a course. Staff-only: see
// errSectionsAreStaffOnly. Returns ErrCourseLocked if the course has already
// been exported. Aggregate counts are recomputed on success.
func (s *TeachingService) AddSection(ctx context.Context, actor, tcID uuid.UUID, in AddSectionInput) (uuid.UUID, error) {
	priv, err := courseAccess(ctx, s.pool, actor, tcID)
	if err != nil {
		return uuid.Nil, err
	}
	if !priv {
		return uuid.Nil, errSectionsAreStaffOnly("การเพิ่ม")
	}
	if in.SecNo == "" || (in.Track != "regular" && in.Track != "special") {
		return uuid.Nil, ErrInvalidInput
	}
	if in.NumStudents < 0 {
		return uuid.Nil, Invalid("จำนวนนักศึกษาต้องไม่ติดลบ")
	}
	lecHrs, labHrs, err := s.creditHrsForCourse(ctx, tcID)
	if err != nil {
		return uuid.Nil, err
	}
	if err := validateSectionSchedules(in.Schedules, lecHrs, labHrs); err != nil {
		return uuid.Nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)
	if err := s.assertNotExported(ctx, tx, tcID); err != nil {
		return uuid.Nil, err
	}
	id := uuid.New()
	if _, err := tx.Exec(ctx,
		`INSERT INTO sections (id, teaching_course_id, sec_no, track, room, num_students)
		 VALUES ($1,$2,$3,$4::section_track,$5,$6)`,
		id, tcID, in.SecNo, in.Track, in.Room, in.NumStudents); err != nil {
		return uuid.Nil, err
	}
	for _, sch := range in.Schedules {
		if _, err := tx.Exec(ctx,
			`INSERT INTO section_schedules (id, section_id, kind, day_of_week, start_time, end_time, room)
			 VALUES ($1,$2,$3,$4,$5::time,$6::time,$7)`,
			uuid.New(), id, sch.Kind, sch.DayOfWeek, sch.StartTime, sch.EndTime, sch.Room); err != nil {
			return uuid.Nil, err
		}
	}
	if err := s.recomputeAggregate(ctx, tx, tcID); err != nil {
		return uuid.Nil, err
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "section.add",
		Entity: "teaching_course", EntityID: tcID.String(), After: in}); err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// ReplaceSectionSchedules atomically clears and rewrites the schedule rows for
// one section. Gated on the course not being exported. Rejects unknown kinds,
// out-of-range days, malformed times, and any pair of blocks that overlap
// within the same day.
//
// Staff and admin may rewrite freely. A lecturer gets one write, and only into
// a section that has no timetable at all — the "WBA" case, where the registrar
// file listed the section but not its meeting times, and the lecturer is the
// only person who knows them. Once written (or if the times came from the file
// to begin with) the timetable is staff's to change: TA requests, workload
// caps and clash detection are all computed off these rows, so a lecturer
// quietly moving a class after TAs were assigned would silently invalidate
// decisions already made. See [[section-schedule-one-shot]].
func (s *TeachingService) ReplaceSectionSchedules(ctx context.Context, actor, tcID, sectionID uuid.UUID, schedules []SectionSchedule) error {
	priv, err := courseAccess(ctx, s.pool, actor, tcID)
	if err != nil {
		return err
	}
	if !priv && len(schedules) == 0 {
		// Saving nothing would burn the lecturer's single write on an empty
		// timetable and leave the section still blocking TA requests.
		return Invalid("ต้องระบุคาบเรียนอย่างน้อย 1 คาบ")
	}
	lecHrs, labHrs, err := s.creditHrsForCourse(ctx, tcID)
	if err != nil {
		return err
	}
	if err := validateSectionSchedules(schedules, lecHrs, labHrs); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := s.assertNotExported(ctx, tx, tcID); err != nil {
		return err
	}
	// Existence, current row count and the one-shot stamp in one read, inside
	// the transaction — two lecturers hitting save at once must not both see
	// "empty" and both write.
	var (
		existing int
		setAt    *time.Time
	)
	if err := tx.QueryRow(ctx,
		`SELECT (SELECT COUNT(*) FROM section_schedules WHERE section_id = s.id),
		        s.schedule_set_by_lecturer_at
		   FROM sections s
		  WHERE s.id = $1 AND s.teaching_course_id = $2
		  FOR UPDATE OF s`,
		sectionID, tcID).Scan(&existing, &setAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if !priv {
		switch {
		case setAt != nil:
			return Forbidden("คุณกำหนดตารางเวลาของกลุ่มนี้ไปแล้วเมื่อ " + thaiDate(*setAt) +
				" กำหนดได้ครั้งเดียว หากต้องแก้ไขกรุณาแจ้งเจ้าหน้าที่")
		case existing > 0:
			return Forbidden("ตารางเวลาของกลุ่มนี้มาจากไฟล์ที่เจ้าหน้าที่นำเข้า แก้ไขได้เฉพาะเจ้าหน้าที่")
		}
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM section_schedules WHERE section_id=$1`, sectionID); err != nil {
		return err
	}
	for _, sch := range schedules {
		if _, err := tx.Exec(ctx,
			`INSERT INTO section_schedules (id, section_id, kind, day_of_week, start_time, end_time, room)
			 VALUES ($1,$2,$3,$4,$5::time,$6::time,$7)`,
			uuid.New(), sectionID, sch.Kind, sch.DayOfWeek, sch.StartTime, sch.EndTime, sch.Room); err != nil {
			return err
		}
	}
	if !priv {
		// Spend the lecturer's one write. Staff edits deliberately leave the
		// stamp alone: it records who filled the blank, not who last saved.
		if _, err := tx.Exec(ctx,
			`UPDATE sections SET schedule_set_by_lecturer_at = NOW() WHERE id = $1`,
			sectionID); err != nil {
			return err
		}
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "section.schedules.replace",
		Entity: "section", EntityID: sectionID.String(), After: schedules}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

// validateSectionSchedules enforces the same rules the lecturer UI enforces so
// stray API callers can't skip them. Only intra-section overlap is checked —
// cross-section conflicts are the lecturer's judgement call and are surfaced
// later when TAs try to take a request that would collide.
//
// lectureHrs/labHrs are the course's per-term credit hours (teaching_courses);
// they gate which schedule kinds are allowed (see validateScheduleKinds).
func validateSectionSchedules(schedules []SectionSchedule, lectureHrs, labHrs int) error {
	kinds := make([]string, 0, len(schedules))
	for i, sch := range schedules {
		if sch.Kind != "lecture" && sch.Kind != "lab" {
			return errors.New("ประเภทคาบต้องเป็นบรรยาย (lecture) หรือปฏิบัติการ (lab) เท่านั้น")
		}
		if sch.DayOfWeek < 0 || sch.DayOfWeek > 6 {
			return errors.New("วันในสัปดาห์ต้องเป็น 0–6")
		}
		if !isHHMM(sch.StartTime) || !isHHMM(sch.EndTime) {
			return errors.New("รูปแบบเวลาต้องเป็น HH:MM")
		}
		if sch.StartTime >= sch.EndTime {
			return errors.New("เวลาสิ้นสุดต้องมากกว่าเวลาเริ่ม")
		}
		for j := 0; j < i; j++ {
			other := schedules[j]
			if other.DayOfWeek != sch.DayOfWeek {
				continue
			}
			if sch.StartTime < other.EndTime && other.StartTime < sch.EndTime {
				return errors.New("มีช่วงเวลาที่ทับซ้อนกันภายใน section เดียวกัน")
			}
		}
		kinds = append(kinds, sch.Kind)
	}
	return validateScheduleKinds(kinds, lectureHrs, labHrs)
}

// validateScheduleKinds enforces the two credit-gating rules for the manual
// section-CRUD paths (the Excel import trusts the registrar file and skips it):
//
//  1. A "lecture" block is only allowed when the course carries lecture credit
//     hours (teaching_courses.lecture_hrs > 0); a "lab" block only when
//     lab_hrs > 0. Otherwise the schedule would inflate the review-hours
//     billing cap downstream for hours the course doesn't actually teach.
//  2. Each kind may appear AT MOST ONCE per section — a section can hold one
//     lecture block and one lab block, no more.
//
// lecture_hrs/lab_hrs are INT NOT NULL DEFAULT 0, so a 0 reliably means "this
// course has no such hours" (never "unknown") and it is safe to block that
// kind on 0.
func validateScheduleKinds(kinds []string, lectureHrs, labHrs int) error {
	seen := map[string]bool{}
	for _, k := range kinds {
		switch k {
		case "lecture":
			if lectureHrs <= 0 {
				return errors.New("รายวิชานี้ไม่มีหน่วยชั่วโมงบรรยาย เพิ่มตารางบรรยายไม่ได้")
			}
		case "lab":
			if labHrs <= 0 {
				return errors.New("รายวิชานี้ไม่มีหน่วยชั่วโมงปฏิบัติการ เพิ่มตารางปฏิบัติการไม่ได้")
			}
		}
		if seen[k] {
			return errors.New("มีตารางบรรยาย/ปฏิบัติการซ้ำในกลุ่มเดียวกัน ระบุได้ประเภทละ 1 รายการต่อกลุ่ม")
		}
		seen[k] = true
	}
	return nil
}

// creditHrsForCourse resolves the teaching course's lecture_hrs / lab_hrs (now
// stored per-term on the course itself) so schedule mutations can gate which
// kinds are allowed.
func (s *TeachingService) creditHrsForCourse(ctx context.Context, tcID uuid.UUID) (lectureHrs, labHrs int, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT lecture_hrs, lab_hrs FROM teaching_courses WHERE id = $1`, tcID).Scan(&lectureHrs, &labHrs)
	return
}

// thaiDate renders a timestamp as d/m/พ.ศ. in Bangkok time, for user-facing
// messages. Buddhist era because that is what every other date the lecturer
// sees in this system uses.
func thaiDate(t time.Time) string {
	d := t.In(timeutil.Bangkok)
	return fmt.Sprintf("%d/%d/%d", d.Day(), int(d.Month()), d.Year()+543)
}

// isHHMM accepts either "HH:MM" or "HH:MM:SS" so callers may send whichever the
// picker widget emits. Postgres will parse both when we cast to TIME.
func isHHMM(v string) bool {
	if len(v) != 5 && len(v) != 8 {
		return false
	}
	if v[2] != ':' {
		return false
	}
	for _, r := range v[:2] + v[3:5] {
		if r < '0' || r > '9' {
			return false
		}
	}
	h := (int(v[0]-'0'))*10 + int(v[1]-'0')
	m := (int(v[3]-'0'))*10 + int(v[4]-'0')
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return false
	}
	if len(v) == 8 {
		if v[5] != ':' {
			return false
		}
		for _, r := range v[6:8] {
			if r < '0' || r > '9' {
				return false
			}
		}
		sec := (int(v[6]-'0'))*10 + int(v[7]-'0')
		if sec < 0 || sec > 59 {
			return false
		}
	}
	return true
}

type UpdateSectionInput struct {
	SecNo       *string `json:"sec_no,omitempty"`
	Room        *string `json:"room,omitempty"`
	NumStudents *int    `json:"num_students,omitempty"`
	// "" clears back to unknown; otherwise one of the CHECK-listed groups.
	// This is the staff override the import respects (re-import fills only
	// NULLs), so a wrong registrar value can be corrected once and stay put.
	Curriculum *string `json:"curriculum,omitempty"`
}

// validCurriculum mirrors the CHECK constraint on sections.curriculum.
func validCurriculum(v string) bool {
	switch v {
	case "CS", "IT", "GIS", "AI", "CY", "OTHER":
		return true
	}
	return false
}

// UpdateSection edits sec_no / room / num_students on a single section.
// Track is intentionally not editable — switching regular↔special would
// invalidate any budget/request math already based on the old track.
func (s *TeachingService) UpdateSection(ctx context.Context, actor, tcID, sectionID uuid.UUID, in UpdateSectionInput) error {
	priv, err := courseAccess(ctx, s.pool, actor, tcID)
	if err != nil {
		return err
	}
	if !priv {
		return errSectionsAreStaffOnly("การแก้ไข")
	}
	if in.NumStudents != nil && *in.NumStudents < 0 {
		return Invalid("จำนวนนักศึกษาต้องไม่ติดลบ")
	}
	if in.Curriculum != nil && *in.Curriculum != "" && !validCurriculum(*in.Curriculum) {
		return Invalid("หลักสูตรไม่ถูกต้อง")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := s.assertNotExported(ctx, tx, tcID); err != nil {
		return err
	}
	sets := []string{}
	args := []any{}
	i := 1
	if in.SecNo != nil {
		sets = append(sets, fmt.Sprintf("sec_no = $%d", i))
		args = append(args, *in.SecNo)
		i++
	}
	if in.Room != nil {
		sets = append(sets, fmt.Sprintf("room = $%d", i))
		if *in.Room == "" {
			args = append(args, nil)
		} else {
			args = append(args, *in.Room)
		}
		i++
	}
	if in.NumStudents != nil {
		sets = append(sets, fmt.Sprintf("num_students = $%d", i))
		args = append(args, *in.NumStudents)
		i++
	}
	if in.Curriculum != nil {
		sets = append(sets, fmt.Sprintf("curriculum = $%d", i))
		if *in.Curriculum == "" {
			args = append(args, nil)
		} else {
			args = append(args, *in.Curriculum)
		}
		i++
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, sectionID, tcID)
	q := fmt.Sprintf("UPDATE sections SET %s WHERE id=$%d AND teaching_course_id=$%d", strings.Join(sets, ", "), i, i+1)
	if _, err := tx.Exec(ctx, q, args...); err != nil {
		return err
	}
	if in.NumStudents != nil {
		if err := s.recomputeAggregate(ctx, tx, tcID); err != nil {
			return err
		}
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "section.update",
		Entity: "section", EntityID: sectionID.String(), After: in}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

// DeleteSection removes a section. Locked after export. Aggregate is
// recomputed. FK violations (e.g. sections still referenced by TA request
// assignments) surface as-is to the caller.
func (s *TeachingService) DeleteSection(ctx context.Context, actor, tcID, sectionID uuid.UUID) error {
	priv, err := courseAccess(ctx, s.pool, actor, tcID)
	if err != nil {
		return err
	}
	if !priv {
		return errSectionsAreStaffOnly("การลบ")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := s.assertNotExported(ctx, tx, tcID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM sections WHERE id=$1 AND teaching_course_id=$2`, sectionID, tcID); err != nil {
		return err
	}
	if err := s.recomputeAggregate(ctx, tx, tcID); err != nil {
		return err
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "section.delete",
		Entity: "section", EntityID: sectionID.String()}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

// MarkExported is called by ExportService after successfully building the zip.
// Sets the exported_at timestamp which freezes section edits.
func (s *TeachingService) MarkExported(ctx context.Context, tcID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE teaching_courses SET exported_at = NOW() WHERE id = $1 AND exported_at IS NULL`, tcID)
	return err
}

// Unexport clears the export lock so an accidentally-exported course can be
// edited again. Admin-only (enforced at the route).
func (s *TeachingService) Unexport(ctx context.Context, actor, tcID uuid.UUID) error {
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "course.unexport", Entity: "teaching_course", EntityID: tcID.String()},
		func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx,
				`UPDATE teaching_courses SET exported_at = NULL WHERE id = $1 AND exported_at IS NOT NULL`, tcID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return Invalid("รายวิชานี้ยังไม่ได้ส่งออก จึงไม่มีอะไรให้ปลดล็อก")
			}
			return nil
		})
}

// kindLabelTH names a class period the way the UI does, so a refusal the
// lecturer reads matches the chip they clicked.
func kindLabelTH(kind string) string {
	switch kind {
	case "lecture":
		return "บรรยาย"
	case "lab":
		return "ปฏิบัติการ"
	}
	return kind
}

// AddMakeup — makeup schedule
func (s *TeachingService) AddMakeup(ctx context.Context, actor, sectionID uuid.UUID, m MakeupSchedule) error {
	// sectionID carries no course id — resolve the parent course so we can
	// enforce ownership and the export lock the section-CRUD paths already use.
	var tcID uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT teaching_course_id FROM sections WHERE id=$1`, sectionID).Scan(&tcID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if err := assertMakeupManager(ctx, s.pool, actor, tcID); err != nil {
		return err
	}
	if err := s.assertNotExported(ctx, nil, tcID); err != nil {
		return err
	}
	origDay, err := time.Parse("2006-01-02", m.OriginalDate)
	if err != nil {
		return Invalid("รูปแบบวันที่ไม่ถูกต้อง")
	}
	// The period being replaced has to exist. Without this check a typo in `kind`
	// would file a makeup that no reader can ever match — the period would stay
	// "ยังไม่ได้กำหนดวันชดเชย" while the constraint refused a second attempt, which
	// is the dead end the old day-level model produced.
	if m.Kind != "lecture" && m.Kind != "lab" {
		return Invalid("ชนิดคาบต้องเป็น lecture หรือ lab")
	}
	var periodExists bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM section_schedules
		   WHERE section_id = $1 AND kind = $2 AND day_of_week = $3)`,
		sectionID, m.Kind, int(origDay.Weekday())).Scan(&periodExists); err != nil {
		return err
	}
	if !periodExists {
		return Invalid("กลุ่มนี้ไม่มีคาบชนิดดังกล่าวในวันที่เลือก")
	}
	makeupDay, err := timeutil.ParseDate(m.MakeupDate)
	if err != nil {
		return Invalid("รูปแบบวันที่ไม่ถูกต้อง")
	}
	// A makeup in a month that has already passed cannot produce payable work:
	// the TA's work-log write for that month is already frozen, so the class
	// would be filed and then be unloggable. The meeting asked for the same
	// no-back-dating rule to cover makeups, not just time entries.
	// Compared by (year, month) to match validateWorkLogEntry exactly.
	now := timeutil.Now()
	my, mm, _ := makeupDay.Date()
	ny, nm, _ := now.Date()
	if my < ny || (my == ny && mm < nm) {
		return Invalid(fmt.Sprintf(
			"กำหนดวันชดเชยย้อนหลังไปเดือนที่ผ่านไปแล้วไม่ได้ (%s) "+
				"เดือนนั้นปิดการลงเวลาแล้ว TA จึงลงบันทึกคาบนี้ไม่ได้ กรุณาเลือกวันตั้งแต่เดือนปัจจุบันเป็นต้นไป",
			m.MakeupDate))
	}
	if m.StartTime != nil && m.EndTime != nil {
		st, ok1 := parseHM(*m.StartTime)
		et, ok2 := parseHM(*m.EndTime)
		if !ok1 || !ok2 {
			return Invalid("รูปแบบเวลาต้องเป็น HH:MM")
		}
		if st >= et {
			return Invalid("เวลาสิ้นสุดต้องมากกว่าเวลาเริ่ม")
		}
	}
	// Nested holiday check: a makeup landing on another closure would just push
	// the problem to a different day. Reject at creation time so the lecturer
	// picks a workable slot up front.
	//
	// Partial-day holidays make this an OVERLAP test, not a date test — and that
	// is the case this feature exists for. A faculty ceremony 08:00–12:00 leaves
	// the afternoon of that same day free, and rescheduling the cancelled morning
	// lecture into it is the obvious move; the old date-level check refused it.
	// A makeup with no times given is treated as occupying the whole day, so any
	// closure on that date still blocks it.
	var nestedHoliday, nestedWindow string
	err = s.pool.QueryRow(ctx, `
		SELECT name_th,
		       CASE WHEN start_time IS NULL THEN 'ทั้งวัน'
		            ELSE TO_CHAR(start_time,'HH24:MI') || '–' || TO_CHAR(end_time,'HH24:MI') END
		  FROM public_holidays
		 WHERE holiday_date = $1::date
		   AND (start_time IS NULL
		        OR $2::time IS NULL OR $3::time IS NULL
		        OR (start_time < $3::time AND end_time > $2::time))
		 LIMIT 1`,
		m.MakeupDate, m.StartTime, m.EndTime).Scan(&nestedHoliday, &nestedWindow)
	if err == nil {
		return Invalid(fmt.Sprintf("วันชดเชย %s ตรงกับวันหยุด (%s · %s) กรุณาเลือกวันหรือช่วงเวลาอื่น",
			m.MakeupDate, nestedHoliday, nestedWindow))
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "makeup.add", Entity: "section", EntityID: sectionID.String(), After: m},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO makeup_schedules (id, section_id, original_date, makeup_date, start_time, end_time, note, kind)
				 VALUES ($1,$2,$3::date,$4::date,$5,$6,$7,$8)`,
				uuid.New(), sectionID, m.OriginalDate, m.MakeupDate, m.StartTime, m.EndTime, m.Note, m.Kind)
			if err != nil {
				// UNIQUE (section_id, original_date, kind) violation — this PERIOD already
				// has a filed makeup. Names the period, because the other period of the
				// same day is a separate row the lecturer may still need to file.
				return Invalid(fmt.Sprintf("คาบ%sของวันที่ %s มีวันชดเชยอยู่แล้ว กรุณาลบวันเดิมก่อนแล้วเพิ่มใหม่",
					kindLabelTH(m.Kind), m.OriginalDate))
			}
			return nil
		})
}

// DeleteMakeup removes a filed makeup. Guards against silently invalidating
// worklog rows that a TA has already submitted or an approver has cleared —
// those need explicit rejection first. Draft rows on the vanishing makeup date
// are removed in the same transaction and the owning TA is notified.
func (s *TeachingService) DeleteMakeup(ctx context.Context, actor, sectionID, makeupID uuid.UUID) error {
	// Resolve makeup + parent course; the section id is trusted from the URL,
	// but we double-check the row belongs to it so a lecturer can't delete a
	// makeup on another section by URL manipulation.
	var tcID uuid.UUID
	var makeupDate time.Time
	if err := s.pool.QueryRow(ctx, `
		SELECT sec.teaching_course_id, m.makeup_date
		FROM makeup_schedules m
		JOIN sections sec ON sec.id = m.section_id
		WHERE m.id = $1 AND m.section_id = $2`, makeupID, sectionID).Scan(&tcID, &makeupDate); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if err := assertMakeupManager(ctx, s.pool, actor, tcID); err != nil {
		return err
	}
	if err := s.assertNotExported(ctx, nil, tcID); err != nil {
		return err
	}
	makeupDateStr := makeupDate.Format("2006-01-02")

	// Block if any submitted/approved worklog references the makeup date on
	// this section — those need to go through Reject first so the audit trail
	// stays intact.
	var reviewedCount int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		WHERE a.section_id = $1
		  AND wl.work_date = $2::date
		  AND wl.status IN ('submitted','approved')`,
		sectionID, makeupDateStr).Scan(&reviewedCount); err != nil {
		return err
	}
	if reviewedCount > 0 {
		return Invalid("ลบไม่ได้: มีบันทึกเวลาในวันชดเชยนี้ถูกส่งอนุมัติแล้ว กรุณาให้อาจารย์/เจ้าหน้าที่ปฏิเสธก่อน")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Collect draft-row TAs so we can notify each one that their day-of-work
	// row has been removed alongside the makeup.
	notifyTargets := []uuid.UUID{}
	nrows, err := tx.Query(ctx, `
		SELECT DISTINCT a.ta_id
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		WHERE a.section_id = $1 AND wl.work_date = $2::date AND wl.status = 'draft'`,
		sectionID, makeupDateStr)
	if err != nil {
		return err
	}
	for nrows.Next() {
		var taID uuid.UUID
		if err := nrows.Scan(&taID); err == nil {
			notifyTargets = append(notifyTargets, taID)
		}
	}
	nrows.Close()

	if _, err := tx.Exec(ctx, `
		DELETE FROM work_logs
		USING ta_request_assignments a
		WHERE work_logs.assignment_id = a.id
		  AND a.section_id = $1
		  AND work_logs.work_date = $2::date
		  AND work_logs.status = 'draft'`,
		sectionID, makeupDateStr); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM makeup_schedules WHERE id = $1`, makeupID); err != nil {
		return err
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "makeup.delete", Entity: "section", EntityID: sectionID.String(), Note: makeupDateStr}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if s.notify != nil {
		body := fmt.Sprintf("อาจารย์ยกเลิกวันชดเชย %s รายการชั่วโมงร่างในวันนั้นถูกลบ", makeupDateStr)
		for _, taID := range notifyTargets {
			s.notify.Send(ctx, taID, "อาจารย์ยกเลิกวันชดเชย", body, "/ta")
		}
	}
	return nil
}

func (s *TeachingService) AddReviewDate(ctx context.Context, actor, sectionID uuid.UUID, r LectureReview) error {
	// sectionID carries no course id — resolve the parent course so we can
	// enforce ownership and the export lock the section-CRUD paths already use.
	var tcID uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT teaching_course_id FROM sections WHERE id=$1`, sectionID).Scan(&tcID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if err := assertCourseManager(ctx, s.pool, actor, tcID); err != nil {
		return err
	}
	if err := s.assertNotExported(ctx, nil, tcID); err != nil {
		return err
	}
	if r.Hours <= 0 || r.Hours > 12 {
		return Invalid("จำนวนชั่วโมงไม่ถูกต้อง")
	}
	if _, err := time.Parse("2006-01-02", r.ReviewDate); err != nil {
		return Invalid("รูปแบบวันที่ไม่ถูกต้อง")
	}
	if r.StartTime != nil && r.EndTime != nil {
		st, ok1 := parseHM(*r.StartTime)
		et, ok2 := parseHM(*r.EndTime)
		if !ok1 || !ok2 {
			return Invalid("รูปแบบเวลาต้องเป็น HH:MM")
		}
		if st >= et {
			return Invalid("เวลาสิ้นสุดต้องมากกว่าเวลาเริ่ม")
		}
	}
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "review_date.add", Entity: "section", EntityID: sectionID.String(), After: r},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO lecture_review_dates (id, section_id, review_date, start_time, end_time, hours, note)
				 VALUES ($1,$2,$3::date,$4,$5,$6,$7)`,
				uuid.New(), sectionID, r.ReviewDate, r.StartTime, r.EndTime, r.Hours, r.Note)
			return err
		})
}

// -----------------------------------------------------------------------------
// Bulk import of "รายวิชาที่เปิดสอน" from the university-supplied Excel.
//
// The Normalized sheet is one row per (course × section × schedule) with 16
// columns: CourseCode, CourseName, Unit, Prerequisite, Section, ReservedFor,
// TotalSeats, Day, Time, SessionType, Room, MidtermExamDate, MidtermExamTime,
// FinalExamDate, FinalExamTime, Officer.
//
// Flow: staff uploads → PreviewImport reports what would happen per course
// (new / existing / unmatched_officer) → staff picks skip codes → CommitImport
// creates courses in per-course transactions so one failure never blocks the
// rest of the file. Course identity (code, names, credit hours, level) comes
// straight from the file — there is no central catalog to pre-populate.
// -----------------------------------------------------------------------------

type ImportResult struct {
	RowCount   int         `json:"row_count"`
	CreatedIDs []uuid.UUID `json:"created_ids,omitempty"`
	// SkippedCodes carries course codes that were deliberately skipped by the
	// staff decision or because the course already exists in this term. Not an
	// error.
	SkippedCodes []string `json:"skipped_codes,omitempty"`
	ErrorCount   int      `json:"error_count"`
	Errors       []string `json:"errors,omitempty"`
}

type ImportPreviewCourse struct {
	Code               string      `json:"code"`
	Name               string      `json:"name"`
	Status             string      `json:"status"` // "new" | "existing" | "unmatched_officer"
	SectionCount       int         `json:"section_count"`
	ScheduleCount      int         `json:"schedule_count"`
	OfficerRaw         string      `json:"officer_raw"`
	OfficerNames       []string    `json:"officer_names"`
	MatchedLecturerIDs []uuid.UUID `json:"matched_lecturer_ids"`
	UnmatchedNames     []string    `json:"unmatched_names,omitempty"`
	Note               string      `json:"note,omitempty"`
}

type ImportPreview struct {
	Filename      string                `json:"filename"`
	Courses       []ImportPreviewCourse `json:"courses"`
	NewCount      int                   `json:"new_count"`
	ExistingCount int                   `json:"existing_count"`
	BlockedCount  int                   `json:"blocked_count"`
}

type parsedSchedule struct {
	kind      string
	dow       int
	startTime string
	endTime   string
	room      string
}

type parsedSection struct {
	secNo       string
	track       string
	room        string
	numStudents int
	curriculum  string // "" = ReservedFor carried no programme token
	schedules   []parsedSchedule
}

type parsedCourse struct {
	code            string
	name            string // Thai name (falls back to the English name / code)
	nameEN          string
	level           string // "undergrad" | "graduate"
	credits         int
	lectureHrs      int
	labHrs          int
	selfHrs         int
	officerRaw      string
	sectionsInOrder []string
	sections        map[string]*parsedSection
}

// Officer tokens that are not real personal names — staff wrote them as
// placeholders (guest lecturer, "and colleagues", honorific-only). They must
// not participate in the users lookup, otherwise every course with them would
// be flagged as unmatched even though the course itself is fine.
var officerNoiseTokens = map[string]struct{}{
	"อจ.พิเศษ": {},
	"และคณะ":   {},
	"อจ.":      {},
	"อาจารย์":  {},
}

// Thai month abbreviations as they appear in the Excel (with the trailing dot).
var thaiMonthAbbrev = map[string]int{
	"ม.ค.":  1,
	"ก.พ.":  2,
	"มี.ค.": 3,
	"เม.ย.": 4,
	"พ.ค.":  5,
	"มิ.ย.": 6,
	"ก.ค.":  7,
	"ส.ค.":  8,
	"ก.ย.":  9,
	"ต.ค.":  10,
	"พ.ย.":  11,
	"ธ.ค.":  12,
}

// dowFromAbbrev maps the day tokens the university uses (Sunday=0 convention,
// same as ScheduleGrid.tsx / TA class schedules).
func dowFromAbbrev(s string) (int, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "SU":
		return 0, true
	case "M":
		return 1, true
	case "TU":
		return 2, true
	case "W":
		return 3, true
	case "TH":
		return 4, true
	case "F":
		return 5, true
	case "SA":
		return 6, true
	}
	return 0, false
}

// parseTimeRange accepts "13:00-16:00" (allows internal whitespace) and
// returns each half as "HH:MM". No parse — Postgres does the type coercion via
// the ::time cast on insert.
func parseTimeRange(raw string) (string, string, bool) {
	s := strings.ReplaceAll(raw, " ", "")
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// parseThaiDate turns "29 ต.ค. 69" (Buddhist Era 2-digit year, Thai month
// abbrev) into an ISO YYYY-MM-DD string. Returns empty string for WBA/blank
// values or anything unparseable — callers treat empty as "no exam date".
func parseThaiDate(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || strings.EqualFold(s, "WBA") {
		return ""
	}
	// Strip a leading "Lec " / "Lab " qualifier if present.
	if strings.HasPrefix(s, "Lec ") || strings.HasPrefix(s, "Lab ") {
		s = strings.TrimSpace(s[4:])
	}
	fields := strings.Fields(s)
	if len(fields) < 3 {
		return ""
	}
	day, err := strconv.Atoi(fields[0])
	if err != nil || day < 1 || day > 31 {
		return ""
	}
	month, ok := thaiMonthAbbrev[fields[1]]
	if !ok {
		return ""
	}
	yy, err := strconv.Atoi(fields[2])
	if err != nil {
		return ""
	}
	// 2-digit Buddhist Era → Gregorian. "69" → 2569 BE → 2026 CE.
	beYear := 2500 + yy
	ce := beYear - 543
	return fmt.Sprintf("%04d-%02d-%02d", ce, month, day)
}

// isExamForLec / isExamForLab were used by the pre-2026-07-14 Excel importer
// to route "Lec ..."/"Lab ..." exam-date cells into per-course columns. Kept
// available in case a future importer needs the same prefix check.
// isExamForLec tests whether the exam-date cell describes a lecture exam. The
// university prefixes lecture exam dates with "Lec ". Everything without the
// prefix (bare "WBA", "Lab 22 ต.ค. 69", …) is treated as non-Lec here.
func isExamForLec(raw string) bool { return strings.HasPrefix(strings.TrimSpace(raw), "Lec ") }
func isExamForLab(raw string) bool { return strings.HasPrefix(strings.TrimSpace(raw), "Lab ") }

// pickImportSheet returns the sheet to read and whether it is the raw registrar
// export (`sysTitle` layout) rather than the flattened `Normalized` table.
// Preference: the flattened sheet (exact name "Normalized", or a header row
// containing "CourseCode"); then the raw sheet (name "sysTitle", or a header row
// containing "COURSECODE1"); then the last sheet, guessed by its header.
func pickImportSheet(f *excelize.File) (name string, raw bool, err error) {
	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return "", false, errors.New("ไฟล์ไม่มี sheet")
	}
	headerHas := func(sheet, needle string) bool {
		rows, e := f.GetRows(sheet)
		if e != nil || len(rows) == 0 {
			return false
		}
		for _, cell := range rows[0] {
			if strings.EqualFold(strings.TrimSpace(cell), needle) {
				return true
			}
		}
		return false
	}
	// 1. Flattened "Normalized" table (preferred — already one row per meeting).
	for _, s := range sheets {
		if s == "Normalized" {
			return s, false, nil
		}
	}
	for _, s := range sheets {
		if headerHas(s, "CourseCode") {
			return s, false, nil
		}
	}
	// 2. Raw registrar export (multi-line cells, header rows + section rows).
	for _, s := range sheets {
		if s == "sysTitle" || headerHas(s, "COURSECODE1") {
			return s, true, nil
		}
	}
	// 3. Fall back to the last sheet; guess raw vs flat from its header.
	last := sheets[len(sheets)-1]
	return last, headerHas(last, "COURSECODE1"), nil
}

// parseUnit reads the registrar's credit notation "N (a-b-c)" into
// credits=N, lectureHrs=a, labHrs=b, selfHrs=c. Unparseable → all zeros.
var unitRe = regexp.MustCompile(`^\s*(\d+)\s*\(\s*(\d+)\s*-\s*(\d+)\s*-\s*(\d+)\s*\)\s*$`)

func parseUnit(s string) (credits, lectureHrs, labHrs, selfHrs int) {
	m := unitRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, 0, 0
	}
	credits, _ = strconv.Atoi(m[1])
	lectureHrs, _ = strconv.Atoi(m[2])
	labHrs, _ = strconv.Atoi(m[3])
	selfHrs, _ = strconv.Atoi(m[4])
	return
}

// courseLevelFromReserved maps the "ReservedFor" text to a course level. The
// registrar writes "ตรี" / "ตรี โครงการพิเศษ" for undergrad and "บัณฑิต" / "โท" /
// "เอก" for graduate. Defaults to undergrad.
//
// Reads only the label of the FIRST clause ("ตรี : SC-IT ปี 3, โท : CP-DSAI ปี
// 1" is comma-separated clauses, each "label : detail") — same "first wins"
// rule curriculumFromReserved already uses for programme tokens. A whole-string
// substring scan used to answer graduate for that example just because "โท"
// appears somewhere in it, even though the section's own primary label is
// "ตรี". A section is one level; the first clause names which one.
func courseLevelFromReserved(reserved string) string {
	label := reserved
	if idx := strings.Index(label, ","); idx >= 0 {
		label = label[:idx]
	}
	if idx := strings.Index(label, ":"); idx >= 0 {
		label = label[:idx]
	}
	for _, kw := range []string{"บัณฑิต", "ปริญญาโท", "ปริญญาเอก", "ป.โท", "ป.เอก", "โท", "เอก"} {
		if strings.Contains(label, kw) {
			return "graduate"
		}
	}
	return "undergrad"
}

// curriculumTokenRE finds programme tokens like "SC-IT", "CP-Cy", "BS-Digi"
// inside the ReservedFor text. The registrar writes one or more of these per
// section, optionally with seat counts and year qualifiers around them.
var curriculumTokenRE = regexp.MustCompile(`([A-Za-z]{2})-([A-Za-z]+)`)

// curriculumFromReserved maps the "ReservedFor" text to a programme group for
// sections.curriculum. The FIRST token wins: the registrar lists the section's
// main reserved group first, extra groups are top-ups ("SC-IT ปี 3 (80),
// CP-Cy ปี 3 (20)" is an IT section that lends 20 seats).
//
// SC-/CP- prefixes are the college's own programmes and map by suffix — GIS
// deliberately collapses SC-GIS and CP-GIS into one group, same programme in
// two code eras. Any other prefix (BS-*, ...) means another faculty's students
// enrol here; per the 06/08/2026 decision those group as OTHER (คณะอื่น ๆ),
// never guessed into a programme. Empty when no token appears — "unknown" and
// "another faculty" are different answers and the dashboard keeps them apart.
func curriculumFromReserved(reserved string) string {
	m := curriculumTokenRE.FindStringSubmatch(reserved)
	if m == nil {
		return ""
	}
	prefix := strings.ToUpper(m[1])
	if prefix != "SC" && prefix != "CP" {
		return "OTHER"
	}
	switch strings.ToUpper(m[2]) {
	case "CS":
		return "CS"
	case "IT":
		return "IT"
	case "GIS":
		return "GIS"
	case "AI":
		return "AI"
	case "CY":
		return "CY"
	}
	// An SC/CP token we don't recognise is still a college code — a new
	// programme would land here. OTHER would silently file it under another
	// faculty, so unknown is the honest value until the mapping learns it.
	return ""
}

// parseNormalizedSheet reads the Excel body and groups its rows into courses.
// It reports per-row structural warnings via `warnings` — malformed rows are
// simply skipped so a single bad row cannot lose the entire course.
func parseNormalizedSheet(body []byte) (courses []*parsedCourse, warnings []string, err error) {
	f, err := excelize.OpenReader(bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	sheet, raw, err := pickImportSheet(f)
	if err != nil {
		return nil, nil, err
	}
	rows, err := f.GetRows(sheet)
	if err != nil {
		return nil, nil, err
	}
	// Raw registrar export (multi-line cells, header + section rows) is flattened
	// by parseRawRows into the same []*parsedCourse the Normalized branch builds.
	if raw {
		return parseRawRows(rows)
	}
	if len(rows) < 2 {
		return nil, nil, nil
	}
	// Header row → column index. Keys are lower-cased with spaces collapsed.
	headers := map[string]int{}
	for i, h := range rows[0] {
		key := strings.ToLower(strings.TrimSpace(h))
		if key != "" {
			headers[key] = i
		}
	}
	get := func(row []string, key string) string {
		i, ok := headers[key]
		if !ok || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}

	byCode := map[string]*parsedCourse{}
	order := []string{}

	for r := 1; r < len(rows); r++ {
		row := rows[r]
		rawCode := get(row, "coursecode")
		code := strings.ToUpper(strings.TrimSpace(rawCode))
		if code == "" {
			continue
		}
		course, seen := byCode[code]
		if !seen {
			name := get(row, "coursename")
			credits, lec, lab, self := parseUnit(get(row, "unit"))
			course = &parsedCourse{
				code:            code,
				name:            name,
				nameEN:          name,
				level:           courseLevelFromReserved(get(row, "reservedfor")),
				credits:         credits,
				lectureHrs:      lec,
				labHrs:          lab,
				selfHrs:         self,
				officerRaw:      get(row, "officer"),
				sections:        map[string]*parsedSection{},
				sectionsInOrder: []string{},
			}
			byCode[code] = course
			order = append(order, code)
		}
		// Take the first non-empty officer we see for the course — often only
		// row 1 of a course lists the officer; later rows leave it blank.
		if course.officerRaw == "" {
			if o := get(row, "officer"); o != "" {
				course.officerRaw = o
			}
		}
		// A course is graduate if ANY of its sections is reserved for grad
		// students (regular rows say "ตรี", so only upgrade, never downgrade).
		if course.level == "undergrad" && courseLevelFromReserved(get(row, "reservedfor")) == "graduate" {
			course.level = "graduate"
		}

		// Per-course exam dates removed 2026-07-14 — exam ranges now live on
		// academic_terms. Import silently skips these Excel columns.

		rawSection := get(row, "section")
		if rawSection == "" {
			continue
		}
		// "SEC 01" → "1"; also handle "SEC01", "01", etc.
		trimmed := strings.TrimSpace(strings.TrimPrefix(strings.ToUpper(rawSection), "SEC"))
		secNo := strings.TrimLeft(trimmed, "0")
		if secNo == "" {
			secNo = "0"
		}

		sec, ok := course.sections[secNo]
		if !ok {
			track := "regular"
			if strings.Contains(get(row, "reservedfor"), "โครงการพิเศษ") {
				track = "special"
			}
			seats, _ := strconv.Atoi(get(row, "totalseats"))
			sec = &parsedSection{secNo: secNo, track: track, numStudents: seats,
				curriculum: curriculumFromReserved(get(row, "reservedfor"))}
			course.sections[secNo] = sec
			course.sectionsInOrder = append(course.sectionsInOrder, secNo)
		}

		kindRaw := strings.ToLower(get(row, "sessiontype"))
		var kind string
		switch kindRaw {
		case "lec", "lecture":
			kind = "lecture"
		case "lab":
			kind = "lab"
		default:
			warnings = append(warnings, fmt.Sprintf("แถว %d (%s SEC %s): ประเภทคาบ '%s' ไม่รู้จัก ข้าม", r+1, code, secNo, kindRaw))
			continue
		}

		dow, dowOK := dowFromAbbrev(get(row, "day"))
		if !dowOK {
			warnings = append(warnings, fmt.Sprintf("แถว %d (%s SEC %s): วัน '%s' ไม่รู้จัก ข้าม", r+1, code, secNo, get(row, "day")))
			continue
		}
		start, end, tOK := parseTimeRange(get(row, "time"))
		if !tOK {
			warnings = append(warnings, fmt.Sprintf("แถว %d (%s SEC %s): เวลา '%s' อ่านไม่ได้ ข้าม", r+1, code, secNo, get(row, "time")))
			continue
		}
		room := get(row, "room")

		// The section-level room is the room of the first Lec (or first row if
		// no Lec appears). schedule-level room stays authoritative per row.
		if sec.room == "" && kind == "lecture" {
			sec.room = room
		}
		if sec.room == "" {
			sec.room = room
		}
		sec.schedules = append(sec.schedules, parsedSchedule{
			kind: kind, dow: dow, startTime: start, endTime: end, room: room,
		})
	}

	courses = make([]*parsedCourse, 0, len(order))
	for _, code := range order {
		courses = append(courses, byCode[code])
	}
	return courses, warnings, nil
}

// splitLines splits a multi-line Excel cell (Alt+Enter line breaks) into its
// lines, normalising CRLF/CR to LF first.
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.Split(s, "\n")
}

// parseRawRows flattens the raw registrar export ("sysTitle" layout) into the
// same []*parsedCourse the Normalized branch produces. Layout by fixed column:
//
//	A code | B name | C unit | D prereq | E "SEC NN" | F reservedFor |
//	G seats | H day(s) | I "time  kind  room"(s) | J..M exam | N officer
//
// A course spans one header row (col A set) followed by its section rows
// (col E set). The Day (H) and Time (I) cells are multi-line — one line per
// meeting, aligned by index — so a section can hold several Lec/Lab meetings.
func parseRawRows(rows [][]string) (courses []*parsedCourse, warnings []string, err error) {
	cell := func(row []string, i int) string {
		if i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}
	byCode := map[string]*parsedCourse{}
	order := []string{}
	var cur *parsedCourse
	for r := 0; r < len(rows); r++ {
		row := rows[r]
		codeRaw := cell(row, 0)
		if strings.EqualFold(codeRaw, "COURSECODE1") {
			continue // header row
		}
		if codeRaw != "" {
			// Course header row.
			code := strings.ToUpper(codeRaw)
			c, ok := byCode[code]
			if !ok {
				name := cell(row, 1)
				credits, lec, lab, self := parseUnit(cell(row, 2))
				c = &parsedCourse{
					code: code, name: name, nameEN: name, level: "undergrad",
					credits: credits, lectureHrs: lec, labHrs: lab, selfHrs: self,
					sections:        map[string]*parsedSection{},
					sectionsInOrder: []string{},
				}
				byCode[code] = c
				order = append(order, code)
			}
			cur = c
			continue
		}
		// Section row.
		rawSection := cell(row, 4)
		if rawSection == "" || cur == nil {
			continue
		}
		trimmed := strings.TrimSpace(strings.TrimPrefix(strings.ToUpper(rawSection), "SEC"))
		secNo := strings.TrimLeft(trimmed, "0")
		if secNo == "" {
			secNo = "0"
		}
		reserved := cell(row, 5)
		if cur.level == "undergrad" && courseLevelFromReserved(reserved) == "graduate" {
			cur.level = "graduate"
		}
		if cur.officerRaw == "" {
			if o := cell(row, 13); o != "" {
				cur.officerRaw = o
			}
		}
		sec, ok := cur.sections[secNo]
		if !ok {
			track := "regular"
			if strings.Contains(reserved, "โครงการพิเศษ") {
				track = "special"
			}
			seats, _ := strconv.Atoi(cell(row, 6))
			sec = &parsedSection{secNo: secNo, track: track, numStudents: seats,
				curriculum: curriculumFromReserved(reserved)}
			cur.sections[secNo] = sec
			cur.sectionsInOrder = append(cur.sectionsInOrder, secNo)
		}
		// Meetings: Day (col H) and "time kind room" (col I) are multi-line,
		// aligned line-by-line. Blank/WBA time → section with no schedule.
		dayLines := splitLines(cell(row, 7))
		timeLines := splitLines(cell(row, 8))
		for i, meetingRaw := range timeLines {
			meeting := strings.TrimSpace(meetingRaw)
			if meeting == "" {
				continue
			}
			fields := strings.Fields(meeting) // "13:00-16:00" "Lec" "CP9127"
			if len(fields) < 2 {
				continue
			}
			var kind string
			switch strings.ToLower(fields[1]) {
			case "lec", "lecture":
				kind = "lecture"
			case "lab":
				kind = "lab"
			default:
				continue
			}
			room := ""
			if len(fields) >= 3 {
				room = strings.Join(fields[2:], " ")
			}
			dayTok := ""
			if i < len(dayLines) {
				dayTok = strings.TrimSpace(dayLines[i])
			} else if len(dayLines) > 0 {
				dayTok = strings.TrimSpace(dayLines[len(dayLines)-1])
			}
			dow, dowOK := dowFromAbbrev(dayTok)
			if !dowOK {
				warnings = append(warnings, fmt.Sprintf("แถว %d (%s SEC %s): วัน '%s' ไม่รู้จัก ข้าม", r+1, cur.code, secNo, dayTok))
				continue
			}
			start, end, tOK := parseTimeRange(fields[0])
			if !tOK {
				warnings = append(warnings, fmt.Sprintf("แถว %d (%s SEC %s): เวลา '%s' อ่านไม่ได้ ข้าม", r+1, cur.code, secNo, fields[0]))
				continue
			}
			if sec.room == "" {
				sec.room = room
			}
			sec.schedules = append(sec.schedules, parsedSchedule{kind: kind, dow: dow, startTime: start, endTime: end, room: room})
		}
	}
	courses = make([]*parsedCourse, 0, len(order))
	for _, code := range order {
		courses = append(courses, byCode[code])
	}
	return courses, warnings, nil
}

// officerTokens splits the raw Officer cell on whitespace and drops the
// placeholder tokens (อจ.พิเศษ, และคณะ, …).
func officerTokens(raw string) []string {
	fields := strings.Fields(raw)
	out := make([]string, 0, len(fields))
	for _, t := range fields {
		if _, noise := officerNoiseTokens[t]; noise {
			continue
		}
		out = append(out, t)
	}
	return out
}

// matchOfficers looks up each name in the users table. A name that matches
// exactly one active lecturer is auto-assigned; anything else (0 matches or
// >1 ambiguous match) is returned as unmatched so staff can resolve it.
func (s *TeachingService) matchOfficers(ctx context.Context, names []string) (matched []uuid.UUID, unmatched []string, err error) {
	// Non-nil so they marshal as [] instead of null — the import preview UI
	// reads .length on both without a guard.
	matched = []uuid.UUID{}
	unmatched = []string{}
	seen := map[uuid.UUID]struct{}{}
	for _, name := range names {
		rows, err := s.pool.Query(ctx, `
			SELECT u.id FROM users u
			JOIN user_roles r ON r.user_id = u.id AND r.role = 'lecturer'
			WHERE u.first_name = $1 AND u.is_active AND u.deleted_at IS NULL`, name)
		if err != nil {
			return nil, nil, err
		}
		var ids []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, nil, err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if len(ids) == 1 {
			if _, dup := seen[ids[0]]; !dup {
				matched = append(matched, ids[0])
				seen[ids[0]] = struct{}{}
			}
		} else {
			// 0 = truly missing, >1 = ambiguous — both surface to staff as
			// unmatched so they can pick.
			unmatched = append(unmatched, name)
		}
	}
	return matched, unmatched, nil
}

// PreviewImport parses the file and reports per-course what CommitImport
// would do. It performs no writes.
func (s *TeachingService) PreviewImport(ctx context.Context, actor uuid.UUID, termID uuid.UUID, filename string, body []byte) (*ImportPreview, error) {
	priv, err := isPrivileged(ctx, s.pool, actor)
	if err != nil {
		return nil, err
	}
	if !priv {
		return nil, ErrForbidden
	}
	courses, _, err := parseNormalizedSheet(body)
	if err != nil {
		return nil, err
	}
	out := &ImportPreview{Filename: filename, Courses: make([]ImportPreviewCourse, 0, len(courses))}
	for _, c := range courses {
		schedCount := 0
		for _, sec := range c.sections {
			schedCount += len(sec.schedules)
		}
		tokens := officerTokens(c.officerRaw)
		row := ImportPreviewCourse{
			Code:          c.code,
			Name:          c.name,
			SectionCount:  len(c.sections),
			ScheduleCount: schedCount,
			OfficerRaw:    c.officerRaw,
			OfficerNames:  tokens,
			// Default to empty slices: an "existing" course returns before
			// matchOfficers runs, and a nil slice would reach the client as
			// JSON null and crash the preview table.
			MatchedLecturerIDs: []uuid.UUID{},
			UnmatchedNames:     []string{},
		}
		// Course identity comes from the file — nothing to pre-populate. A course
		// is "existing" only when it was already imported into THIS term.
		var existingID uuid.UUID
		err := s.pool.QueryRow(ctx,
			`SELECT id FROM teaching_courses WHERE term_id = $1 AND code = $2`, termID, c.code).Scan(&existingID)
		if err == nil {
			row.Status = "existing"
			out.ExistingCount++
			out.Courses = append(out.Courses, row)
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		matched, unmatched, err := s.matchOfficers(ctx, tokens)
		if err != nil {
			return nil, err
		}
		row.MatchedLecturerIDs = matched
		row.UnmatchedNames = unmatched
		if len(unmatched) > 0 {
			row.Status = "unmatched_officer"
			out.BlockedCount++
		} else {
			row.Status = "new"
			out.NewCount++
		}
		out.Courses = append(out.Courses, row)
	}
	return out, nil
}

// CommitImport writes the courses that pass PreviewImport's checks. Each
// course is written in its own tx: one failed course never blocks the rest of
// the file. Codes listed in skipCodes are ignored, letting staff resolve
// unmatched-officer rows preview-side.
func (s *TeachingService) CommitImport(ctx context.Context, actor uuid.UUID, termID uuid.UUID, filename string, body []byte, skipCodes []string) (*ImportResult, error) {
	priv, err := isPrivileged(ctx, s.pool, actor)
	if err != nil {
		return nil, err
	}
	if !priv {
		return nil, ErrForbidden
	}
	courses, warnings, err := parseNormalizedSheet(body)
	if err != nil {
		return nil, err
	}
	res := &ImportResult{Errors: warnings, ErrorCount: len(warnings)}
	skipSet := map[string]struct{}{}
	for _, c := range skipCodes {
		if v := strings.ToUpper(strings.TrimSpace(c)); v != "" {
			skipSet[v] = struct{}{}
		}
	}

	for _, c := range courses {
		res.RowCount++
		if _, skip := skipSet[c.code]; skip {
			res.SkippedCodes = append(res.SkippedCodes, c.code)
			continue
		}
		id, err := s.commitOneCourse(ctx, actor, termID, c)
		if err != nil {
			// Skip signals a benign non-creation (already exists in this term).
			// Only real errors bump the error count.
			if err == errImportSkipped {
				res.SkippedCodes = append(res.SkippedCodes, c.code)
				continue
			}
			res.ErrorCount++
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", c.code, err))
			continue
		}
		res.CreatedIDs = append(res.CreatedIDs, id)
	}

	summary := map[string]any{
		"row_count":     res.RowCount,
		"created_count": len(res.CreatedIDs),
		"skipped_count": len(res.SkippedCodes),
		"error_count":   res.ErrorCount,
	}
	// The import ledger row and its audit entry describe the same run, so they
	// go in together. The ledger write used to be `_, _ =` — discarded outright,
	// which meant a failed insert left the import unrecorded and unnoticed.
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "schedule.import", Entity: "term", EntityID: termID.String(), After: summary},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO schedule_imports (id, imported_by, filename, row_count, error_count, summary, at)
				 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
				uuid.New(), actor, filename, res.RowCount, res.ErrorCount, summary, time.Now())
			return err
		}); err != nil {
		return nil, err
	}
	return res, nil
}

// errImportSkipped signals commitOneCourse chose not to create the row because
// the course was already imported into this term. The caller records the code
// in SkippedCodes without incrementing ErrorCount.
var errImportSkipped = errors.New("skipped")

func (s *TeachingService) commitOneCourse(ctx context.Context, actor, termID uuid.UUID, c *parsedCourse) (uuid.UUID, error) {
	// Identity comes from the file; a course is unique within a term by code.
	var existing uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM teaching_courses WHERE term_id = $1 AND code = $2`, termID, c.code).Scan(&existing)
	if err == nil {
		// The course itself is untouched, but a re-import is the designated way
		// to BACKFILL sections.curriculum for terms imported before the column
		// existed. Only NULLs are filled — a staff override must survive the
		// registrar file being uploaded again.
		for _, secNo := range c.sectionsInOrder {
			sec := c.sections[secNo]
			if sec.curriculum == "" {
				continue
			}
			if _, err := s.pool.Exec(ctx,
				`UPDATE sections SET curriculum = $1
				  WHERE teaching_course_id = $2 AND sec_no = $3 AND curriculum IS NULL`,
				sec.curriculum, existing, sec.secNo); err != nil {
				return uuid.Nil, err
			}
		}
		return uuid.Nil, errImportSkipped
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	matched, _, err := s.matchOfficers(ctx, officerTokens(c.officerRaw))
	if err != nil {
		return uuid.Nil, err
	}

	nameTH := strings.TrimSpace(c.name)
	if nameTH == "" {
		nameTH = c.code
	}
	var nameEN *string
	if v := strings.TrimSpace(c.nameEN); v != "" {
		nameEN = &v
	}
	level := c.level
	if level != "graduate" {
		level = "undergrad"
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)

	id := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO teaching_courses (
			id, term_id, code, name_th, name_en, level,
			credits, lecture_hrs, lab_hrs, self_hrs, created_by
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		id, termID, c.code, nameTH, nameEN, level,
		c.credits, c.lectureHrs, c.labHrs, c.selfHrs, actor); err != nil {
		return uuid.Nil, err
	}
	for i, lid := range matched {
		if _, err := tx.Exec(ctx,
			`INSERT INTO teaching_lecturers (teaching_course_id, lecturer_id, is_primary) VALUES ($1,$2,$3)
			 ON CONFLICT DO NOTHING`, id, lid, i == 0); err != nil {
			return uuid.Nil, err
		}
	}

	var sumRegular, sumSpecial int
	for _, secNo := range c.sectionsInOrder {
		sec := c.sections[secNo]
		secID := uuid.New()
		var roomPtr *string
		if sec.room != "" {
			r := sec.room
			roomPtr = &r
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO sections (id, teaching_course_id, sec_no, track, room, num_students, curriculum)
			 VALUES ($1,$2,$3,$4::section_track,$5,$6,$7)`,
			secID, id, sec.secNo, sec.track, roomPtr, sec.numStudents,
			emptyToNil(sec.curriculum)); err != nil {
			return uuid.Nil, err
		}
		if sec.track == "special" {
			sumSpecial += sec.numStudents
		} else {
			sumRegular += sec.numStudents
		}
		// The registrar file is the source of truth: a section may legitimately
		// hold several lab meetings (multiple rooms) and its session labels may
		// not line up with the course's credit split. We trust the file and
		// insert every meeting as-is rather than applying the manual-entry
		// credit-gate (validateScheduleKinds), which would reject those rows.
		for _, sch := range sec.schedules {
			var schRoomPtr *string
			if sch.room != "" {
				r := sch.room
				schRoomPtr = &r
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO section_schedules (id, section_id, kind, day_of_week, start_time, end_time, room)
				 VALUES ($1,$2,$3,$4,$5::time,$6::time,$7)`,
				uuid.New(), secID, sch.kind, sch.dow, sch.startTime, sch.endTime, schRoomPtr); err != nil {
				return uuid.Nil, err
			}
		}
	}
	if sumRegular+sumSpecial > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE teaching_courses SET num_students=$1, num_students_regular=$2, num_students_special=$3
			 WHERE id=$4`, sumRegular+sumSpecial, sumRegular, sumSpecial, id); err != nil {
			return uuid.Nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// emptyToNil turns a raw string ("" ⇒ nil, else pointer). Used for optional
// date/text columns in INSERT statements.
func emptyToNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ImportScheduleExcel is a thin backward-compatibility wrapper. New callers
// should use PreviewImport + CommitImport. The wrapper commits without any
// skip list — equivalent to "import all novel courses, skip everything else".
func (s *TeachingService) ImportScheduleExcel(ctx context.Context, actor uuid.UUID, termID uuid.UUID, filename string, body []byte) (*ImportResult, error) {
	return s.CommitImport(ctx, actor, termID, filename, body, nil)
}

// Terms
type Term struct {
	ID           uuid.UUID `json:"id"`
	AcademicYear int       `json:"academic_year"`
	Semester     int       `json:"semester"`
	StartsOn     *string   `json:"starts_on,omitempty"`
	EndsOn       *string   `json:"ends_on,omitempty"`
	// Faculty-published exam windows. Worklog entries falling inside either
	// range are rejected — the university closes the ledger during exams.
	// Required when creating/updating a term.
	MidtermStartsOn *string `json:"midterm_starts_on,omitempty"`
	MidtermEndsOn   *string `json:"midterm_ends_on,omitempty"`
	FinalStartsOn   *string `json:"final_starts_on,omitempty"`
	FinalEndsOn     *string `json:"final_ends_on,omitempty"`
	Months          int     `json:"months"`
	IsActive        bool    `json:"is_active"`
}

// TermFilter narrows a ListTerms query. All fields optional; nil means no
// filter. Used to keep the /terms payload small when the settings UI only
// wants to render a slice of years at a time.
type TermFilter struct {
	Year     *int // exact match
	YearFrom *int // inclusive lower bound
	YearTo   *int // inclusive upper bound
}

func (s *TeachingService) ListTerms(ctx context.Context, f TermFilter) ([]Term, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, academic_year, semester,
		        TO_CHAR(starts_on,'YYYY-MM-DD'), TO_CHAR(ends_on,'YYYY-MM-DD'),
		        TO_CHAR(midterm_starts_on,'YYYY-MM-DD'), TO_CHAR(midterm_ends_on,'YYYY-MM-DD'),
		        TO_CHAR(final_starts_on,'YYYY-MM-DD'),   TO_CHAR(final_ends_on,'YYYY-MM-DD'),
		        months, is_active
		 FROM academic_terms
		 WHERE ($1::int IS NULL OR academic_year = $1::int)
		   AND ($2::int IS NULL OR academic_year >= $2::int)
		   AND ($3::int IS NULL OR academic_year <= $3::int)
		 ORDER BY academic_year DESC, semester DESC`,
		f.Year, f.YearFrom, f.YearTo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Term{}
	for rows.Next() {
		var t Term
		if err := rows.Scan(
			&t.ID, &t.AcademicYear, &t.Semester,
			&t.StartsOn, &t.EndsOn,
			&t.MidtermStartsOn, &t.MidtermEndsOn,
			&t.FinalStartsOn, &t.FinalEndsOn,
			&t.Months, &t.IsActive,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// TermYearsCount returns the total number of distinct academic years. The
// settings UI reads this to show "N ปี" in the counter without needing the
// full term list up-front.
func (s *TeachingService) TermYearsCount(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(DISTINCT academic_year) FROM academic_terms`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// demoteOtherActiveTerms clears is_active on every term except keep, so that
// promoting one term automatically retires the previous current term.
//
// "ภาคเรียนปัจจุบัน" is a system-wide singleton — the term switcher, the default
// on every screen, and the windows that open "this term" all read it. Without
// this the box could be ticked on a second term while the first stayed ticked,
// and which one won depended on row order.
//
// It must run in the SAME transaction as the write that promotes `keep`, and
// before it: ux_academic_terms_single_active is a non-deferrable unique index,
// so a promotion landing while the old row is still active is rejected outright.
// Called with active=false it does nothing — unticking the only current term is
// allowed, since a year can end before the next one is set up.
func demoteOtherActiveTerms(ctx context.Context, tx pgx.Tx, keep uuid.UUID, active bool) error {
	if !active {
		return nil
	}
	_, err := tx.Exec(ctx,
		`UPDATE academic_terms SET is_active = FALSE WHERE is_active AND id <> $1`, keep)
	return err
}

// UpsertTerm creates a new term (when in.ID is nil) or updates an existing
// one by ID. On update, academic_year and semester are LOCKED — changing
// them would orphan any teaching_courses / schedules / budgets that already
// reference this term. Callers should delete + recreate if they truly need
// to rename the (year, semester) key.
func (s *TeachingService) UpsertTerm(ctx context.Context, actor uuid.UUID, in Term) (*Term, error) {
	if in.AcademicYear < 2500 || in.AcademicYear > 2700 {
		return nil, ErrInvalidInput
	}
	if in.Semester < 1 || in.Semester > 3 {
		return nil, ErrInvalidInput
	}
	if in.Months == 0 {
		in.Months = 4
	}
	if in.Months < 1 || in.Months > 12 {
		return nil, ErrInvalidInput
	}
	if in.StartsOn != nil && in.EndsOn != nil && *in.StartsOn != "" && *in.EndsOn != "" && *in.StartsOn >= *in.EndsOn {
		return nil, ErrInvalidInput
	}
	// Exam windows are required and must be closed intervals (start <= end).
	// Faculty publishes these once per term — no partial saves.
	if !nonEmpty(in.MidtermStartsOn) || !nonEmpty(in.MidtermEndsOn) ||
		!nonEmpty(in.FinalStartsOn) || !nonEmpty(in.FinalEndsOn) {
		return nil, ErrInvalidInput
	}
	if *in.MidtermStartsOn > *in.MidtermEndsOn || *in.FinalStartsOn > *in.FinalEndsOn {
		return nil, ErrInvalidInput
	}

	if in.ID == uuid.Nil {
		in.ID = uuid.New()
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM academic_terms WHERE academic_year=$1 AND semester=$2)`,
			in.AcademicYear, in.Semester).Scan(&exists); err != nil {
			return nil, err
		}
		if exists {
			return nil, ErrConflict
		}
		if err := writeAudited(ctx, s.pool, s.aud,
			audit.Entry{ActorID: &actor, Action: "term.create", Entity: "term", EntityID: in.ID.String(), After: in},
			func(tx pgx.Tx) error {
				if err := demoteOtherActiveTerms(ctx, tx, in.ID, in.IsActive); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`INSERT INTO academic_terms
					   (id, academic_year, semester, starts_on, ends_on,
					    midterm_starts_on, midterm_ends_on, final_starts_on, final_ends_on,
					    months, is_active)
					 VALUES ($1,$2,$3,$4::date,$5::date,$6::date,$7::date,$8::date,$9::date,$10,$11)`,
					in.ID, in.AcademicYear, in.Semester,
					nilStr(in.StartsOn), nilStr(in.EndsOn),
					nilStr(in.MidtermStartsOn), nilStr(in.MidtermEndsOn),
					nilStr(in.FinalStartsOn), nilStr(in.FinalEndsOn),
					in.Months, in.IsActive)
				return err
			}); err != nil {
			return nil, err
		}
		return &in, nil
	}

	// Update by ID — reject if year/semester changed (they are the durable identity).
	var existYear, existSem int
	if err := s.pool.QueryRow(ctx,
		`SELECT academic_year, semester FROM academic_terms WHERE id=$1`, in.ID).Scan(&existYear, &existSem); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if existYear != in.AcademicYear || existSem != in.Semester {
		return nil, ErrConflict
	}
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "term.update", Entity: "term", EntityID: in.ID.String(), After: in},
		func(tx pgx.Tx) error {
			if err := demoteOtherActiveTerms(ctx, tx, in.ID, in.IsActive); err != nil {
				return err
			}
			_, err := tx.Exec(ctx,
				`UPDATE academic_terms SET
				   starts_on=$2::date, ends_on=$3::date,
				   midterm_starts_on=$4::date, midterm_ends_on=$5::date,
				   final_starts_on=$6::date,   final_ends_on=$7::date,
				   months=$8, is_active=$9
				 WHERE id=$1`,
				in.ID,
				nilStr(in.StartsOn), nilStr(in.EndsOn),
				nilStr(in.MidtermStartsOn), nilStr(in.MidtermEndsOn),
				nilStr(in.FinalStartsOn), nilStr(in.FinalEndsOn),
				in.Months, in.IsActive)
			return err
		}); err != nil {
		return nil, err
	}
	return &in, nil
}

// nonEmpty returns true if the pointer is non-nil and its string is non-empty.
func nonEmpty(s *string) bool { return s != nil && *s != "" }

// TermUsage summarizes how many rows reference this term. Used to preview
// the blast radius before a delete or to prevent it.
type TermUsage struct {
	TeachingCourses int `json:"teaching_courses"`
	ClassSchedules  int `json:"class_schedules"`
	Exports         int `json:"exports"`
	RequestWindows  int `json:"request_windows"`
}

// Blocking returns true if any references would be orphaned by a delete.
//
// The counted tables are exactly the ones whose FK to academic_terms is
// ON DELETE NO ACTION — with any of them present the DELETE fails inside
// Postgres, so this set has to mirror them or the user gets a 500 instead of a
// reason. Everything else that points at a term (request windows, submission
// periods, appointment orders, document progress, signature checklist)
// CASCADEs, so it doesn't block.
func (u TermUsage) Blocking() bool {
	return u.TeachingCourses+u.ClassSchedules+u.Exports > 0
}

func (s *TeachingService) TermUsage(ctx context.Context, id uuid.UUID) (*TermUsage, error) {
	u := &TermUsage{}
	err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM teaching_courses    WHERE term_id = $1),
		  (SELECT COUNT(*) FROM ta_class_schedules  WHERE term_id = $1),
		  (SELECT COUNT(*) FROM exports             WHERE term_id = $1),
		  (SELECT COUNT(*) FROM ta_request_windows  WHERE term_id = $1)
	`, id).Scan(&u.TeachingCourses, &u.ClassSchedules, &u.Exports, &u.RequestWindows)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// DeleteTerm removes a term only when no dependent rows would be orphaned.
// Request windows attached to the term cascade away automatically.
func (s *TeachingService) DeleteTerm(ctx context.Context, actor uuid.UUID, id uuid.UUID) (*TermUsage, error) {
	var t Term
	if err := s.pool.QueryRow(ctx,
		`SELECT id, academic_year, semester, months, is_active FROM academic_terms WHERE id=$1`, id).
		Scan(&t.ID, &t.AcademicYear, &t.Semester, &t.Months, &t.IsActive); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	usage, err := s.TermUsage(ctx, id)
	if err != nil {
		return nil, err
	}
	if usage.Blocking() {
		return usage, ErrConflict
	}
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "term.delete", Entity: "term", EntityID: id.String(), Before: t},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM academic_terms WHERE id=$1`, id)
			return err
		}); err != nil {
		return nil, err
	}
	return usage, nil
}

func nilStr(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

// unused import guard
var _ io.Reader = (io.Reader)(nil)

// LecturerSupervisesTA reports whether the lecturer has this TA assigned in
// any of their courses (either as the requesting lecturer or as a co-lecturer
// on the course). Used to scope the timetable form: the form is a personal
// weekly schedule, so "is a lecturer" alone must not open it.
func (s *TeachingService) LecturerSupervisesTA(ctx context.Context, lecturerID, taID uuid.UUID) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM ta_request_assignments a
			JOIN ta_requests r ON r.id = a.request_id
			LEFT JOIN teaching_lecturers tl ON tl.teaching_course_id = r.teaching_course_id
			WHERE a.ta_id = $1
			  AND (r.lecturer_id = $2 OR tl.lecturer_id = $2)
		)`, taID, lecturerID).Scan(&ok)
	return ok, err
}
