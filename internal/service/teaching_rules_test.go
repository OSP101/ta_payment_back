package service

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"

	"ta-payment-back/internal/audit"
)

// Regression tests for the course/term/makeup/import/holiday validation holes
// reproduced in real mode on 2026-10-01 (fix brief WP2). Each one failed
// before the fix — the value was accepted — and is refused (or confirmed)
// after.

func wantRuleErr(t *testing.T, err error, status int, contains string) {
	t.Helper()
	var ue *UserError
	if !errors.As(err, &ue) {
		t.Fatalf("want a %d UserError, got %v", status, err)
	}
	if ue.Status != status {
		t.Fatalf("status = %d, want %d (%s)", ue.Status, status, ue.Msg)
	}
	if contains != "" && !strings.Contains(ue.Msg, contains) {
		t.Fatalf("message %q does not mention %q", ue.Msg, contains)
	}
}

// ---- 1. Create applies the shared number rules ------------------------------

func TestCreateCourse_RefusesImplausibleNumbers(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)
	base := func(code string) CreateTeachingCourseInput {
		return CreateTeachingCourseInput{
			TermID: f.TermID, Code: code, NameTH: "วิชาทดสอบ",
			Credits: 3, LectureHrs: 3, LabHrs: 0, SelfHrs: 6,
			LecturerIDs: []uuid.UUID{f.LecturerID},
			Sections:    []CourseSectionInput{{SecNo: "1", Track: "regular", NumStudents: 40}},
		}
	}
	bad := map[string]func(in *CreateTeachingCourseInput){
		"negative section":   func(in *CreateTeachingCourseInput) { in.Sections[0].NumStudents = -50 },
		"2,000,000 students": func(in *CreateTeachingCourseInput) { in.Sections[0].NumStudents = 2000000 },
		"2,000,000 hours":    func(in *CreateTeachingCourseInput) { in.LectureHrs = 2000000 },
		"course over 3000":   func(in *CreateTeachingCourseInput) { in.NumStudents = 3001 },
		"credits 99":         func(in *CreateTeachingCourseInput) { in.Credits = 99 },
		"3 (9-9-9)":          func(in *CreateTeachingCourseInput) { in.LectureHrs, in.LabHrs, in.SelfHrs = 9, 9, 9 },
		"sections sum to 3002": func(in *CreateTeachingCourseInput) {
			in.Sections = append(in.Sections, CourseSectionInput{SecNo: "2", Track: "regular", NumStudents: 2962})
		},
		"reversed dates": func(in *CreateTeachingCourseInput) {
			in.StartsOn, in.EndsOn = strp(day(20)), strp(day(3))
		},
		"outside the term": func(in *CreateTeachingCourseInput) {
			in.StartsOn, in.EndsOn = strp("2030-01-01"), strp("2030-02-01")
		},
	}
	for name, mut := range bad {
		in := base("CP900001")
		mut(&in)
		_, err := svc.Create(f.ctx, f.StaffID, in)
		var ue *UserError
		if !errors.As(err, &ue) || ue.Status != 400 {
			t.Errorf("%s: want a 400 refusal, got %v", name, err)
		}
	}
	var n int
	if err := f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM teaching_courses WHERE code='CP900001'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d invalid courses were saved", n)
	}
	// The real registrar shape still passes, including a 36-hour lab.
	ok := base("CP900002")
	ok.Credits, ok.LectureHrs, ok.LabHrs, ok.SelfHrs = 9, 0, 36, 18
	if _, err := svc.Create(f.ctx, f.StaffID, ok); err != nil {
		t.Fatalf("a real 9 (0-36-18) course was refused: %v", err)
	}
}

