package service

import (
	"github.com/google/uuid"
	"strings"
	"ta-payment-back/internal/audit"
	"testing"
)

func existingMergeFixture(t *testing.T) (*fixture, *TeachingService, uuid.UUID, uuid.UUID) {
	t.Helper()
	f := newFixture(t, fixtureOpts{NoRequest: true})
	s := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	makeCourse := func(code string, secs []CourseSectionInput) uuid.UUID {
		id, e := s.Create(f.ctx, f.StaffID, CreateTeachingCourseInput{TermID: f.TermID, Code: code, NameTH: "SAME CLASS", Credits: 3, LectureHrs: 3, LecturerIDs: []uuid.UUID{f.LecturerID}, Sections: secs})
		if e != nil {
			t.Fatal(e)
		}
		return id
	}
	target := makeCourse("CP999001", []CourseSectionInput{{SecNo: "1", Track: "regular", NumStudents: 40}})
	source := makeCourse("SC999001", []CourseSectionInput{{SecNo: "1", Track: "regular", NumStudents: 5}, {SecNo: "2", Track: "special", NumStudents: 7}})
	return f, s, target, source
}
func existingMergePlan(t *testing.T, f *fixture, s *TeachingService, target, source uuid.UUID) (ExistingMergeInput, ExistingMergePreview) {
	t.Helper()
	in := ExistingMergeInput{SourceID: source}
	p, e := s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, false)
	if e != nil {
		t.Fatal(e)
	}
	for _, sec := range p.Source.Sections {
		m := ExistingMergeMapping{Source: sec.ID}
		if sec.Track == "regular" {
			id := p.Target.Sections[0].ID
			m.Target = &id
		}
		in.Mappings = append(in.Mappings, m)
	}
	p, e = s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, false)
	if e != nil {
		t.Fatal(e)
	}
	return in, p
}
func TestExistingCourseMergePreservesSeparateSectionAndSumsExplicitPartner(t *testing.T) {
	f, s, target, source := existingMergeFixture(t)
	in, p := existingMergePlan(t, f, s, target, source)
	if len(p.Blockers) > 0 {
		t.Fatalf("unexpected blockers: %v", p.Blockers)
	}
	var kept uuid.UUID
	for _, sec := range p.Source.Sections {
		if sec.Track == "special" {
			kept = sec.ID
		}
	}
	in.Token = p.Token
	in.Confirmed = true
	if _, e := s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, true); e != nil {
		t.Fatal(e)
	}
	var count, regular, special int
	var code string
	if e := f.Pool.QueryRow(f.ctx, `SELECT num_students_regular,num_students_special FROM teaching_courses WHERE id=$1`, target).Scan(&regular, &special); e != nil {
		t.Fatal(e)
	}
	if regular != 45 || special != 7 {
		t.Fatalf("counts %d/%d", regular, special)
	}
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM teaching_courses WHERE id=$1`, source).Scan(&count)
	if count != 0 {
		t.Fatal("source remains separate")
	}
	if e := f.Pool.QueryRow(f.ctx, `SELECT sec_no FROM sections WHERE id=$1 AND teaching_course_id=$2`, kept, target).Scan(&code); e != nil || code != "SC999001-2" {
		t.Fatalf("separate section identity lost: %s %v", code, e)
	}
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM teaching_lecturers WHERE teaching_course_id=$1`, target).Scan(&count)
	if count != 1 {
		t.Fatal("lecturer duplicated")
	}
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM teaching_courses WHERE id=$1 AND 'SC999001'=ANY(alt_codes) AND jsonb_array_length(reg_source)>0`, target).Scan(&count)
	if count != 1 {
		t.Fatal("source code or evidence lost")
	}
}
func TestExistingCourseMergeRefusesStaleAndOperationalReferences(t *testing.T) {
	f, s, target, source := existingMergeFixture(t)
	in, p := existingMergePlan(t, f, s, target, source)
	in.Token = p.Token
	in.Confirmed = true
	f.exec(`UPDATE sections SET num_students=num_students+1 WHERE teaching_course_id=$1`, source)
	if _, e := s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, true); e == nil {
		t.Fatal("stale preview accepted")
	}
	if _, e := s.ExistingCourseMerge(f.ctx, f.LecturerID, target, in, false); e == nil {
		t.Fatal("lecturer permitted to merge")
	}
	f.exec(`CREATE TABLE merge_usage_probe(id uuid PRIMARY KEY,teaching_course_id uuid REFERENCES teaching_courses(id) ON DELETE CASCADE)`)
	f.exec(`INSERT INTO merge_usage_probe VALUES($1,$2)`, uuid.New(), source)
	p, e := s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, false)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(strings.Join(p.Blockers, " "), "ข้อมูลการใช้งาน") {
		t.Fatal("cascading operational reference not detected")
	}
	in.Token = p.Token
	if _, e = s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, true); e == nil {
		t.Fatal("reference guard bypassed")
	}
	var count int
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM teaching_courses WHERE id=ANY($1)`, []uuid.UUID{target, source}).Scan(&count)
	if count != 2 {
		t.Fatal("failed merge wrote partial changes")
	}
}
func TestExistingCourseMergeRequiresMappingAndHonorsExportLock(t *testing.T) {
	f, s, target, source := existingMergeFixture(t)
	p, e := s.ExistingCourseMerge(f.ctx, f.StaffID, target, ExistingMergeInput{SourceID: source}, false)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Blockers) == 0 {
		t.Fatal("missing mappings accepted")
	}
	in, p := existingMergePlan(t, f, s, target, source)
	in.Mappings[0].Target = &uuid.Nil
	if _, e = s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, false); e == nil {
		t.Fatal("foreign section target accepted")
	}
	in, p = existingMergePlan(t, f, s, target, source)
	in.Token = p.Token
	in.Confirmed = true
	f.exec(`UPDATE teaching_courses SET exported_at=now() WHERE id=$1`, target)
	p, e = s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, false)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Blockers) == 0 {
		t.Fatal("export lock ignored")
	}
	if _, e = s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, true); e == nil {
		t.Fatal("stale locked merge accepted")
	}
}

