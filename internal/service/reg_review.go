package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Only editable domain values travel back from the browser. Source cells are
// re-read from the uploaded file, never accepted from the client as evidence.
type REGReview struct {
	FileSHA256     string        `json:"file_sha256"`
	ValidationHash string        `json:"validation_hash"`
	Courses        []REGCourse   `json:"courses"`
	Acknowledged   []string      `json:"acknowledged"`
	Confirmed      bool          `json:"confirmed"`
	Issues         []REGIssue    `json:"issues"`
	Lecturers      []REGLecturer `json:"lecturers"`
	Ready          bool          `json:"ready"`
}
type REGLecturer struct {
	Email string    `json:"email"`
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
}
type REGIssue struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Severity  string `json:"severity"`
	Code      string `json:"code"`
	SectionID string `json:"section_id,omitempty"`
	Message   string `json:"message"`
}
type REGCourse struct {
	SourceFormat          string       `json:"source_format,omitempty"`
	SourceHeaders         []string     `json:"source_headers,omitempty"`
	SourceCells           []string     `json:"source_cells"`
	Code                  string       `json:"code"`
	Name                  string       `json:"name"`
	Unit                  string       `json:"unit"`
	Level                 string       `json:"level"`
	SourceRow             int          `json:"source_row"`
	OfficerRaw            string       `json:"officer_raw"`
	UnmatchedOfficerNames []string     `json:"unmatched_officer_names"`
	LecturerMatchVerified bool         `json:"lecturer_match_verified"`
	LecturerIDs           []uuid.UUID  `json:"lecturer_ids"`
	UnassignedReason      string       `json:"unassigned_reason"`
	Skip                  bool         `json:"skip"`
	SkipReason            string       `json:"skip_reason"`
	ExistingID            string       `json:"existing_id"`
	ExistingVersion       string       `json:"existing_version"`
	MergeInto             string       `json:"merge_into"`
	Sections              []REGSection `json:"sections"`
}
type REGSection struct {
	SourceCapacity *int           `json:"source_capacity,omitempty"`
	SourceRows     []REGSourceRow `json:"source_rows,omitempty"`
	SourceCells    []string       `json:"source_cells"`
	ID             string         `json:"id"`
	SourceRow      int            `json:"source_row"`
	SecNo          string         `json:"sec_no"`
	Track          string         `json:"track"`
	ReservedFor    string         `json:"reserved_for"`
	Curriculum     string         `json:"curriculum"`
	Students       *int           `json:"students"`
	PendingReason  string         `json:"pending_reason"`
	Schedules      []REGSchedule  `json:"schedules"`
}
type REGSourceRow struct {
	Row   int      `json:"row"`
	Cells []string `json:"cells"`
}
type REGSchedule struct {
	Day     int    `json:"day"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Kind    string `json:"kind"`
	Room    string `json:"room"`
	Pending bool   `json:"pending"`
	Raw     string `json:"raw"`
}

func regFileHash(body []byte) string { h := sha256.Sum256(body); return hex.EncodeToString(h[:]) }

func newREGReview(courses []*parsedCourse, body []byte) *REGReview {
	r := &REGReview{FileSHA256: regFileHash(body), Courses: []REGCourse{}, Acknowledged: []string{}, Issues: []REGIssue{}, Lecturers: []REGLecturer{}}
	for _, c := range courses {
		v := REGCourse{SourceCells: append([]string{}, c.rawCells...), Code: c.code, Name: c.name, Unit: c.unitRaw, Level: c.level, SourceRow: c.firstRow, OfficerRaw: c.officerRaw, LecturerIDs: []uuid.UUID{}, Sections: []REGSection{}}
		v.SourceFormat, v.SourceHeaders = c.sourceFormat, c.sourceHeaders
		for _, key := range c.sectionsInOrder {
			s := c.sections[key]
			n := s.numStudents
			sec := REGSection{SourceCells: append([]string{}, s.rawCells...), ID: fmt.Sprintf("%s:%d:%s", c.code, s.sourceRow, key), SourceRow: s.sourceRow, SecNo: s.secNo, Track: s.track, ReservedFor: s.reserved, Curriculum: s.curriculum, Students: &n, Schedules: []REGSchedule{}}
			sec.SourceRows = s.sourceRows
			if c.sourceFormat == "programme" {
				if capacity, err := strconv.Atoi(s.capacityRaw); err == nil {
					sec.SourceCapacity = &capacity
				}
				if _, err := strconv.Atoi(s.studentCountRaw); err != nil {
					sec.Students = nil
				}
			} else if len(s.rawCells) > 6 {
				if _, err := strconv.Atoi(strings.TrimSpace(s.rawCells[6])); err != nil {
					sec.Students = nil
				}
			}
			if c.sourceFormat != "programme" && len(s.rawCells) > 6 && strings.TrimSpace(s.rawCells[6]) == "" {
				sec.Students = nil
			}
			if s.reviewSchedules != nil {
				sec.Schedules = append(sec.Schedules, s.reviewSchedules...)
			} else if c.requiresReview {
				days := splitLines(s.rawDays)
				for i, line := range splitLines(s.rawTimes) {
					if strings.TrimSpace(line) == "" {
						continue
					}
					fields := strings.Fields(line)
					slot := REGSchedule{Day: -1, Kind: "", Raw: line}
					if len(fields) > 1 {
						if strings.EqualFold(fields[1], "Lec") || strings.EqualFold(fields[1], "Lecture") {
							slot.Kind = "lecture"
						} else if strings.EqualFold(fields[1], "Lab") {
							slot.Kind = "lab"
						} else if !strings.EqualFold(fields[1], "Lec") && !strings.EqualFold(fields[1], "Lecture") {
							slot.Kind = fields[1]
						}
					}
					if len(fields) > 2 && !strings.EqualFold(strings.Join(fields[2:], " "), "WBA") {
						slot.Room = strings.Join(fields[2:], " ")
					}
					if len(fields) > 0 {
						slot.Start, slot.End, _ = parseTimeRange(fields[0])
					}
					day := ""
					if i < len(days) {
						day = strings.ToUpper(strings.TrimSpace(days[i]))
					}
					if d, ok := dowFromAbbrev(day); ok {
						slot.Day = d
					}
					if day == "MW" {
						slot.Day = 1
						slot.Pending = slot.Start == "" || slot.End == ""
						sec.Schedules = append(sec.Schedules, slot)
						slot.Day = 3
					}
					slot.Pending = slot.Day < 0 || slot.Start == "" || slot.End == ""
					sec.Schedules = append(sec.Schedules, slot)
				}
			} else {
				for _, sch := range s.schedules {
					sec.Schedules = append(sec.Schedules, REGSchedule{Day: sch.dow, Start: sch.startTime, End: sch.endTime, Kind: sch.kind, Room: sch.room})
				}
			}
			v.Sections = append(v.Sections, sec)
		}
		r.Courses = append(r.Courses, v)
	}
	return r
}

// Review identity is immutable: no course/section from the source can disappear
// unnoticed. Exclusion is an explicit course decision with a recorded reason.
func bindREGReview(source, input *REGReview) error {
	if source.FileSHA256 != input.FileSHA256 || len(source.Courses) != len(input.Courses) {
		return Invalid("ไฟล์หรือจำนวนรายวิชาเปลี่ยน กรุณาอัปโหลดและตรวจสอบใหม่")
	}
	for i, original := range source.Courses {
		c := &input.Courses[i]
		if c.Code != original.Code || len(c.Sections) != len(original.Sections) {
			return Invalid("ห้ามลบรหัสวิชาหรือ Section จากข้อมูลตรวจสอบ ให้เลือกข้ามรายวิชาพร้อมเหตุผล")
		}
		c.Name = strings.TrimSpace(c.Name)
		c.MergeInto = strings.ToUpper(strings.TrimSpace(c.MergeInto))
		c.SourceCells = original.SourceCells
		c.SourceFormat, c.SourceHeaders = original.SourceFormat, original.SourceHeaders
		c.SourceRow = original.SourceRow
		c.OfficerRaw = original.OfficerRaw
		for j, s := range original.Sections {
			if c.Sections[j].ID != s.ID {
				return Invalid("ลำดับ Section เปลี่ยน กรุณาตรวจสอบใหม่")
			}
			sec := &c.Sections[j]
			sec.SecNo = strings.TrimSpace(strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(sec.SecNo)), "SEC"))
			if _, err := strconv.Atoi(sec.SecNo); err == nil {
				sec.SecNo = strings.TrimLeft(sec.SecNo, "0")
				if sec.SecNo == "" {
					sec.SecNo = "0"
				}
			}
			sec.Track = strings.ToLower(strings.TrimSpace(sec.Track))
			sec.Curriculum = strings.ToUpper(strings.TrimSpace(sec.Curriculum))
			for k := range sec.Schedules {
				sch := &sec.Schedules[k]
				sch.Start = strings.TrimSpace(sch.Start)
				sch.End = strings.TrimSpace(sch.End)
				sch.Room = strings.TrimSpace(sch.Room)
				sch.Kind = strings.ToLower(strings.TrimSpace(sch.Kind))
			}
			c.Sections[j].SourceCells = s.SourceCells
			c.Sections[j].SourceRows = s.SourceRows
			c.Sections[j].SourceCapacity = s.SourceCapacity
			c.Sections[j].SourceRow = s.SourceRow
			c.Sections[j].ReservedFor = s.ReservedFor
			if len(c.Sections[j].Schedules) > 200 {
				return Invalid("จำนวนคาบต่อ Section มากเกินไป")
			}
		}
	}
	return nil
}

func regIssue(r *REGReview, code, section, kind, severity, message string, facts any) {
	b, _ := json.Marshal(facts)
	h := sha256.Sum256(append([]byte(code+section+kind+message), b...))
	r.Issues = append(r.Issues, REGIssue{ID: hex.EncodeToString(h[:12]), Kind: kind, Severity: severity, Code: code, SectionID: section, Message: message})
}

// Validation always runs server-side, including on commit. Warnings can be
// acknowledged; invalid values cannot be waived by ticking a checkbox.
func validateREGReview(r *REGReview) {
	validateREGReviewWithTrustedMatches(r, nil)
}

// Trust comes only from a fresh source lookup, never from a client flag or
// similarity score. Keep the order: the first lecturer is the primary owner.
func sameREGOrderedLecturers(a, b []uuid.UUID) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func trustedREGSourceLecturers(raw string, matched []uuid.UUID, unmatched []string) bool {
	if len(matched) == 0 || len(unmatched) != 0 {
		return false
	}
	for _, token := range strings.Fields(raw) {
		// These placeholders stand for additional unnamed teachers.
		if token == "และคณะ" || token == "อจ.พิเศษ" {
			return false
		}
	}
	return len(officerTokens(raw)) > 0
}

func validateREGReviewWithTrustedMatches(r *REGReview, trusted map[string][]uuid.UUID) {
	r.Issues = []REGIssue{}
	r.Ready = true
	names := map[string][]string{}
	for _, c := range r.Courses {
		names[normalizeCourseName(c.Name)] = append(names[normalizeCourseName(c.Name)], c.Code)
	}
	for _, c := range r.Courses {
		countLabel := "จำนวนรับ"
		if c.SourceFormat == "programme" {
			countLabel = "จำนวนผู้ลงทะเบียน"
		}
		issue := func(kind, severity, message string, facts any) {
			regIssue(r, c.Code, "", kind, severity, message, facts)
		}
		if c.Skip {
			if strings.TrimSpace(c.SkipReason) == "" {
				issue("skip", "error", "กรุณาระบุเหตุผลที่ข้ามรายวิชา", c.SkipReason)
			}
			continue
		}
		if c.ExistingID != "" {
			issue("existing", "error", "รายวิชามีในระบบแล้ว กรุณาข้ามเพื่อรักษาข้อมูลเดิม", c.ExistingID)
		}
		if !courseCodeRe.MatchString(c.Code) {
			issue("code", "error", "รหัสวิชาไม่ถูกต้อง", c.Code)
		}
		if strings.TrimSpace(c.Name) == "" || len([]rune(c.Name)) > 500 {
			issue("name", "error", "ชื่อวิชาต้องมี 1–500 ตัวอักษร", c.Name)
		}
		if strings.ContainsAny(c.Name, "\r\n") {
			issue("name_lines", "warning", "ชื่อวิชามีหลายบรรทัด กรุณาตรวจชื่อเต็ม", c.Name)
		}
		cr, l, p, ss, ok := parseUnit(c.Unit)
		if !ok {
			issue("unit", "error", "หน่วยกิตต้องเป็นรูปแบบ 3 (2-2-5)", c.Unit)
		} else if e := validateCourseNumbers(courseNumbers{cr, l, p, ss}); e != nil {
			issue("unit", "error", e.Error(), c.Unit)
		}
		if c.Level != "undergrad" && c.Level != "graduate" {
			issue("level", "error", "กรุณาเลือกระดับการศึกษา", c.Level)
		}
		if len(c.LecturerIDs) == 0 {
			if strings.TrimSpace(c.UnassignedReason) == "" {
				issue("lecturer", "error", "กรุณาเลือกอาจารย์ หรือระบุเหตุผลที่ยังไม่ผูกอาจารย์", nil)
			} else {
				issue("lecturer", "warning", "เปิดวิชาโดยยังไม่ผูกอาจารย์: "+c.UnassignedReason, c.UnassignedReason)
			}
		}
		if len(c.Sections) == 0 {
			issue("section", "error", "รายวิชาต้องมีอย่างน้อยหนึ่ง Section", nil)
		}
		if len(c.LecturerIDs) > 0 && !sameREGOrderedLecturers(c.LecturerIDs, trusted[c.Code]) {
			issue("lecturer_match", "warning", "ตรวจรายชื่ออาจารย์ที่จับคู่กับข้อความจาก REG และผู้รับผิดชอบหลัก", c.LecturerIDs)
		}
		if related := names[normalizeCourseName(c.Name)]; len(related) > 1 {
			issue("shared_name", "warning", "ชื่อวิชาตรงกับรหัสอื่น: "+strings.Join(related, ", ")+" ตรวจว่าจะเปิดแยกหรือเรียนรวม", c.MergeInto)
		}
		if c.MergeInto != "" {
			issue("merge", "warning", "ยืนยันเรียนรวมกับรหัส "+c.MergeInto, c.MergeInto)
		}
		seen := map[string]bool{}
		total := 0
		for _, sec := range c.Sections {
			warn := func(kind, severity, message string, facts any) {
				regIssue(r, c.Code, sec.ID, kind, severity, fmt.Sprintf("แถว %d SEC %s: %s", sec.SourceRow, sec.SecNo, message), facts)
			}
			key := sectionImportKey(sec.SecNo, sec.Track)
			if seen[key] {
				warn("duplicate", "error", "เลข Section และประเภทภาคซ้ำกัน", key)
			}
			seen[key] = true
			if strings.TrimSpace(sec.SecNo) == "" || len([]rune(sec.SecNo)) > 50 {
				warn("section_no", "error", "กรุณาระบุเลข Section ไม่เกิน 50 ตัวอักษร", sec.SecNo)
			}
			if sec.Track != "regular" && sec.Track != "special" {
				warn("track", "error", "กรุณาเลือกภาคปกติหรือภาคพิเศษ", sec.Track)
			}
			if sec.Students == nil {
				warn("students", "error", countLabel+"ยังว่าง กรุณากรอกจำนวนที่ถูกต้อง", nil)
			} else {
				if e := validateSectionStudents(*sec.Students, "Section"); e != nil {
					warn("students", "error", e.Error(), sec.Students)
				}
				total += *sec.Students
				if *sec.Students == 0 {
					warn("zero", "warning", countLabel+"เป็น 0 คน กรุณายืนยัน", 0)
				}
			}
			if !validSectionCurricula[sec.Curriculum] {
				warn("curriculum", "error", "ระบุกลุ่มหลักสูตรก่อนนำเข้า (OTHER สำหรับคณะอื่นหรือกลุ่มทั่วไป)", sec.Curriculum)
			}
			if len(curriculumTokenRE.FindAllString(sec.ReservedFor, -1)) > 1 {
				warn("multi_programme", "warning", "รับหลายหลักสูตร ตรวจกลุ่มหลักที่เลือก และเก็บรายละเอียดทั้งหมดจาก REG ไว้อ้างอิง", sec.Curriculum)
			}
			if strings.Contains(sec.ReservedFor, "เรียนรวม") {
				warn("shared", "warning", "REG ระบุเรียนรวม: "+sec.ReservedFor, c.MergeInto)
			}
			pending := len(sec.Schedules) == 0
			hours := map[string]int{}
			slots := map[string]bool{}
			sameSlot := map[string]int{}
			exactSlots := map[string]bool{}
			missingRoom := false
			for _, sch := range sec.Schedules {
				if sch.Pending {
					pending = true
					continue
				}
				a, aok := parseHM(sch.Start)
				b, bok := parseHM(sch.End)
				if sch.Day < 0 || sch.Day > 6 || !aok || !bok || a >= b || (sch.Kind != "lecture" && sch.Kind != "lab") {
					warn("schedule", "error", "วัน เวลา หรือประเภทคาบไม่ถูกต้อง", sch)
					continue
				}
				k := fmt.Sprintf("%d/%s/%s/%s", sch.Day, sch.Start, sch.End, sch.Kind)
				if !slots[k] {
					hours[sch.Kind] += b - a
					slots[k] = true
				}
				sameSlot[k]++
				fullKey := k + "/" + strings.TrimSpace(sch.Room)
				if exactSlots[fullKey] {
					warn("exact_duplicate", "error", "คาบวัน เวลา ประเภท และห้องซ้ำกัน กรุณาลบคาบซ้ำ", sch)
				}
				exactSlots[fullKey] = true
				if strings.TrimSpace(sch.Room) == "" || strings.EqualFold(sch.Room, "WBA") {
					missingRoom = true
				}
			}
			if pending {
				if strings.TrimSpace(sec.PendingReason) == "" {
					warn("pending", "error", "มีคาบที่ยังไม่กำหนดวัน/เวลา ระบุเหตุผลเพื่อเก็บไว้เติมภายหลัง หรือแก้คาบให้ครบ", sec.Schedules)
				} else {
					warn("pending", "warning", "เก็บตารางที่ยังไม่ครบไว้เติมภายหลัง: "+sec.PendingReason, sec.Schedules)
				}
			}
			if missingRoom {
				warn("room", "warning", "มีคาบที่ยังไม่ระบุห้อง ยืนยันให้เติมภายหลัง", sec.Schedules)
			}
			multi := false
			for _, n := range sameSlot {
				if n > 1 {
					multi = true
				}
			}
			if multi {
				warn("rooms", "warning", "มีหลายห้องในเวลาเดียวกัน ตรวจว่าเป็นการแบ่งห้องจริง", sec.Schedules)
			}
			if ok && !pending && (hours["lecture"] != l*60 || hours["lab"] != p*60) {
				warn("hours", "warning", fmt.Sprintf("ตาราง Lec %.1f / Lab %.1f ชั่วโมง ไม่ตรงกับหน่วยกิต Lec %d / Lab %d", float64(hours["lecture"])/60, float64(hours["lab"])/60, l, p), []any{c.Unit, sec.Schedules})
			}
			if strings.Contains(sec.ReservedFor, "พิเศษ") && sec.Track != "special" {
				warn("track_override", "warning", "เลือกภาคต่างจากข้อความ REG กรุณายืนยัน", sec.Track)
			}
		}
		if total > 100000 {
			issue("total", "error", countLabel+"รวมเกิน 100,000 คน กรุณาตรวจตัวเลข", total)
		} else if total > 3000 {
			issue("total", "warning", fmt.Sprintf("%sรวม %d คน สูงกว่า 3,000 คน กรุณายืนยันรายวิชาขนาดใหญ่", countLabel, total), total)
		}
	}
	acks := map[string]bool{}
	for _, a := range r.Acknowledged {
		acks[a] = true
	}
	for _, i := range r.Issues {
		if i.Severity == "error" || !acks[i.ID] {
			r.Ready = false
		}
	}
}

func regValidationHash(r *REGReview, term uuid.UUID) string {
	b, _ := json.Marshal(struct {
		File         string
		Term         uuid.UUID
		Courses      []REGCourse
		Issues       []REGIssue
		Acknowledged []string
	}{r.FileSHA256, term, r.Courses, r.Issues, r.Acknowledged})
	return regFileHash(b)
}

func (s *TeachingService) prepareREGReview(ctx context.Context, term uuid.UUID, body []byte, input *REGReview) (*REGReview, []*parsedCourse, error) {
	courses, _, _, err := parseNormalizedSheet(body)
	if err != nil {
		return nil, nil, err
	}
	if len(courses) > 2000 {
		return nil, nil, Invalid("ไฟล์มีมากกว่า 2,000 รายวิชา กรุณาแยกไฟล์")
	}
	if len(courses) == 0 {
		return nil, nil, errImportNoCourses
	}
	r := newREGReview(courses, body)
	originals := r
	var validTerm bool
	if err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM academic_terms WHERE id=$1)`, term).Scan(&validTerm); err != nil {
		return nil, nil, err
	}
	if !validTerm {
		return nil, nil, Invalid("ไม่พบภาคการศึกษา")
	}
	if input != nil {
		if err = bindREGReview(r, input); err != nil {
			return nil, nil, err
		}
		r = input
	}
	// Return all active candidates so ambiguous or absent first names can be
	// resolved explicitly without creating users as a side effect of import.
	rows, err := s.pool.Query(ctx, `SELECT u.id,u.first_name || ' ' || u.last_name,u.email::text FROM users u WHERE u.is_active AND u.deleted_at IS NULL AND EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id=u.id AND r.role='lecturer') ORDER BY u.first_name,u.last_name,u.id`)
	if err != nil {
		return nil, nil, err
	}
	r.Lecturers = []REGLecturer{}
	active := map[uuid.UUID]bool{}
	for rows.Next() {
		var l REGLecturer
		if err = rows.Scan(&l.ID, &l.Name, &l.Email); err != nil {
			rows.Close()
			return nil, nil, err
		}
		r.Lecturers = append(r.Lecturers, l)
		active[l.ID] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	unmatchedByCode := map[string][]string{}
	trustedLecturers := map[string][]uuid.UUID{}
	for i := range r.Courses {
		c := &r.Courses[i]
		var id, version string
		id, version, err = regExistingSnapshot(ctx, s.pool, term, c.Code)
		if err != nil {
			return nil, nil, err
		}
		c.ExistingID = id
		c.ExistingVersion = version
		if id != "" {
			c.Skip = true
			if strings.TrimSpace(c.SkipReason) == "" {
				c.SkipReason = "มีรายวิชาในระบบแล้ว เก็บข้อมูลเดิม"
			}
		}
		var matched []uuid.UUID
		var unmatched []string
		var e error
		if c.SourceFormat == "programme" {
			matched, unmatched = matchProgrammeOfficers(c.OfficerRaw, r.Lecturers)
		} else {
			matched, unmatched, e = s.matchOfficers(ctx, officerTokens(c.OfficerRaw))
		}
		if e != nil {
			return nil, nil, e
		}
		unmatchedByCode[c.Code] = unmatched
		c.UnmatchedOfficerNames = unmatched
		if input == nil {
			c.LecturerIDs = matched
			if id != "" {
				c.Skip = true
				c.SkipReason = "มีรายวิชาในระบบแล้ว เก็บข้อมูลเดิม"
			}
		}
		c.LecturerMatchVerified = trustedREGSourceLecturers(c.OfficerRaw, matched, unmatched) && sameREGOrderedLecturers(c.LecturerIDs, matched)
		if c.LecturerMatchVerified {
			trustedLecturers[c.Code] = matched
		}
	}
	validateREGReviewWithTrustedMatches(r, trustedLecturers)
	for i, c := range r.Courses {
		if c.Skip {
			continue
		}
		if names := unmatchedByCode[c.Code]; len(names) > 0 {
			regIssue(r, c.Code, "", "unmatched_names", "warning", "ชื่อจาก REG ที่ไม่พบหรือกำกวม: "+strings.Join(names, ", ")+" ตรวจเลือกบัญชีอาจารย์หรือยืนยันเหตุผลที่ยังไม่ผูก", c.LecturerIDs)
		}
		original := originals.Courses[i]
		for j, sec := range c.Sections {
			raw := regSourceSection(courses[i], sec.ID)
			if raw == nil {
				continue
			}
			if strings.Contains(raw.rawDays, "MW") || strings.Contains("\n"+raw.rawDays+"\n", "\nS\n") {
				regIssue(r, c.Code, sec.ID, "day_alias", "warning", fmt.Sprintf("แถว %d: แปล MW เป็นจันทร์และพุธ และ S เป็นเสาร์ กรุณาตรวจวันเรียน", sec.SourceRow), sec.Schedules)
			}
			if c.SourceFormat != "programme" && len(splitLines(raw.rawDays)) != len(splitLines(raw.rawTimes)) && strings.TrimSpace(raw.rawTimes) != "" {
				regIssue(r, c.Code, sec.ID, "alignment", "warning", fmt.Sprintf("แถว %d: จำนวนบรรทัดวันกับเวลาใน REG ไม่เท่ากัน ตรวจคาบที่แก้ไขทุกบรรทัด", sec.SourceRow), sec.Schedules)
			}
			before, _ := json.Marshal(original.Sections[j])
			after, _ := json.Marshal(sec)
			if string(before) != string(after) {
				regIssue(r, c.Code, sec.ID, "correction", "warning", fmt.Sprintf("แถว %d: ยืนยันค่าที่แก้ไขจาก REG สำหรับ Section นี้", sec.SourceRow), sec)
			}
		}
	}
	// Include the newly-added source warnings in readiness.
	ackSet := map[string]bool{}
	for _, id := range r.Acknowledged {
		ackSet[id] = true
	}
	for _, i := range r.Issues {
		if i.Severity == "error" || !ackSet[i.ID] {
			r.Ready = false
		}
	}
	for _, c := range r.Courses {
		if c.Skip {
			continue
		}
		seen := map[uuid.UUID]bool{}
		for _, id := range c.LecturerIDs {
			if !active[id] || seen[id] {
				regIssue(r, c.Code, "", "lecturer_id", "error", "บัญชีอาจารย์ไม่พร้อมใช้งานหรือเลือกซ้ำ", id)
				r.Ready = false
			}
			seen[id] = true
		}
	}
	// A merge target must exist, remain included, and cannot itself be merged.
	byCode := map[string]REGCourse{}
	for _, c := range r.Courses {
		byCode[c.Code] = c
	}
	for _, c := range r.Courses {
		if c.Skip || c.MergeInto == "" {
			continue
		}
		target, ok := byCode[c.MergeInto]
		if ok && target.ExistingID == "" && primaryCode(target.Code, []string{c.Code}) != target.Code {
			regIssue(r, c.Code, "", "merge_base", "error", "เลือกรหัสหลักสูตรใหม่กว่าเป็นวิชาหลัก (CP ก่อน SC ก่อนรหัสตัวเลข) เพื่อเก็บตารางและหน่วยกิตของหลักสูตรใหม่", c.MergeInto)
			r.Ready = false
		}
		if !ok || target.Code == c.Code || (target.Skip && target.ExistingID == "") || target.MergeInto != "" {
			regIssue(r, c.Code, "", "merge_target", "error", "รหัสหลักต้องเป็นวิชาในชุดนี้ที่ไม่ถูกข้ามหรือรวมต่อ และต้องไม่ใช่รหัสตัวเอง", c.MergeInto)
			r.Ready = false
		}
	}
	// Keep the acknowledgement order deterministic for a stable commit hash.
	sort.Strings(r.Acknowledged)
	r.ValidationHash = regValidationHash(r, term)
	return r, courses, nil
}

