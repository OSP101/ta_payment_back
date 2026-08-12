package service

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// docXMLOf pulls word/document.xml out of a generated .docx so a test can read
// what the form actually says.
func docXMLOf(t *testing.T, doc []byte) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(doc), int64(len(doc)))
	if err != nil {
		t.Fatalf("not a docx: %v", err)
	}
	for _, f := range zr.File {
		if f.Name != "word/document.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		var b bytes.Buffer
		if _, err := b.ReadFrom(rc); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	t.Fatal("word/document.xml missing")
	return ""
}

// The form's job is to say WHAT a graduate TA's claimed hours were: helping
// teach, preparing, marking — month by month. These tests pin that the hours
// land on the right line and in the right month.
func TestBuildGradWorkloadForms_SplitsHoursByMonthAndActivity(t *testing.T) {
	f := newTCFixture(t)
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP363761", Curriculum: "CY", LectureHrs: 3})
	ta := f.newTA("บัณฑิตปกติ", "phd")
	assign := f.assignTA(ta, courseID, regSec, "phd", nil)
	// June: 3h helping teach (lecture) + 2h marking.
	f.gradLogHours(assign, "2026-06-10", "lecture", 3)
	f.gradLogHours(assign, "2026-06-11", "review", 2)
	// July: 4h prep. activity='other' is the only thing เตรียมการสอน can be —
	// for graduate TAs prep_hrs and other_hrs share activity='other'.
	f.gradLogHours(assign, "2026-07-10", "other", 4)

	forms, err := f.svc.BuildGradWorkloadForms(f.ctx, courseID, nil)
	if err != nil {
		t.Fatalf("BuildGradWorkloadForms: %v", err)
	}
	if len(forms) != 1 {
		t.Fatalf("got %d forms, want 1 (one per grad-regular TA)", len(forms))
	}
	doc := docXMLOf(t, forms[0].Doc)
	for _, want := range []string{
		"แบบแสดงรายละเอียดภาระงานของผู้ช่วยสอน",
		"CP363761",
		"เดือน มิถุนายน 2569",
		"เดือน กรกฎาคม 2569",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("form is missing %q", want)
		}
	}
	// June totals 5 (3 + 2), July 4. A form that only carried a term total
	// would be indistinguishable from the evidence sheet it is meant to back.
	for _, want := range []string{">5<", ">4<", ">3<", ">2<"} {
		if !strings.Contains(doc, want) {
			t.Errorf("expected an hour cell containing %q in the month blocks", want)
		}
	}
}

// Grad-special is เหมาจ่าย — no hours are logged and none can be broken down,
// so no form is produced. Printing an empty one would ask a lecturer to certify
// a blank sheet.
func TestBuildGradWorkloadForms_SkipsGradSpecial(t *testing.T) {
	f := newTCFixture(t)
	courseID, _, specSec := f.insertCourse(tcCourseOpts{Code: "CP363762", Curriculum: "CY", LectureHrs: 3})
	ta := f.newTA("บัณฑิตพิเศษ", "phd")
	f.assignTA(ta, courseID, specSec, "phd", nil)

	forms, err := f.svc.BuildGradWorkloadForms(f.ctx, courseID, nil)
	if err != nil {
		t.Fatalf("BuildGradWorkloadForms: %v", err)
	}
	if len(forms) != 0 {
		t.Fatalf("got %d forms for a grad-special-only course, want 0 — "+
			"เหมาจ่าย has no hours to certify", len(forms))
	}
}

// An undergrad course produces none: this whole pair of documents is the
// graduate filing, and an undergrad pack must be exactly what it was.
func TestBuildGradWorkloadForms_NoneForUndergrad(t *testing.T) {
	f := newTCFixture(t)
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP111111", Curriculum: "CY", LectureHrs: 3})
	ta := f.newTA("ปริญญาตรี", "undergrad")
	f.assignTA(ta, courseID, regSec, "undergrad", []int{10, 11})

	forms, err := f.svc.BuildGradWorkloadForms(f.ctx, courseID, nil)
	if err != nil {
		t.Fatalf("BuildGradWorkloadForms: %v", err)
	}
	if len(forms) != 0 {
		t.Fatalf("got %d forms for an undergrad course, want 0", len(forms))
	}
}

// activity='other' covers both เตรียมการสอน and อื่นๆ for graduate TAs and the
// two cannot be told apart after the fact — the form prints them on the
// เตรียมการสอน line. Pinned so the choice is deliberate rather than incidental.
func TestWorkloadActivityRow_MapsActivitiesOntoTheFormsThreeLines(t *testing.T) {
	for activity, want := range map[string]string{
		"lecture": "help_teach",
		"lab":     "help_teach",
		"review":  "review",
		"other":   "prep",
	} {
		if got := workloadActivityRow(activity); got != want {
			t.Errorf("workloadActivityRow(%q) = %q, want %q", activity, got, want)
		}
	}
}

// Zero hours print as an empty cell, not "0": the college's form leaves a line
// blank when there was no work of that kind, and a printed 0 reads as a claim
// that was zeroed out on review.
func TestWorkloadHoursText_BlankForZeroAndTrimsWholeHours(t *testing.T) {
	for in, want := range map[float64]string{0: "", 6: "6", 6.5: "6.5", 30: "30"} {
		if got := workloadHoursText(in); got != want {
			t.Errorf("workloadHoursText(%v) = %q, want %q", in, got, want)
		}
	}
}
