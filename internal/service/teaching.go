package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"

	"ta-payment-back/internal/audit"
)

type TeachingCourse struct {
	ID                 uuid.UUID `json:"id"`
	FacultyCourseID    uuid.UUID `json:"faculty_course_id"`
	TermID             uuid.UUID `json:"term_id"`
	Code               string    `json:"code"`
	NameTH             string    `json:"name_th"`
	StartsOn           *string   `json:"starts_on,omitempty"`
	EndsOn             *string   `json:"ends_on,omitempty"`
	MidtermLectureDate *string   `json:"midterm_lecture_date,omitempty"`
	MidtermLabDate     *string   `json:"midterm_lab_date,omitempty"`
	FinalLectureDate   *string   `json:"final_lecture_date,omitempty"`
	FinalLabDate       *string   `json:"final_lab_date,omitempty"`
	NumStudents        int       `json:"num_students"`         // aggregate (regular + special)
	NumStudentsRegular int       `json:"num_students_regular"`
	NumStudentsSpecial int       `json:"num_students_special"`
	// ExportedAt is set the first time staff builds the export zip for this
	// course. Once set, section list and per-section student counts are
	// frozen — the export file is considered the source of truth.
	ExportedAt         *string   `json:"exported_at,omitempty"`
	Sections           []Section `json:"sections,omitempty"`
	Lecturers       []struct {
		ID        uuid.UUID `json:"id"`
		FirstName string    `json:"first_name"`
		LastName  string    `json:"last_name"`
		IsPrimary bool      `json:"is_primary"`
	} `json:"lecturers,omitempty"`
}

type Section struct {
	ID               uuid.UUID          `json:"id"`
	TeachingCourseID uuid.UUID          `json:"teaching_course_id"`
	SecNo            string             `json:"sec_no"`
	Track            string             `json:"track"`
	Room             *string            `json:"room,omitempty"`
	NumStudents      int                `json:"num_students"`
	Schedules        []SectionSchedule  `json:"schedules,omitempty"`
	Exams            []ExamSchedule     `json:"exams,omitempty"`
	Makeups          []MakeupSchedule   `json:"makeups,omitempty"`
	Reviews          []LectureReview    `json:"reviews,omitempty"`
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
	StartTime    *string   `json:"start_time,omitempty"`
	EndTime      *string   `json:"end_time,omitempty"`
	Note         *string   `json:"note,omitempty"`
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
	pool *pgxpool.Pool
	aud  *audit.Auditor
}

// Create a teaching course with sections + schedules in one transaction.
type CreateTeachingCourseInput struct {
	FacultyCourseID    uuid.UUID `json:"faculty_course_id"`
	TermID             uuid.UUID `json:"term_id"`
	StartsOn           *string   `json:"starts_on,omitempty"`
	EndsOn             *string   `json:"ends_on,omitempty"`
	MidtermLectureDate *string   `json:"midterm_lecture_date,omitempty"`
	MidtermLabDate     *string   `json:"midterm_lab_date,omitempty"`
	FinalLectureDate   *string   `json:"final_lecture_date,omitempty"`
	FinalLabDate       *string   `json:"final_lab_date,omitempty"`
	NumStudents        int       `json:"num_students"`
	LecturerIDs        []uuid.UUID `json:"lecturer_ids"`
	Sections           []struct {
		SecNo       string           `json:"sec_no"`
		Track       string           `json:"track"`
		Room        *string          `json:"room,omitempty"`
		NumStudents int              `json:"num_students"`
		Schedules   []SectionSchedule `json:"schedules,omitempty"`
		Exams       []ExamSchedule    `json:"exams,omitempty"`
	} `json:"sections"`
}

