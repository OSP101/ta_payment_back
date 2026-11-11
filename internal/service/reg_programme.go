package service

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"
)

var programmeRequiredHeaders = []string{"รหัสวิชา", "ชื่อวิชา", "หน่วยกิต", "Section", "จำนวนรับ", "ปกติ", "พิเศษ", "วันเรียน", "เวลาเรียน", "อาจารย์ประจำวิชา", "หลักสูตร"}

func programmeHeaderKey(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), "")) }
func programmeHeader(rows [][]string) (int, map[string]int, bool) {
	for r, row := range rows {
		if r >= 30 {
			break
		}
		cols := map[string]int{}
		for i, v := range row {
			cols[programmeHeaderKey(v)] = i
		}
		ok := true
		for _, h := range programmeRequiredHeaders {
			if _, found := cols[programmeHeaderKey(h)]; !found {
				ok = false
				break
			}
		}
		if ok {
			return r, cols, true
		}
	}
	return 0, nil, false
}

type programmeMerge struct{ col, start, end int }

// Only explicit vertical merges carry values forward. Blank separators never
// inherit an unrelated course/Section, and original evidence is never filled.
func parseProgrammeSheet(f *excelize.File, sheet string, rows [][]string, header int, cols map[string]int) (courses []*parsedCourse, warnings, errs []string, err error) {
	if len(rows) > 20000 {
		return nil, nil, nil, Invalid("ไฟล์มีมากกว่า 20,000 แถว กรุณาแยกไฟล์")
	}
	merges, e := f.GetMergeCells(sheet)
	if e != nil {
		return nil, nil, nil, Invalid(errImportUnreadableMsg)
	}
	vertical := []programmeMerge{}
	for _, m := range merges {
		a, r1, e1 := excelize.CellNameToCoordinates(m.GetStartAxis())
		b, r2, e2 := excelize.CellNameToCoordinates(m.GetEndAxis())
		if e1 == nil && e2 == nil && a == b && r2 > r1 {
			vertical = append(vertical, programmeMerge{a - 1, r1 - 1, r2 - 1})
		}
	}
	original := func(r, c int) string {
		if r < 0 || r >= len(rows) || c < 0 || c >= len(rows[r]) {
			return ""
		}
		return strings.TrimSpace(rows[r][c])
	}
	value := func(r int, h string) (string, int) {
		col := cols[programmeHeaderKey(h)]
		for _, m := range vertical {
			if m.col == col && r >= m.start && r <= m.end {
				return original(m.start, col), m.start
			}
		}
		return original(r, col), r
	}
	byCode := map[string]*parsedCourse{}
	sectionAnchors := map[string]*parsedSection{}
	for r := header + 1; r < len(rows); r++ {
		row := rows[r]
		if len(row) > 100 {
			return nil, nil, nil, Invalid(fmt.Sprintf("แถว %d มีคอลัมน์มากเกินรูปแบบรายวิชาหลักสูตร", r+1))
		}
		code, courseAnchor := value(r, "รหัสวิชา")
		rawSection, secAnchor := value(r, "Section")
		day, _ := value(r, "วันเรียน")
		meeting, _ := value(r, "เวลาเรียน")
		if code == "" {
			if rawSection != "" || meeting != "" || day != "" {
				return nil, nil, nil, Invalid(fmt.Sprintf("แถว %d: มี Section หรือตารางเรียนแต่ไม่ระบุรหัสวิชา และไม่มีเซลล์รวมเชื่อมกับรายวิชา", r+1))
			}
			continue
		}
		code = strings.ToUpper(code)
		if !courseCodeRe.MatchString(code) {
			return nil, nil, nil, Invalid(fmt.Sprintf("แถว %d: รหัสวิชา %q ไม่ถูกต้อง", r+1, code))
		}
		name, _ := value(r, "ชื่อวิชา")
		unit, _ := value(r, "หน่วยกิต")
		officer, _ := value(r, "อาจารย์ประจำวิชา")
		curriculum, _ := value(r, "หลักสูตร")
		c, exists := byCode[code]
		if !exists {
			cr, l, p, ss, ok := parseUnit(unit)
			c = &parsedCourse{sourceFormat: "programme", sourceHeaders: append([]string{}, rows[header]...), requiresReview: true, code: code, name: name, nameEN: name, unitRaw: unit, unitOK: ok, credits: cr, lectureHrs: l, labHrs: p, selfHrs: ss, level: "undergrad", firstRow: courseAnchor + 1, officerRaw: officer, rawCells: append([]string{}, rows[courseAnchor]...), sections: map[string]*parsedSection{}}
			byCode[code] = c
			courses = append(courses, c)
		} else if c.unitRaw != unit || normalizeCourseName(c.name) != normalizeCourseName(name) {
			return nil, nil, nil, Invalid(fmt.Sprintf("แถว %d: รหัส %s ซ้ำแต่ชื่อหรือหน่วยกิตต่างกัน", r+1, code))
		}
		if officer != "" && officer != c.officerRaw {
			known := false
			for _, part := range strings.Split(c.officerRaw, "\n") {
				if part == officer {
					known = true
				}
			}
			if !known {
				c.officerRaw += "\n" + officer
			}
		}
		if rawSection == "" {
			if meeting != "" || day != "" {
				return nil, nil, nil, Invalid(fmt.Sprintf("แถว %d: มีตารางเรียนแต่ไม่ระบุ Section หรือเซลล์รวมของ Section", r+1))
			}
			continue
		}
		anchorKey := fmt.Sprintf("%s:%d", code, secAnchor)
		sec, exists := sectionAnchors[anchorKey]
		if !exists {
			no := strings.TrimSpace(strings.TrimPrefix(strings.ToUpper(rawSection), "SEC"))
			if n, e := strconv.Atoi(no); e == nil {
				no = strconv.Itoa(n)
			}
			regular, _ := value(r, "ปกติ")
			special, _ := value(r, "พิเศษ")
			track := ""
			if regular != "" && special == "" {
				track = "regular"
			}
			if special != "" && regular == "" {
				track = "special"
			}
			capacity, _ := value(r, "จำนวนรับ")
			students := ""
			if track == "regular" {
				students = regular
			} else if track == "special" {
				students = special
			}
			n, _ := strconv.Atoi(students)
			key := sectionImportKey(no, track)
			if c.sections[key] != nil {
				key = fmt.Sprintf("%s|row%d", key, secAnchor+1)
			}
			programme := ""
			switch strings.TrimSpace(curriculum) {
			case "ภูมิสารสนเทศศาสตร์", "GIS", "CP-GIS", "SC-GIS":
				programme = "GIS"
			}
			sec = &parsedSection{secNo: no, track: track, numStudents: n, studentCountRaw: students, capacityRaw: capacity, curriculum: programme, sourceRow: secAnchor + 1, reserved: curriculum, rawCells: append([]string{}, rows[secAnchor]...), reviewSchedules: []REGSchedule{}}
			c.sections[key] = sec
			c.sectionsInOrder = append(c.sectionsInOrder, key)
			sectionAnchors[anchorKey] = sec
		}
		sec.originalRows = append(sec.originalRows, append([]string{}, row...))
		sec.sourceRows = append(sec.sourceRows, REGSourceRow{Row: r + 1, Cells: append([]string{}, row...)})
		sec.rawDays += day + "\n"
		sec.rawTimes += meeting + "\n"
		// A vertically merged time belongs to its anchor, not once per lower row.
		_, timeAnchor := value(r, "เวลาเรียน")
		if timeAnchor != r {
			continue
		}
		slots := programmeMeetings(day, meeting)
		sec.reviewSchedules = append(sec.reviewSchedules, slots...)
		for _, s := range slots {
			if !s.Pending && (s.Kind == "lecture" || s.Kind == "lab") {
				sec.schedules = append(sec.schedules, parsedSchedule{kind: s.Kind, dow: s.Day, startTime: s.Start, endTime: s.End, room: s.Room})
			}
		}
	}
	return courses, []string{}, []string{}, nil
}

