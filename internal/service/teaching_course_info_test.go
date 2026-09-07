package service

import (
	"strings"
	"testing"
)

// Correcting a course after it is open.
//
// The registrar file is the usual source of a course's code and name, but it
// arrives with typos and with courses missing. Until now the only way to fix one
// was to delete the course and rebuild every section and schedule by hand, which
// is why these fields need an edit path at all — and why that path has to be
// fenced: the code and name are printed on documents, and the same code opened
// twice would make every per-course figure ambiguous.

func courseInfoSvc(f *fixture) *TeachingService {
	return &TeachingService{pool: f.Pool, aud: f.Svc.aud}
}

func courseFields(f *fixture) (code, nameTH string, credits, lec, lab, self int) {
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT code, name_th, credits, lecture_hrs, lab_hrs, self_hrs
		FROM teaching_courses WHERE id = $1`, f.CourseID).
		Scan(&code, &nameTH, &credits, &lec, &lab, &self); err != nil {
		f.t.Fatal(err)
	}
	return
}

func strp(s string) *string { return &s }
func intp(i int) *int       { return &i }

func TestUpdateCourseInfo_StaffCanCorrectTheIdentityFields(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := courseInfoSvc(f)

	if err := svc.UpdateCourseInfo(f.ctx, f.StaffID, f.CourseID, UpdateCourseInfoInput{
		Code: strp("CP353201"), NameTH: strp("ชื่อวิชาที่แก้แล้ว"),
		Credits: intp(4), LectureHrs: intp(2), LabHrs: intp(4), SelfHrs: intp(6),
	}); err != nil {
		t.Fatalf("staff must be able to correct a course: %v", err)
	}
	code, name, credits, lec, lab, self := courseFields(f)
	if code != "CP353201" || name != "ชื่อวิชาที่แก้แล้ว" ||
		credits != 4 || lec != 2 || lab != 4 || self != 6 {
		t.Errorf("got %s / %s / %d(%d-%d-%d)", code, name, credits, lec, lab, self)
	}
}

// nil means "leave alone" — a screen that edits one field must not blank the
// rest simply by not sending them.
func TestUpdateCourseInfo_OmittedFieldsAreUntouched(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := courseInfoSvc(f)
	beforeCode, beforeName, beforeCredits, _, _, _ := courseFields(f)

	if err := svc.UpdateCourseInfo(f.ctx, f.StaffID, f.CourseID,
		UpdateCourseInfoInput{Credits: intp(2)}); err != nil {
		t.Fatal(err)
	}
	code, name, credits, _, _, _ := courseFields(f)
	if code != beforeCode || name != beforeName {
		t.Errorf("code/name changed to %s/%s when only credits were sent", code, name)
	}
	if credits != 2 || beforeCredits == 2 {
		t.Errorf("credits = %d, want 2 (was %d)", credits, beforeCredits)
	}
}

// The same code twice in one term makes every per-course figure ambiguous.
func TestUpdateCourseInfo_RefusesACodeAlreadyOpenThisTerm(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := courseInfoSvc(f)
	var otherCode string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT code FROM teaching_courses WHERE id = $1`, f.CourseID).Scan(&otherCode); err != nil {
		t.Fatal(err)
	}
	// A second course in the same term, then try to take the first one's code.
	f.exec(`INSERT INTO teaching_courses (id, term_id, code, name_th, level, credits, lecture_hrs, lab_hrs, self_hrs)
	        VALUES (gen_random_uuid(), $1, 'CP999999', 'อีกวิชา', 'undergrad', 3, 3, 0, 6)`, f.TermID)

	err := svc.UpdateCourseInfo(f.ctx, f.StaffID, f.CourseID,
		UpdateCourseInfoInput{Code: strp("CP999999")})
	if err == nil {
		t.Fatal("two courses now share one code in the same term")
	}
	if !strings.Contains(err.Error(), "CP999999") {
		t.Errorf("the refusal should name the clashing code, got: %v", err)
	}
}

