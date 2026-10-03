package service

import (
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

func sc362005Sections() []courseSection {
	return []courseSection{
		{id: uuid.New(), secNo: "01", track: "regular", students: 44},
		{id: uuid.New(), secNo: "02", track: "regular", students: 40},
		{id: uuid.New(), secNo: "03", track: "special", students: 11},
	}
}

// REG numbers land on the section the import folded that code's group into:
// the primary's own sec N, and 342233's groups by mergeCodeTx's rule.
func TestMatchRegSection_FollowsTheImportFold(t *testing.T) {
	secs := sc362005Sections()
	cases := []struct {
		r    SectionCount
		want int
	}{
		{SectionCount{Code: "SC362005", SecNo: "01", Count: 40}, 0},
		{SectionCount{Code: "SC362005", SecNo: "02", Count: 33}, 1},
		{SectionCount{Code: "SC362005", SecNo: "03", Special: true, Count: 4}, 2},
		{SectionCount{Code: "342233", SecNo: "02", Count: 6}, 1},               // same number + track
		{SectionCount{Code: "342233", SecNo: "01", Special: true, Count: 2}, 2}, // only special section
	}
	for _, c := range cases {
		got, ok := matchRegSection(secs, "SC362005", c.r)
		if !ok || got != secs[c.want].id {
			t.Errorf("%s sec %s → %v (ok=%v), want sec %s", c.r.Code, c.r.SecNo, got, ok, secs[c.want].secNo)
		}
	}
	// The primary's sec 04 does not exist: no guessing for the primary code.
	if _, ok := matchRegSection(secs, "SC362005", SectionCount{Code: "SC362005", SecNo: "04"}); ok {
		t.Error("a primary section that does not exist must not match")
	}
}

// An Excel paste only has track totals: 79 regular over 44 + 40 seats.
func TestSpreadByWeight_AddsUpExactly(t *testing.T) {
	secs := sc362005Sections()
	got := spreadByWeight(secs, "regular", 79)
	if got[secs[0].id]+got[secs[1].id] != 79 {
		t.Fatalf("regular split %v does not add up to 79", got)
	}
	if got[secs[0].id] != 41 || got[secs[1].id] != 38 {
		t.Errorf("split = %d / %d, want 41 / 38 (proportional to 44 / 40)", got[secs[0].id], got[secs[1].id])
	}
	if sp := spreadByWeight(secs, "special", 6); sp[secs[2].id] != 6 {
		t.Errorf("one special section must take the whole 6, got %v", sp)
	}
	empty := []courseSection{{id: uuid.New(), track: "regular"}, {id: uuid.New(), track: "regular"}}
	e := spreadByWeight(empty, "regular", 5)
	if e[empty[0].id]+e[empty[1].id] != 5 {
		t.Errorf("even split of 5 = %v", e)
	}
}

// The save path end to end: a REG row's per-section numbers land on the
// sections, and the sections then add up to the course (03/10/2026).
func TestBulkNumStudents_SectionsFollowTheCourse(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	teaching := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	var code string
	if err := f.Pool.QueryRow(f.ctx, `SELECT code FROM teaching_courses WHERE id = $1`, f.CourseID).Scan(&code); err != nil {
		t.Fatal(err)
	}
	second := uuid.New()
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track, num_students)
	        VALUES ($1, $2, '02', 'regular', 20)`, second, f.CourseID)

	reg := 30
	rows := []BulkCountRow{{Code: code, Regular: &reg, Sections: []SectionCount{
		{SecNo: "01", Count: 18}, {SecNo: "02", Count: 12},
	}}}
	if _, err := teaching.BulkSetNumStudents(f.ctx, f.StaffID, f.TermID, rows, true, false, false); err != nil {
		t.Fatal(err)
	}
	var first, other int
	_ = f.Pool.QueryRow(f.ctx, `SELECT num_students FROM sections WHERE id = $1`, f.SectionID).Scan(&first)
	_ = f.Pool.QueryRow(f.ctx, `SELECT num_students FROM sections WHERE id = $1`, second).Scan(&other)
	if first != 18 || other != 12 {
		t.Errorf("sections = %d / %d, want the registrar's 18 / 12", first, other)
	}

	// An Excel row has totals only: spread over the sections by their share.
	reg = 15
	rows = []BulkCountRow{{Code: code, Regular: &reg}}
	if _, err := teaching.BulkSetNumStudents(f.ctx, f.StaffID, f.TermID, rows, true, false, false); err != nil {
		t.Fatal(err)
	}
	_ = f.Pool.QueryRow(f.ctx, `SELECT num_students FROM sections WHERE id = $1`, f.SectionID).Scan(&first)
	_ = f.Pool.QueryRow(f.ctx, `SELECT num_students FROM sections WHERE id = $1`, second).Scan(&other)
	if first+other != 15 || first != 9 {
		t.Errorf("sections = %d / %d, want 9 / 6 adding up to 15", first, other)
	}
}
