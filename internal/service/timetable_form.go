package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"ta-payment-back/internal/pdfgen"
)

// The faculty's signed form — "ตารางเรียนและตารางปฏิบัติงาน (TA)".
//
// One page per TA per term, covering EVERY course they assist, with their own
// classes and their TA duties in the same weekly grid. That layout is the whole
// point of the document: a duty scheduled on top of a lecture the TA has to
// attend is visible by looking down a column, which no per-course view can show.
//
// The system already holds every field the paper form has, so it builds the form
// rather than asking anyone to retype it:
//
//	own classes  → ta_class_schedules   (TA fills these in "ตารางเรียนของฉัน")
//	TA duties    → section_schedules of the sections they are assigned
//	grading      → ta_review_schedules  (the TA's own weekly grading slot)
//	ปกติ/พิเศษ    → sections.track
//	signatures   → the lecturer who SUBMITTED each request (ta_requests.lecturer_id)

// TimetableBlock is one coloured block on the grid.
type TimetableBlock struct {
	// Kind drives the colour and the label:
	//   own_class → the TA's own lecture/lab as a student
	//   lecture / lab / review → their duty on a course they assist
	Kind       string `json:"kind"`
	CourseCode string `json:"course_code"`
	CourseName string `json:"course_name,omitempty"`
	SecNo      string `json:"sec_no,omitempty"`
	// Track is ปกติ / พิเศษ — printed in the label exactly as the paper form does.
	Track     string  `json:"track,omitempty"`
	DayOfWeek int     `json:"day_of_week"`
	StartTime string  `json:"start_time"`
	EndTime   string  `json:"end_time"`
	Room      *string `json:"room,omitempty"`
	// Occurrences: how many times this slot fell inside the requested month and
	// how many of those the TA actually logged. Zero-valued when no month was
	// asked for. This is what turns a weekly PLAN into something a reviewer can
	// check a month of actuals against, without reading every day.
	Expected int `json:"expected,omitempty"`
	Logged   int `json:"logged,omitempty"`
}

// TimetableSigner is one signature block: a lecturer and the courses they are
// signing for. The paper form groups them this way — one lecturer teaching two
// courses signs once, under both course codes.
type TimetableSigner struct {
	LecturerID   uuid.UUID `json:"lecturer_id"`
	LecturerName string    `json:"lecturer_name"`
	Courses      []string  `json:"courses"`      // codes, in the order shown
	CourseNames  []string  `json:"course_names"` // parallel to Courses
}

// TimetableOutOfGrid is a logged entry that matches no slot on the grid — the
// rows a reviewer has to actually read.
type TimetableOutOfGrid struct {
	WorkDate   string  `json:"work_date"`
	StartTime  string  `json:"start_time"`
	EndTime    string  `json:"end_time"`
	Hours      float64 `json:"hours"`
	Activity   string  `json:"activity"`
	CourseCode string  `json:"course_code"`
	SecNo      string  `json:"sec_no"`
	Note       *string `json:"note,omitempty"`
	Source     string  `json:"source"`
}

type TimetableForm struct {
	TAName    string `json:"ta_name"`
	StudentID string `json:"student_id,omitempty"`
	TermLabel string `json:"term_label"`
	// YearMonth is set only when the caller asked for a month; the occurrence
	// counts and OutOfGrid below are relative to it.
	YearMonth     string               `json:"year_month,omitempty"`
	Blocks        []TimetableBlock     `json:"blocks"`
	Signers       []TimetableSigner    `json:"signers"`
	OutOfGrid     []TimetableOutOfGrid `json:"out_of_grid"`
	HasOwnClasses bool                 `json:"has_own_classes"`
}

