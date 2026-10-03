package service

import (
	"bytes"
	"errors"
	"math"
	"os"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/pdfgen"
)

// Staff feedback round of 02/10/2026 — one test per behaviour that changed.

// An account closed because it was created wrong must not hold its e-mail
// hostage: a fresh account can take the address. But only ONE account per
// address may be active, so reopening the old one while the new one is live
// is refused with a reason that names the other account.
func TestEmailReusableOnceClosedButOneActiveAccount(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	users := &UserService{pool: f.Pool, aud: audit.New(f.Pool)}
	const email = "reuse@example.test"

	first, err := users.Create(f.ctx, f.StaffID, CreateUserInput{
		Email: email, FirstName: "ผิด", LastName: "บทบาท", Roles: []string{"lecturer"},
	})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	// While the first account is active the address is taken.
	if _, err := users.Create(f.ctx, f.StaffID, CreateUserInput{
		Email: email, FirstName: "ซ้ำ", LastName: "ซ้อน", Roles: []string{"ta"},
	}); !isConflict(err) {
		t.Fatalf("second active account on the same e-mail must be refused, got %v", err)
	}

	if err := users.Deactivate(f.ctx, f.StaffID, first.User.ID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	second, err := users.Create(f.ctx, f.StaffID, CreateUserInput{
		Email: email, FirstName: "ถูก", LastName: "ต้อง", Roles: []string{"ta"},
	})
	if err != nil {
		t.Fatalf("after closing the first account the e-mail must be free again: %v", err)
	}

	err = users.Activate(f.ctx, f.StaffID, first.User.ID)
	if !isConflict(err) {
		t.Fatalf("reopening the old account while the new one is active must be refused, got %v", err)
	}

	// Close the new one and the old one may come back.
	if err := users.Deactivate(f.ctx, f.StaffID, second.User.ID); err != nil {
		t.Fatalf("deactivate second: %v", err)
	}
	if err := users.Activate(f.ctx, f.StaffID, first.User.ID); err != nil {
		t.Fatalf("with no other active account the old one must reopen: %v", err)
	}
	// Sign-in resolves the address to the one active account.
	u, _, err := users.FindByEmail(f.ctx, email)
	if err != nil || u.ID != first.User.ID {
		t.Fatalf("FindByEmail must return the active account %s, got %v / %v", first.User.ID, u, err)
	}
}

func isConflict(err error) bool {
	var ue *UserError
	return errors.As(err, &ue) && ue.Status == 409
}

// เพดานงบรายวิชา is one figure per term shared by every course: unset = the
// formula; set = min(formula, cap), with the track split scaled so the pools
// still add up to the ceiling. A new term starts from the latest term's cap.
func TestBudgetCapIsMinOfFormulaAndCap(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	budget := &BudgetService{pool: f.Pool}
	teaching := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	f.exec(`UPDATE teaching_courses SET num_students_regular = 30, num_students_special = 30, num_students = 60 WHERE id = $1`, f.CourseID)

	before, err := budget.Compute(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if before.BudgetCapBaht != nil || before.CapApplied || before.PerCourseMaxBaht != before.FormulaBaht {
		t.Fatalf("no cap: the ceiling must be the formula, got %+v", before)
	}
	formula := before.FormulaBaht
	if formula <= 0 {
		t.Fatalf("fixture formula must be positive, got %v", formula)
	}

	low := math.Floor(formula / 2)
	if err := teaching.SetTermBudgetCap(f.ctx, f.StaffID, f.TermID, &low, false); err != nil {
		t.Fatalf("set cap: %v", err)
	}
	got, _ := budget.Compute(f.ctx, f.CourseID)
	if !got.CapApplied || got.PerCourseMaxBaht != low || got.FormulaBaht != formula {
		t.Fatalf("a cap below the formula must become the ceiling: %+v", got)
	}
	if d := got.TermPayRegular + got.TermPaySpecial - low; math.Abs(d) > 0.01 {
		t.Fatalf("track pools must add up to the capped ceiling, off by %v", d)
	}
	if v, _ := courseFormulaBudget(f.ctx, f.Pool, f.CourseID); math.Abs(v-low) > 0.01 {
		t.Fatalf("the edit guard must see the capped figure too, got %v want %v", v, low)
	}

	high := formula * 3
	if err := teaching.SetTermBudgetCap(f.ctx, f.StaffID, f.TermID, &high, false); err != nil {
		t.Fatalf("raise cap: %v", err)
	}
	got, _ = budget.Compute(f.ctx, f.CourseID)
	if got.CapApplied || got.PerCourseMaxBaht != formula {
		t.Fatalf("a cap above the formula must never raise the budget: %+v", got)
	}

	if err := teaching.SetTermBudgetCap(f.ctx, f.StaffID, f.TermID, nil, false); err != nil {
		t.Fatalf("clear cap: %v", err)
	}
	got, _ = budget.Compute(f.ctx, f.CourseID)
	if got.BudgetCapBaht != nil || got.PerCourseMaxBaht != formula {
		t.Fatalf("clearing the cap must return to the formula: %+v", got)
	}

	cap20k := 20000.0
	if err := teaching.SetTermBudgetCap(f.ctx, f.StaffID, f.TermID, &cap20k, false); err != nil {
		t.Fatal(err)
	}
	next, err := teaching.UpsertTerm(f.ctx, f.StaffID, Term{AcademicYear: 2570, Semester: 1,
		StartsOn: strPtr("2027-06-01"), EndsOn: strPtr("2027-10-31"),
		MidtermStartsOn: strPtr("2027-08-01"), MidtermEndsOn: strPtr("2027-08-07"),
		FinalStartsOn: strPtr("2027-10-20"), FinalEndsOn: strPtr("2027-10-27")})
	if err != nil {
		t.Fatalf("create next term: %v", err)
	}
	var inherited *float64
	_ = f.Pool.QueryRow(f.ctx, `SELECT course_budget_cap_baht::float8 FROM academic_terms WHERE id = $1`, next.ID).Scan(&inherited)
	if inherited == nil || *inherited != cap20k {
		t.Fatalf("a new term must start from the latest term's cap, got %v", inherited)
	}
}

// Pasted real enrolment: codes of one merged course are summed, unknown codes
// are reported, a dry run writes nothing, and the real run applies.
func TestBulkNumStudentsSumsMergedCodes(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	teaching := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	var code string
	if err := f.Pool.QueryRow(f.ctx, `SELECT code FROM teaching_courses WHERE id = $1`, f.CourseID).Scan(&code); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE teaching_courses SET alt_codes = ARRAY['SC999001'] WHERE id = $1`, f.CourseID)

	reg1, reg2, spc := 25, 7, 3
	rows := []BulkCountRow{
		{Code: code, Regular: &reg1},
		{Code: "sc 999001", Regular: &reg2, Special: &spc},
		{Code: "XX123456", Regular: &reg1},
	}
	preview, err := teaching.BulkSetNumStudents(f.ctx, f.StaffID, f.TermID, rows, false, false, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	var course, missing *BulkCountResult
	for i := range preview {
		switch preview[i].Status {
		case "not_found":
			missing = &preview[i]
		default:
			if preview[i].CourseID != nil {
				course = &preview[i]
			}
		}
	}
	if missing == nil || course == nil {
		t.Fatalf("want one course row and one not_found row, got %+v", preview)
	}
	if course.Status != "update" || *course.NewRegular != 32 || *course.NewSpecial != 3 || len(course.Codes) != 2 {
		t.Fatalf("merged codes must be summed into one course: %+v", course)
	}
	var stored int
	_ = f.Pool.QueryRow(f.ctx, `SELECT num_students_regular FROM teaching_courses WHERE id = $1`, f.CourseID).Scan(&stored)
	if stored != 40 {
		t.Fatalf("a dry run must not write, regular is now %d", stored)
	}

	if _, err := teaching.BulkSetNumStudents(f.ctx, f.StaffID, f.TermID, rows, false, false, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var r, s, total int
	_ = f.Pool.QueryRow(f.ctx, `SELECT num_students_regular, num_students_special, num_students FROM teaching_courses WHERE id = $1`, f.CourseID).Scan(&r, &s, &total)
	if r != 32 || s != 3 || total != 35 {
		t.Fatalf("applied counts = %d/%d (total %d), want 32/3 (35)", r, s, total)
	}
}

// A row that would take a course with students down to 0 — what the
// registrar shows before registration opens — is held back unless staff say
// so (03/10/2026).
func TestBulkNumStudentsHoldsBackDropToZero(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	teaching := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	var code string
	if err := f.Pool.QueryRow(f.ctx, `SELECT code FROM teaching_courses WHERE id = $1`, f.CourseID).Scan(&code); err != nil {
		t.Fatal(err)
	}
	zero := 0
	rows := []BulkCountRow{{Code: code, Regular: &zero, Special: &zero}}

	for _, dry := range []bool{true, false} {
		res, err := teaching.BulkSetNumStudents(f.ctx, f.StaffID, f.TermID, rows, false, false, dry)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 || res[0].Status != "zero" {
			t.Fatalf("dry=%v: %+v, want status zero", dry, res)
		}
	}
	var n int
	_ = f.Pool.QueryRow(f.ctx, `SELECT num_students FROM teaching_courses WHERE id = $1`, f.CourseID).Scan(&n)
	if n == 0 {
		t.Fatal("a held-back row was written")
	}

	res, err := teaching.BulkSetNumStudents(f.ctx, f.StaffID, f.TermID, rows, false, true, false)
	if err != nil || res[0].Status != "update" {
		t.Fatalf("with allowZero: %+v, %v", res, err)
	}
	_ = f.Pool.QueryRow(f.ctx, `SELECT num_students FROM teaching_courses WHERE id = $1`, f.CourseID).Scan(&n)
	if n != 0 {
		t.Fatalf("num_students = %d after an allowed drop to 0", n)
	}
}

// Documents print every registrar code of a merged course, primary first.
func TestCourseCodesSQLJoinsAltCodes(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	f.exec(`UPDATE teaching_courses SET code = 'CP245201', alt_codes = ARRAY['SC363001'] WHERE id = $1`, f.CourseID)
	var got string
	if err := f.Pool.QueryRow(f.ctx, `SELECT `+CourseCodesSQL("tc")+` FROM teaching_courses tc WHERE tc.id = $1`, f.CourseID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "CP245201/SC363001" {
		t.Fatalf("got %q, want CP245201/SC363001", got)
	}
	if fs := courseCodesFileSafe(got); fs != "CP245201_SC363001" {
		t.Fatalf("file-safe label %q", fs)
	}
}

// A student id the TA typed wrong is corrected in place — the active period
// and every assignment snapshot taken from it — not opened as a new period.
func TestCorrectTAStudentIDFixesSnapshots(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	docs := &DocsService{pool: f.Pool, aud: audit.New(f.Pool)}
	enrollment := uuid.New()
	f.exec(`INSERT INTO ta_enrollments (id, user_id, student_id, study_level) VALUES ($1, $2, '670000001-1', 'undergrad')`, enrollment, f.TAID)
	f.exec(`UPDATE users SET student_id = '670000001-1' WHERE id = $1`, f.TAID)
	f.exec(`UPDATE ta_request_assignments SET enrollment_id = $1, student_id_snapshot = '670000001-1' WHERE id = $2`, enrollment, f.AssignmentID)

	fixed := "670000002-9"
	if err := docs.CorrectTAIdentity(f.ctx, f.StaffID, f.TAID, CorrectTAIdentityInput{StudentID: &fixed, Reason: "แสดงบัตรที่ห้องธุรการ"}); err != nil {
		t.Fatalf("correct: %v", err)
	}
	var userSID, periodSID, snapSID string
	var periods int
	_ = f.Pool.QueryRow(f.ctx, `SELECT student_id FROM users WHERE id = $1`, f.TAID).Scan(&userSID)
	_ = f.Pool.QueryRow(f.ctx, `SELECT student_id FROM ta_enrollments WHERE id = $1`, enrollment).Scan(&periodSID)
	_ = f.Pool.QueryRow(f.ctx, `SELECT student_id_snapshot FROM ta_request_assignments WHERE id = $1`, f.AssignmentID).Scan(&snapSID)
	_ = f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM ta_enrollments WHERE user_id = $1`, f.TAID).Scan(&periods)
	if userSID != fixed || periodSID != fixed || snapSID != fixed || periods != 1 {
		t.Fatalf("users=%q period=%q snapshot=%q periods=%d; want all %q in ONE period", userSID, periodSID, snapSID, periods, fixed)
	}
	// No reason, no change.
	if err := docs.CorrectTAIdentity(f.ctx, f.StaffID, f.TAID, CorrectTAIdentityInput{StudentID: &fixed}); err == nil {
		t.Fatal("a correction without a reason must be refused")
	}
}

// The WBA year-4 rule reads the year off the student id, not a typed field.
func TestWBAUsesYearDerivedFromStudentID(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	ws := &WorkloadService{pool: f.Pool, aud: audit.New(f.Pool)}
	// Admitted 2566 → year 4 in academic year 2569; study_year left NULL.
	f.exec(`UPDATE users SET student_id = '663380001-2', study_year = NULL WHERE id = $1`, f.TAID)
	wba := []ClassBlock{{IsWBA: true, StartTime: "00:00", EndTime: "00:00"}}
	if err := ws.ReplaceClasses(f.ctx, f.TAID, f.TermID, wba); err != nil {
		t.Fatalf("a year-4 student (by id) must be allowed WBA: %v", err)
	}
	f.exec(`UPDATE users SET student_id = '683380001-2' WHERE id = $1`, f.TAID)
	if err := ws.ReplaceClasses(f.ctx, f.TAID, f.TermID, wba); err == nil {
		t.Fatal("a year-2 student (by id) must be refused WBA")
	}
}

// A merged course's CP sec 01 and SC sec 01 are one class: the timetable form
// draws it once, while a genuinely different slot or section stays.
func TestTimetableBlocksDedupeMergedCourseSections(t *testing.T) {
	b := TimetableBlock{Kind: "lecture", CourseCode: "CP245201/SC363001", SecNo: "01", Track: "regular",
		DayOfWeek: 1, StartTime: "09:00:00", EndTime: "12:00:00"}
	other := b
	other.SecNo = "02"
	got := dedupeTimetableBlocks([]TimetableBlock{b, b, other})
	if len(got) != 2 {
		t.Fatalf("want sec 01 once plus sec 02, got %d blocks: %+v", len(got), got)
	}
}

// After a correction, staff rebuild the TA's signed creditor form from the
// stored PDF with the new citizen ID: same document row and review status, new
// file, old file gone from storage.
func TestRegenerateCreditorFormReplacesStoredFile(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	store := newMemStore()
	docs := &DocsService{pool: f.Pool, aud: audit.New(f.Pool), store: store, pii: testPIICipher(t)}
	tpl, fonts := "../../assets/creditor_form_template.pdf", "../../assets/fonts"

	orig, err := pdfgen.FillCreditor(pdfgen.CreditorInput{TemplatePath: tpl, FontDir: fonts, Data: pdfgen.CreditorData{
		Prefix: "นาย", FullName: "ทดสอบ ระบบ", NationalID: "1100700000001", AccountNo: "1234567890",
	}})
	if err != nil {
		t.Fatal(err)
	}
	oldKey, size, err := store.Save("ta_docs", "form.pdf", bytes.NewReader(orig))
	if err != nil {
		t.Fatal(err)
	}
	docID := uuid.New()
	f.exec(`INSERT INTO ta_profiles (user_id, prefix, status) VALUES ($1, 'นาย', 'approved')`, f.TAID)
	f.exec(`INSERT INTO ta_documents (id, user_id, kind, filename, mime, size_bytes, storage_key, status)
	        VALUES ($1, $2, 'creditor_form', 'form.pdf', 'application/pdf', $3, $4, 'approved')`, docID, f.TAID, size, oldKey)

	nid := "3101234567893"
	if !validThaiCitizenID(nid) {
		t.Fatalf("fixture citizen id %s fails its own checksum", nid)
	}
	if err := docs.CorrectTAIdentity(f.ctx, f.StaffID, f.TAID, CorrectTAIdentityInput{NationalID: &nid, Reason: "แสดงบัตรที่ห้องธุรการ"}); err != nil {
		t.Fatalf("correct: %v", err)
	}
	body, _, err := docs.RegenerateCreditorForm(f.ctx, f.StaffID, f.TAID, tpl, fonts)
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if string(body[:5]) != "%PDF-" {
		t.Fatal("regenerated form is not a PDF")
	}
	var key, status string
	_ = f.Pool.QueryRow(f.ctx, `SELECT storage_key, status::text FROM ta_documents WHERE id = $1`, docID).Scan(&key, &status)
	if key == oldKey || status != "approved" {
		t.Fatalf("want a new file on the SAME approved row, got key changed=%v status=%s", key != oldKey, status)
	}
	if _, err := store.Open(oldKey); err == nil {
		t.Fatal("the old signed form must be deleted from storage")
	}
}

// Claim forms sign with the abbreviated rank as the account holds it — the
// lecturer on the หลักฐาน sheet and the certifier both (office, 02/10/2026).
func TestClaimSignaturesUseAbbreviatedRank(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	ex := &ExportService{pool: f.Pool}
	f.exec(`UPDATE users SET title = 'ผศ. ดร.' WHERE id = $1`, f.LecturerID)
	got, err := ex.lecturerSignatory(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ผศ. ดร.lecturer Test" {
		t.Fatalf("lecturer signs as %q, want the abbreviated rank", got)
	}

	f.exec(`INSERT INTO admin_officers (user_id, academic_prefix, full_name, title, is_active)
	        VALUES ($1, 'ผู้ช่วยศาสตราจารย์ ดร.', 'lecturer Test', 'หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์', TRUE)`, f.LecturerID)
	c, err := ex.ResolveCertifier(f.ctx, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "ผศ. ดร.lecturer Test" {
		t.Fatalf("certifier signs as %q, want the abbreviated rank", c.Name)
	}
}

// Migration 0145 folds an alternate-code section into its partner: students
// added, the same TA's assignment merged (a sitting logged on both copies kept
// once, a distinct one kept), makeups moved, the alternate section gone; a
// section with no partner is left alone.
func TestFoldAltCodeSectionsMigration(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	body, err := os.ReadFile("../../migrations/0145_fold_alt_code_sections.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE sections SET num_students = 30 WHERE id = $1`, f.SectionID)
	f.exec(`UPDATE teaching_courses SET alt_codes = ARRAY['SC999001'] WHERE id = $1`, f.CourseID)
	alt, lone := uuid.New(), uuid.New()
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track, num_students, course_code)
	        VALUES ($1, $2, 'SC999001-1', 'regular', 5, 'SC999001'),
	               ($3, $2, 'SC999001-7', 'special', 4, 'SC999001')`, alt, f.CourseID, lone) // no special section to fold into
	altAsg := uuid.New()
	f.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
	        SELECT $1, request_id, $2, ta_id, level FROM ta_request_assignments WHERE id = $3`, altAsg, alt, f.AssignmentID)
	d := day(1)
	f.exec(`INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity) VALUES
	          ($1, $3::date, '09:00', '11:00', 2, 'lecture'),
	          ($2, $3::date, '09:00', '11:00', 2, 'lecture'),
	          ($2, $3::date, '13:00', '14:00', 1, 'lecture')`, f.AssignmentID, altAsg, d)
	// Monday 27/7 is a lecture day of the partner (fixture: Mon 09–12), so its
	// makeup moves; Wednesday 29/7 is not, so that one has no class to make up.
	f.exec(`INSERT INTO makeup_schedules (section_id, original_date, makeup_date, start_time, end_time, kind)
	        VALUES ($1, '2026-07-27', '2026-08-01', '09:00', '11:00', 'lecture'),
	               ($1, '2026-07-29', '2026-08-02', '09:00', '11:00', 'lecture')`, alt)

	if _, err := f.Pool.Exec(f.ctx, string(body)); err != nil {
		t.Fatalf("migration: %v", err)
	}

	var secs, students, logs, makeups, asgs int
	_ = f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM sections WHERE teaching_course_id = $1`, f.CourseID).Scan(&secs)
	_ = f.Pool.QueryRow(f.ctx, `SELECT num_students FROM sections WHERE id = $1`, f.SectionID).Scan(&students)
	_ = f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM work_logs WHERE assignment_id = $1`, f.AssignmentID).Scan(&logs)
	_ = f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM makeup_schedules WHERE section_id = $1`, f.SectionID).Scan(&makeups)
	_ = f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM ta_request_assignments WHERE ta_id = $1`, f.TAID).Scan(&asgs)
	if secs != 2 { // the course's sec + the unpartnered SC999001-7
		t.Errorf("sections = %d, want 2 (alt folded, unpartnered kept)", secs)
	}
	if students != 35 {
		t.Errorf("partner students = %d, want 30+5", students)
	}
	if logs != 2 {
		t.Errorf("work logs on the kept assignment = %d, want 2 (duplicate sitting dropped, distinct one moved)", logs)
	}
	if makeups != 1 {
		t.Errorf("makeups on the partner = %d, want 1", makeups)
	}
	if asgs != 1 {
		t.Errorf("assignments for the TA = %d, want 1", asgs)
	}
	var exists bool
	_ = f.Pool.QueryRow(f.ctx, `SELECT EXISTS (SELECT 1 FROM sections WHERE id = $1)`, lone).Scan(&exists)
	if !exists {
		t.Error("a section with no partner must be left alone")
	}
}