func (s *TeachingService) Create(ctx context.Context, actor uuid.UUID, in CreateTeachingCourseInput) (uuid.UUID, error) {
	if in.FacultyCourseID == uuid.Nil || in.TermID == uuid.Nil {
		return uuid.Nil, ErrInvalidInput
	}
	for _, sec := range in.Sections {
		if err := validateSectionSchedules(sec.Schedules); err != nil {
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
			id, faculty_course_id, term_id, starts_on, ends_on,
			midterm_lecture_date, midterm_lab_date, final_lecture_date, final_lab_date,
			num_students, created_by
		 ) VALUES ($1,$2,$3,$4::date,$5::date,$6::date,$7::date,$8::date,$9::date,$10,$11)`,
		id, in.FacultyCourseID, in.TermID,
		nilStr(in.StartsOn), nilStr(in.EndsOn),
		nilStr(in.MidtermLectureDate), nilStr(in.MidtermLabDate),
		nilStr(in.FinalLectureDate), nilStr(in.FinalLabDate),
		in.NumStudents, actor)
	if err != nil {
		return uuid.Nil, err
	}
	// Auto-add the actor as primary lecturer if the caller didn't pass an
	// explicit lecturer list — this is the "lecturer opens their own course"
	// path. Staff/admin passing LecturerIDs keep full control.
	lecturerIDs := in.LecturerIDs
	if len(lecturerIDs) == 0 {
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
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "teaching_course.create", Entity: "teaching_course", EntityID: id.String(), After: in})
	return id, nil
}

func (s *TeachingService) Get(ctx context.Context, id uuid.UUID) (*TeachingCourse, error) {
	tc := &TeachingCourse{}
	err := s.pool.QueryRow(ctx,
		`SELECT tc.id, tc.faculty_course_id, tc.term_id, fc.code, fc.name_th,
		        TO_CHAR(tc.starts_on,'YYYY-MM-DD'), TO_CHAR(tc.ends_on,'YYYY-MM-DD'),
		        TO_CHAR(tc.midterm_lecture_date,'YYYY-MM-DD'),
		        TO_CHAR(tc.midterm_lab_date,'YYYY-MM-DD'),
		        TO_CHAR(tc.final_lecture_date,'YYYY-MM-DD'),
		        TO_CHAR(tc.final_lab_date,'YYYY-MM-DD'),
		        tc.num_students, tc.num_students_regular, tc.num_students_special,
		        TO_CHAR(tc.exported_at,'YYYY-MM-DD"T"HH24:MI:SSTZ')
		 FROM teaching_courses tc JOIN faculty_courses fc ON fc.id = tc.faculty_course_id
		 WHERE tc.id = $1`, id).Scan(&tc.ID, &tc.FacultyCourseID, &tc.TermID, &tc.Code, &tc.NameTH,
		&tc.StartsOn, &tc.EndsOn,
		&tc.MidtermLectureDate, &tc.MidtermLabDate, &tc.FinalLectureDate, &tc.FinalLabDate,
		&tc.NumStudents, &tc.NumStudentsRegular, &tc.NumStudentsSpecial,
		&tc.ExportedAt)
	if err != nil {
		return nil, err
	}
	// Sections
	rows, err := s.pool.Query(ctx,
		`SELECT id, sec_no, track::text, room, num_students FROM sections WHERE teaching_course_id=$1 ORDER BY sec_no`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sec Section
		if err := rows.Scan(&sec.ID, &sec.SecNo, &sec.Track, &sec.Room, &sec.NumStudents); err != nil {
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
	return tc, nil
}

func (s *TeachingService) List(ctx context.Context, termID *uuid.UUID, lecturerID *uuid.UUID) ([]TeachingCourse, error) {
	q := `SELECT tc.id, tc.faculty_course_id, tc.term_id, fc.code, fc.name_th,
	             tc.num_students, tc.num_students_regular, tc.num_students_special
	      FROM teaching_courses tc JOIN faculty_courses fc ON fc.id=tc.faculty_course_id`
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
	q += " ORDER BY fc.code"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TeachingCourse
	for rows.Next() {
		var tc TeachingCourse
		if err := rows.Scan(&tc.ID, &tc.FacultyCourseID, &tc.TermID, &tc.Code, &tc.NameTH,
			&tc.NumStudents, &tc.NumStudentsRegular, &tc.NumStudentsSpecial); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, nil
}

// ListForTA returns teaching courses where the given TA is assigned via an
// approved TA request. Optionally filtered by term.
func (s *TeachingService) ListForTA(ctx context.Context, taID uuid.UUID, termID *uuid.UUID) ([]TeachingCourse, error) {
	q := `SELECT DISTINCT tc.id, tc.faculty_course_id, tc.term_id, fc.code, fc.name_th,
	             tc.num_students, tc.num_students_regular, tc.num_students_special
	      FROM ta_request_assignments a
	      JOIN sections s ON s.id = a.section_id
	      JOIN teaching_courses tc ON tc.id = s.teaching_course_id
	      JOIN faculty_courses fc ON fc.id = tc.faculty_course_id
	      JOIN ta_requests r ON r.id = a.request_id
	      WHERE a.ta_id = $1 AND r.status = 'approved'`
	args := []any{taID}
	if termID != nil {
		q += " AND tc.term_id = $2"
		args = append(args, *termID)
	}
	q += " ORDER BY fc.code"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TeachingCourse
	for rows.Next() {
		var tc TeachingCourse
		if err := rows.Scan(&tc.ID, &tc.FacultyCourseID, &tc.TermID, &tc.Code, &tc.NameTH,
			&tc.NumStudents, &tc.NumStudentsRegular, &tc.NumStudentsSpecial); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, nil
}

// SetNumStudents updates aggregate + per-track counts. Callers may pass -1 for
// a field they don't want to change (current value is kept).
func (s *TeachingService) SetNumStudents(ctx context.Context, actor, id uuid.UUID, total, regular, special int) error {
	if err := assertCourseManager(ctx, s.pool, actor, id); err != nil {
		return err
	}
	// Fetch current values so we can preserve untouched fields.
	var curTotal, curRegular, curSpecial int
	if err := s.pool.QueryRow(ctx,
		`SELECT num_students, num_students_regular, num_students_special
		 FROM teaching_courses WHERE id = $1`, id).Scan(&curTotal, &curRegular, &curSpecial); err != nil {
		return err
	}
	if regular < 0 { regular = curRegular }
	if special < 0 { special = curSpecial }
	// Per-track is source of truth for the aggregate; ignore an explicit total
	// unless neither track was provided.
	if regular != curRegular || special != curSpecial {
		total = regular + special
	} else if total < 0 {
		total = curTotal
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE teaching_courses
		SET num_students = $1,
		    num_students_regular = $2,
		    num_students_special = $3,
		    updated_at = NOW()
		WHERE id = $4`, total, regular, special, id)
	if err == nil {
		s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "teaching_course.num_students",
			Entity: "teaching_course", EntityID: id.String(),
			After: map[string]int{"num_students": total, "regular": regular, "special": special}})
	}
	return err
}