// BuildTimetableForm assembles the form for one TA in one term. yearMonth is
// optional ("2569-08"); when empty the form is the plan alone, with no counts.
func (s *TeachingService) BuildTimetableForm(
	ctx context.Context, taID, termID uuid.UUID, yearMonth string,
) (*TimetableForm, error) {
	out := &TimetableForm{
		Blocks:    []TimetableBlock{},
		Signers:   []TimetableSigner{},
		OutOfGrid: []TimetableOutOfGrid{},
		YearMonth: yearMonth,
	}

	if err := s.pool.QueryRow(ctx, `
		SELECT u.first_name || ' ' || u.last_name, COALESCE(u.student_id,''),
		       t.academic_year || '/' || t.semester
		  FROM users u, academic_terms t
		 WHERE u.id = $1 AND t.id = $2`, taID, termID,
	).Scan(&out.TAName, &out.StudentID, &out.TermLabel); err != nil {
		return nil, err
	}

	// The TA's own classes. is_wba rows are a "year 4, no fixed timetable"
	// marker rather than a real period, so they carry no time to draw.
	ownRows, err := s.pool.Query(ctx, `
		SELECT COALESCE(course_code,''), COALESCE(course_name,''), COALESCE(sec_no,''),
		       day_of_week, start_time::text, end_time::text
		  FROM ta_class_schedules
		 WHERE user_id = $1 AND term_id = $2 AND NOT is_wba
		 ORDER BY day_of_week, start_time`, taID, termID)
	if err != nil {
		return nil, err
	}
	defer ownRows.Close()
	for ownRows.Next() {
		b := TimetableBlock{Kind: "own_class"}
		if err := ownRows.Scan(&b.CourseCode, &b.CourseName, &b.SecNo,
			&b.DayOfWeek, &b.StartTime, &b.EndTime); err != nil {
			return nil, err
		}
		out.HasOwnClasses = true
		out.Blocks = append(out.Blocks, b)
	}

	// TA duties across every course they assist this term.
	dutyRows, err := s.pool.Query(ctx, `
		SELECT sch.kind, tc.code, tc.name_th, sec.sec_no, sec.track::text,
		       sch.day_of_week, sch.start_time::text, sch.end_time::text, sch.room
		  FROM ta_request_assignments a
		  JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		  JOIN sections sec ON sec.id = a.section_id
		  JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		  JOIN section_schedules sch ON sch.section_id = sec.id
		 WHERE a.ta_id = $1 AND tc.term_id = $2 AND a.state <> 'dropped'
		 ORDER BY sch.day_of_week, sch.start_time, tc.code`, taID, termID)
	if err != nil {
		return nil, err
	}
	defer dutyRows.Close()
	for dutyRows.Next() {
		var b TimetableBlock
		if err := dutyRows.Scan(&b.Kind, &b.CourseCode, &b.CourseName, &b.SecNo,
			&b.Track, &b.DayOfWeek, &b.StartTime, &b.EndTime, &b.Room); err != nil {
			return nil, err
		}
		out.Blocks = append(out.Blocks, b)
	}

	// Grading slots the TA set for themselves. They belong to an assignment, so
	// they carry the course they grade for.
	revRows, err := s.pool.Query(ctx, `
		SELECT tc.code, tc.name_th, sec.sec_no, sec.track::text,
		       rs.day_of_week, rs.start_time::text, rs.end_time::text, rs.room
		  FROM ta_review_schedules rs
		  JOIN ta_request_assignments a ON a.id = rs.assignment_id
		  JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		  JOIN sections sec ON sec.id = a.section_id
		  JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		 WHERE a.ta_id = $1 AND tc.term_id = $2 AND a.state <> 'dropped'
		 ORDER BY rs.day_of_week, rs.start_time`, taID, termID)
	if err != nil {
		return nil, err
	}
	defer revRows.Close()
	for revRows.Next() {
		b := TimetableBlock{Kind: "review"}
		if err := revRows.Scan(&b.CourseCode, &b.CourseName, &b.SecNo, &b.Track,
			&b.DayOfWeek, &b.StartTime, &b.EndTime, &b.Room); err != nil {
			return nil, err
		}
		out.Blocks = append(out.Blocks, b)
	}

	// Signature blocks: the lecturer who SUBMITTED each request, grouped so a
	// lecturer covering two of the TA's courses signs once.
	sigRows, err := s.pool.Query(ctx, `
		SELECT DISTINCT r.lecturer_id, u.first_name || ' ' || u.last_name, tc.code, tc.name_th
		  FROM ta_request_assignments a
		  JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		  JOIN users u ON u.id = r.lecturer_id
		  JOIN sections sec ON sec.id = a.section_id
		  JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		 WHERE a.ta_id = $1 AND tc.term_id = $2 AND a.state <> 'dropped'
		 -- DISTINCT requires every ORDER BY expression to be selected, and the
		 -- selected name is the concatenation, not first_name.
		 ORDER BY 2, 3`, taID, termID)
	if err != nil {
		return nil, err
	}
	defer sigRows.Close()
	byLecturer := map[uuid.UUID]int{}
	for sigRows.Next() {
		var id uuid.UUID
		var name, code, cname string
		if err := sigRows.Scan(&id, &name, &code, &cname); err != nil {
			return nil, err
		}
		idx, ok := byLecturer[id]
		if !ok {
			out.Signers = append(out.Signers, TimetableSigner{LecturerID: id, LecturerName: name})
			idx = len(out.Signers) - 1
			byLecturer[id] = idx
		}
		out.Signers[idx].Courses = append(out.Signers[idx].Courses, code)
		out.Signers[idx].CourseNames = append(out.Signers[idx].CourseNames, cname)
	}

	if yearMonth == "" {
		return out, nil
	}
	if err := s.fillMonthCounts(ctx, out, taID, termID, yearMonth); err != nil {
		return nil, err
	}
	return out, nil
}

