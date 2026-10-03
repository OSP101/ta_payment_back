package service

import (
	"os"
	"testing"

	"ta-payment-back/internal/audit"
)

// 03/10/2026: the test registrar file lists 342233 (two WBA placeholder
// groups, 5 seats each) before SC362005 (three timetabled groups). Merging
// them on the file's first code folded SC362005's real sections into the
// placeholders and dropped every timetable. The base must be the
// newest-curriculum code whichever primary the preview sent.
func TestCommitImport_MergeKeepsTheNewCodesSections(t *testing.T) {
	body, err := os.ReadFile("../../../docs/รายวิชาที่เปิดสอน-1-2569-test.xlsx")
	if err != nil {
		t.Skip("test registrar file not present:", err)
	}
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	admin := f.insertUser("admin", "admin")
	preview, err := svc.PreviewImport(f.ctx, admin, f.TermID, "t.xlsx", body)
	if err != nil {
		t.Fatal(err)
	}
	var proceed []string
	for _, c := range preview.Courses {
		if c.Status == "unmatched_officer" {
			proceed = append(proceed, c.Code)
		}
	}
	// The old default: the file's first code as primary.
	merges := []ImportMerge{{Primary: "342233", Codes: []string{"SC362005"}}}
	if _, err := svc.CommitImport(f.ctx, admin, f.TermID, "t.xlsx", body, nil, merges, proceed...); err != nil {
		t.Fatal(err)
	}

	rows, err := f.Pool.Query(f.ctx, `
		SELECT ltrim(s.sec_no, '0'), s.track::text, s.num_students,
		       (SELECT COUNT(*) FROM section_schedules ss WHERE ss.section_id = s.id)
		  FROM sections s JOIN teaching_courses tc ON tc.id = s.teaching_course_id
		 WHERE tc.term_id = $1 AND tc.code = 'SC362005'
		 ORDER BY s.sec_no`, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type sec struct {
		no, track       string
		students, sched int
	}
	var got []sec
	for rows.Next() {
		var s sec
		if err := rows.Scan(&s.no, &s.track, &s.students, &s.sched); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	// SC362005's three groups, each with its lecture + lab; 342233's regular
	// sec 02 joins sec 02, its one special group joins the one special sec 03.
	want := []sec{{"1", "regular", 44, 2}, {"2", "regular", 40, 2}, {"3", "special", 11, 2}}
	if len(got) != len(want) {
		t.Fatalf("sections = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("section %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