// UpdateSettingsInput carries only the fields a lecturer is allowed to edit
// on the course-settings page. Course code / name / credits come from
// faculty_courses and stay read-only. Pointer semantics: nil = leave alone;
// an empty string clears the DB column (sets to NULL).
type UpdateSettingsInput struct {
	StartsOn           *string `json:"starts_on,omitempty"`
	EndsOn             *string `json:"ends_on,omitempty"`
	MidtermLectureDate *string `json:"midterm_lecture_date,omitempty"`
	MidtermLabDate     *string `json:"midterm_lab_date,omitempty"`
	FinalLectureDate   *string `json:"final_lecture_date,omitempty"`
	FinalLabDate       *string `json:"final_lab_date,omitempty"`
}

func (s *TeachingService) UpdateSettings(ctx context.Context, actor, id uuid.UUID, in UpdateSettingsInput) error {
	if err := assertCourseManager(ctx, s.pool, actor, id); err != nil {
		return err
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
	add("midterm_lecture_date", in.MidtermLectureDate)
	add("midterm_lab_date", in.MidtermLabDate)
	add("final_lecture_date", in.FinalLectureDate)
	add("final_lab_date", in.FinalLabDate)
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = NOW()")
	args = append(args, id)
	q := fmt.Sprintf("UPDATE teaching_courses SET %s WHERE id = $%d", strings.Join(sets, ", "), i)
	if _, err := s.pool.Exec(ctx, q, args...); err != nil {
		return err
	}
	s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "teaching_course.update_settings",
		Entity: "teaching_course", EntityID: id.String(), After: in,
	})
	return nil
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

