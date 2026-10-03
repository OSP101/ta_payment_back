package service

import (
	"testing"

	"ta-payment-back/internal/audit"
)

// A section timed only after the import (a WBA group the lecturer filled in)
// must be re-matched against TDBM straight away — before 03/10/2026 its TDBM
// makeups stayed loose until staff pressed sync.
func TestReplaceSectionSchedules_RematchesTDBM(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	tdbm := &TDBMService{pool: f.Pool, aud: audit.New(f.Pool)}
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool), tdbm: tdbm}
	var code, secNo string
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT tc.code, s.sec_no FROM sections s JOIN teaching_courses tc ON tc.id = s.teaching_course_id
		 WHERE s.id = $1`, f.SectionID).Scan(&code, &secNo); err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO tdbm_extra_teachings (extra_class_id, academic_year, semester, course_code, section_label)
	        VALUES (424242, $1, 1, $2, $3)`, f.AcademicYear, code, "กลุ่มที่ "+secNo)

	err := svc.ReplaceSectionSchedules(f.ctx, f.StaffID, f.CourseID, f.SectionID, []SectionSchedule{
		{Kind: "lecture", DayOfWeek: 2, StartTime: "09:00", EndTime: "12:00"},
		{Kind: "lab", DayOfWeek: 4, StartTime: "13:00", EndTime: "16:00"},
	})
	if err != nil {
		t.Fatalf("ReplaceSectionSchedules: %v", err)
	}
	var matched bool
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT section_id = $1 FROM tdbm_extra_teachings WHERE extra_class_id = 424242`, f.SectionID).Scan(&matched); err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Error("TDBM row not matched to the section after its timetable was saved")
	}
}