func TestUpdateCourseInfo_RejectsAMalformedCode(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := courseInfoSvc(f)
	for _, bad := range []string{"CP12", "cp353201x", "12345", ""} {
		if err := svc.UpdateCourseInfo(f.ctx, f.StaffID, f.CourseID,
			UpdateCourseInfoInput{Code: strp(bad)}); err == nil {
			t.Errorf("accepted %q as a course code", bad)
		}
	}
}

// A blank name would leave the course with nothing to be called on any screen
// or document.
func TestUpdateCourseInfo_RejectsABlankName(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := courseInfoSvc(f)
	if err := svc.UpdateCourseInfo(f.ctx, f.StaffID, f.CourseID,
		UpdateCourseInfoInput{NameTH: strp("   ")}); err == nil {
		t.Error("a whitespace-only course name was accepted")
	}
}

// THE ONE THAT PROTECTS THE DOCUMENT. The code and name are printed on the
// claim forms and the appointment order.
func TestUpdateCourseInfo_RefusedOnceTheCourseIsExported(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := courseInfoSvc(f)
	f.exec(`UPDATE teaching_courses SET exported_at = NOW() WHERE id = $1`, f.CourseID)

	if err := svc.UpdateCourseInfo(f.ctx, f.StaffID, f.CourseID,
		UpdateCourseInfoInput{NameTH: strp("เปลี่ยนหลังส่งออก")}); err == nil {
		t.Fatal("a course was renamed after its documents had been issued")
	}
	_, name, _, _, _, _ := courseFields(f)
	if name == "เปลี่ยนหลังส่งออก" {
		t.Error("the refusal still wrote")
	}
}

// Course identity is the registrar's and staff's, not a per-course preference.
func TestUpdateCourseInfo_LecturerAndTAAreRefused(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := courseInfoSvc(f)
	for who, actor := range map[string]interface{ String() string }{
		"lecturer": f.LecturerID, "TA": f.TAID,
	} {
		if err := svc.UpdateCourseInfo(f.ctx, f.LecturerID, f.CourseID,
			UpdateCourseInfoInput{NameTH: strp("x")}); err == nil {
			t.Errorf("%s (%s) changed the course identity", who, actor)
		}
	}
}

// Curriculum lives on the section, but staff set it once for the whole course —
// which is what the open dialog already promises can be corrected later.
func TestUpdateCourseInfo_CurriculumReachesEverySection(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := courseInfoSvc(f)

	if err := svc.UpdateCourseInfo(f.ctx, f.StaffID, f.CourseID,
		UpdateCourseInfoInput{Curriculum: strp("CS")}); err != nil {
		t.Fatalf("UpdateCourseInfo: %v", err)
	}
	var n, total int
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT COUNT(*) FILTER (WHERE curriculum = 'CS'), COUNT(*)
		FROM sections WHERE teaching_course_id = $1`, f.CourseID).Scan(&n, &total); err != nil {
		t.Fatal(err)
	}
	if total == 0 || n != total {
		t.Errorf("%d of %d sections carry the curriculum", n, total)
	}

	// "" puts them back to ยังไม่ระบุ rather than storing an empty string.
	if err := svc.UpdateCourseInfo(f.ctx, f.StaffID, f.CourseID,
		UpdateCourseInfoInput{Curriculum: strp("")}); err != nil {
		t.Fatal(err)
	}
	var nulls int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM sections WHERE teaching_course_id = $1 AND curriculum IS NULL`,
		f.CourseID).Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls != total {
		t.Errorf("%d of %d sections cleared", nulls, total)
	}
}

func TestUpdateCourseInfo_RejectsAnUnknownCurriculum(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := courseInfoSvc(f)
	if err := svc.UpdateCourseInfo(f.ctx, f.StaffID, f.CourseID,
		UpdateCourseInfoInput{Curriculum: strp("NOTREAL")}); err == nil {
		t.Error("an unknown curriculum token was accepted")
	}
}
