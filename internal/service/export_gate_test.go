package service

import (
	"fmt"
	"strings"
	"testing"
)

// Downloading the ZIP is the freeze point: it becomes the claim document AND
// locks the months behind it. So it must refuse until every stage is finished —
// otherwise finance holds a payout file over worklogs that are still moving.

// payoutReady clears the OTHER export gate — approved profile + creditor form —
// so these tests fail on the pipeline stage under test rather than on paperwork.
func payoutReady(f *fixture) {
	f.exec(`INSERT INTO ta_profiles (user_id, prefix, status, completed_at, current_round)
	        VALUES ($1, 'นาย', 'approved', now(), 1)
	        ON CONFLICT (user_id) DO UPDATE SET status = 'approved'`, f.TAID)
	f.exec(`INSERT INTO ta_documents (id, user_id, kind, filename, mime, size_bytes, storage_key, status)
	        VALUES (gen_random_uuid(), $1, 'creditor_form', 'x.pdf', 'application/pdf', 1, 'k', 'approved')`, f.TAID)
}

// blockerKinds is the set of stages a course is currently stuck at.
func blockerKinds(t *testing.T, f *fixture) map[string]int {
	t.Helper()
	bs, err := exportSvcFor(f).CourseExportBlockers(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, b := range bs {
		out[b.Kind] += b.Rows
		if b.Kind == "unreviewed" {
			out[b.Kind]++ // unreviewed carries no row count
		}
	}
	return out
}

func TestExportGate_BlocksWhileTheLecturerStillHasRows(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	payoutReady(f)
	f.addSubmissionPeriod(currentMonthMM(), "2026-12-31", "", false)
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}

	if got := blockerKinds(t, f)["waiting_lecturer"]; got != 1 {
		t.Fatalf("waiting_lecturer = %d, want 1", got)
	}
	_, _, _, err := exportSvcFor(f).BuildCourseZip(f.ctx, f.CourseID)
	if err == nil {
		t.Fatal("BuildCourseZip must refuse while the lecturer has unapproved rows")
	}
	if !strings.Contains(err.Error(), "รออาจารย์อนุมัติ") {
		t.Errorf("error must name the stage, got: %v", err)
	}
}

func TestExportGate_BlocksWhileTheTAStillHasAnOpenDraft(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	// Deadline well in the future — the TA can still send this, so it is a
	// genuine blocker rather than a forfeit.
	f.addSubmissionPeriod(currentMonthMM(), "2099-12-31", "", false)
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))

	if got := blockerKinds(t, f)["waiting_ta"]; got != 1 {
		t.Fatalf("waiting_ta = %d, want 1", got)
	}
	if _, _, _, err := exportSvcFor(f).BuildCourseZip(f.ctx, f.CourseID); err == nil {
		t.Fatal("BuildCourseZip must refuse while the TA still has a sendable draft")
	}
}

// The counterweight: a draft the TA can no longer send is NOT a blocker. Treat
// it as one and a course whose TA missed a deadline could never be paid at all.
func TestExportGate_ForfeitedDraftDoesNotBlock(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	pid := f.addSubmissionPeriod(currentMonthMM(), "2099-12-31", "", false)
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	// The deadline passes with the row still unsent — forfeited, not owed.
	f.exec(`UPDATE submission_periods SET is_closed = true, due_date = '2020-01-01' WHERE id = $1`, pid)

	if got := blockerKinds(t, f); got["waiting_ta"] != 0 {
		t.Errorf("waiting_ta = %d, want 0 — a closed period is forfeited, not outstanding", got["waiting_ta"])
	}
}