// AddSection adds a new section to a course. Returns ErrCourseLocked if the
// course has already been exported. Aggregate counts are recomputed on success.
func (s *TeachingService) AddSection(ctx context.Context, actor, tcID uuid.UUID, in AddSectionInput) (uuid.UUID, error) {
	if err := assertCourseManager(ctx, s.pool, actor, tcID); err != nil {
		return uuid.Nil, err
	}
	if in.SecNo == "" || (in.Track != "regular" && in.Track != "special") {
		return uuid.Nil, ErrInvalidInput
	}
	if in.NumStudents < 0 {
		return uuid.Nil, Invalid("จำนวนนักศึกษาต้องไม่ติดลบ")
	}
	if err := validateSectionSchedules(in.Schedules); err != nil {
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
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "section.add",
		Entity: "teaching_course", EntityID: tcID.String(), After: in})
	return id, nil
}

// ReplaceSectionSchedules atomically clears and rewrites the schedule rows for
// one section. Gated on the course not being exported. Rejects unknown kinds,
// out-of-range days, malformed times, and any pair of blocks that overlap
// within the same day.
func (s *TeachingService) ReplaceSectionSchedules(ctx context.Context, actor, tcID, sectionID uuid.UUID, schedules []SectionSchedule) error {
	if err := assertCourseManager(ctx, s.pool, actor, tcID); err != nil {
		return err
	}
	if err := validateSectionSchedules(schedules); err != nil {
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
	var owned bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM sections WHERE id=$1 AND teaching_course_id=$2)`,
		sectionID, tcID).Scan(&owned); err != nil {
		return err
	}
	if !owned {
		return ErrNotFound
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
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "section.schedules.replace",
		Entity: "section", EntityID: sectionID.String(), After: schedules})
	return nil
}

// validateSectionSchedules enforces the same rules the lecturer UI enforces so
// stray API callers can't skip them. Only intra-section overlap is checked —
// cross-section conflicts are the lecturer's judgement call and are surfaced
// later when TAs try to take a request that would collide.
func validateSectionSchedules(schedules []SectionSchedule) error {
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
	}
	return nil
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
}

// UpdateSection edits sec_no / room / num_students on a single section.
// Track is intentionally not editable — switching regular↔special would
// invalidate any budget/request math already based on the old track.
func (s *TeachingService) UpdateSection(ctx context.Context, actor, tcID, sectionID uuid.UUID, in UpdateSectionInput) error {
	if err := assertCourseManager(ctx, s.pool, actor, tcID); err != nil {
		return err
	}
	if in.NumStudents != nil && *in.NumStudents < 0 {
		return Invalid("จำนวนนักศึกษาต้องไม่ติดลบ")
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
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "section.update",
		Entity: "section", EntityID: sectionID.String(), After: in})
	return nil
}

// DeleteSection removes a section. Locked after export. Aggregate is
// recomputed. FK violations (e.g. sections still referenced by TA request
// assignments) surface as-is to the caller.
func (s *TeachingService) DeleteSection(ctx context.Context, actor, tcID, sectionID uuid.UUID) error {
	if err := assertCourseManager(ctx, s.pool, actor, tcID); err != nil {
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
	if _, err := tx.Exec(ctx,
		`DELETE FROM sections WHERE id=$1 AND teaching_course_id=$2`, sectionID, tcID); err != nil {
		return err
	}
	if err := s.recomputeAggregate(ctx, tx, tcID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "section.delete",
		Entity: "section", EntityID: sectionID.String()})
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
	tag, err := s.pool.Exec(ctx,
		`UPDATE teaching_courses SET exported_at = NULL WHERE id = $1 AND exported_at IS NOT NULL`, tcID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return Invalid("รายวิชานี้ยังไม่ได้ส่งออก จึงไม่มีอะไรให้ปลดล็อก")
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "course.unexport", Entity: "teaching_course", EntityID: tcID.String()})
	return nil
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
	if err := assertCourseManager(ctx, s.pool, actor, tcID); err != nil {
		return err
	}
	if err := s.assertNotExported(ctx, nil, tcID); err != nil {
		return err
	}
	if _, err := time.Parse("2006-01-02", m.OriginalDate); err != nil {
		return Invalid("รูปแบบวันที่ไม่ถูกต้อง")
	}
	if _, err := time.Parse("2006-01-02", m.MakeupDate); err != nil {
		return Invalid("รูปแบบวันที่ไม่ถูกต้อง")
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
	_, err := s.pool.Exec(ctx,
		`INSERT INTO makeup_schedules (id, section_id, original_date, makeup_date, start_time, end_time, note)
		 VALUES ($1,$2,$3::date,$4::date,$5,$6,$7)`,
		uuid.New(), sectionID, m.OriginalDate, m.MakeupDate, m.StartTime, m.EndTime, m.Note)
	if err == nil {
		s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "makeup.add", Entity: "section", EntityID: sectionID.String(), After: m})
	}
	return err
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
	_, err := s.pool.Exec(ctx,
		`INSERT INTO lecture_review_dates (id, section_id, review_date, start_time, end_time, hours, note)
		 VALUES ($1,$2,$3::date,$4,$5,$6,$7)`,
		uuid.New(), sectionID, r.ReviewDate, r.StartTime, r.EndTime, r.Hours, r.Note)
	if err == nil {
		s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "review_date.add", Entity: "section", EntityID: sectionID.String(), After: r})
	}
	return err
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
// (new / existing / missing_catalog / unmatched_officer) → staff picks skip
// codes → CommitImport creates courses in per-course transactions so one
// failure never blocks the rest of the file.
// -----------------------------------------------------------------------------

type ImportResult struct {
	RowCount   int         `json:"row_count"`
	CreatedIDs []uuid.UUID `json:"created_ids,omitempty"`
	// SkippedCodes carries course codes that were deliberately skipped by the
	// staff decision or because the course already existed / had no matching
	// faculty catalog row. Not an error.
	SkippedCodes []string `json:"skipped_codes,omitempty"`
	ErrorCount   int      `json:"error_count"`
	Errors       []string `json:"errors,omitempty"`
}

type ImportPreviewCourse struct {
	Code               string      `json:"code"`
	Name               string      `json:"name"`
	Status             string      `json:"status"` // "new" | "existing" | "missing_catalog" | "unmatched_officer"
	SectionCount       int         `json:"section_count"`
	ScheduleCount      int         `json:"schedule_count"`
	OfficerRaw         string      `json:"officer_raw"`
	OfficerNames       []string    `json:"officer_names"`
	MatchedLecturerIDs []uuid.UUID `json:"matched_lecturer_ids"`
	UnmatchedNames     []string    `json:"unmatched_names,omitempty"`
	Note               string      `json:"note,omitempty"`
}

type ImportPreview struct {
	Filename            string                `json:"filename"`
	Courses             []ImportPreviewCourse `json:"courses"`
	NewCount            int                   `json:"new_count"`
	ExistingCount       int                   `json:"existing_count"`
	BlockedCount        int                   `json:"blocked_count"`
	MissingCatalogCount int                   `json:"missing_catalog_count"`
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
	schedules   []parsedSchedule
}

type parsedCourse struct {
	code               string
	name               string
	officerRaw         string
	sectionsInOrder    []string
	sections           map[string]*parsedSection
	midtermLectureDate string
	midtermLabDate     string
	finalLectureDate   string
	finalLabDate       string
}

// Officer tokens that are not real personal names — staff wrote them as
// placeholders (guest lecturer, "and colleagues", honorific-only). They must
// not participate in the users lookup, otherwise every course with them would
// be flagged as unmatched even though the course itself is fine.
var officerNoiseTokens = map[string]struct{}{
	"อจ.พิเศษ":  {},
	"และคณะ":   {},
	"อจ.":      {},
	"อาจารย์": {},
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

// isExamForLec tests whether the exam-date cell describes a lecture exam. The
// university prefixes lecture exam dates with "Lec ". Everything without the
// prefix (bare "WBA", "Lab 22 ต.ค. 69", …) is treated as non-Lec here.
func isExamForLec(raw string) bool { return strings.HasPrefix(strings.TrimSpace(raw), "Lec ") }
func isExamForLab(raw string) bool { return strings.HasPrefix(strings.TrimSpace(raw), "Lab ") }

// pickImportSheet returns the sheet name of the Normalized data. Preference:
// exact name "Normalized" (as produced by the current template), then any
// sheet whose header row 1 contains "CourseCode", then the last sheet.
func pickImportSheet(f *excelize.File) (string, error) {
	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return "", errors.New("ไฟล์ไม่มี sheet")
	}
	for _, name := range sheets {
		if name == "Normalized" {
			return name, nil
		}
	}
	for _, name := range sheets {
		rows, err := f.GetRows(name)
		if err != nil || len(rows) == 0 {
			continue
		}
		for _, cell := range rows[0] {
			if strings.EqualFold(strings.TrimSpace(cell), "CourseCode") {
				return name, nil
			}
		}
	}
	return sheets[len(sheets)-1], nil
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
	sheet, err := pickImportSheet(f)
	if err != nil {
		return nil, nil, err
	}
	rows, err := f.GetRows(sheet)
	if err != nil {
		return nil, nil, err
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
			course = &parsedCourse{
				code:            code,
				name:            get(row, "coursename"),
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

		// Exam dates — pick first parseable per Lec/Lab bucket per course.
		if course.midtermLectureDate == "" || course.finalLectureDate == "" || course.midtermLabDate == "" || course.finalLabDate == "" {
			mRaw := get(row, "midtermexamdate")
			fRaw := get(row, "finalexamdate")
			if isExamForLec(mRaw) && course.midtermLectureDate == "" {
				course.midtermLectureDate = parseThaiDate(mRaw)
			}
			if isExamForLab(mRaw) && course.midtermLabDate == "" {
				course.midtermLabDate = parseThaiDate(mRaw)
			}
			if isExamForLec(fRaw) && course.finalLectureDate == "" {
				course.finalLectureDate = parseThaiDate(fRaw)
			}
			if isExamForLab(fRaw) && course.finalLabDate == "" {
				course.finalLabDate = parseThaiDate(fRaw)
			}
		}

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
			sec = &parsedSection{secNo: secNo, track: track, numStudents: seats}
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
		}
		var facultyID uuid.UUID
		if err := s.pool.QueryRow(ctx, `SELECT id FROM faculty_courses WHERE code = $1`, c.code).Scan(&facultyID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				row.Status = "missing_catalog"
				row.Note = "ยังไม่ได้เพิ่มรหัสวิชานี้ในบัญชีรายวิชากลาง"
				out.MissingCatalogCount++
				out.Courses = append(out.Courses, row)
				continue
			}
			return nil, err
		}
		var existingID uuid.UUID
		err := s.pool.QueryRow(ctx,
			`SELECT id FROM teaching_courses WHERE faculty_course_id = $1 AND term_id = $2`, facultyID, termID).Scan(&existingID)
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
			// Skip signals a benign non-creation (existing / missing catalog).
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
	_, _ = s.pool.Exec(ctx,
		`INSERT INTO schedule_imports (id, imported_by, filename, row_count, error_count, summary, at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		uuid.New(), actor, filename, res.RowCount, res.ErrorCount, summary, time.Now())
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "schedule.import", Entity: "term", EntityID: termID.String(), After: summary})
	return res, nil
}