var programmeTimeStart = regexp.MustCompile(`\d{1,2}\s*:\s*\d{2}\s*[-–]\s*\d{1,2}\s*:\s*\d{2}`)

func programmeDay(raw string) (int, bool) {
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "วัน"))
	for i, d := range []string{"อาทิตย์", "จันทร์", "อังคาร", "พุธ", "พฤหัสบดี", "ศุกร์", "เสาร์"} {
		if s == d {
			return i, true
		}
	}
	return dowFromAbbrev(s)
}
func programmeMeetings(day, times string) []REGSchedule {
	times = strings.TrimSpace(times)
	if times == "" {
		return nil
	}
	matches := programmeTimeStart.FindAllStringIndex(times, -1)
	if len(matches) == 0 {
		return []REGSchedule{{Day: -1, Pending: true, Raw: times}}
	}
	days := splitLines(day)
	out := []REGSchedule{}
	if prefix := strings.TrimSpace(times[:matches[0][0]]); prefix != "" {
		out = append(out, REGSchedule{Day: -1, Pending: true, Raw: prefix})
	}
	for i, loc := range matches {
		end := len(times)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		text := strings.TrimSpace(times[loc[0]:end])
		timeText := strings.ReplaceAll(times[loc[0]:loc[1]], "–", "-")
		start, finish, _ := parseTimeRange(timeText)
		normalise := func(v string) string {
			if n, ok := parseHM(v); ok {
				return fmt.Sprintf("%02d:%02d", n/60, n%60)
			}
			return v
		}
		start = normalise(start)
		finish = normalise(finish)
		fields := strings.Fields(times[loc[1]:end])
		kind, room := "", ""
		if len(fields) > 0 {
			kind = strings.ToLower(fields[0])
			if kind == "lec" {
				kind = "lecture"
			}
		}
		if len(fields) > 1 {
			room = strings.Join(fields[1:], " ")
			if strings.EqualFold(room, "WBA") {
				room = ""
			}
		}
		d := -1
		if len(days) == 1 {
			if n, ok := programmeDay(days[0]); ok {
				d = n
			}
		} else if len(days) == len(matches) {
			if n, ok := programmeDay(days[i]); ok {
				d = n
			}
		}
		_, a := parseHM(start)
		_, b := parseHM(finish)
		out = append(out, REGSchedule{Day: d, Start: start, End: finish, Kind: kind, Room: room, Pending: d < 0 || !a || !b, Raw: text})
	}
	return out
}