func TestSectionAndHeadcountEdits_ShareTheCeilings(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)
	wantRuleErr(t, svc.UpdateSection(f.ctx, f.StaffID, f.CourseID, f.SectionID,
		UpdateSectionInput{NumStudents: intp(3001)}), 400, "3000")
	_, err := svc.AddSection(f.ctx, f.StaffID, f.CourseID, AddSectionInput{SecNo: "9", Track: "regular", NumStudents: -1})
	wantRuleErr(t, err, 400, "ติดลบ")
	// Per course, not per track: 2000 + 2000 is over.
	wantRuleErr(t, svc.SetNumStudents(f.ctx, f.StaffID, f.CourseID, -1, 2000, 2000, true), 400, "3000")
	wantRuleErr(t, svc.SetNumStudents(f.ctx, f.StaffID, f.CourseID, -1, -5, -1, true), 400, "ติดลบ")
	// /info: same notation rule as Create.
	wantRuleErr(t, svc.UpdateCourseInfo(f.ctx, f.StaffID, f.CourseID,
		UpdateCourseInfoInput{LectureHrs: intp(9), Credits: intp(3)}), 400, "หน่วยกิต")
}

// ---- 3. Budget-moving edits on a course with approved TAs --------------------

func courseBudget(t *testing.T, f *fixture) float64 {
	t.Helper()
	b, err := courseFormulaBudget(f.ctx, f.Pool, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBudgetChange_NeedsConfirmOnceTAsAreApproved(t *testing.T) {
	f := newFixture(t, fixtureOpts{}) // approved request + assignment
	svc := newTeachingSvc(t, f)
	f.exec(`UPDATE pay_rates SET ug_lecture_hours_per_credit = 1, ug_lab_hours_per_credit = 1,
	          baseline_students_lecture = 60, baseline_students_lab = 30, ug_workload_rate_regular = 300`)
	before := courseBudget(t, f)
	if before <= 0 {
		t.Fatalf("setup: budget = %v, want > 0", before)
	}

	// /info lecture_hrs 0 used to answer 200 and zero the ceiling.
	err := svc.UpdateCourseInfo(f.ctx, f.StaffID, f.CourseID, UpdateCourseInfoInput{LectureHrs: intp(0), LabHrs: intp(0)})
	wantRuleErr(t, err, 409, BudgetConfirmPrefix)
	if !strings.Contains(err.Error(), "ta Test") && !strings.Contains(err.Error(), "TA ที่ได้รับอนุมัติแล้ว 1 คน") {
		t.Errorf("preview does not name the affected TA: %v", err)
	}
	if got := courseBudget(t, f); got != before {
		t.Fatalf("refused edit still wrote: budget %v → %v", before, got)
	}
	// /num-students regular 0, same.
	wantRuleErr(t, svc.SetNumStudents(f.ctx, f.StaffID, f.CourseID, -1, 0, -1, false), 409, BudgetConfirmPrefix)
	// A section headcount, same.
	wantRuleErr(t, svc.UpdateSection(f.ctx, f.StaffID, f.CourseID, f.SectionID,
		UpdateSectionInput{NumStudents: intp(1)}), 409, BudgetConfirmPrefix)
	// An edit that does not move the budget needs no confirm.
	if err := svc.UpdateCourseInfo(f.ctx, f.StaffID, f.CourseID, UpdateCourseInfoInput{SelfHrs: intp(5)}); err != nil {
		t.Fatalf("self-study hours do not move the budget: %v", err)
	}
	// Confirmed, it goes through.
	if err := svc.SetNumStudents(f.ctx, f.StaffID, f.CourseID, -1, 10, -1, true); err != nil {
		t.Fatalf("confirmed change refused: %v", err)
	}
	if got := courseBudget(t, f); got >= before {
		t.Fatalf("budget %v → %v, want lower", before, got)
	}
}

func TestBudgetChange_NoConfirmWithoutApprovedTAs(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := newTeachingSvc(t, f)
	if err := svc.SetNumStudents(f.ctx, f.StaffID, f.CourseID, -1, 10, -1, false); err != nil {
		t.Fatalf("a course with no TA needs no confirmation: %v", err)
	}
}

func TestBudgetChange_RefusedOnceAMonthIsExported(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := newTeachingSvc(t, f)
	period := uuid.New()
	f.exec(`INSERT INTO submission_periods (id, term_id, year_month, starts_on, due_date, label)
	        VALUES ($1, $2, $3, $4::date, $5::date, 'เดือนทดสอบ')`,
		period, f.TermID, "2569-"+currentMonthMM(), day(1), openDueDate())
	f.exec(`INSERT INTO submission_period_status (id, submission_period_id, ta_id, teaching_course_id, status)
	        VALUES (gen_random_uuid(), $1, $2, $3, 'exported')`, period, f.TAID, f.CourseID)
	err := svc.SetNumStudents(f.ctx, f.StaffID, f.CourseID, -1, 0, -1, true)
	wantRuleErr(t, err, 409, "เดือนทดสอบ")
}

func TestUpdateCourseInfo_LevelIsRefusedNotIgnored(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	err := newTeachingSvc(t, f).UpdateCourseInfo(f.ctx, f.StaffID, f.CourseID,
		UpdateCourseInfoInput{Level: strp("graduate")})
	wantRuleErr(t, err, 400, "ระดับ")
}

// ---- 2. Term months are tied to the dates and frozen after export ---------

func TestUpsertTerm_MonthsBoundedByDatesAndFrozenAfterExport(t *testing.T) {
	svc, ctx, actor := newUpsertSvc(t)
	d := func(s string) *string { return &s }
	in := Term{AcademicYear: 2569, Semester: 1, Months: 12,
		StartsOn: d("2026-06-01"), EndsOn: d("2026-09-30"),
		MidtermStartsOn: d("2026-07-20"), MidtermEndsOn: d("2026-07-24"),
		FinalStartsOn: d("2026-09-21"), FinalEndsOn: d("2026-09-25")}
	_, err := svc.UpsertTerm(ctx, actor, in)
	wantRuleErr(t, err, 400, "1–4")

	in.Months = 0 // defaults to the span
	term, err := svc.UpsertTerm(ctx, actor, in)
	if err != nil {
		t.Fatal(err)
	}
	if term.Months != 4 {
		t.Fatalf("months = %d, want the 4 the dates span", term.Months)
	}

	// Export one month; months and dates are then frozen, exam windows not.
	var tcID, taID, period uuid.UUID = uuid.New(), uuid.New(), uuid.New()
	mustExec := func(sql string, args ...any) {
		if _, err := svc.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	mustExec(`INSERT INTO teaching_courses (id, term_id, code, name_th, level) VALUES ($1,$2,'CP777777','x','undergrad')`, tcID, term.ID)
	mustExec(`INSERT INTO users (id,email,first_name,last_name,is_active) VALUES ($1,$2,'t','a',TRUE)`, taID, "ta-"+taID.String()+"@example.test")
	mustExec(`INSERT INTO submission_periods (id, term_id, year_month, starts_on, due_date, label)
	          VALUES ($1,$2,'2569-07',DATE '2026-07-01',DATE '2026-08-05','ก.ค. 2569')`, period, term.ID)
	mustExec(`INSERT INTO submission_period_status (id, submission_period_id, ta_id, teaching_course_id, status)
	          VALUES (gen_random_uuid(),$1,$2,$3,'exported')`, period, taID, tcID)

	upd := in
	upd.ID = term.ID
	upd.Months = 3
	_, err = svc.UpsertTerm(ctx, actor, upd)
	wantRuleErr(t, err, 409, "ก.ค. 2569")
	upd.Months = 4
	upd.EndsOn = d("2026-09-29")
	_, err = svc.UpsertTerm(ctx, actor, upd)
	wantRuleErr(t, err, 409, "")
	upd.EndsOn = d("2026-09-30")
	upd.FinalEndsOn = d("2026-09-26")
	if _, err := svc.UpsertTerm(ctx, actor, upd); err != nil {
		t.Fatalf("exam window edits stay allowed after export: %v", err)
	}
}

func TestTermMonthSpan(t *testing.T) {
	d := func(s string) time.Time { v, _ := time.Parse("2006-01-02", s); return v }
	for _, c := range []struct {
		a, b string
		want int
	}{{"2026-06-01", "2026-09-30", 4}, {"2026-07-06", "2026-11-08", 5}, {"2026-11-16", "2027-03-14", 5}, {"2026-06-01", "2026-06-30", 1}} {
		if got := TermMonthSpan(d(c.a), d(c.b)); got != c.want {
			t.Errorf("%s..%s = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// ---- 4/5. Makeups and waivers stay inside the course -----------------------

func TestAddMakeup_RefusesDatesOutsideTheCourse(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	err := f.teaching().AddMakeup(f.ctx, f.LecturerID, f.SectionID, MakeupSchedule{
		OriginalDate: firstMonday(), MakeupDate: "2045-12-31", Kind: "lab",
	})
	wantRuleErr(t, err, 400, "ช่วงของรายวิชา")
}

func TestWaiveMakeup_AppliesAddMakeupsChecks(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	ts := f.teaching()
	// Outside the course (a Monday in 2040).
	wantRuleErr(t, ts.WaiveMakeup(f.ctx, f.LecturerID, f.SectionID,
		WaiveMakeupRequest{OriginalDate: "2040-01-02", Kind: "lab"}), 400, "ช่วงของรายวิชา")
	// A sitting with approved hours evidently ran.
	mon := firstMonday()
	f.insertLog(mon, "lab", "approved", "13:00", "16:00")
	wantRuleErr(t, ts.WaiveMakeup(f.ctx, f.LecturerID, f.SectionID,
		WaiveMakeupRequest{OriginalDate: mon, Kind: "lab"}), 400, "อนุมัติแล้ว")
	// A bad kind answers in Thai now that the tag is gone.
	wantRuleErr(t, ts.WaiveMakeup(f.ctx, f.LecturerID, f.SectionID,
		WaiveMakeupRequest{OriginalDate: mon, Kind: "x"}), 400, "ชนิดคาบ")
}

// A waiver of an ordinary (non-holiday) day must show up for the lecturer, or
// it can never be undone.
func TestHolidayImpacts_ListsNonHolidayWaivers(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	next := monthStart().AddDate(0, 0, firstMondayOffset()+7).Format("2006-01-02")
	if err := f.teaching().WaiveMakeup(f.ctx, f.LecturerID, f.SectionID,
		WaiveMakeupRequest{OriginalDate: next, Kind: "lecture"}); err != nil {
		t.Fatal(err)
	}
	res, err := f.holidays().ImpactsForCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.OtherMakeups) != 1 || res.OtherMakeups[0].OriginalDate != next || !res.OtherMakeups[0].Waived {
		t.Fatalf("other makeups = %+v, want the waiver of %s", res.OtherMakeups, next)
	}
}

// ---- 7. Holidays and request windows ---------------------------------------

func TestHoliday_RefusesDuplicatesAndAbsurdYears(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	hs := f.holidays()
	for _, y := range []string{"1900-01-01", "9999-12-31"} {
		_, err := hs.Create(f.ctx, f.StaffID, HolidayInput{HolidayDate: y, NameTH: "x"})
		wantRuleErr(t, err, 400, "นอกช่วง")
	}
	date := time.Now().AddDate(1, 0, 0).Format("2006-01-02")
	if _, err := hs.Create(f.ctx, f.StaffID, HolidayInput{HolidayDate: date, NameTH: "ครั้งแรก", Source: "faculty"}); err != nil {
		t.Fatal(err)
	}
	// Same date, same (whole-day) window, another type: still the same closure.
	_, err := hs.Create(f.ctx, f.StaffID, HolidayInput{HolidayDate: date, NameTH: "ซ้ำ", Source: "custom"})
	wantRuleErr(t, err, 400, "ครั้งแรก")
	// A different window on that date is a different row; only exact
	// repeats (same date + same window) are refused.
	if _, err := hs.Create(f.ctx, f.StaffID, HolidayInput{HolidayDate: date, NameTH: "บ่าย", Source: "custom",
		StartTime: strp("13:00"), EndTime: strp("16:00")}); err != nil {
		t.Fatalf("a different window was refused: %v", err)
	}
}

func TestRequestWindow_ClosesAfterItOpens(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	rs := &TARequestService{pool: f.Pool, aud: audit.New(f.Pool)}
	now := time.Now()
	_, err := rs.UpsertWindow(f.ctx, f.StaffID, Window{TermID: f.TermID, OpensAt: now, ClosesAt: now.Add(-time.Hour)})
	wantRuleErr(t, err, 400, "ปิดรับ")
}

// ---- 6. Import: preview and commit agree ----------------------------------

func buildImportXlsxWithOfficer(t *testing.T, rows [][]string) []byte {
	t.Helper()
	f := excelize.NewFile()
	if err := f.SetSheetName("Sheet1", "Normalized"); err != nil {
		t.Fatal(err)
	}
	header := []string{"CourseCode", "CourseName", "Unit", "Section", "ReservedFor",
		"TotalSeats", "Day", "Time", "SessionType", "Room", "Officer"}
	if err := f.SetSheetRow("Normalized", "A1", &header); err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		cell := row
		if err := f.SetSheetRow("Normalized", "A"+strconv.Itoa(i+2), &cell); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImport_PreviewAndCommitAgree(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	admin := f.insertUser("admin", "admin")
	body := buildImportXlsxWithOfficer(t, [][]string{
		{"ZZ100001", "หน่วยกิตเสีย", "abc", "1", "", "40", "M", "09:00-12:00", "lec", "R1", ""},
		{"ZZ100002", "หน่วยกิตเพี้ยน", "3 (9-9-9)", "1", "", "40", "M", "09:00-12:00", "lec", "R1", ""},
		{"ZZ100003", "อาจารย์ไม่รู้จัก", "3 (3-0-6)", "1", "", "40", "TH", "09:00-12:00", "lec", "R1", "ไม่มีชื่อนี้"},
		{"ZZ100004", "ปกติ", "3 (3-0-6)", "1", "", "40", "W", "09:00-12:00", "lec", "R1", ""},
	})
	p, err := svc.PreviewImport(f.ctx, admin, f.TermID, "x.xlsx", body)
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, c := range p.Courses {
		status[c.Code] = c.Status
		if c.Status == "invalid" && len(c.Problems) == 0 {
			t.Errorf("%s is invalid with no reason shown", c.Code)
		}
	}
	want := map[string]string{"ZZ100001": "invalid", "ZZ100002": "invalid", "ZZ100003": "unmatched_officer", "ZZ100004": "new"}
	for code, st := range want {
		if status[code] != st {
			t.Errorf("preview %s = %q, want %q", code, status[code], st)
		}
	}
	if p.InvalidCount != 2 || p.BlockedCount != 1 || p.NewCount != 1 {
		t.Errorf("counts invalid/blocked/new = %d/%d/%d, want 2/1/1", p.InvalidCount, p.BlockedCount, p.NewCount)
	}

	// No decision for the unmatched course → it is NOT created.
	res, err := svc.CommitImport(f.ctx, admin, f.TermID, "x.xlsx", body, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.CreatedCodes) != 1 || res.CreatedCodes[0] != "ZZ100004" {
		t.Fatalf("created = %v, want only ZZ100004", res.CreatedCodes)
	}
	if res.ErrorCount != 2 {
		t.Errorf("errors = %v, want the two invalid courses", res.Errors)
	}
	// With staff's explicit "create unassigned", it is.
	res, err = svc.CommitImport(f.ctx, admin, f.TermID, "x.xlsx", body, nil, nil, "ZZ100003")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.CreatedCodes) != 1 || res.CreatedCodes[0] != "ZZ100003" {
		t.Fatalf("created = %v, want ZZ100003", res.CreatedCodes)
	}
}

func TestImport_EmptyAndCorruptFilesAnswerInThai(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	admin := f.insertUser("admin", "admin")

	empty := buildImportXlsxWithOfficer(t, nil)
	_, err := svc.PreviewImport(f.ctx, admin, f.TermID, "empty.xlsx", empty)
	wantRuleErr(t, err, 400, "ไม่พบรายวิชา")
	_, err = svc.CommitImport(f.ctx, admin, f.TermID, "empty.xlsx", empty, nil, nil)
	wantRuleErr(t, err, 400, "ไม่พบรายวิชา")

	corrupt := append([]byte("PK\x03\x04"), []byte("not a real xlsx")...)
	_, err = svc.PreviewImport(f.ctx, admin, f.TermID, "bad.xlsx", corrupt)
	wantRuleErr(t, err, 400, "เปิดไฟล์ Excel ไม่ได้")
	if strings.Contains(err.Error(), "zip") || strings.Contains(err.Error(), "EOF") {
		t.Errorf("library error leaked: %q", err)
	}
}