// errImportSkipped signals commitOneCourse chose not to create the row
// because it already exists or its faculty catalog entry is missing. The
// caller records the code in SkippedCodes without incrementing ErrorCount.
var errImportSkipped = errors.New("skipped")

func (s *TeachingService) commitOneCourse(ctx context.Context, actor, termID uuid.UUID, c *parsedCourse) (uuid.UUID, error) {
	var facultyID uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT id FROM faculty_courses WHERE code = $1`, c.code).Scan(&facultyID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, errImportSkipped
		}
		return uuid.Nil, err
	}
	var existing uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM teaching_courses WHERE faculty_course_id = $1 AND term_id = $2`, facultyID, termID).Scan(&existing)
	if err == nil {
		return uuid.Nil, errImportSkipped
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	matched, _, err := s.matchOfficers(ctx, officerTokens(c.officerRaw))
	if err != nil {
		return uuid.Nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)

	id := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO teaching_courses (
			id, faculty_course_id, term_id,
			midterm_lecture_date, midterm_lab_date, final_lecture_date, final_lab_date,
			created_by
		) VALUES ($1,$2,$3,$4::date,$5::date,$6::date,$7::date,$8)`,
		id, facultyID, termID,
		emptyToNil(c.midtermLectureDate), emptyToNil(c.midtermLabDate),
		emptyToNil(c.finalLectureDate), emptyToNil(c.finalLabDate),
		actor); err != nil {
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
			`INSERT INTO sections (id, teaching_course_id, sec_no, track, room, num_students)
			 VALUES ($1,$2,$3,$4::section_track,$5,$6)`,
			secID, id, sec.secNo, sec.track, roomPtr, sec.numStudents); err != nil {
			return uuid.Nil, err
		}
		if sec.track == "special" {
			sumSpecial += sec.numStudents
		} else {
			sumRegular += sec.numStudents
		}
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
	Months       int       `json:"months"`
	IsActive     bool      `json:"is_active"`
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
		`SELECT id, academic_year, semester, TO_CHAR(starts_on,'YYYY-MM-DD'), TO_CHAR(ends_on,'YYYY-MM-DD'), months, is_active
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
	var out []Term
	for rows.Next() {
		var t Term
		if err := rows.Scan(&t.ID, &t.AcademicYear, &t.Semester, &t.StartsOn, &t.EndsOn, &t.Months, &t.IsActive); err != nil {
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
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO academic_terms (id, academic_year, semester, starts_on, ends_on, months, is_active)
			 VALUES ($1,$2,$3,$4::date,$5::date,$6,$7)`,
			in.ID, in.AcademicYear, in.Semester, nilStr(in.StartsOn), nilStr(in.EndsOn), in.Months, in.IsActive); err != nil {
			return nil, err
		}
		s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "term.create", Entity: "term", EntityID: in.ID.String(), After: in})
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
	if _, err := s.pool.Exec(ctx,
		`UPDATE academic_terms SET starts_on=$2::date, ends_on=$3::date, months=$4, is_active=$5 WHERE id=$1`,
		in.ID, nilStr(in.StartsOn), nilStr(in.EndsOn), in.Months, in.IsActive); err != nil {
		return nil, err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "term.update", Entity: "term", EntityID: in.ID.String(), After: in})
	return &in, nil
}

// TermUsage summarizes how many rows reference this term. Used to preview
// the blast radius before a delete or to prevent it.
type TermUsage struct {
	TeachingCourses   int `json:"teaching_courses"`
	ClassSchedules    int `json:"class_schedules"`
	BudgetAllocations int `json:"budget_allocations"`
	RequestWindows    int `json:"request_windows"`
}

// Blocking returns true if any references would be orphaned by a delete.
// Request windows CASCADE with academic_terms so they don't block.
func (u TermUsage) Blocking() bool {
	return u.TeachingCourses+u.ClassSchedules+u.BudgetAllocations > 0
}

func (s *TeachingService) TermUsage(ctx context.Context, id uuid.UUID) (*TermUsage, error) {
	u := &TermUsage{}
	err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM teaching_courses    WHERE term_id = $1),
		  (SELECT COUNT(*) FROM ta_class_schedules  WHERE term_id = $1),
		  (SELECT COUNT(*) FROM budget_allocations  WHERE term_id = $1),
		  (SELECT COUNT(*) FROM ta_request_windows  WHERE term_id = $1)
	`, id).Scan(&u.TeachingCourses, &u.ClassSchedules, &u.BudgetAllocations, &u.RequestWindows)
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
	if _, err := s.pool.Exec(ctx, `DELETE FROM academic_terms WHERE id=$1`, id); err != nil {
		return nil, err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "term.delete", Entity: "term", EntityID: id.String(), Before: t})
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
