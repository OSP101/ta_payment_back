package service

import (
	"bytes"
	"encoding/json"
	"github.com/xuri/excelize/v2"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"ta-payment-back/internal/audit"
)

func regReviewRows() [][]string {
	return [][]string{
		{"", "", "", "", "", "", "", "", "", "", "", "", "", "", "ระบบจัดตารางเรียน"},
		{"รหัสวิชา", "ชื่อวิชา", "หน่วยกิต", "txtPrerequisite", "Section", "สำหรับ", "จำนวนรับ", "วันเรียน", "เวลาเรียน", "txtMExamDate", "txtMExamtime", "txtExamDate", "txtExamtime", "อาจารย์ประจำวิชา"},
		{"SC333302", "WEB MAPPING APPLICATION", "3 (2-3-6)", "NONE"},
		{"", "", "", "", "SEC 01", "ตรี : CP-GIS ปี 3", "5", "M\nTU", "13:00-15:00 Lec ROOM1\n09:00-12:00 Lab ROOM2", "", "", "", "", "ผู้สอน"},
		{"", "", "", "", "SEC 01", "ตรี (พิเศษ) : CP-GIS ปี 3", "7", "M\nTU", "13:00-15:00 Lec ROOM1\n09:00-12:00 Lab ROOM2", "", "", "", "", "ผู้สอน"},
	}
}

func TestREGSourceTracksAndMultiline(t *testing.T) {
	cs, _, _, err := parseRawRows(regReviewRows())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || len(cs[0].sections) != 2 {
		t.Fatalf("lost course/track: %+v", cs)
	}
	r := newREGReview(cs, []byte("file"))
	c := &r.Courses[0]
	c.LecturerIDs = []uuid.UUID{uuid.New()}
	if len(c.Sections) != 2 || c.Sections[1].Track != "special" || *c.Sections[1].Students != 7 || len(c.Sections[0].Schedules) != 2 {
		t.Fatalf("lost multiline/track: %+v", c)
	}
	validateREGReview(r)
	for _, i := range r.Issues {
		if i.Severity == "error" {
			t.Fatalf("unexpected validation: %+v", i)
		}
		r.Acknowledged = append(r.Acknowledged, i.ID)
	}
	validateREGReview(r)
	if !r.Ready {
		t.Fatal("fully acknowledged review not ready")
	}
	c.Sections[0].Schedules[0].End = "11:00"
	validateREGReview(r)
	if r.Ready {
		t.Fatal("invalid interval waived")
	}
}

func TestREGBindingAndAcknowledgements(t *testing.T) {
	cs, _, _, _ := parseRawRows(regReviewRows())
	source := newREGReview(cs, []byte("file"))
	b, _ := json.Marshal(source)
	var input REGReview
	json.Unmarshal(b, &input)
	input.Courses[0].Sections = input.Courses[0].Sections[:1]
	if bindREGReview(source, &input) == nil {
		t.Fatal("silently removed section")
	}
	json.Unmarshal(b, &input)
	input.FileSHA256 = "different"
	if bindREGReview(source, &input) == nil {
		t.Fatal("accepted different file")
	}
	json.Unmarshal(b, &input)
	input.Courses[0].LecturerIDs = []uuid.UUID{uuid.New()}
	input.Courses[0].Sections[0].Students = intp(0)
	validateREGReview(&input)
	if input.Ready {
		t.Fatal("unacknowledged warnings accepted")
	}
	for _, i := range input.Issues {
		input.Acknowledged = append(input.Acknowledged, i.ID)
	}
	validateREGReview(&input)
	if !input.Ready {
		t.Fatalf("not ready: %+v", input.Issues)
	}
	input.Courses[0].LecturerIDs = []uuid.UUID{uuid.New()}
	validateREGReview(&input)
	if input.Ready {
		t.Fatal("old acknowledgement accepted changed lecturer")
	}
}

