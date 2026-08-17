// export_claimbook.go builds the OFFICIAL faculty claim workbook — the exact
// document the college signs (แบบใบเบิกค่าตอบแทนผู้ช่วยสอนและผู้ช่วยปฏิบัติงาน),
// one file per (TA × course × month), three sheets:
//
//	ตารางสอน     the TA's weekly grid across EVERY course they assist
//	ภาคปกติ       claim rows for the regular sections (secs merged per sitting)
//	โครงการพิเศษ  claim rows for the special sections
//
// Built on a TEMPLATE cut from the college's own file (assets/templates/
// ta_claim_form.xlsx) so the formulas, fonts, merges and checkbox text are
// theirs, not a reconstruction: the claim sheets' H/I columns COMPUTE the hours
// from the printed time range, and the totals/money lines chain off those.
// This code only writes data cells and the timetable blocks.
//
// Row-building rules were decoded from the college's filled examples
// (test_webapp_it/*.xlsx) and are locked by the row-level comparison there:
//   - a sheet covers ONE track; sections sharing a sitting merge into one row
//     ("ปกติ Sec1-2", "13.00 - 17.00" = sec1 13-15 + sec2 15-17);
//   - เช็คชื่อ/ตรวจงาน bill in the บรรยาย column, สอนปฏิบัติ in ปฏิบัติการ —
//     enforced by the template's own formulas keyed on the หมายเหตุ text;
//   - the day abbreviation prints only on the first row of each date.
package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"
)

var thaiMonthNames = [...]string{"", "มกราคม", "กุมภาพันธ์", "มีนาคม", "เมษายน",
	"พฤษภาคม", "มิถุนายน", "กรกฎาคม", "สิงหาคม", "กันยายน", "ตุลาคม",
	"พฤศจิกายน", "ธันวาคม"}

var thaiDayAbbrev = [...]string{"อา.", "จ.", "อ.", "พ.", "พฤ.", "ศ.", "ส."}

// claimLogRow is one work_log joined with its section.
type claimLogRow struct {
	SecNo    string
	Track    string
	Date     time.Time
	StartMin int
	EndMin   int
	Activity string
	Makeup   bool
}

// claimSheetRow is one printed row of a claim sheet.
type claimSheetRow struct {
	Date  time.Time
	Group string // "ปกติ Sec1-2"
	Range string // "13.00 - 17.00"
	Note  string // "สอนปฏิบัติ"
}

func claimNote(activity string, makeup bool) string {
	base := map[string]string{
		"lecture": "เช็คชื่อ", "lab": "สอนปฏิบัติ", "review": "ตรวจงาน", "other": "งานอื่นๆ",
	}[activity]
	if base == "" {
		base = activity
	}
	if makeup {
		return base + " (ชดเชย)"
	}
	return base
}

func fmtClaimRange(s, e int) string {
	return fmt.Sprintf("%02d.%02d - %02d.%02d", s/60, s%60, e/60, e%60)
}

// secRunLabel renders a sorted distinct section list the way the forms do:
// "Sec1", "Sec1-2", "Sec1-3".
func secRunLabel(secs []string) string {
	uniq := map[string]bool{}
	for _, s := range secs {
		uniq[s] = true
	}
	var list []string
	for s := range uniq {
		list = append(list, s)
	}
	sort.Strings(list)
	if len(list) == 1 {
		return "Sec" + list[0]
	}
	return "Sec" + list[0] + "-" + list[len(list)-1]
}