var programmeTitle = regexp.MustCompile(`^(?:(?:ผศ|รศ|ศ|ดร)\.\s*|(?:ผศ|รศ|ศ|ดร)\s+|อ\.\s*|อ\s+|อาจารย์\s+)`)

func programmeName(raw string) string {
	s := strings.TrimSpace(raw)
	for {
		next := programmeTitle.ReplaceAllString(s, "")
		if next == s {
			break
		}
		s = strings.TrimSpace(next)
	}
	return strings.ToLower(strings.Join(strings.Fields(s), ""))
}
func matchProgrammeOfficers(raw string, lecturers []REGLecturer) (matched []uuid.UUID, unmatched []string) {
	matched = []uuid.UUID{}
	unmatched = []string{}
	byName := map[string][]uuid.UUID{}
	for _, l := range lecturers {
		key := programmeName(l.Name)
		if key != "" {
			byName[key] = append(byName[key], l.ID)
		}
	}
	seen := map[uuid.UUID]bool{}
	for _, name := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == '\r' }) {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		ids := byName[programmeName(name)]
		if len(ids) != 1 {
			unmatched = append(unmatched, name)
			continue
		}
		if !seen[ids[0]] {
			matched = append(matched, ids[0])
			seen[ids[0]] = true
		}
	}
	return
}