func TestREGRealLegacyWorkbook(t *testing.T) {
	path := os.Getenv("REG_IMPORT_FIXTURE")
	if path == "" {
		t.Skip("set REG_IMPORT_FIXTURE to the original private REG .xls; private files are not committed")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cs, _, _, err := parseNormalizedSheet(body)
	if err != nil {
		t.Fatal(err)
	}
	r := newREGReview(cs, body)
	total, regular, special := 0, 0, 0
	for _, c := range r.Courses {
		for _, s := range c.Sections {
			total++
			if s.Track == "special" {
				special++
			} else {
				regular++
			}
		}
	}
	if len(cs) != 159 || total != 465 || regular != 297 || special != 168 {
		t.Fatalf("lost source data: courses=%d sections=%d regular=%d special=%d", len(cs), total, regular, special)
	}
	if cs[0].name != "ABCD FOR ALL PROFESSIONS" {
		t.Fatalf("corrupt BIFF text: %q", cs[0].name)
	}
	if expectedPath := os.Getenv("REG_EXPECTED_ROWS"); expectedPath != "" {
		data, e := os.ReadFile(expectedPath)
		if e != nil {
			t.Fatal(e)
		}
		var expected [][]string
		if e = json.Unmarshal(data, &expected); e != nil {
			t.Fatal(e)
		}
		actual, e := readLegacyREGRows(body)
		if e != nil {
			t.Fatal(e)
		}
		for i, row := range expected {
			for j, value := range row {
				got := ""
				if i < len(actual) && j < len(actual[i]) {
					got = actual[i][j]
				}
				if got != value {
					t.Fatalf("BIFF decode differs from independent reader at row %d col %d: got %q want %q", i+1, j+1, got, value)
				}
			}
		}
	}
	for _, c := range r.Courses {
		if c.Code == "CP352003" && len(c.Sections[0].Schedules) != 2 {
			t.Fatal("MW not expanded")
		}
	}
}

func TestREGRealWorkbookReviewedCommit(t *testing.T) {
	path := os.Getenv("REG_IMPORT_FIXTURE")
	if path == "" {
		t.Skip("private REG fixture not configured")
	}
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	actor := f.insertUser("admin", "admin")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xls", body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dest := os.Getenv("REG_PREVIEW_OUTPUT"); dest != "" {
		b, _ := json.Marshal(p)
		if err = os.WriteFile(dest, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	r := p.Review
	// Simulated staff corrections in the isolated test database only.
	for i := range r.Courses {
		c := &r.Courses[i]
		c.LecturerIDs = []uuid.UUID{f.LecturerID}
		for j := range c.Sections {
			sec := &c.Sections[j]
			if sec.Curriculum == "" {
				sec.Curriculum = "OTHER"
			}
			if len(sec.Schedules) == 0 {
				sec.PendingReason = "REG ยังไม่กำหนดตาราง เจ้าหน้าที่จะเติมภายหลัง"
			}
			for _, sch := range sec.Schedules {
				if sch.Pending {
					sec.PendingReason = "REG ยังไม่กำหนดตาราง เจ้าหน้าที่จะเติมภายหลัง"
				}
			}
		}
	}
	p, err = svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xls", body, r)
	if err != nil {
		t.Fatal(err)
	}
	r = p.Review
	for _, issue := range r.Issues {
		if issue.Severity == "error" {
			t.Fatalf("uncorrected issue %+v", issue)
		}
		r.Acknowledged = append(r.Acknowledged, issue.ID)
	}
	p, err = svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xls", body, r)
	if err != nil {
		t.Fatal(err)
	}
	r = p.Review
	r.Confirmed = true
	result, err := svc.CommitREGReview(f.ctx, actor, f.TermID, "reg.xls", body, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.CreatedIDs) != 159 {
		t.Fatalf("created %d courses", len(result.CreatedIDs))
	}
	var total, special int
	err = f.Pool.QueryRow(f.ctx, `SELECT count(*),count(*) FILTER(WHERE track='special') FROM sections WHERE teaching_course_id=ANY($1)`, result.CreatedIDs).Scan(&total, &special)
	if err != nil {
		t.Fatal(err)
	}
	if total != 465 || special != 168 {
		t.Fatalf("sections lost: total %d special %d", total, special)
	}
	var students int
	if err = f.Pool.QueryRow(f.ctx, `SELECT num_students FROM teaching_courses WHERE term_id=$1 AND code='CP001001'`, f.TermID).Scan(&students); err != nil || students != 7543 {
		t.Fatalf("large course count %d err %v", students, err)
	}
	var schedules, pending int
	if err = f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM section_schedules ss JOIN sections s ON s.id=ss.section_id WHERE s.teaching_course_id=ANY($1)`, result.CreatedIDs).Scan(&schedules); err != nil {
		t.Fatal(err)
	}
	if err = f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM sections WHERE teaching_course_id=ANY($1) AND reg_pending_schedule`, result.CreatedIDs).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if schedules != 590 || pending != 100 {
		t.Fatalf("multiline/pending data lost: schedules=%d pending=%d", schedules, pending)
	}
	t.Logf("159 courses / 465 sections / 168 special / 590 timetable records / 100 pending sections / 7,543 seats in CP001001 committed with source evidence")
}

func TestREGRollbackOnDatabaseFailure(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	actor := f.insertUser("admin", "admin")
	rows := regReviewRows()
	rows = append(rows, []string{"CP999999", "Force transaction rollback", "3 (3-0-6)"}, []string{"", "", "", "", "SEC 01", "ตรี : CP-CS", "10", "M", "09:00-12:00 Lec R"})
	body := buildREGXlsx(t, rows)
	p, err := svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := p.Review
	for i := range r.Courses {
		r.Courses[i].LecturerIDs = []uuid.UUID{f.LecturerID}
	}
	p, err = svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, r)
	if err != nil {
		t.Fatal(err)
	}
	r = p.Review
	for _, i := range r.Issues {
		r.Acknowledged = append(r.Acknowledged, i.ID)
	}
	p, err = svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, r)
	if err != nil {
		t.Fatal(err)
	}
	r = p.Review
	r.Confirmed = true
	_, err = f.Pool.Exec(f.ctx, `CREATE FUNCTION fail_reg_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.code='CP999999' THEN RAISE EXCEPTION 'forced test failure'; END IF;RETURN NEW;END $$;CREATE TRIGGER fail_reg_test BEFORE INSERT ON teaching_courses FOR EACH ROW EXECUTE FUNCTION fail_reg_test()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.CommitREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, r); err == nil {
		t.Fatal("expected forced database failure")
	}
	var count int
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM teaching_courses WHERE code IN('SC333302','CP999999')`).Scan(&count)
	if count != 0 {
		t.Fatal("partial course commit survived rollback")
	}
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM schedule_imports WHERE file_sha256=$1`, r.FileSHA256).Scan(&count)
	if count != 0 {
		t.Fatal("success ledger survived rollback")
	}
}

func TestREGAtomicCommitAndRevalidation(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	actor := f.insertUser("admin", "admin")
	// A raw sysTitle xlsx exercises the same parser as BIFF without committing
	// private staff/student data to the repository.
	body := buildREGXlsx(t, regReviewRows())
	p, err := svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := p.Review
	r.Courses[0].LecturerIDs = []uuid.UUID{f.LecturerID}
	p, err = svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, r)
	if err != nil {
		t.Fatal(err)
	}
	r = p.Review
	for _, i := range r.Issues {
		if i.Severity == "error" {
			t.Fatal(i)
		}
		r.Acknowledged = append(r.Acknowledged, i.ID)
	}
	p, err = svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, r)
	if err != nil {
		t.Fatal(err)
	}
	r = p.Review
	r.Confirmed = true
	old := r.Courses[0].Name
	r.Courses[0].Name = "changed after validation"
	if _, err = svc.CommitREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, r); err == nil {
		t.Fatal("stale review accepted")
	}
	var count int
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM teaching_courses WHERE code='SC333302'`).Scan(&count)
	if count != 0 {
		t.Fatal("stale review wrote data")
	}
	r.Courses[0].Name = old
	p, err = svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, r)
	if err != nil {
		t.Fatal(err)
	}
	r = p.Review
	r.Confirmed = true
	result, err := svc.CommitREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.CreatedIDs) != 1 {
		t.Fatalf("result %+v", result)
	}
	var students int
	f.Pool.QueryRow(f.ctx, `SELECT count(*),sum(num_students) FROM sections WHERE teaching_course_id=$1`, result.CreatedIDs[0]).Scan(&count, &students)
	if count != 2 || students != 12 {
		t.Fatalf("lost tracks: sections=%d students=%d", count, students)
	}
	var evidence string
	err = f.Pool.QueryRow(f.ctx, `SELECT review_data::text FROM schedule_imports WHERE file_sha256=$1`, r.FileSHA256).Scan(&evidence)
	if err != nil || !strings.Contains(evidence, "ตรี (พิเศษ)") {
		t.Fatalf("missing audit evidence %v", err)
	}
}

