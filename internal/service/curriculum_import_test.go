package service

import (
	"testing"

	"ta-payment-back/internal/audit"
)

// The dashboard's per-curriculum rollup is only as good as the value the
// import derives from ReservedFor. Two things are worth a test here: the
// mapping itself (real-world strings from the 1/2569 registrar file), and the
// re-import contract — a staff override must survive the registrar file being
// uploaded again, while a NULL gets backfilled.

func TestCurriculumFromReserved(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"ตรี : SC-IT ปี 3 ขึ้นไป", "IT"},
		{"ตรี : SC-CS ชั้นปี 4", "CS"},
		{"ตรี โครงการพิเศษ : CP-Cy ชั้นปี 1", "CY"},
		{"ตรี : CP-AI ปี 2 ขึ้นไป (42)", "AI"},
		// GIS is one programme in two code eras — both collapse to GIS.
		{"ตรี : SC-GIS ปี 2 ขึ้นไป", "GIS"},
		{"ตรี โครงการพิเศษ : CP-GIS ปี 3 ขึ้นไป", "GIS"},
		// Multi-programme sections: the FIRST token is the main reserved group.
		{"ตรี : SC-IT ปี 3 ขึ้นไป (80),CP-Cy ปี 3 ขึ้นไป (20)", "IT"},
		{"ตรี : SC-CS ปี 2 ขึ้นไป ,CP-AI ปี 2 ขึ้นไป", "CS"},
		// Non-SC/CP prefix = another faculty's students → OTHER, per the
		// 06/08/2026 decision. Never guessed into a programme.
		{"ตรี : BS-Digi ปี 3 ขึ้นไป (35)", "OTHER"},
		{"ตรี : BS-De ชั้นปี 2", "OTHER"},
		// The registrar sometimes appends a Facebook URL; the hyphenated token
		// scanner must not trip on it (no XX-YYY shape in the URL).
		{"ตรี : SC-IT ปี 2 ขึ้นไป , https://www.facebook.com/share/g/16VtXRikps", "IT"},
		// No token at all → unknown, NOT other: "we don't know" and "another
		// faculty" are different answers.
		{"", ""},
		{"ตรี ทุกหลักสูตร", ""},
		// A college code we don't recognise yet (new programme) → unknown.
		{"ตรี : SC-DS ชั้นปี 1", ""},
	}
	for _, c := range cases {
		if got := curriculumFromReserved(c.in); got != c.want {
			t.Errorf("curriculumFromReserved(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestImportWritesSectionCurriculum(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}

	c := &parsedCourse{
		code: "ZZ101", name: "วิชาทดสอบหลักสูตร", level: "undergrad",
		credits: 3, lectureHrs: 3,
		sectionsInOrder: []string{"1", "2", "3"},
		sections: map[string]*parsedSection{
			"1": {secNo: "1", track: "regular", numStudents: 30, curriculum: "IT"},
			"2": {secNo: "2", track: "regular", numStudents: 10, curriculum: "OTHER"},
			"3": {secNo: "3", track: "regular", numStudents: 5}, // no token in file
		},
	}
	id, err := svc.commitOneCourse(f.ctx, f.StaffID, f.TermID, c)
	if err != nil {
		t.Fatalf("commitOneCourse: %v", err)
	}

	got := map[string]*string{}
	rows, err := f.Pool.Query(f.ctx,
		`SELECT sec_no, curriculum FROM sections WHERE teaching_course_id = $1`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var sec string
		var cur *string
		if err := rows.Scan(&sec, &cur); err != nil {
			t.Fatal(err)
		}
		got[sec] = cur
	}
	want := func(sec, cur string) {
		t.Helper()
		v, ok := got[sec]
		if !ok {
			t.Fatalf("section %s missing", sec)
		}
		if cur == "" {
			if v != nil {
				t.Fatalf("section %s: curriculum = %q, want NULL", sec, *v)
			}
			return
		}
		if v == nil || *v != cur {
			t.Fatalf("section %s: curriculum = %v, want %q", sec, v, cur)
		}
	}
	want("1", "IT")
	want("2", "OTHER")
	want("3", "") // unknown stays NULL, not guessed
}

func TestReimportBackfillsOnlyNulls(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}

	// First import: the file carried no token for sec 1, staff later corrected
	// sec 2 by hand. Then the registrar file (now carrying tokens) is uploaded
	// again.
	first := &parsedCourse{
		code: "ZZ102", name: "วิชาทดสอบ re-import", level: "undergrad",
		credits: 3, lectureHrs: 3,
		sectionsInOrder: []string{"1", "2"},
		sections: map[string]*parsedSection{
			"1": {secNo: "1", track: "regular", numStudents: 30},                   // NULL
			"2": {secNo: "2", track: "regular", numStudents: 10, curriculum: "IT"}, // file says IT
		},
	}
	id, err := svc.commitOneCourse(f.ctx, f.StaffID, f.TermID, first)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	// Staff override on sec 2: the registrar was wrong, this group is CS.
	f.exec(`UPDATE sections SET curriculum = 'CS' WHERE teaching_course_id = $1 AND sec_no = '2'`, id)

	second := &parsedCourse{
		code: "ZZ102", name: "วิชาทดสอบ re-import", level: "undergrad",
		credits: 3, lectureHrs: 3,
		sectionsInOrder: []string{"1", "2"},
		sections: map[string]*parsedSection{
			"1": {secNo: "1", track: "regular", numStudents: 30, curriculum: "GIS"},
			"2": {secNo: "2", track: "regular", numStudents: 10, curriculum: "IT"},
		},
	}
	if _, err := svc.commitOneCourse(f.ctx, f.StaffID, f.TermID, second); err != errImportSkipped {
		t.Fatalf("second import of an existing course should skip, got %v", err)
	}

	var sec1, sec2 string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COALESCE(curriculum,'') FROM sections WHERE teaching_course_id=$1 AND sec_no='1'`, id).Scan(&sec1); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COALESCE(curriculum,'') FROM sections WHERE teaching_course_id=$1 AND sec_no='2'`, id).Scan(&sec2); err != nil {
		t.Fatal(err)
	}
	if sec1 != "GIS" {
		t.Fatalf("sec 1 was NULL and the re-import carried GIS — want backfill, got %q", sec1)
	}
	if sec2 != "CS" {
		t.Fatalf("sec 2 was a staff override (CS) — re-import must not clobber it, got %q", sec2)
	}
}