func TestExistingCourseMergeRollsBackAllChangesOnDeleteFailure(t *testing.T) {
	f, s, target, source := existingMergeFixture(t)
	in, p := existingMergePlan(t, f, s, target, source)
	in.Token = p.Token
	in.Confirmed = true
	f.exec(`CREATE FUNCTION deny_existing_merge_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced rollback'; END; $$`)
	f.exec(`CREATE TRIGGER deny_existing_merge_delete BEFORE DELETE ON teaching_courses FOR EACH ROW EXECUTE FUNCTION deny_existing_merge_delete()`)
	if _, e := s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, true); e == nil {
		t.Fatal("forced failure accepted")
	}
	var n, total int
	f.Pool.QueryRow(f.ctx, `SELECT count(*),sum(num_students) FROM sections WHERE teaching_course_id=ANY($1)`, []uuid.UUID{target, source}).Scan(&n, &total)
	if n != 3 || total != 52 {
		t.Fatalf("partial changes after rollback: sections=%d students=%d", n, total)
	}
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM teaching_courses WHERE id=ANY($1)`, []uuid.UUID{target, source}).Scan(&n)
	if n != 2 {
		t.Fatal("source course was lost")
	}
}

func TestExistingCourseMergeGuardsCompatibilityAndSectionLimits(t *testing.T) {
	f, s, target, source := existingMergeFixture(t)
	in, _ := existingMergePlan(t, f, s, target, source)
	f.exec(`UPDATE teaching_courses SET lecture_hrs=4 WHERE id=$1`, source)
	p, e := s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, false)
	if e != nil || !strings.Contains(strings.Join(p.Blockers, " "), "ชั่วโมง") {
		t.Fatalf("incompatible hours allowed: %+v %v", p.Blockers, e)
	}
	f.exec(`UPDATE teaching_courses SET lecture_hrs=3 WHERE id=$1`, source)
	bad := in
	bad.Mappings = append([]ExistingMergeMapping{}, in.Mappings...)
	for i := range bad.Mappings {
		for _, sec := range p.Source.Sections {
			if sec.ID == bad.Mappings[i].Source && sec.Track == "special" {
				dst := p.Target.Sections[0].ID
				bad.Mappings[i].Target = &dst
			}
		}
	}
	p, e = s.ExistingCourseMerge(f.ctx, f.StaffID, target, bad, false)
	if e != nil || !strings.Contains(strings.Join(p.Blockers, " "), "ประเภทภาคเดียวกัน") {
		t.Fatalf("cross-track fold allowed: %+v %v", p.Blockers, e)
	}
	f.exec(`UPDATE sections SET num_students=2990 WHERE teaching_course_id=$1 AND track='regular'`, source)
	p, e = s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, false)
	if e != nil || len(p.Blockers) == 0 {
		t.Fatal("Section headcount overflow accepted")
	}
	f.exec(`UPDATE academic_terms SET is_active=false WHERE id=$1`, f.TermID)
	f.AcademicYear = 2570
	other := f.insertTerm(fixtureOpts{TermStart: "2027-06-01", TermEnd: "2027-10-31", TermMonths: 4})
	f.exec(`UPDATE teaching_courses SET term_id=$2 WHERE id=$1`, source, other)
	p, e = s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, false)
	if e != nil || !strings.Contains(strings.Join(p.Blockers, " "), "ภาคการศึกษาเดียวกัน") {
		t.Fatalf("cross-term merge allowed: %+v %v", p.Blockers, e)
	}
}
func TestExistingCourseMergePreservesEnteredZeroAndRechecksTeacher(t *testing.T) {
	f, s, target, source := existingMergeFixture(t)
	f.exec(`UPDATE sections SET num_students=0 WHERE teaching_course_id=$1 AND track='special'`, source)
	f.exec(`UPDATE teaching_courses SET num_students_special_entered=true WHERE id=$1`, source)
	in, p := existingMergePlan(t, f, s, target, source)
	in.Token = p.Token
	in.Confirmed = true
	f.exec(`UPDATE users SET is_active=false WHERE id=$1`, f.LecturerID)
	if _, e := s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, true); e == nil {
		t.Fatal("inactive lecturer accepted")
	}
	f.exec(`UPDATE users SET is_active=true WHERE id=$1`, f.LecturerID)
	in, p = existingMergePlan(t, f, s, target, source)
	in.Token = p.Token
	in.Confirmed = true
	if _, e := s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, true); e != nil {
		t.Fatal(e)
	}
	var entered bool
	var students int
	if e := f.Pool.QueryRow(f.ctx, `SELECT num_students_special_entered,num_students_special FROM teaching_courses WHERE id=$1`, target).Scan(&entered, &students); e != nil || !entered || students != 0 {
		t.Fatalf("entered zero lost: entered=%v students=%d error=%v", entered, students, e)
	}
}
