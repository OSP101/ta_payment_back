package service

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestCurriculumManagement(t *testing.T) {
	svc, ctx, pool, term := newCourseGroupFixture(t)
	actor := cgActor(t, pool)
	in := CreateCurriculumInput{Code: " new_test ", UpdateCurriculumInput: UpdateCurriculumInput{SheetName: "NEW_TEST", FullNameTH: "หลักสูตรทดสอบ", Level: "undergrad", SortOrder: 20}}
	cur, err := svc.CreateCurriculum(ctx, actor, in)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Code != "NEW_TEST" {
		t.Fatalf("code=%q", cur.Code)
	}
	if _, err := svc.CreateCurriculum(ctx, actor, in); err == nil {
		t.Fatal("duplicate accepted")
	}
	in.Code = "DIFFERENT"
	in.SheetName = "new_test"
	if _, err := svc.CreateCurriculum(ctx, actor, in); err == nil {
		t.Fatal("duplicate sheet accepted")
	}
	update := UpdateCurriculumInput{SheetName: "Renamed", FullNameTH: "หลักสูตรใหม่", Level: "undergrad", SortOrder: 2}
	if err := svc.UpdateCurriculum(ctx, actor, cur.Code, update); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.ListCurricula(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Code == cur.Code {
			found = true
			if row.SheetName != "Renamed" || row.FullNameTH != update.FullNameTH {
				t.Fatalf("updated=%+v", row)
			}
		}
	}
	if !found {
		t.Fatal("created curriculum not listed")
	}
	tc := cgInsertCourse(t, pool, term, "XX123456", "Test")
	sec := cgInsertSection(t, pool, tc, "1", "regular", "CS")
	if err := svc.UpdateSectionCurriculum(ctx, actor, sec, cur.Code); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteCurriculum(ctx, actor, cur.Code); err == nil {
		t.Fatal("used curriculum deleted")
	}
	update.Level = "graduate"
	if err := svc.UpdateCurriculum(ctx, actor, cur.Code, update); err == nil {
		t.Fatal("used curriculum level changed")
	}
	if err := svc.UpdateSectionCurriculum(ctx, actor, sec, "CS"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteCurriculum(ctx, actor, cur.Code); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteCurriculum(ctx, actor, cur.Code); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing delete=%v", err)
	}
	if err := svc.UpdateSectionCurriculum(ctx, actor, sec, cur.Code); err == nil {
		t.Fatal("deleted curriculum accepted")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE entity='curriculum' AND entity_id=$1`, cur.Code).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("audit count=%d, want create/update/delete", n)
	}
}

func TestCurriculumValidation(t *testing.T) {
	for _, sheet := range []string{"", "invalid/name", "invalid:name", "'quoted", "quoted'", "12345678901234567890123456789012"} {
		in := UpdateCurriculumInput{SheetName: sheet, FullNameTH: "Test", Level: "undergrad"}
		if validateCurriculumDetails(&in) == nil {
			t.Errorf("accepted sheet=%q", sheet)
		}
	}
	for _, code := range []string{"CS_GRAD", "DSAI_GRAD", "NEW_GRAD"} {
		if got := printCurriculumCode(code, "graduate"); got != code {
			t.Errorf("graduate %s=%s", code, got)
		}
	}
}

func TestCurriculumCustomCodeUsedByCourseInfo(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := courseInfoSvc(f)
	_, err := svc.CreateCurriculum(f.ctx, f.StaffID, CreateCurriculumInput{Code: "CUSTOM", UpdateCurriculumInput: UpdateCurriculumInput{SheetName: "Custom", FullNameTH: "Custom curriculum", Level: "undergrad"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateCourseInfo(f.ctx, f.StaffID, f.CourseID, UpdateCourseInfoInput{Curriculum: strp("CUSTOM")}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM sections WHERE teaching_course_id=$1 AND curriculum='CUSTOM'`, f.CourseID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("custom code did not reach sections")
	}
}

func TestCurriculumCustomCodesExport(t *testing.T) {
	for _, level := range []string{"undergrad", "graduate"} {
		t.Run(level, func(t *testing.T) {
			f := newCSFixture(t)
			_, err := f.svc.teaching.CreateCurriculum(f.ctx, f.lectID, CreateCurriculumInput{Code: "NEW_PROGRAM", UpdateCurriculumInput: UpdateCurriculumInput{SheetName: "NewProgram", FullNameTH: "New curriculum", Level: level}})
			if err != nil {
				t.Fatal(err)
			}
			id := f.insertCourse(csCourseOpts{Code: "CP388099", NameTH: "New course", Credits: 3, LectureHrs: 3, NumRegular: 10, Curriculum: "NEW_PROGRAM", Level: level})
			f.insertSubmittedRequest(id)
			body, _ := f.build()
			wb := openWorkbook(t, body)
			defer wb.Close()
			if sheets := wb.GetSheetList(); len(sheets) != 1 || sheets[0] != "NewProgram" {
				t.Fatalf("sheets=%v", sheets)
			}
			if name, _ := wb.GetCellValue("NewProgram", "A4"); name != "New curriculum" {
				t.Fatalf("heading=%q", name)
			}
		})
	}
}