// fillMonthCounts turns the weekly plan into something checkable against one
// month of actuals: per slot, how many occurrences that month should have had and
// how many were logged; plus every logged row that matches no slot.
//
// "Matches a slot" is deliberately by (weekday, start, end, activity) rather than
// by anything stored: a row generated from the timetable reproduces the slot
// exactly, and a row typed by hand at a different hour is the one worth reading.
func (s *TeachingService) fillMonthCounts(
	ctx context.Context, form *TimetableForm, taID, termID uuid.UUID, yearMonth string,
) error {
	// The section is part of the key. Without it, two sections taught at the same
	// hour (which is the normal shape here — sec 1 and sec 2 share a lecture slot)
	// collapse into one bucket, and the count reads 6/3: every section's hours
	// piled onto one block while the other block showed none.
	type slotKey struct {
		dow                             int
		start, end, kind, course, secNo string
	}
	logged := map[slotKey]int{}

	rows, err := s.pool.Query(ctx, `
		SELECT EXTRACT(DOW FROM wl.work_date)::int, wl.start_time::text, wl.end_time::text,
		       wl.activity, TO_CHAR(wl.work_date,'YYYY-MM-DD'), wl.hours,
		       tc.code, sec.sec_no, wl.note, wl.source
		  FROM work_logs wl
		  JOIN ta_request_assignments a ON a.id = wl.assignment_id
		  JOIN sections sec ON sec.id = a.section_id
		  JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		  JOIN academic_terms trm ON trm.id = tc.term_id
		 WHERE a.ta_id = $1 AND tc.term_id = $2 AND a.state <> 'dropped'
		   AND trm.academic_year::text || '-' || to_char(wl.work_date,'MM') = $3
		 ORDER BY wl.work_date, wl.start_time`, taID, termID, yearMonth)
	if err != nil {
		return err
	}
	defer rows.Close()

	slotIndex := map[slotKey]bool{}
	for _, b := range form.Blocks {
		if b.Kind == "own_class" {
			continue
		}
		slotIndex[slotKey{b.DayOfWeek, b.StartTime, b.EndTime, b.Kind, b.CourseCode, b.SecNo}] = true
	}

	for rows.Next() {
		var k slotKey
		var o TimetableOutOfGrid
		if err := rows.Scan(&k.dow, &k.start, &k.end, &k.kind,
			&o.WorkDate, &o.Hours, &o.CourseCode, &o.SecNo, &o.Note, &o.Source); err != nil {
			return err
		}
		k.course, k.secNo = o.CourseCode, o.SecNo
		o.StartTime, o.EndTime, o.Activity = k.start, k.end, k.kind
		if slotIndex[k] {
			logged[k]++
			continue
		}
		form.OutOfGrid = append(form.OutOfGrid, o)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Expected occurrences: weekdays of that month inside the course's own date
	// range. Counted in SQL so the month boundary and the term boundary are
	// applied by the same engine that stored the dates.
	for i := range form.Blocks {
		b := &form.Blocks[i]
		if b.Kind == "own_class" {
			continue
		}
		k := slotKey{b.DayOfWeek, b.StartTime, b.EndTime, b.Kind, b.CourseCode, b.SecNo}
		b.Logged = logged[k]
		var expected int
		// The Thai academic year in year_month is BE; the calendar is CE.
		if err := s.pool.QueryRow(ctx, `
			WITH t AS (SELECT starts_on, ends_on FROM academic_terms WHERE id = $3),
			     m AS (SELECT MAKE_DATE(split_part($1,'-',1)::int - 543,
			                            split_part($1,'-',2)::int, 1) AS first_day)
			SELECT COUNT(*)
			  FROM t, m,
			       generate_series(
			         GREATEST(m.first_day, t.starts_on),
			         LEAST((m.first_day + INTERVAL '1 month - 1 day')::date, t.ends_on),
			         INTERVAL '1 day') AS gs(d)
			 WHERE EXTRACT(DOW FROM gs.d)::int = $2
			   AND NOT EXISTS (
			         SELECT 1 FROM public_holidays h WHERE h.holiday_date = gs.d::date)`,
			yearMonth, b.DayOfWeek, termID).Scan(&expected); err != nil {
			return fmt.Errorf("expected occurrences: %w", err)
		}
		b.Expected = expected
	}
	return nil
}

// BuildTimetableFormPDF renders the same form the web page draws, server-side.
//
// It calls BuildTimetableForm rather than re-querying: the browser print and the
// PDF that ships in the payout zip have to be the same document, and the fastest
// way to guarantee that is to give them one source. Only the drawing differs.
//
// Returns ErrNoFontDir when fonts are not configured, so callers can decide
// whether that is fatal (a direct download) or skippable (the export bundle).
func (s *TeachingService) BuildTimetableFormPDF(
	ctx context.Context, taID, termID uuid.UUID, yearMonth string,
) ([]byte, error) {
	if s.fontDir == "" {
		return nil, ErrNoFontDir
	}
	form, err := s.BuildTimetableForm(ctx, taID, termID, yearMonth)
	if err != nil {
		return nil, err
	}
	return pdfgen.BuildTimetableFormPDF(pdfgen.TimetableFormInput{
		FontDir: s.fontDir,
		Data:    timetableFormToPDF(form),
	})
}

// ErrNoFontDir marks "the server has no fonts", which is a deployment gap, not
// a bad request — the export bundle skips the PDF, a direct download says so.
var ErrNoFontDir = errors.New("ยังไม่ได้ตั้งค่าฟอนต์สำหรับสร้าง PDF (FONT_DIR)")

// timetableFormToPDF is the only place the two shapes meet. Kept as a plain
// function so a test can assert the mapping without a database.
func timetableFormToPDF(f *TimetableForm) pdfgen.TimetableFormData {
	d := pdfgen.TimetableFormData{
		TAName:    f.TAName,
		StudentID: f.StudentID,
		TermLabel: f.TermLabel,
		YearMonth: f.YearMonth,
	}
	for _, b := range f.Blocks {
		d.Blocks = append(d.Blocks, pdfgen.TimetableFormBlock{
			Kind:       b.Kind,
			CourseCode: b.CourseCode,
			SecNo:      b.SecNo,
			Track:      b.Track,
			DayOfWeek:  b.DayOfWeek,
			StartTime:  b.StartTime,
			EndTime:    b.EndTime,
			Expected:   b.Expected,
			Logged:     b.Logged,
		})
	}
	for _, sg := range f.Signers {
		d.Signers = append(d.Signers, pdfgen.TimetableFormSigner{
			LecturerName: sg.LecturerName,
			Courses:      sg.Courses,
		})
	}
	for _, o := range f.OutOfGrid {
		row := pdfgen.TimetableFormOutOfGrid{
			Date:   o.WorkDate,
			Start:  o.StartTime,
			End:    o.EndTime,
			Kind:   activityTH(o.Activity),
			Course: o.CourseCode,
			SecNo:  o.SecNo,
			Source: o.Source,
		}
		if o.Note != nil {
			row.Note = *o.Note
		}
		d.OutOfGrid = append(d.OutOfGrid, row)
	}
	return d
}

// activityTH renders a work_logs.activity for the paper form.
func activityTH(a string) string {
	switch a {
	case "lecture":
		return "บรรยาย"
	case "lab":
		return "ปฏิบัติการ"
	case "review":
		return "ตรวจงาน"
	case "other":
		return "อื่นๆ"
	}
	return a
}