// Staff fold a section the automatic rule left alone into the section it is
// taught with; a different track is refused.
func TestFoldSectionByHand(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	lone, special := uuid.New(), uuid.New()
	f.exec(`UPDATE sections SET num_students = 40 WHERE id = $1`, f.SectionID)
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track, num_students, course_code)
	        VALUES ($1, $3, 'SC999001-7', 'regular', 4, 'SC999001'),
	               ($2, $3, 'SC999001-8', 'special', 3, 'SC999001')`, lone, special, f.CourseID)
	if err := svc.FoldSection(f.ctx, f.StaffID, f.CourseID, special, f.SectionID); err == nil {
		t.Fatal("folding a special section into a regular one must be refused")
	}
	if err := svc.FoldSection(f.ctx, f.LecturerID, f.CourseID, lone, f.SectionID); err == nil {
		t.Fatal("a lecturer must not fold sections")
	}
	if err := svc.FoldSection(f.ctx, f.StaffID, f.CourseID, lone, f.SectionID); err != nil {
		t.Fatalf("fold: %v", err)
	}
	var n int
	var exists bool
	_ = f.Pool.QueryRow(f.ctx, `SELECT num_students FROM sections WHERE id = $1`, f.SectionID).Scan(&n)
	_ = f.Pool.QueryRow(f.ctx, `SELECT EXISTS (SELECT 1 FROM sections WHERE id = $1)`, lone).Scan(&exists)
	if exists || n != 44 {
		t.Fatalf("after fold: section still there=%v, partner students=%d (want gone, 40+4)", exists, n)
	}
}
