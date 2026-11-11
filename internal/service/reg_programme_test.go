package service

import (
	"encoding/json"
	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"
	"os"
	"ta-payment-back/internal/audit"
	"testing"
)

func programmeRows() [][]string {
	return [][]string{
		{"ลำดับที่", "รหัสวิชา", "ชื่อวิชา", "หน่วยกิต", "Section", "จำนวนรับ", " ปกติ", "พิเศษ", "วันเรียน", "เวลาเรียน", "อาจารย์ประจำวิชา", "หลักสูตร"},
		{"1", "CP373231", "WEB MAPPING APPLICATION", "3 (2-3-6)", "SEC 01", "47", "42", "", "อังคาร", "13:00-15:00 Lec ROOM-A", "ผศ. ดร.สมชาย ใจดี, อ.ดร.สมหญิง ใจงาม", "ภูมิสารสนเทศศาสตร์"},
		{"", "", "", "", "", "", "", "", "", "09:00-12:00 Lab ROOM-B"},
		{"", "", "", "", "SEC 02", "5", "", "0", "อังคาร", "13:00-15:00 Lec ROOM-A 15:00-18:00 Lab ROOM-B"},
	}
}
func programmeBook(t *testing.T, rows [][]string, merges bool) []byte {
	t.Helper()
	f := excelize.NewFile()
	defer f.Close()
	for r, row := range rows {
		axis, _ := excelize.CoordinatesToCellName(1, r+1)
		if err := f.SetSheetRow("Sheet1", axis, &row); err != nil {
			t.Fatal(err)
		}
	}
	if merges {
		for _, pair := range [][2]string{{"A2", "A4"}, {"B2", "B4"}, {"C2", "C4"}, {"D2", "D4"}, {"K2", "K4"}, {"L2", "L4"}, {"E2", "E3"}, {"F2", "F3"}, {"G2", "G3"}, {"I2", "I3"}} {
			if err := f.MergeCell("Sheet1", pair[0], pair[1]); err != nil {
				t.Fatal(err)
			}
		}
	}
	b, err := f.WriteToBuffer()
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestREGProgrammeMergedRowsAndEnrollment(t *testing.T) {
	body := programmeBook(t, programmeRows(), true)
	courses, _, _, err := parseNormalizedSheet(body)
	if err != nil {
		t.Fatal(err)
	}
	r := newREGReview(courses, body)
	if len(r.Courses) != 1 || len(r.Courses[0].Sections) != 2 {
		t.Fatalf("lost merged rows: %+v", r)
	}
	c := r.Courses[0]
	a, b := c.Sections[0], c.Sections[1]
	if c.SourceFormat != "programme" || len(c.SourceHeaders) != 12 || a.Curriculum != "GIS" {
		t.Fatalf("wrong source layout: %+v", c)
	}
	if *a.Students != 42 || *a.SourceCapacity != 47 || *b.Students != 0 || *b.SourceCapacity != 5 || a.Track != "regular" || b.Track != "special" {
		t.Fatalf("capacity/enrollment/zero/track confused: %+v %+v", a, b)
	}
	if len(a.Schedules) != 2 || len(b.Schedules) != 2 || a.Schedules[1].Room != "ROOM-B" || b.Schedules[1].Start != "15:00" || a.Schedules[1].Day != 2 {
		t.Fatalf("lost continued or inline meetings: %+v %+v", a.Schedules, b.Schedules)
	}
	if len(a.SourceRows) != 2 || a.SourceRows[1].Row != 3 || a.SourceRows[1].Cells[9] != "09:00-12:00 Lab ROOM-B" {
		t.Fatal("original continuation evidence lost")
	}
	data, _ := json.Marshal(r)
	var input REGReview
	json.Unmarshal(data, &input)
	input.Courses[0].SourceFormat = "raw"
	input.Courses[0].SourceHeaders = []string{"fake"}
	input.Courses[0].Sections[0].SourceCapacity = nil
	input.Courses[0].Sections[0].SourceRows = nil
	if err = bindREGReview(r, &input); err != nil {
		t.Fatal(err)
	}
	if input.Courses[0].SourceFormat != "programme" || *input.Courses[0].Sections[0].SourceCapacity != 47 || len(input.Courses[0].Sections[0].SourceRows) != 2 {
		t.Fatal("client could replace source evidence")
	}
}
func TestREGProgrammeUncertainDataRemainsReviewable(t *testing.T) {
	rows := programmeRows()
	rows[3][6] = "0" // both columns populated: do not guess a track/count
	rows[3][9] = "WBA"
	courses, _, _, err := parseNormalizedSheet(programmeBook(t, rows, true))
	if err != nil {
		t.Fatal(err)
	}
	r := newREGReview(courses, []byte("fixture"))
	s := r.Courses[0].Sections[1]
	if s.Track != "" || s.Students != nil || len(s.Schedules) != 1 || !s.Schedules[0].Pending {
		t.Fatalf("ambiguous data hidden: %+v", s)
	}
	validateREGReview(r)
	if r.Ready {
		t.Fatal("ambiguous input was accepted")
	}
	rows = programmeRows()
	rows[2][4] = ""
	rows[2][1] = "" // no merge connecting this continuation
	if _, _, _, err = parseNormalizedSheet(programmeBook(t, rows, false)); err == nil {
		t.Fatal("orphan schedule silently inherited a course")
	}
	slots := programmeMeetings("จันทร์\nพุธ\nศุกร์", "09:00-10:00 Lec A 10:00-11:00 Lab B")
	if len(slots) != 2 || !slots[0].Pending || !slots[1].Pending {
		t.Fatal("ambiguous day alignment was guessed")
	}
	if s := programmeMeetings("จันทร์", "09:00-10:00 Unknown ROOM"); len(s) != 1 || s[0].Kind != "unknown" {
		t.Fatal("unknown kind disappeared")
	}
}
func TestREGProgrammeFullNameMatching(t *testing.T) {
	a, b, dup := uuid.New(), uuid.New(), uuid.New()
	ls := []REGLecturer{{ID: a, Name: "สมชาย ใจดี"}, {ID: b, Name: "สมหญิง ใจงาม"}}
	matched, unmatched := matchProgrammeOfficers("ผศ. ดร.สมชาย ใจดี, อ.ดร.สมหญิง ใจงาม", ls)
	if !sameREGOrderedLecturers(matched, []uuid.UUID{a, b}) || len(unmatched) != 0 {
		t.Fatalf("full name/title handling failed: %v %v", matched, unmatched)
	}
	ls = append(ls, REGLecturer{ID: dup, Name: "สมชาย ใจดี"})
	matched, unmatched = matchProgrammeOfficers("ผศ.ดร.สมชาย ใจดี, อ ดร.สมหญิง ใจงาม", ls)
	if len(matched) != 1 || matched[0] != b || len(unmatched) != 1 {
		t.Fatal("duplicate full name was chosen automatically")
	}
	if programmeName("ศักดิ์พจน์ ทองเลี่ยมนาค") != "ศักดิ์พจน์ทองเลี่ยมนาค" {
		t.Fatal("a real name prefix was stripped as an honorific")
	}
}
func TestREGProgrammeOriginalWorkbook(t *testing.T) {
	path := os.Getenv("REG_GIS_IMPORT_FIXTURE")
	if path == "" {
		t.Skip("private GIS workbook not configured")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	courses, _, _, err := parseNormalizedSheet(body)
	if err != nil {
		t.Fatal(err)
	}
	r := newREGReview(courses, body)
	sections, regular, special, students, capacity, meetings, pending := 0, 0, 0, 0, 0, 0, 0
	for _, c := range r.Courses {
		for _, s := range c.Sections {
			sections++
			if s.Track == "regular" {
				regular++
			} else if s.Track == "special" {
				special++
			}
			if s.Students == nil || s.SourceCapacity == nil {
				t.Fatalf("lost counts %s section %s", c.Code, s.SecNo)
			}
			students += *s.Students
			capacity += *s.SourceCapacity
			for _, slot := range s.Schedules {
				if slot.Pending {
					pending++
				} else {
					meetings++
				}
			}
		}
	}
	if len(courses) != 15 || sections != 32 || regular != 17 || special != 15 || students != 600 || capacity != 1109 || meetings != 44 || pending != 10 {
		t.Fatalf("counts: courses=%d sections=%d regular=%d special=%d students=%d capacity=%d meetings=%d pending=%d", len(courses), sections, regular, special, students, capacity, meetings, pending)
	}
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	actor := f.insertUser("admin", "admin")
	p, err := svc.PreviewREGReview(f.ctx, actor, f.TermID, "gis.xlsx", body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if output := os.Getenv("REG_GIS_PREVIEW_OUTPUT"); output != "" {
		b, _ := json.Marshal(p)
		if err = os.WriteFile(output, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i := range p.Review.Courses {
		c := &p.Review.Courses[i]
		c.LecturerIDs = []uuid.UUID{f.LecturerID}
		for j := range c.Sections {
			if len(c.Sections[j].Schedules) == 0 {
				c.Sections[j].PendingReason = "รอข้อมูลตารางเรียน"
			}
			for _, s := range c.Sections[j].Schedules {
				if s.Pending {
					c.Sections[j].PendingReason = "รอข้อมูลตารางเรียน"
				}
			}
		}
	}
	p, err = svc.PreviewREGReview(f.ctx, actor, f.TermID, "gis.xlsx", body, p.Review)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range p.Review.Issues {
		if i.Severity == "error" {
			t.Fatal(i)
		}
		p.Review.Acknowledged = append(p.Review.Acknowledged, i.ID)
	}
	p, err = svc.PreviewREGReview(f.ctx, actor, f.TermID, "gis.xlsx", body, p.Review)
	if err != nil {
		t.Fatal(err)
	}
	p.Review.Confirmed = true
	if !p.Review.Ready {
		t.Fatal("review not ready")
	}
	result, err := svc.CommitREGReview(f.ctx, actor, f.TermID, "gis.xlsx", body, p.Review)
	if err != nil {
		t.Fatal(err)
	}
	var dbCount, dbStudents, dbSchedules, dbPending int
	if err = f.Pool.QueryRow(f.ctx, `SELECT count(*),sum(num_students),count(*) FILTER(WHERE reg_pending_schedule) FROM sections WHERE teaching_course_id=ANY($1)`, result.CreatedIDs).Scan(&dbCount, &dbStudents, &dbPending); err != nil {
		t.Fatal(err)
	}
	if err = f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM section_schedules ss JOIN sections s ON s.id=ss.section_id WHERE s.teaching_course_id=ANY($1)`, result.CreatedIDs).Scan(&dbSchedules); err != nil {
		t.Fatal(err)
	}
	if len(result.CreatedIDs) != 15 || dbCount != 32 || dbStudents != 600 || dbSchedules != 44 || dbPending != 10 {
		t.Fatalf("persisted courses=%d sections=%d students=%d schedules=%d pending=%d", len(result.CreatedIDs), dbCount, dbStudents, dbSchedules, dbPending)
	}
	var evidence []byte
	if err = f.Pool.QueryRow(f.ctx, `SELECT reg_source FROM teaching_courses WHERE term_id=$1 AND code='CP372012'`, f.TermID).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	var saved []struct {
		Reviewed REGCourse `json:"reviewed"`
	}
	if err = json.Unmarshal(evidence, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || len(saved[0].Reviewed.Sections[0].SourceRows) != 2 || *saved[0].Reviewed.Sections[0].SourceCapacity != 40 || *saved[0].Reviewed.Sections[0].Students != 0 {
		t.Fatal("source counts or continuation evidence not persisted")
	}
}