func buildREGXlsx(t *testing.T, rows [][]string) []byte {
	t.Helper()
	f := excelize.NewFile()
	defer f.Close()
	f.SetSheetName("Sheet1", "sysTitle")
	for i, row := range rows {
		if err := f.SetSheetRow("sysTitle", "A"+strconv.Itoa(i+1), &row); err != nil {
			t.Fatal(err)
		}
	}
	var b bytes.Buffer
	if err := f.Write(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestREGPendingTimetableBlocksDownstreamUntilCompleted(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	actor := f.insertUser("admin", "admin")
	body := buildREGXlsx(t, regReviewRows())
	p, err := svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := p.Review
	r.Courses[0].LecturerIDs = []uuid.UUID{f.LecturerID}
	r.Courses[0].Sections[0].Schedules[1].Pending = true
	r.Courses[0].Sections[0].PendingReason = "ยังไม่ทราบเวลาปฏิบัติการ"
	p, err = svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, r)
	if err != nil {
		t.Fatal(err)
	}
	r = p.Review
	for _, i := range r.Issues {
		if i.Severity == "error" {
			t.Fatal(i)
		}
		r.Acknowledged = append(r.Acknowledged, i.ID)
	}
	p, err = svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, r)
	if err != nil {
		t.Fatal(err)
	}
	r = p.Review
	r.Confirmed = true
	res, err := svc.CommitREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, r)
	if err != nil {
		t.Fatal(err)
	}
	c, err := svc.Get(f.ctx, res.CreatedIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !c.HasMissingSchedule {
		t.Fatal("partly-known timetable appears complete")
	}
	var sid uuid.UUID
	for _, sec := range c.Sections {
		if sec.Track == "regular" {
			sid = sec.ID
			if !sec.REGPendingSchedule || len(sec.Schedules) != 1 {
				t.Fatalf("pending source lost: %+v", sec)
			}
		}
	}
	err = svc.ReplaceSectionSchedules(f.ctx, actor, c.ID, sid, []SectionSchedule{{Kind: "lecture", DayOfWeek: 1, StartTime: "13:00", EndTime: "15:00"}, {Kind: "lab", DayOfWeek: 2, StartTime: "09:00", EndTime: "12:00"}})
	if err != nil {
		t.Fatal(err)
	}
	c, err = svc.Get(f.ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.HasMissingSchedule {
		t.Fatal("completed timetable still marked pending")
	}
}

func TestREGSourceDuplicateAndUnknownKindsAreReviewable(t *testing.T) {
	rows := regReviewRows()
	rows = append(rows, rows[3])
	cs, _, _, err := parseRawRows(rows)
	if err != nil {
		t.Fatal(err)
	}
	r := newREGReview(cs, []byte("file"))
	if len(r.Courses[0].Sections) != 3 {
		t.Fatal("duplicate source row disappeared")
	}
	r.Courses[0].LecturerIDs = []uuid.UUID{uuid.New()}
	validateREGReview(r)
	found := false
	for _, i := range r.Issues {
		if i.Severity == "error" && strings.Contains(i.Message, "ซ้ำ") {
			found = true
		}
	}
	if !found {
		t.Fatal("duplicate not blocked")
	}
	rows = regReviewRows()
	rows[3][8] = "13:00-15:00"
	rows[3][7] = "M"
	cs, _, _, err = parseRawRows(rows)
	if err != nil {
		t.Fatal(err)
	}
	r = newREGReview(cs, []byte("file"))
	r.Courses[0].LecturerIDs = []uuid.UUID{uuid.New()}
	validateREGReview(r)
	if r.Courses[0].Sections[0].Schedules[0].Kind != "" {
		t.Fatal("missing session kind was guessed")
	}
}

func TestREGNormalizedRetainsUnscheduledSourceRows(t *testing.T) {
	body := buildImportXlsxForHistoryTest(t, [][]string{{"CP123456", "Old normalized format", "3 (3-0-6)", "1", "ตรี : CP-CS", "30", "", "09:00-12:00", "lec", "ROOM"}})
	courses, _, parseErrors, err := parseNormalizedSheet(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(parseErrors) != 1 {
		t.Fatalf("legacy parse errors=%v", parseErrors)
	}
	r := newREGReview(courses, body)
	sec := &r.Courses[0].Sections[0]
	if len(sec.Schedules) != 1 || !sec.Schedules[0].Pending || sec.Schedules[0].Start != "09:00" {
		t.Fatalf("source meeting disappeared: %+v", sec)
	}
	if len(courses[0].sections["1"].originalRows) != 1 {
		t.Fatal("normalized source evidence lost")
	}
	r.Courses[0].LecturerIDs = []uuid.UUID{uuid.New()}
	validateREGReview(r)
	for _, i := range r.Issues {
		r.Acknowledged = append(r.Acknowledged, i.ID)
	}
	validateREGReview(r)
	if r.Ready {
		t.Fatal("missing-day meeting accepted without correction or pending reason")
	}
}

func TestREGOriginalNormalizedWorkbookReviewedCommit(t *testing.T) {
	path := os.Getenv("REG_OLD_IMPORT_FIXTURE")
	if path == "" {
		t.Skip("private old REG fixture not configured")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	actor := f.insertUser("admin", "admin")
	p, err := svc.PreviewREGReview(f.ctx, actor, f.TermID, "old.xlsx", body, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := p.Review
	if len(r.Courses) != 127 {
		t.Fatalf("courses=%d", len(r.Courses))
	}
	sections, pendingMeetings := 0, 0
	for i := range r.Courses {
		c := &r.Courses[i]
		c.LecturerIDs = []uuid.UUID{f.LecturerID}
		for j := range c.Sections {
			sec := &c.Sections[j]
			sections++
			if sec.Curriculum == "" {
				sec.Curriculum = "OTHER"
			}
			if sec.Students == nil {
				sec.Students = intp(0)
			}
			if len(sec.Schedules) == 0 {
				sec.PendingReason = "REG ยังไม่กำหนดตาราง"
			}
			for _, slot := range sec.Schedules {
				if slot.Pending {
					pendingMeetings++
					sec.PendingReason = "REG ยังไม่กำหนดวันหรือเวลา"
				}
			}
		}
	}
	if sections != 287 || pendingMeetings != 6 {
		t.Fatalf("old source completeness: sections=%d pending meetings=%d", sections, pendingMeetings)
	}
	p, err = svc.PreviewREGReview(f.ctx, actor, f.TermID, "old.xlsx", body, r)
	if err != nil {
		t.Fatal(err)
	}
	r = p.Review
	for _, i := range r.Issues {
		if i.Severity == "error" {
			t.Fatal(i)
		}
		r.Acknowledged = append(r.Acknowledged, i.ID)
	}
	p, err = svc.PreviewREGReview(f.ctx, actor, f.TermID, "old.xlsx", body, r)
	if err != nil {
		t.Fatal(err)
	}
	r = p.Review
	r.Confirmed = true
	res, err := svc.CommitREGReview(f.ctx, actor, f.TermID, "old.xlsx", body, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.CreatedIDs) != 127 {
		t.Fatalf("created=%d", len(res.CreatedIDs))
	}
	var count int
	if err = f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM sections WHERE teaching_course_id=ANY($1)`, res.CreatedIDs).Scan(&count); err != nil || count != sections {
		t.Fatalf("sections=%d err=%v", count, err)
	}
	t.Log("old XLSX: 127 courses / 287 sections; all 6 missing-day meetings retained for review")
}

func TestREGReviewedMergePreservesTrackCountsAndSource(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	actor := f.insertUser("admin", "admin")
	rows := regReviewRows()
	row := append([]string{}, rows[3]...)
	row[6] = "10"
	rows = append(rows, []string{"CP999999", "Primary course", "3 (2-3-6)"}, row)
	body := buildREGXlsx(t, rows)
	p, err := svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := p.Review
	for i := range r.Courses {
		r.Courses[i].LecturerIDs = []uuid.UUID{f.LecturerID}
	}
	r.Courses[0].MergeInto = "CP999999"
	p, err = svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, r)
	if err != nil {
		t.Fatal(err)
	}
	r = p.Review
	for _, i := range r.Issues {
		if i.Severity == "error" {
			t.Fatal(i)
		}
		r.Acknowledged = append(r.Acknowledged, i.ID)
	}
	p, err = svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, r)
	if err != nil {
		t.Fatal(err)
	}
	r = p.Review
	r.Confirmed = true
	res, err := svc.CommitREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.CreatedIDs) != 1 || len(res.MergedCodes) != 1 {
		t.Fatalf("wrong merge result %+v", res)
	}
	var regular, special, sources int
	err = f.Pool.QueryRow(f.ctx, `SELECT num_students_regular,num_students_special,jsonb_array_length(reg_source) FROM teaching_courses WHERE id=$1`, res.CreatedIDs[0]).Scan(&regular, &special, &sources)
	if err != nil {
		t.Fatal(err)
	}
	if regular != 15 || special != 7 || sources != 2 {
		t.Fatalf("merge lost counts/evidence: %d %d %d", regular, special, sources)
	}
}
