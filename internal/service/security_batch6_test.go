package service

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Batch 6, finding 14: the registrar's Officer cell lists lecturers by first
// name, space-separated — but a cell holding one lecturer's FULL name must not
// hand a second, unrelated lecturer (whose first name is that surname) the
// course.

func lecturerNamed(f *fixture, first, last string) uuid.UUID {
	id := f.insertUser("lecturer", first)
	f.exec(`UPDATE users SET last_name = $2 WHERE id = $1`, id, last)
	return id
}

func TestMatchOfficers_FullNameDoesNotAttachTheSurnamesNamesake(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := newTeachingSvc(t, f)
	somchai := lecturerNamed(f, "Somchai", "Jaidee")
	jaidee := lecturerNamed(f, "Jaidee", "Other")

	matched, unmatched, err := svc.matchOfficers(f.ctx, officerTokens("Somchai Jaidee"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range matched {
		if id == jaidee {
			t.Fatal("the lecturer whose first name is the surname was attached automatically")
		}
	}
	if len(matched) != 1 || matched[0] != somchai {
		t.Fatalf("the named lecturer must still match, got %v", matched)
	}
	if len(unmatched) != 1 || unmatched[0] != "Jaidee" {
		t.Fatalf("the ambiguous token must go to staff, got %v", unmatched)
	}
}

func TestMatchOfficers_SurnameAloneIsSkippedNotFlagged(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := newTeachingSvc(t, f)
	somchai := lecturerNamed(f, "Somchai", "Jaidee")

	matched, unmatched, err := svc.matchOfficers(f.ctx, officerTokens("Somchai Jaidee"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 1 || matched[0] != somchai || len(unmatched) != 0 {
		t.Fatalf("a plain full name is one lecturer and nothing to resolve, got %v / %v", matched, unmatched)
	}
}

// The real file's format ("ณกร ศรัณย์" = two lecturers) keeps working.
func TestMatchOfficers_SpaceSeparatedFirstNamesStillMatchEach(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := newTeachingSvc(t, f)
	a := lecturerNamed(f, "ณกร", "หนึ่ง")
	b := lecturerNamed(f, "ศรัณย์", "สอง")

	matched, unmatched, err := svc.matchOfficers(f.ctx, officerTokens("ณกร ศรัณย์ และคณะ"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 2 || matched[0] != a || matched[1] != b || len(unmatched) != 0 {
		t.Fatalf("both first names must match, got %v / %v", matched, unmatched)
	}
}

// Batch 6, NV-1: a name that starts with "=" is text in the finance workbook,
// never a live formula; the code's own formulas still calculate.
func TestCourseSummary_NameStartingWithEqualsIsTextNotAFormula(t *testing.T) {
	f := newCSFixture(t)
	evil := `=HYPERLINK("http://example.invalid","x")`
	courseID := f.insertCourse(csCourseOpts{
		Code: "CP100009", NameTH: evil, Credits: 3, LectureHrs: 3, LabHrs: 3, SelfHrs: 6,
		NumRegular: 40, Curriculum: "CS",
	})
	f.addApprovedTA(courseID, evil, "undergrad", 3, 0)

	body, _ := f.build()
	wb := openWorkbook(t, body)
	defer wb.Close()

	for _, cell := range []string{"C5", "G5"} {
		if fm, _ := wb.GetCellFormula("CS", cell); fm != "" {
			t.Errorf("%s became a formula: %q", cell, fm)
		}
		if v, _ := wb.GetCellValue("CS", cell); !strings.HasPrefix(v, "=HYPERLINK") {
			t.Errorf("%s must show the name exactly as typed, got %q", cell, v)
		}
	}
	// The code-built formulas are unaffected: the total row still sums.
	if fm, _ := wb.GetCellFormula("CS", "L6"); !strings.HasPrefix(fm, "SUM") {
		t.Errorf("L6 must still be a SUM formula, got %q", fm)
	}
}

// A workbook formula built as a plain string is written as TEXT now (NV-1):
// only xlFormula calculates. Five formulas built with backtick literals were
// missed once and silently stopped working, so any Sprintf that starts with
// "=" — either quote style — must be xlf instead.
func TestNoFormulaBuiltAsPlainString(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	bad := regexp.MustCompile("Sprintf\\((\"|`)=")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if bad.MatchString(line) {
				t.Errorf("%s:%d builds a formula with fmt.Sprintf — use xlf so it is written as a formula, not text", f, i+1)
			}
		}
	}
}
