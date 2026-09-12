package service

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// One class, two registrar codes. Staff add the student counts together and
// budget the pair as one course; the system must land on the same numbers
// and must never let the second code be opened again on its own.

func mergeFixtureCourse(code string, students int, officer string) *parsedCourse {
	return &parsedCourse{
		code: code, name: "INTERNETWORKING", nameEN: "INTERNETWORKING", level: "undergrad",
		credits: 3, lectureHrs: 2, labHrs: 2, officerRaw: officer,
		sectionsInOrder: []string{"01", "02"},
		sections: map[string]*parsedSection{
			"01": {secNo: "01", track: "regular", numStudents: students, curriculum: "CS",
				schedules: []parsedSchedule{{kind: "lecture", dow: 4, startTime: "15:00", endTime: "17:00", room: "SC9524"}}},
			"02": {secNo: "02", track: "special", numStudents: 10,
				schedules: []parsedSchedule{{kind: "lecture", dow: 4, startTime: "08:30", endTime: "10:30", room: "SC9524"}}},
		},
	}
}

func TestImportMerge_FoldsSecondCodeIntoOneCourseAndAddsStudents(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}

	primary := mergeFixtureCourse("CP353301", 35, "")
	other := mergeFixtureCourse("SC313302", 20, "")

	id, err := svc.commitOneCourse(f.ctx, f.StaffID, f.TermID, primary)
	if err != nil {
		t.Fatalf("commitOneCourse: %v", err)
	}
	if err := svc.mergeParsedCourse(f.ctx, f.StaffID, f.TermID, id, other); err != nil {
		t.Fatalf("mergeParsedCourse: %v", err)
	}

	tc, err := svc.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(tc.AltCodes) != 1 || tc.AltCodes[0] != "SC313302" {
		t.Fatalf("alt_codes = %v, want [SC313302]", tc.AltCodes)
	}
	// 35 + 20 regular, 10 + 10 special — the counts staff would have added.
	if tc.NumStudentsRegular != 55 || tc.NumStudentsSpecial != 20 || tc.NumStudents != 75 {
		t.Fatalf("students = %d/%d/%d, want 55/20/75", tc.NumStudentsRegular, tc.NumStudentsSpecial, tc.NumStudents)
	}
	secNos := map[string]*string{}
	for _, s := range tc.Sections {
		secNos[s.SecNo] = s.CourseCode
	}
	if len(secNos) != 4 {
		t.Fatalf("sections = %v, want 4", secNos)
	}
	if secNos["01"] != nil || secNos["SC313302-01"] == nil || *secNos["SC313302-01"] != "SC313302" {
		t.Fatalf("sections = %v: primary sections keep bare sec_no, merged ones carry the code", secNos)
	}

	// The merged code now counts as "existing" in a re-import…
	if _, err := svc.commitOneCourse(f.ctx, f.StaffID, f.TermID, other); !errors.Is(err, errImportSkipped) {
		t.Fatalf("re-import of merged code: err = %v, want errImportSkipped", err)
	}
	// …and cannot be opened by hand either.
	_, err = svc.Create(f.ctx, f.StaffID, CreateTeachingCourseInput{
		TermID: f.TermID, Code: "SC313302", NameTH: "x", LecturerIDs: []uuid.UUID{f.LecturerID},
	})
	var ue *UserError
	if !errors.As(err, &ue) || ue.Status != 409 {
		t.Fatalf("Create with a merged code: err = %v, want 409", err)
	}
}

func TestImportPreview_GroupsSameNameDifferentCode(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}

	// A same-name course already open in the term is offered as a merge
	// target too — the file's course would fold into it.
	existing := mergeFixtureCourse("CP423211", 47, "")
	existing.name, existing.nameEN = "Internetworking", "Internetworking" // case differs on purpose
	existingID, err := svc.commitOneCourse(f.ctx, f.StaffID, f.TermID, existing)
	if err != nil {
		t.Fatal(err)
	}

	a := mergeFixtureCourse("CP353301", 35, "บุญทรัพย์")
	b := mergeFixtureCourse("SC313302", 20, "บุญทรัพย์")
	// Same name, but a different lecturer and slot: must not be suggested.
	c := mergeFixtureCourse("CP999001", 5, "สมชาย")
	c.sections["01"].schedules[0].dow = 1
	c.sections["02"].schedules[0].dow = 1
	lone := mergeFixtureCourse("CP888001", 5, "")
	lone.name, lone.nameEN = "SOMETHING ELSE", "SOMETHING ELSE"

	groups, err := svc.detectImportMergeGroups(f.ctx, f.TermID, []*parsedCourse{a, b, c, lone},
		map[string]string{"CP353301": "new", "SC313302": "new", "CP999001": "new", "CP888001": "new"})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want exactly the INTERNETWORKING group", groups)
	}
	g := groups[0]
	got := map[string]ImportMergeMember{}
	for _, m := range g.Members {
		got[m.Code] = m
	}
	if len(got) != 4 {
		t.Fatalf("members = %v, want CP353301, SC313302, CP999001, CP423211", got)
	}
	if !got["CP353301"].Suggested || !got["SC313302"].Suggested {
		t.Errorf("same lecturer + same slot should be suggested: %+v", got)
	}
	if got["CP999001"].Suggested {
		t.Errorf("different lecturer and slot must not be suggested: %+v", got["CP999001"])
	}
	ex := got["CP423211"]
	if ex.Status != "existing" || ex.ExistingID == nil || *ex.ExistingID != existingID {
		t.Errorf("existing member = %+v, want status existing with its id", ex)
	}
}

