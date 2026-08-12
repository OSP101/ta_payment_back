package service

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// The two graduate documents are only useful if they actually reach the pack
// staff download. These tests walk the whole gate — TA sends, lecturer
// approves, staff sign off — and then look inside the ZIP.

// zipEntryNames lists what a built pack contains.
func zipEntryNames(t *testing.T, body []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		out = append(out, f.Name)
	}
	return out
}

// exportReadyZip drives a fixture all the way to a downloadable pack.
func exportReadyZip(t *testing.T, f *fixture) []byte {
	t.Helper()
	payoutReady(f)
	pid := f.addSubmissionPeriod(currentMonthMM(), "2026-12-31", "", false)
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, "", false); err != nil {
		t.Fatal(err)
	}
	if err := f.Periods.MarkStaffReviewed(f.ctx, f.StaffID, pid, f.TAID, f.CourseID, ""); err != nil {
		t.Fatal(err)
	}
	body, _, _, err := exportSvcFor(f).BuildCourseZip(f.ctx, f.CourseID, nil)
	if err != nil {
		t.Fatalf("BuildCourseZip: %v", err)
	}
	return body
}

func TestBuildCourseZip_GradCourseCarriesBothGraduateDocuments(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master", Track: "regular"})
	names := zipEntryNames(t, exportReadyZip(t, f))

	var evidence, workload bool
	for _, n := range names {
		if strings.Contains(n, "หลักฐาน-บัณฑิต.xlsx") {
			evidence = true
		}
		if strings.Contains(n, "ภาระงาน-") && strings.HasSuffix(n, ".docx") {
			workload = true
		}
	}
	if !evidence {
		t.Errorf("the graduate หลักฐานการจ่ายเงิน is missing from the pack: %v", names)
	}
	if !workload {
		t.Errorf("the แบบแสดงรายละเอียดภาระงาน .docx is missing from the pack: %v", names)
	}
	// ...and NO undergrad book, because this course has no undergrad TA. The
	// two levels are claimed on separate paperwork, so an empty undergrad form
	// in the pack would be a claim document naming nobody.
	for _, n := range names {
		if strings.Contains(n, "-เบิกจ่าย.xlsx") {
			t.Errorf("a graduate-only course shipped the undergrad book %q: %v", n, names)
		}
	}
}

// An undergrad course's pack must be exactly what it has always been. This is
// the regression that matters most: every course in the college files it, and
// a stray graduate document in the pack is a document the finance office has
// to ask about.
func TestBuildCourseZip_UndergradCourseGainsNothing(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	names := zipEntryNames(t, exportReadyZip(t, f))

	for _, n := range names {
		if strings.Contains(n, "บัณฑิต") || strings.HasSuffix(n, ".docx") {
			t.Errorf("an undergrad pack gained a graduate document: %q (all: %v)", n, names)
		}
	}
}
