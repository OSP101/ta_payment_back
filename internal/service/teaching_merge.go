// teaching_merge.go — one course open under several registrar codes.
//
// The registrar file lists the same class under more than one code when a
// curriculum reorganisation leaves the old and new code both running (see
// migration 0111). Staff have always treated such a pair as ONE course: the
// student counts are added together and one budget is computed. The system
// does the same by keeping ONE teaching_courses row whose `code` is the
// primary and whose `alt_codes` carries the rest; the sections opened under
// an alternate code sit on the same course with the code folded into their
// sec_no ("SC313302-01"), so every screen and printed document that names a
// section already tells the two "sec 01"s apart.
//
// Nothing here merges on its own. The import previews same-name groups and
// asks; the manual open-course form asks when the typed name already exists
// in the term. A wrong merge puts one class's students on another's budget,
// so the decision stays with the person who knows the timetable.
package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/audit"
)

// normalizeCourseName is the same-name test: case- and whitespace-insensitive,
// because the registrar types "Data Structure" and "DATA STRUCTURE" for the
// same class and pads names with stray spaces.
func normalizeCourseName(s string) string {
	return strings.ToUpper(strings.Join(strings.Fields(s), " "))
}

// codeTakenInTerm reports whether code is already the primary OR an alternate
// code of any course in the term other than exclude. Every path that gives a
// course a code (Create, UpdateCourseInfo, the import, MergeCourseCode) goes
// through this, because the UNIQUE (term_id, code) constraint cannot see
// inside alt_codes.
func codeTakenInTerm(ctx context.Context, q querier, termID uuid.UUID, code string, exclude uuid.UUID) (bool, error) {
	var taken bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM teaching_courses
			 WHERE term_id = $1 AND id <> $3
			   AND (code = $2 OR $2 = ANY(alt_codes)))`, termID, code, exclude).Scan(&taken)
	return taken, err
}

// codeRank orders a course's codes for choosing the primary — the one the
// exported documents print. The college is mid-way through renumbering:
// CP-prefixed codes are the new curriculum, SC-prefixed the previous one, and
// bare six-digit codes the oldest; the old codes stay open for students who
// failed and must retake under their own curriculum. The newest code is the
// course's name going forward, so it wins regardless of which code was
// opened first.
func codeRank(code string) int {
	switch {
	case strings.HasPrefix(code, "CP"):
		return 0
	case strings.HasPrefix(code, "SC"):
		return 1
	case courseCodeRe.MatchString(code):
		return 2
	}
	return 3
}

// primaryCode picks the code that prints first from the course's current
// primary and the rest. Within a rank the current primary keeps its place —
// two CP codes stay in the order staff merged them.
func primaryCode(current string, others []string) string {
	best := current
	for _, c := range others {
		if codeRank(c) < codeRank(best) {
			best = c
		}
	}
	return best
}

// PrintSecNoSQL is the section number as the documents print it: the bare
// number, without the "<code>-" prefix a section merged in under an alternate
// code carries on screen. The documents name the course by its primary code
// alone (see codeRank), so a prefixed section number there would name a code
// the document never mentions.
func PrintSecNoSQL(secAlias string) string {
	return `CASE WHEN ` + secAlias + `.course_code IS NULL THEN ` + secAlias + `.sec_no
	             ELSE substr(` + secAlias + `.sec_no, length(` + secAlias + `.course_code) + 2) END`
}

// altSecNo is how a section opened under an alternate code is numbered on the
// merged course: the code first, so "01" of the primary and "01" of the
// alternate never collide and a reader always sees which code a section came
// from.
func altSecNo(code, secNo string) string {
	return code + "-" + secNo
}

// MergeCodeInput is the manual path: staff typed a course whose name already
// exists in the term under a different code and chose to fold it in.
type MergeCodeInput struct {
	Code        string               `json:"code" validate:"required"`
	LecturerIDs []uuid.UUID          `json:"lecturer_ids"`
	Sections    []CourseSectionInput `json:"sections"`
}

// MergeCourseCode adds code (and the sections opened under it) to the course
// targetID. Staff-only, refused once the course is exported, and refused when
// the code is already open anywhere in the term — the same rules as opening
// the course would have. The course's student counts are recomputed from its
// sections, so the budget picks the merge up on the next read.
func (s *TeachingService) MergeCourseCode(ctx context.Context, actor, targetID uuid.UUID, in MergeCodeInput) error {
	priv, err := isPrivileged(ctx, s.pool, actor)
	if err != nil {
		return err
	}
	if !priv {
		return Forbidden("การรวมรหัสวิชาต้องให้เจ้าหน้าที่ดำเนินการ")
	}
	code := strings.ToUpper(strings.Join(strings.Fields(in.Code), ""))
	if !courseCodeRe.MatchString(code) {
		return Invalid("รูปแบบรหัสวิชาไม่ถูกต้อง ต้องเป็นตัวเลข 6 หลัก (เช่น 342233) หรือตัวอักษรพิมพ์ใหญ่ 2 ตัวตามด้วยตัวเลข 6 หลัก (เช่น CP353201)")
	}
	var termID uuid.UUID
	var lecHrs, labHrs int
	if err := s.pool.QueryRow(ctx,
		`SELECT term_id, lecture_hrs, lab_hrs FROM teaching_courses WHERE id = $1`, targetID).
		Scan(&termID, &lecHrs, &labHrs); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	for _, sec := range in.Sections {
		if strings.TrimSpace(sec.SecNo) == "" || (sec.Track != "regular" && sec.Track != "special") {
			return ErrInvalidInput
		}
		if err := validateSectionSchedules(sec.Schedules, lecHrs, labHrs); err != nil {
			return err
		}
		if sec.Curriculum != nil && *sec.Curriculum != "" && !validCurriculum(*sec.Curriculum) {
			return Invalid("หลักสูตรไม่ถูกต้อง")
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := s.assertNotExported(ctx, tx, targetID); err != nil {
		return err
	}
	secs := make([]mergeSection, 0, len(in.Sections))
	for _, sec := range in.Sections {
		ms := mergeSection{
			secNo: strings.TrimSpace(sec.SecNo), track: sec.Track, room: sec.Room,
			numStudents: sec.NumStudents, exams: sec.Exams,
		}
		if sec.Curriculum != nil && *sec.Curriculum != "" {
			ms.curriculum = sec.Curriculum
		}
		for _, sch := range sec.Schedules {
			ms.schedules = append(ms.schedules, parsedSchedule{
				kind: sch.Kind, dow: sch.DayOfWeek, startTime: sch.StartTime, endTime: sch.EndTime,
				room: derefStr(sch.Room),
			})
		}
		secs = append(secs, ms)
	}
	if err := s.mergeCodeTx(ctx, tx, termID, targetID, code, in.LecturerIDs, secs); err != nil {
		return err
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "teaching_course.merge_code",
		Entity: "teaching_course", EntityID: targetID.String(), After: in}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// mergeSection is the shape both merge paths (typed form, registrar file)
// reduce their sections to before mergeCodeTx writes them.
type mergeSection struct {
	secNo       string
	track       string
	room        *string
	numStudents int
	curriculum  *string
	schedules   []parsedSchedule
	exams       []ExamSchedule
}

// mergeCodeTx does the writes: records the alternate code, adds any lecturers
// the course does not have yet, opens the sections under the prefixed sec_no,
// and recomputes the student counts. Callers own the transaction and the
// authorisation / export-lock checks.
func (s *TeachingService) mergeCodeTx(ctx context.Context, tx pgx.Tx, termID, targetID uuid.UUID,
	code string, lecturerIDs []uuid.UUID, secs []mergeSection) error {
	if taken, err := codeTakenInTerm(ctx, tx, termID, code, targetID); err != nil {
		return err
	} else if taken {
		return Conflict(fmt.Sprintf("รหัสวิชา %s มีอยู่แล้วในภาคเรียนนี้", code))
	}
	var already bool
	if err := tx.QueryRow(ctx,
		`SELECT code = $2 OR $2 = ANY(alt_codes) FROM teaching_courses WHERE id = $1`, targetID, code).
		Scan(&already); err != nil {
		return err
	}
	if already {
		return Conflict(fmt.Sprintf("รหัสวิชา %s รวมอยู่ในวิชานี้แล้ว", code))
	}
	if _, err := tx.Exec(ctx,
		`UPDATE teaching_courses SET alt_codes = alt_codes || $2::text, updated_at = NOW() WHERE id = $1`,
		targetID, code); err != nil {
		return err
	}
	for _, lid := range lecturerIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO teaching_lecturers (teaching_course_id, lecturer_id, is_primary) VALUES ($1,$2,false)
			 ON CONFLICT DO NOTHING`, targetID, lid); err != nil {
			return err
		}
	}
	for _, sec := range secs {
		secID := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO sections (id, teaching_course_id, sec_no, track, room, num_students, curriculum, course_code)
			 VALUES ($1,$2,$3,$4::section_track,$5,$6,$7,$8)`,
			secID, targetID, altSecNo(code, sec.secNo), sec.track, sec.room, sec.numStudents,
			sec.curriculum, code); err != nil {
			return err
		}
		for _, sch := range sec.schedules {
			if _, err := tx.Exec(ctx,
				`INSERT INTO section_schedules (id, section_id, kind, day_of_week, start_time, end_time, room)
				 VALUES ($1,$2,$3,$4,$5::time,$6::time,$7)`,
				uuid.New(), secID, sch.kind, sch.dow, sch.startTime, sch.endTime, emptyToNil(sch.room)); err != nil {
				return err
			}
		}
		for _, e := range sec.exams {
			if _, err := tx.Exec(ctx,
				`INSERT INTO exam_schedules (id, section_id, kind, exam_date, start_time, end_time, room)
				 VALUES ($1,$2,$3::exam_kind,$4::date,$5,$6,$7)`,
				uuid.New(), secID, e.Kind, e.ExamDate, e.StartTime, e.EndTime, e.Room); err != nil {
				return err
			}
		}
	}
	// After the sections are in, so a newly promoted primary's sections are
	// there to lose their prefix.
	if err := s.promotePrimaryCode(ctx, tx, targetID); err != nil {
		return err
	}
	return s.recomputeAggregate(ctx, tx, targetID)
}

// mergeParsedCourse folds one course from the registrar file into targetID —
// the import's counterpart of MergeCourseCode. The file is trusted the same
// way commitOneCourse trusts it (no credit-gate on the schedule kinds).
func (s *TeachingService) mergeParsedCourse(ctx context.Context, actor, termID, targetID uuid.UUID, c *parsedCourse) error {
	matched, _, err := s.matchOfficers(ctx, officerTokens(c.officerRaw))
	if err != nil {
		return err
	}
	secs := make([]mergeSection, 0, len(c.sectionsInOrder))
	for _, secNo := range c.sectionsInOrder {
		sec := c.sections[secNo]
		secs = append(secs, mergeSection{
			secNo: sec.secNo, track: sec.track, room: emptyToNil(sec.room),
			numStudents: sec.numStudents, curriculum: emptyToNil(sec.curriculum),
			schedules: sec.schedules,
		})
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := s.assertNotExported(ctx, tx, targetID); err != nil {
		return err
	}
	if err := s.mergeCodeTx(ctx, tx, termID, targetID, c.code, matched, secs); err != nil {
		return err
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "teaching_course.merge_code",
		Entity: "teaching_course", EntityID: targetID.String(),
		After: map[string]any{"code": c.code, "source": "import", "sections": len(secs)}}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------------------
// Import preview: same-name groups for staff to decide on.

// ImportMergeMember is one code inside a same-name group: either a course in
// the file that is about to be created, or a course already open in the term
// that the file's course could be folded into.
type ImportMergeMember struct {
	Code string `json:"code"`
	// Status mirrors ImportPreviewCourse.Status: "new" / "unmatched_officer"
	// for file courses, "existing" for a course already open in the term.
	Status     string     `json:"status"`
	ExistingID *uuid.UUID `json:"existing_id,omitempty"`
	// AltCodes of an existing course, so the row can show every code it
	// already carries.
	AltCodes     []string `json:"alt_codes"`
	Lecturers    []string `json:"lecturers"`
	SectionCount int      `json:"section_count"`
	Students     int      `json:"students"`
	// Suggested is the system's guess that this member is the same class as
	// another member — same lecturer or a section at the same day, time and
	// room. A guess only: the preview pre-ticks it, staff decide.
	Suggested bool `json:"suggested"`
}

type ImportMergeGroup struct {
	Name    string              `json:"name"`
	Members []ImportMergeMember `json:"members"`
}

// ImportMerge is one decision sent back with the commit: fold every code in
// Codes into the course whose code is Primary. Primary may be a course in the
// file (created first, then merged into) or a course already open in the term.
type ImportMerge struct {
	Primary string   `json:"primary"`
	Codes   []string `json:"codes"`
}

// mergeFacts is what the suggestion compares between members.
type mergeFacts struct {
	lecturers  map[string]struct{}
	signatures map[string]struct{}
}

// overlaps is the "same class?" guess. A section meeting at the same day,
// time, kind and room is the strong signal. The lecturer alone counts only
// when one side has no timetable to compare (WBA courses — co-op, projects):
// with timetables on both sides that disagree, a shared lecturer is just a
// lecturer teaching two courses (CP352203 and CP410844 are both วชิราวุธ's
// game-development courses, in different programmes on different days).
func (f mergeFacts) overlaps(o mergeFacts) bool {
	for k := range f.signatures {
		if _, ok := o.signatures[k]; ok && k != "" {
			return true
		}
	}
	if len(f.signatures) > 0 && len(o.signatures) > 0 {
		return false
	}
	for k := range f.lecturers {
		if _, ok := o.lecturers[k]; ok {
			return true
		}
	}
	return false
}

func parsedCourseFacts(c *parsedCourse) mergeFacts {
	f := mergeFacts{lecturers: map[string]struct{}{}, signatures: map[string]struct{}{}}
	for _, t := range officerTokens(c.officerRaw) {
		f.lecturers[t] = struct{}{}
	}
	for _, sec := range c.sections {
		rows := make([]scheduleFingerprint, 0, len(sec.schedules))
		for _, sch := range sec.schedules {
			rows = append(rows, scheduleFingerprint{dayOfWeek: sch.dow, startTime: sch.startTime,
				endTime: sch.endTime, kind: sch.kind, room: sch.room})
		}
		if len(rows) > 0 {
			f.signatures[sectionSignature(rows)] = struct{}{}
		}
	}
	return f
}

// existingCourseFacts loads the same facts for a course already in the DB.
// Lecturer identity is the bare first name — the only thing the registrar's
// officer column carries, so it is the only thing that can match.
func (s *TeachingService) existingCourseFacts(ctx context.Context, tcID uuid.UUID) (mergeFacts, error) {
	f := mergeFacts{lecturers: map[string]struct{}{}, signatures: map[string]struct{}{}}
	rows, err := s.pool.Query(ctx, `
		SELECT u.first_name FROM teaching_lecturers tl JOIN users u ON u.id = tl.lecturer_id
		 WHERE tl.teaching_course_id = $1`, tcID)
	if err != nil {
		return f, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return f, err
		}
		f.lecturers[strings.TrimSpace(n)] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return f, err
	}
	rows, err = s.pool.Query(ctx, `
		SELECT sec.id, sch.day_of_week, sch.start_time::text, sch.end_time::text, sch.kind, COALESCE(sch.room,'')
		  FROM sections sec JOIN section_schedules sch ON sch.section_id = sec.id
		 WHERE sec.teaching_course_id = $1`, tcID)
	if err != nil {
		return f, err
	}
	defer rows.Close()
	bySec := map[uuid.UUID][]scheduleFingerprint{}
	for rows.Next() {
		var sid uuid.UUID
		var fp scheduleFingerprint
		if err := rows.Scan(&sid, &fp.dayOfWeek, &fp.startTime, &fp.endTime, &fp.kind, &fp.room); err != nil {
			return f, err
		}
		// Registrar times are "HH:MM"; Postgres renders "HH:MM:SS".
		fp.startTime = strings.TrimSuffix(fp.startTime, ":00")
		fp.endTime = strings.TrimSuffix(fp.endTime, ":00")
		bySec[sid] = append(bySec[sid], fp)
	}
	for _, r := range bySec {
		f.signatures[sectionSignature(r)] = struct{}{}
	}
	return f, rows.Err()
}

// detectImportMergeGroups builds the same-name groups for a preview: file
// courses that are not already in the term, plus courses already open in the
// term under the same name. Only groups with two or more members are worth a
// question. statusOf gives each file course its preview status.
func (s *TeachingService) detectImportMergeGroups(ctx context.Context, termID uuid.UUID,
	courses []*parsedCourse, statusOf map[string]string) ([]ImportMergeGroup, error) {
	type member struct {
		ImportMergeMember
		facts mergeFacts
	}
	groups := map[string][]*member{}
	display := map[string]string{}
	order := []string{}
	add := func(name string, m *member) {
		key := normalizeCourseName(name)
		if key == "" {
			return
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
			display[key] = strings.Join(strings.Fields(name), " ")
		}
		groups[key] = append(groups[key], m)
	}

	// Courses already open in the term, keyed by every name they go by.
	rows, err := s.pool.Query(ctx, `
		SELECT tc.id, tc.code, tc.alt_codes, tc.name_th, COALESCE(tc.name_en,''),
		       (SELECT COUNT(*) FROM sections sx WHERE sx.teaching_course_id = tc.id),
		       tc.num_students
		  FROM teaching_courses tc WHERE tc.term_id = $1`, termID)
	if err != nil {
		return nil, err
	}
	type dbCourse struct {
		id                   uuid.UUID
		code, nameTH, nameEN string
		alt                  []string
		sections, students   int
	}
	var existing []dbCourse
	for rows.Next() {
		var d dbCourse
		if err := rows.Scan(&d.id, &d.code, &d.alt, &d.nameTH, &d.nameEN, &d.sections, &d.students); err != nil {
			rows.Close()
			return nil, err
		}
		existing = append(existing, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// File courses first so a group's first member (the default primary) is
	// the file's own order; existing courses are appended after.
	fileNames := map[string]struct{}{}
	for _, c := range courses {
		st := statusOf[c.code]
		if st == "existing" {
			continue
		}
		students := 0
		for _, sec := range c.sections {
			students += sec.numStudents
		}
		facts := parsedCourseFacts(c)
		lect := make([]string, 0, len(facts.lecturers))
		for l := range facts.lecturers {
			lect = append(lect, l)
		}
		sort.Strings(lect)
		m := &member{ImportMergeMember: ImportMergeMember{
			Code: c.code, Status: st, AltCodes: []string{}, Lecturers: lect,
			SectionCount: len(c.sections), Students: students,
		}, facts: facts}
		name := c.nameEN
		if strings.TrimSpace(name) == "" {
			name = c.name
		}
		fileNames[normalizeCourseName(name)] = struct{}{}
		add(name, m)
	}
	for _, d := range existing {
		names := map[string]struct{}{}
		for _, n := range []string{d.nameTH, d.nameEN} {
			if k := normalizeCourseName(n); k != "" {
				names[k] = struct{}{}
			}
		}
		hit := ""
		for k := range names {
			if _, ok := fileNames[k]; ok {
				hit = k
				break
			}
		}
		if hit == "" {
			continue
		}
		facts, err := s.existingCourseFacts(ctx, d.id)
		if err != nil {
			return nil, err
		}
		lect := make([]string, 0, len(facts.lecturers))
		for l := range facts.lecturers {
			lect = append(lect, l)
		}
		sort.Strings(lect)
		id := d.id
		alt := d.alt
		if alt == nil {
			alt = []string{}
		}
		groups[hit] = append(groups[hit], &member{ImportMergeMember: ImportMergeMember{
			Code: d.code, Status: "existing", ExistingID: &id, AltCodes: alt, Lecturers: lect,
			SectionCount: d.sections, Students: d.students,
		}, facts: facts})
	}

	out := []ImportMergeGroup{}
	for _, key := range order {
		ms := groups[key]
		if len(ms) < 2 {
			continue
		}
		for i := range ms {
			for j := range ms {
				if i != j && ms[i].facts.overlaps(ms[j].facts) {
					ms[i].Suggested = true
					break
				}
			}
		}
		g := ImportMergeGroup{Name: display[key]}
		for _, m := range ms {
			g.Members = append(g.Members, m.ImportMergeMember)
		}
		out = append(out, g)
	}
	return out, nil
}

// promotePrimaryCode re-picks the course's primary after a merge (see
// codeRank). When the primary changes, the sections swap numbering with it:
// the new primary's sections drop their "<code>-" prefix and the old
// primary's gain one, so the bare "sec 1" always belongs to the code that
// prints on the documents. Section ids never change — worklogs and TA
// assignments point at ids, not numbers.
func (s *TeachingService) promotePrimaryCode(ctx context.Context, tx pgx.Tx, tcID uuid.UUID) error {
	var current string
	var alts []string
	if err := tx.QueryRow(ctx,
		`SELECT code, alt_codes FROM teaching_courses WHERE id = $1`, tcID).Scan(&current, &alts); err != nil {
		return err
	}
	best := primaryCode(current, alts)
	if best == current {
		return nil
	}
	rest := make([]string, 0, len(alts))
	rest = append(rest, current)
	for _, c := range alts {
		if c != best {
			rest = append(rest, c)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return codeRank(rest[i]) < codeRank(rest[j]) })
	if _, err := tx.Exec(ctx,
		`UPDATE teaching_courses SET code = $2, alt_codes = $3, updated_at = NOW() WHERE id = $1`,
		tcID, best, rest); err != nil {
		return err
	}
	// Old primary's sections take its code as prefix…
	if _, err := tx.Exec(ctx,
		`UPDATE sections SET sec_no = $2 || '-' || sec_no, course_code = $2
		  WHERE teaching_course_id = $1 AND course_code IS NULL`, tcID, current); err != nil {
		return err
	}
	// …and the new primary's sections drop theirs.
	_, err := tx.Exec(ctx,
		`UPDATE sections SET sec_no = substr(sec_no, length($2) + 2), course_code = NULL
		  WHERE teaching_course_id = $1 AND course_code = $2`, tcID, best)
	return err
}