// buildClaimSheetRows merges one track's logs for one month into printed rows.
func buildClaimSheetRows(rows []claimLogRow, trackWord string) []claimSheetRow {
	type key struct {
		date time.Time
		note string
	}
	grouped := map[key][]claimLogRow{}
	for _, r := range rows {
		grouped[key{r.Date, claimNote(r.Activity, r.Makeup)}] = append(
			grouped[key{r.Date, claimNote(r.Activity, r.Makeup)}], r)
	}
	var out []claimSheetRow
	for k, g := range grouped {
		sort.Slice(g, func(i, j int) bool { return g[i].StartMin < g[j].StartMin })
		curS, curE := g[0].StartMin, g[0].EndMin
		secs := []string{g[0].SecNo}
		flush := func() {
			out = append(out, claimSheetRow{
				Date: k.date, Group: trackWord + " " + secRunLabel(secs),
				Range: fmtClaimRange(curS, curE), Note: k.note,
			})
		}
		for _, r := range g[1:] {
			if r.StartMin <= curE { // overlapping or contiguous = one sitting
				if r.EndMin > curE {
					curE = r.EndMin
				}
				secs = append(secs, r.SecNo)
			} else {
				flush()
				curS, curE, secs = r.StartMin, r.EndMin, []string{r.SecNo}
			}
		}
		flush()
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Date.Equal(out[j].Date) {
			return out[i].Date.Before(out[j].Date)
		}
		return out[i].Range < out[j].Range
	})
	return out
}

// ttColOf maps minutes-from-midnight onto the grid column (C = 08:00, one
// column per half hour).
func ttColOf(min int) int { return 3 + (min-8*60)/30 }

// gridBlock is one merged, coloured block on the ตารางสอน sheet.
type gridBlock struct {
	Row      int
	StartMin int
	EndMin   int
	Label    string
	Course   string // course code, "" for the TA's own classes
	TARow    bool   // TA duty style (small red) vs class style
}

// timetableCoursePalette is the set of fills the college's own filled
// timetables use to tell one course's blocks from another's: every course gets
// ONE colour, worn by its class row, its TA duty row and its line in the
// signature block alike. Cycled over courses sorted by code, which reproduces
// the college's example exactly (CP321002 green, SC362004 peach, SC363101
// blue). The TA's own classes stay yellow, outside the palette.
var timetableCoursePalette = []string{"D8E4BD", "FEE9D9", "DAEEF3"}

const timetableOwnClassFill = "FFFF00"

// timetableCourseColors assigns each course the TA assists its palette colour.
// Deterministic (codes sorted, palette cycled) so the grid and the signature
// block — filled by different functions — always agree on a course's colour.
func (s *ExportService) timetableCourseColors(ctx context.Context, taID, termID uuid.UUID) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT tc.code
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id=a.section_id
		JOIN teaching_courses tc ON tc.id=sec.teaching_course_id AND tc.term_id=$2
		WHERE a.ta_id=$1 AND a.state <> 'dropped'`, taID, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var codes []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(codes)
	colors := make(map[string]string, len(codes))
	for i, code := range codes {
		colors[code] = timetableCoursePalette[i%len(timetableCoursePalette)]
	}
	return colors, nil
}

// dayRow: the sheet fixes two rows per day.
var claimDayRow = map[int]int{1: 6, 2: 8, 3: 10, 4: 12, 5: 14, 6: 16, 0: 18}

// trackWords renders the (ปกติ)/(พิเศษ)/(ปกติ-พิเศษ) suffix.
func trackWords(tracks map[string]bool) string {
	switch {
	case tracks["regular"] && tracks["special"]:
		return "ปกติ-พิเศษ"
	case tracks["special"]:
		return "พิเศษ"
	default:
		return "ปกติ"
	}
}

func (s *ExportService) claimTemplatePath() string {
	// Kept beside the fonts: both are render-time assets resolved from the
	// working directory in dev and the image root in deployment.
	return "assets/templates/ta_claim_form.xlsx"
}

// claimLogs loads the month's work logs of every assignment the TA holds on
// the course. Drafts count: the form documents the plan the lecturer signs,
// and the export gate upstream decides what may leave the building.
func (s *ExportService) claimLogs(ctx context.Context, taID, courseID uuid.UUID, year, month int) ([]claimLogRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sec.sec_no, sec.track::text, wl.work_date,
		       EXTRACT(HOUR FROM wl.start_time)*60 + EXTRACT(MINUTE FROM wl.start_time),
		       EXTRACT(HOUR FROM wl.end_time)*60 + EXTRACT(MINUTE FROM wl.end_time),
		       wl.activity, COALESCE(wl.note,'') LIKE '%ชดเชย%'
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		JOIN sections sec ON sec.id = a.section_id
		WHERE a.ta_id = $1 AND sec.teaching_course_id = $2
		  AND wl.status <> 'rejected'
		  AND EXTRACT(YEAR FROM wl.work_date) = $3
		  AND EXTRACT(MONTH FROM wl.work_date) = $4
		ORDER BY wl.work_date, wl.start_time`, taID, courseID, year, month)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []claimLogRow
	for rows.Next() {
		var r claimLogRow
		var sm, em float64
		if err := rows.Scan(&r.SecNo, &r.Track, &r.Date, &sm, &em, &r.Activity, &r.Makeup); err != nil {
			return nil, err
		}
		r.StartMin, r.EndMin = int(sm), int(em)
		out = append(out, r)
	}
	return out, rows.Err()
}