func TestMergeCourseCode_ManualPathRefusesDuplicateAndRecounts(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}

	id, err := svc.Create(f.ctx, f.StaffID, CreateTeachingCourseInput{
		TermID: f.TermID, Code: "CP373042", NameTH: "INTRODUCTION TO LAND USE PLANNING",
		LectureHrs: 3, Credits: 3, LecturerIDs: []uuid.UUID{f.LecturerID},
		Sections: []CourseSectionInput{{SecNo: "1", Track: "regular", NumStudents: 65}},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = svc.MergeCourseCode(f.ctx, f.StaffID, id, MergeCodeInput{
		Code: "sc333034",
		Sections: []CourseSectionInput{
			{SecNo: "1", Track: "regular", NumStudents: 5},
			{SecNo: "2", Track: "special", NumStudents: 5},
		},
	})
	if err != nil {
		t.Fatalf("MergeCourseCode: %v", err)
	}
	tc, err := svc.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if tc.NumStudentsRegular != 70 || tc.NumStudentsSpecial != 5 {
		t.Fatalf("students = %d/%d, want 70/5", tc.NumStudentsRegular, tc.NumStudentsSpecial)
	}
	if len(tc.AltCodes) != 1 || tc.AltCodes[0] != "SC333034" {
		t.Fatalf("alt_codes = %v (code is upper-cased on the way in)", tc.AltCodes)
	}
	// Merging the same code twice, or the course's own code, is refused.
	if err := svc.MergeCourseCode(f.ctx, f.StaffID, id, MergeCodeInput{Code: "SC333034"}); err == nil {
		t.Fatal("second merge of the same code should fail")
	}
	if err := svc.MergeCourseCode(f.ctx, f.StaffID, id, MergeCodeInput{Code: "CP373042"}); err == nil {
		t.Fatal("merging the course's own code should fail")
	}
	// Lecturers cannot merge.
	if err := svc.MergeCourseCode(f.ctx, f.LecturerID, id, MergeCodeInput{Code: "SC999999"}); err == nil {
		t.Fatal("lecturer merge should be forbidden")
	}
}

func TestMerge_NewestCurriculumCodeBecomesPrimary(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}

	// Opened first under the OLD code (bare six digits), then the SC code and
	// finally the CP code arrive. The documents must print the CP code, and
	// "sec 1" must be the CP section.
	old := mergeFixtureCourse("342233", 5, "")
	id, err := svc.commitOneCourse(f.ctx, f.StaffID, f.TermID, old)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.mergeParsedCourse(f.ctx, f.StaffID, f.TermID, id, mergeFixtureCourse("SC362005", 42, "")); err != nil {
		t.Fatal(err)
	}
	if err := svc.mergeParsedCourse(f.ctx, f.StaffID, f.TermID, id, mergeFixtureCourse("CP362005", 30, "")); err != nil {
		t.Fatal(err)
	}
	tc, err := svc.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if tc.Code != "CP362005" {
		t.Fatalf("code = %s, want CP362005 (CP > SC > six digits)", tc.Code)
	}
	if len(tc.AltCodes) != 2 || tc.AltCodes[0] != "SC362005" || tc.AltCodes[1] != "342233" {
		t.Fatalf("alt_codes = %v, want [SC362005 342233]", tc.AltCodes)
	}
	got := map[string]string{}
	for _, sec := range tc.Sections {
		got[sec.SecNo] = derefStr(sec.CourseCode)
	}
	want := map[string]string{
		"01": "", "02": "",
		"SC362005-01": "SC362005", "SC362005-02": "SC362005",
		"342233-01": "342233", "342233-02": "342233",
	}
	if len(got) != len(want) {
		t.Fatalf("sections = %v, want %v", got, want)
	}
	for k, v := range want {
		if cc, ok := got[k]; !ok || cc != v {
			t.Fatalf("sections = %v, want %v", got, want)
		}
	}
	if tc.NumStudentsRegular != 77 {
		t.Fatalf("regular students = %d, want 5+42+30", tc.NumStudentsRegular)
	}
}

func TestPrintSecNo_DocumentsDropTheAltCodePrefix(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	id, err := svc.commitOneCourse(f.ctx, f.StaffID, f.TermID, mergeFixtureCourse("CP353301", 35, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.mergeParsedCourse(f.ctx, f.StaffID, f.TermID, id, mergeFixtureCourse("SC313302", 20, "")); err != nil {
		t.Fatal(err)
	}
	rows, err := f.Pool.Query(f.ctx,
		`SELECT sec.sec_no, `+PrintSecNoSQL("sec")+` FROM sections sec WHERE sec.teaching_course_id = $1`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var raw, printed string
		if err := rows.Scan(&raw, &printed); err != nil {
			t.Fatal(err)
		}
		got[raw] = printed
	}
	want := map[string]string{"01": "01", "02": "02", "SC313302-01": "01", "SC313302-02": "02"}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("printed sec_no = %v, want %v", got, want)
		}
	}
}