func (s *TeachingService) PreviewREGReview(ctx context.Context, actor, term uuid.UUID, filename string, body []byte, input *REGReview) (*ImportPreview, error) {
	priv, err := isPrivileged(ctx, s.pool, actor)
	if err != nil {
		return nil, err
	}
	if !priv {
		return nil, ErrForbidden
	}
	r, _, err := s.prepareREGReview(ctx, term, body, input)
	if err != nil {
		return nil, err
	}
	r.Confirmed = false
	return &ImportPreview{Filename: filename, Review: r, Courses: []ImportPreviewCourse{}, Warnings: []string{}, Errors: []string{}, MergeGroups: []ImportMergeGroup{}}, nil
}

// source evidence is bound by the file hash; every correction and exclusion is
// stored beside it in schedule_imports.review_data in the same transaction.
func regSourceData(c *parsedCourse, review REGCourse) []byte {
	type evidence struct {
		Header       []string     `json:"header"`
		Headers      []string     `json:"headers,omitempty"`
		Sections     [][]string   `json:"sections"`
		OriginalRows [][][]string `json:"original_rows,omitempty"`
		Reviewed     REGCourse    `json:"reviewed"`
	}
	v := evidence{Header: c.rawCells, Headers: c.sourceHeaders, Sections: [][]string{}, Reviewed: review}
	for _, key := range c.sectionsInOrder {
		v.Sections = append(v.Sections, c.sections[key].rawCells)
		if len(c.sections[key].originalRows) > 0 {
			v.OriginalRows = append(v.OriginalRows, c.sections[key].originalRows)
		}
	}
	b, _ := json.Marshal([]evidence{v})
	return b
}

func regSourceSection(c *parsedCourse, id string) *parsedSection {
	for _, key := range c.sectionsInOrder {
		s := c.sections[key]
		if fmt.Sprintf("%s:%d:%s", c.code, s.sourceRow, key) == id {
			return s
		}
	}
	return nil
}

func regExistingSnapshot(ctx context.Context, q querier, term uuid.UUID, code string) (id, version string, err error) {
	err = q.QueryRow(ctx, `SELECT COALESCE((SELECT id::text FROM teaching_courses WHERE term_id=$1 AND (code=$2 OR $2=ANY(alt_codes))),''),COALESCE((SELECT extract(epoch from updated_at)::text FROM teaching_courses WHERE term_id=$1 AND (code=$2 OR $2=ANY(alt_codes))),'')`, term, code).Scan(&id, &version)
	return
}