// fillTimetableGrid draws the weekly grid: row A of each day carries the
// teaching schedule of the TA's courses, their own classes, and their grading
// slots; row B carries the TA's working blocks (เช็คชื่อ and lab duty).
// claimDutyLabel names a TA-nominated duty slot the way the printed grid does.
// "ตรวจงาน" used to be hard-coded here, which was right when grading was the
// only kind of slot and wrong the moment other-work joined it.
func claimDutyLabel(kind string) string {
	if kind == DutyReview {
		return "ตรวจงาน"
	}
	return "งานอื่นๆ"
}

func (s *ExportService) fillTimetableGrid(ctx context.Context, f *excelize.File, taID, termID uuid.UUID) error {
	const tt = "ตารางสอน"
	colors, err := s.timetableCourseColors(ctx, taID, termID)
	if err != nil {
		return err
	}
	// Block styles, built per (fill, kind, size) as needed. Class blocks are
	// TH Sarabun New 12 bold black; TA duty blocks bold RED, sized to fit
	// their box the way the college's file hand-shrinks them.
	type blockStyleKey struct {
		fill string
		ta   bool
		size float64
	}
	styleCache := map[blockStyleKey]int{}
	blockStyle := func(fill string, ta bool, size float64) (int, error) {
		k := blockStyleKey{fill, ta, size}
		if id, ok := styleCache[k]; ok {
			return id, nil
		}
		font := &excelize.Font{Family: "TH Sarabun New", Size: size, Bold: true}
		if ta {
			font.Color = "FF0000"
		}
		id, err := f.NewStyle(&excelize.Style{
			Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{fill}},
			Font:      font,
			Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
			Border:    thinBorder(),
		})
		if err != nil {
			return 0, err
		}
		styleCache[k] = id
		return id, nil
	}

	// The template's empty grid carries scars where the college's example
	// blocks were cleared: stray full boxes and bare cells with no rules at
	// all, mixed into the hour pattern (rows 6-9, 14-15 and 18 of the shipped
	// template). Repaint the WHOLE grid to the clean pattern first — top rule
	// plus hour verticals on each class row, bottom rule plus hour verticals
	// on each duty row, exactly the pattern the undamaged rows carry — so the
	// blocks below land on an even canvas whatever state the template is in.
	gridStyle := func(vertical string, edge string) (int, error) {
		return f.NewStyle(&excelize.Style{
			Font: &excelize.Font{Family: "TH Sarabun New", Size: 12},
			Border: []excelize.Border{
				{Type: vertical, Style: 1, Color: "000000"},
				{Type: edge, Style: 1, Color: "000000"},
			},
		})
	}
	var gridIDs [4]int // [class|duty][hourStart|hourEnd]
	for i, spec := range [][2]string{
		{"left", "top"}, {"right", "top"}, {"left", "bottom"}, {"right", "bottom"},
	} {
		id, err := gridStyle(spec[0], spec[1])
		if err != nil {
			return err
		}
		gridIDs[i] = id
	}
	const gridFirstCol, gridLastCol = 3, 28 // C (08:00) … AB (20:30)
	for _, dayRow := range claimDayRow {
		for col := gridFirstCol; col <= gridLastCol; col++ {
			i := (col - gridFirstCol) % 2 // 0 = hour start, 1 = hour end
			classCell, _ := excelize.CoordinatesToCellName(col, dayRow)
			dutyCell, _ := excelize.CoordinatesToCellName(col, dayRow+1)
			if err := f.SetCellStyle(tt, classCell, classCell, gridIDs[i]); err != nil {
				return err
			}
			if err := f.SetCellStyle(tt, dutyCell, dutyCell, gridIDs[2+i]); err != nil {
				return err
			}
		}
	}

	var blocks []gridBlock

	// (a) teaching periods of the TA's courses, merged across co-scheduled
	// sections: same course+kind+day+time = one sitting, one block.
	type sitKey struct {
		code, kind string
		day, s, e  int
	}
	sits := map[sitKey]*struct {
		secs   []string
		tracks map[string]bool
	}{}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT tc.code, ss.kind, ss.day_of_week,
		       EXTRACT(HOUR FROM ss.start_time)*60+EXTRACT(MINUTE FROM ss.start_time),
		       EXTRACT(HOUR FROM ss.end_time)*60+EXTRACT(MINUTE FROM ss.end_time),
		       sec.sec_no, sec.track::text
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id=a.request_id AND r.status='approved'
		JOIN sections sec ON sec.id=a.section_id
		JOIN teaching_courses tc ON tc.id=sec.teaching_course_id AND tc.term_id=$2
		JOIN section_schedules ss ON ss.section_id=sec.id
		WHERE a.ta_id=$1 AND a.state <> 'dropped'`, taID, termID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var code, kind, secNo, track string
		var day int
		var sm, em float64
		if err := rows.Scan(&code, &kind, &day, &sm, &em, &secNo, &track); err != nil {
			rows.Close()
			return err
		}
		k := sitKey{code, kind, day, int(sm), int(em)}
		if sits[k] == nil {
			sits[k] = &struct {
				secs   []string
				tracks map[string]bool
			}{tracks: map[string]bool{}}
		}
		sits[k].secs = append(sits[k].secs, secNo)
		sits[k].tracks[track] = true
	}
	rows.Close()
	for k, v := range sits {
		kindLbl := "Lect."
		if k.kind == "lab" {
			kindLbl = "Lab"
		}
		blocks = append(blocks, gridBlock{
			Row: claimDayRow[k.day], StartMin: k.s, EndMin: k.e, Course: k.code,
			Label: fmt.Sprintf("%s %s %s", k.code, secRunLabel(v.secs), kindLbl),
		})
	}

	// (b) the TA's own classes.
	ownRows, err := s.pool.Query(ctx, `
		SELECT COALESCE(NULLIF(course_code,''), NULLIF(course_label,''), 'วิชาเรียน'),
		       day_of_week,
		       EXTRACT(HOUR FROM start_time)*60+EXTRACT(MINUTE FROM start_time),
		       EXTRACT(HOUR FROM end_time)*60+EXTRACT(MINUTE FROM end_time)
		FROM ta_class_schedules WHERE user_id=$1 AND term_id=$2 AND NOT is_wba`, taID, termID)
	if err != nil {
		return err
	}
	for ownRows.Next() {
		var label string
		var day int
		var sm, em float64
		if err := ownRows.Scan(&label, &day, &sm, &em); err != nil {
			ownRows.Close()
			return err
		}
		blocks = append(blocks, gridBlock{
			Row: claimDayRow[day], StartMin: int(sm), EndMin: int(em), Label: label,
		})
	}
	ownRows.Close()

	// (c) grading slots — row A, merged like sittings.
	revSits := map[sitKey]*struct {
		secs   []string
		tracks map[string]bool
	}{}
	revRows, err := s.pool.Query(ctx, `
		SELECT tc.code, rs.day_of_week,
		       EXTRACT(HOUR FROM rs.start_time)*60+EXTRACT(MINUTE FROM rs.start_time),
		       EXTRACT(HOUR FROM rs.end_time)*60+EXTRACT(MINUTE FROM rs.end_time),
		       sec.sec_no, sec.track::text, rs.kind
		FROM ta_review_schedules rs
		JOIN ta_request_assignments a ON a.id=rs.assignment_id
		JOIN sections sec ON sec.id=a.section_id
		JOIN teaching_courses tc ON tc.id=sec.teaching_course_id AND tc.term_id=$2
		WHERE a.ta_id=$1 AND a.state <> 'dropped'`, taID, termID)
	if err != nil {
		return err
	}
	for revRows.Next() {
		var code, secNo, track, dutyKind string
		var day int
		var sm, em float64
		if err := revRows.Scan(&code, &day, &sm, &em, &secNo, &track, &dutyKind); err != nil {
			revRows.Close()
			return err
		}
		// Key on the duty kind so grading and other-work never merge into one
		// block — they print under different labels.
		k := sitKey{code, dutyKind, day, int(sm), int(em)}
		if revSits[k] == nil {
			revSits[k] = &struct {
				secs   []string
				tracks map[string]bool
			}{tracks: map[string]bool{}}
		}
		revSits[k].secs = append(revSits[k].secs, secNo)
		revSits[k].tracks[track] = true
	}
	revRows.Close()
	for k, v := range revSits {
		blocks = append(blocks, gridBlock{
			Row: claimDayRow[k.day], StartMin: k.s, EndMin: k.e, Course: k.code, TARow: true,
			Label: fmt.Sprintf("TA %s %s %s (%s)", k.code, secRunLabel(v.secs), claimDutyLabel(k.kind), trackWords(v.tracks)),
		})
	}

	// (d) working blocks (row B) from the generated weekly pattern: distinct
	// non-makeup (course, activity, weekday, window) across the term.
	//
	// Grad-special TAs are excluded here and sourced from their section's own
	// schedule instead (below): they no longer log work_logs at all (2026
	// meeting — the system computes their pay automatically), so there is
	// nothing left in work_logs to reconstruct their duty pattern from. Their
	// duty IS the class's own schedule, so that's what prints.
	dutyRows, err := s.pool.Query(ctx, `
		SELECT DISTINCT tc.code, wl.activity, EXTRACT(DOW FROM wl.work_date)::int,
		       EXTRACT(HOUR FROM wl.start_time)*60+EXTRACT(MINUTE FROM wl.start_time),
		       EXTRACT(HOUR FROM wl.end_time)*60+EXTRACT(MINUTE FROM wl.end_time),
		       sec.sec_no, sec.track::text
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id=wl.assignment_id
		JOIN sections sec ON sec.id=a.section_id
		JOIN users u ON u.id=a.ta_id
		JOIN teaching_courses tc ON tc.id=sec.teaching_course_id AND tc.term_id=$2
		WHERE a.ta_id=$1 AND wl.status <> 'rejected'
		  AND wl.activity IN ('lecture','lab')
		  AND COALESCE(wl.note,'') NOT LIKE '%ชดเชย%'
		  AND NOT (a.level::text IN ('master','phd') AND sec.track = 'special')`, taID, termID)
	if err != nil {
		return err
	}
	dutySits := map[sitKey]*struct {
		secs   []string
		tracks map[string]bool
	}{}
	for dutyRows.Next() {
		var code, act, secNo, track string
		var day int
		var sm, em float64
		if err := dutyRows.Scan(&code, &act, &day, &sm, &em, &secNo, &track); err != nil {
			dutyRows.Close()
			return err
		}
		k := sitKey{code, act, day, int(sm), int(em)}
		if dutySits[k] == nil {
			dutySits[k] = &struct {
				secs   []string
				tracks map[string]bool
			}{tracks: map[string]bool{}}
		}
		dutySits[k].secs = append(dutySits[k].secs, secNo)
		dutySits[k].tracks[track] = true
	}
	dutyRows.Close()

	// grad-special's duty pattern, read straight from the section's own
	// schedule rather than work_logs — see comment above.
	gradSpecialDutyRows, err := s.pool.Query(ctx, `
		SELECT DISTINCT tc.code, ss.kind, ss.day_of_week,
		       EXTRACT(HOUR FROM ss.start_time)*60+EXTRACT(MINUTE FROM ss.start_time),
		       EXTRACT(HOUR FROM ss.end_time)*60+EXTRACT(MINUTE FROM ss.end_time),
		       sec.sec_no, sec.track::text
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		JOIN sections sec ON sec.id = a.section_id AND sec.track = 'special'
		JOIN users u ON u.id = a.ta_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id AND tc.term_id = $2
		JOIN section_schedules ss ON ss.section_id = sec.id AND ss.kind IN ('lecture','lab')
		WHERE a.ta_id = $1 AND a.state <> 'dropped'
		  AND a.level::text IN ('master','phd')`, taID, termID)
	if err != nil {
		return err
	}
	for gradSpecialDutyRows.Next() {
		var code, act, secNo, track string
		var day int
		var sm, em float64
		if err := gradSpecialDutyRows.Scan(&code, &act, &day, &sm, &em, &secNo, &track); err != nil {
			gradSpecialDutyRows.Close()
			return err
		}
		k := sitKey{code, act, day, int(sm), int(em)}
		if dutySits[k] == nil {
			dutySits[k] = &struct {
				secs   []string
				tracks map[string]bool
			}{tracks: map[string]bool{}}
		}
		dutySits[k].secs = append(dutySits[k].secs, secNo)
		dutySits[k].tracks[track] = true
	}
	gradSpecialDutyRows.Close()
	if err := gradSpecialDutyRows.Err(); err != nil {
		return err
	}
	for k, v := range dutySits {
		var label string
		if k.kind == "lecture" {
			label = fmt.Sprintf("TA %s %s (%s) เช็คชื่อ", k.code, secRunLabel(v.secs), trackWords(v.tracks))
		} else {
			label = fmt.Sprintf("TA %s %s Lab (%s)", k.code, secRunLabel(v.secs), trackWords(v.tracks))
		}
		blocks = append(blocks, gridBlock{
			Row: claimDayRow[claimDOW(k.day)] + 1, StartMin: k.s, EndMin: k.e,
			Course: k.code, Label: label, TARow: true,
		})
	}

	// Draw. Blocks in the same lane never overlap by construction (clash gates
	// upstream); a duplicate key would merge over itself harmlessly. Each block
	// wears its course's colour; the TA's own classes stay yellow.
	for _, b := range blocks {
		c1 := ttColOf(b.StartMin)
		c2 := ttColOf(b.EndMin) - 1
		if c1 < 3 || c2 < c1 {
			continue
		}
		start, _ := excelize.CoordinatesToCellName(c1, b.Row)
		end, _ := excelize.CoordinatesToCellName(c2, b.Row)
		if c2 > c1 {
			if err := f.MergeCell(tt, start, end); err != nil {
				return err
			}
		}
		fill := timetableOwnClassFill
		if b.Course != "" {
			if c, ok := colors[b.Course]; ok {
				fill = c
			} else {
				fill = timetableCoursePalette[0]
			}
		}
		size := 12.0
		if b.TARow {
			// The college hand-shrinks red duty labels to their box; a label
			// that fits about eight characters per column keeps the larger
			// size, a denser one drops to the small one their file uses.
			size = 11.0
			if len([]rune(b.Label)) > (c2-c1+1)*8 {
				size = 8.5
			}
		}
		style, err := blockStyle(fill, b.TARow, size)
		if err != nil {
			return err
		}
		if err := f.SetCellStyle(tt, start, end, style); err != nil {
			return err
		}
		f.SetCellValue(tt, start, b.Label)
	}
	return nil
}

// claimDOW is the identity map kept for readability at the call site: work_log
// weekdays are already 0=Sunday…6=Saturday.
func claimDOW(d int) int { return d }

// fillClaimSignatures writes the student block and one block per lecturer,
// grouped exactly as the paper form groups them.
func (s *ExportService) fillClaimSignatures(ctx context.Context, f *excelize.File, taID, termID uuid.UUID, fullName string) error {
	const tt = "ตารางสอน"
	// The student block (B21–B23) and the claim sheets' A36 live in the
	// template as the college's own formulas (='('&I3&')' etc.) — writing
	// literals over them would break the linkage their file relies on.
	_ = fullName

	// Lecturers sign with their academic title, and each course line wears the
	// same fill its blocks wear on the grid above — both straight from the
	// college's own filled examples.
	colors, err := s.timetableCourseColors(ctx, taID, termID)
	if err != nil {
		return err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(NULLIF(u.title,''),'')||COALESCE(u.first_name,'')||' '||COALESCE(u.last_name,''),
		       tc.code, COALESCE(tc.name_th,'')
		FROM ta_requests r
		JOIN users u ON u.id=r.lecturer_id
		JOIN teaching_courses tc ON tc.id=r.teaching_course_id AND tc.term_id=$2
		JOIN ta_request_assignments a ON a.request_id=r.id
		WHERE a.ta_id=$1 AND r.status='approved' AND a.state <> 'dropped'
		GROUP BY u.id, u.title, u.first_name, u.last_name, tc.code, tc.name_th, r.submitted_at
		ORDER BY MIN(r.submitted_at)`, taID, termID)
	if err != nil {
		return err
	}
	defer rows.Close()
	type courseRef struct{ code, nameTH string }
	type signer struct {
		name    string
		courses []courseRef
	}
	var signers []signer
	for rows.Next() {
		var name, code, nameTH string
		if err := rows.Scan(&name, &code, &nameTH); err != nil {
			return err
		}
		found := false
		for i := range signers {
			if signers[i].name == name {
				signers[i].courses = append(signers[i].courses, courseRef{code, nameTH})
				found = true
			}
		}
		if !found {
			signers = append(signers, signer{name, []courseRef{{code, nameTH}}})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sort.SliceStable(signers, func(i, j int) bool {
		return len(signers[i].courses) < len(signers[j].courses)
	})
	// The template keeps the fills of the example it was cut from in the
	// course-line cells; clear them first so a signer with fewer courses does
	// not leave a stale coloured box behind.
	courseLine := func(size float64, fill string) (int, error) {
		st := &excelize.Style{
			Font:      &excelize.Font{Family: "TH Sarabun New", Size: size},
			Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
		}
		if fill != "" {
			st.Fill = excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{fill}}
		}
		return f.NewStyle(st)
	}
	for _, col := range []string{"I", "R"} {
		for r := 23; r <= 25; r++ {
			plain, err := courseLine(14, "")
			if err != nil {
				return err
			}
			if err := f.SetCellStyle(tt, col+fmt.Sprint(r), col+fmt.Sprint(r), plain); err != nil {
				return err
			}
		}
	}
	cols := []string{"I", "R"}
	for i, sg := range signers {
		if i >= len(cols) {
			break
		}
		col := cols[i]
		f.SetCellValue(tt, col+"21", "ลงชื่อ .......................................................")
		f.SetCellValue(tt, col+"22", "("+sg.name+")")
		for j, course := range sg.courses {
			prefix, size := "", 12.0
			if j == 0 {
				prefix, size = "อาจารย์ประจำวิชา ", 14.0
			}
			cell := fmt.Sprintf("%s%d", col, 23+j)
			f.SetCellValue(tt, cell, prefix+course.code+" "+course.nameTH)
			style, err := courseLine(size, colors[course.code])
			if err != nil {
				return err
			}
			if err := f.SetCellStyle(tt, cell, cell, style); err != nil {
				return err
			}
		}
	}
	return nil
}

func thinBorder() []excelize.Border {
	return []excelize.Border{
		{Type: "left", Style: 1, Color: "000000"},
		{Type: "right", Style: 1, Color: "000000"},
		{Type: "top", Style: 1, Color: "000000"},
		{Type: "bottom", Style: 1, Color: "000000"},
	}
}