func TestExportGate_BlocksAnApprovedMonthStaffNeverSignedOff(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	payoutReady(f)
	f.addSubmissionPeriod(currentMonthMM(), "2026-12-31", "", false)
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, "", false); err != nil {
		t.Fatal(err)
	}

	got := blockerKinds(t, f)
	if got["waiting_lecturer"] != 0 || got["waiting_ta"] != 0 {
		t.Fatalf("nothing should be waiting on a person now: %+v", got)
	}
	if got["unreviewed"] != 1 {
		t.Fatalf("unreviewed = %d, want 1 — staff have not signed the month off", got["unreviewed"])
	}
	_, _, _, err := exportSvcFor(f).BuildCourseZip(f.ctx, f.CourseID)
	if err == nil {
		t.Fatal("BuildCourseZip must refuse a month staff never reviewed — " +
			"MarkCourseExported would skip locking it, leaving the file over editable rows")
	}
	if !strings.Contains(err.Error(), "ตรวจสอบเบิกจ่าย") {
		t.Errorf("error must name the staff stage, got: %v", err)
	}
}

func TestExportGate_ClearsOnceStaffSignOff(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	pid := f.addSubmissionPeriod(currentMonthMM(), "2026-12-31", "", false)
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, "", false); err != nil {
		t.Fatal(err)
	}
	staff := f.StaffID
	if err := f.Periods.MarkStaffReviewed(f.ctx, staff, pid, f.TAID, f.CourseID, ""); err != nil {
		t.Fatal(err)
	}

	bs, err := exportSvcFor(f).CourseExportBlockers(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 0 {
		t.Errorf("blockers = %+v, want none once every stage is done", bs)
	}
}

// The preview's can_export is what the download button reads. If it ever
// disagrees with the gate the button starts offering what the server refuses —
// the failure this whole change exists to close.
func TestCoursePreview_CanExportAgreesWithTheGate(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	payoutReady(f)
	f.addSubmissionPeriod(currentMonthMM(), "2026-12-31", "", false)
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	svc := exportSvcFor(f)

	prev, err := svc.CoursePreview(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, buildErr := svc.BuildCourseZip(f.ctx, f.CourseID)
	if prev.CanExport != (buildErr == nil) {
		t.Errorf("can_export = %v but BuildCourseZip error = %v — the button and the server disagree",
			prev.CanExport, buildErr)
	}
	if len(prev.Blockers) == 0 {
		t.Error("preview must carry the reasons, or the screen can only say no without saying why")
	}
}

// submission_periods.year_month is Buddhist-era already. Reusing the settlement's
// Gregorian labeller on it printed "สิงหาคม 3112" — 2569 + 543.
func TestExportBlockers_MonthLabelsStayBuddhistEra(t *testing.T) {
	got := thaiMonthLabelsBE([]string{"2569-08", "2569-12", "2570-01"})
	want := []string{"สิงหาคม 2569", "ธันวาคม 2569", "มกราคม 2570"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("label[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// The Gregorian one must keep converting — they are not interchangeable.
	if g := thaiMonthLabels([]string{"2026-08"}); g[0] != "สิงหาคม 2569" {
		t.Errorf("thaiMonthLabels(2026-08) = %q, want สิงหาคม 2569", g[0])
	}
}

// The finance office files these by term, so the name is
// ปีการศึกษา_เทอม_รหัสวิชา. It carried a download timestamp until 04/08/2026,
// which named every re-download differently and left staff guessing which copy
// was current.
func TestBuildCourseZip_NamesTheFileByTermAndCourse(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
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

	_, name, _, err := exportSvcFor(f).BuildCourseZip(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	var code string
	f.Pool.QueryRow(f.ctx, `SELECT code FROM teaching_courses WHERE id=$1`, f.CourseID).Scan(&code)
	want := fmt.Sprintf("%d_1_%s.zip", f.AcademicYear, code) // the fixture term is semester 1
	if name != want {
		t.Errorf("filename = %q, want %q", name, want)
	}
	// Two downloads of the same term must land on the same name.
	_, again, _, err := exportSvcFor(f).BuildCourseZip(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if again != name {
		t.Errorf("re-download named %q, first was %q — the name must not drift", again, name)
	}
}
