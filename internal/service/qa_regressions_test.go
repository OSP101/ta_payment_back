package service

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// Regression tests for the QA pass of 2026-09-27. Each one pins a hole that
// was demonstrated end-to-end against a copy of the dev database.

// ---------------------------------------------------------------------------
// Submission periods: second-semester template
// ---------------------------------------------------------------------------

// The second-semester template gave ธันวาคม a window of 1 ม.ค. → 5 ม.ค. of the
// SAME year: already closed on creation, so every December worklog was
// forfeited. Labels for ม.ค.–มี.ค. also carried the academic year.
func TestBulkCreateForTerm_SecondSemesterWrapsDecember(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	termID := uuid.New()
	f.exec(`INSERT INTO academic_terms (id, academic_year, semester, starts_on, ends_on, months)
	        VALUES ($1, 2568, 2, '2025-11-17', '2026-03-15', 4)`, termID)

	got, err := f.Periods.BulkCreateForTerm(f.ctx, f.StaffID, termID)
	if err != nil {
		t.Fatal(err)
	}
	byYM := map[string]SubmissionPeriod{}
	for _, p := range got {
		byYM[p.YearMonth] = p
	}
	cases := []struct{ ym, starts, due, label string }{
		{"2568-11", "2025-11-01", "2025-12-05", "พฤศจิกายน 2568"},
		{"2568-12", "2025-12-01", "2026-01-05", "ธันวาคม 2568"},
		{"2568-01", "2026-01-01", "2026-02-05", "มกราคม 2569"},
		{"2568-03", "2026-03-01", "2026-04-05", "มีนาคม 2569"},
	}
	for _, c := range cases {
		p, ok := byYM[c.ym]
		if !ok {
			t.Fatalf("no period %s", c.ym)
		}
		if p.StartsOn != c.starts || p.DueDate != c.due || p.Label != c.label {
			t.Errorf("%s = (%s → %s, %q), want (%s → %s, %q)",
				c.ym, p.StartsOn, p.DueDate, p.Label, c.starts, c.due, c.label)
		}
	}
}

// First semester keeps the ประกาศ's shared 31 ก.ค. due date for สิงหาคม in the
// same year — the December wrap fix must not push it a year forward.
func TestBulkCreateForTerm_FirstSemesterSharedDueDateStays(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	termID := uuid.New()
	f.exec(`INSERT INTO academic_terms (id, academic_year, semester, starts_on, ends_on, months)
	        VALUES ($1, 2570, 1, '2027-06-21', '2027-10-17', 4)`, termID)
	got, err := f.Periods.BulkCreateForTerm(f.ctx, f.StaffID, termID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		if p.YearMonth == "2570-08" && (p.DueDate != "2027-07-31" || p.StartsOn != "2027-07-01") {
			t.Errorf("สิงหาคม = %s → %s, want 2027-07-01 → 2027-07-31", p.StartsOn, p.DueDate)
		}
	}
}

// ---------------------------------------------------------------------------
// Makeups
// ---------------------------------------------------------------------------

func firstMonday() string {
	return monthStart().AddDate(0, 0, firstMondayOffset()).Format("2006-01-02")
}

func (f *fixture) qaTeaching() *TeachingService {
	return &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
}

func (f *fixture) insertLog(date, activity, status, start, end string) uuid.UUID {
	id := uuid.New()
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES ($1, $2, $3::date, $4::time, $5::time,
	                EXTRACT(EPOCH FROM ($5::time - $4::time)) / 3600, $6, $7::worklog_status)`,
		id, f.AssignmentID, date, start, end, activity, status)
	return id
}

// A sitting the TA already sent (or had approved) was evidently taught; a
// makeup on top of it billed the same period twice.
func TestAddMakeup_RefusesSittingAlreadyLogged(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mon := firstMonday()
	f.insertLog(mon, "lab", "submitted", "13:00", "16:00")

	err := f.qaTeaching().AddMakeup(f.ctx, f.LecturerID, f.SectionID, MakeupSchedule{
		OriginalDate: mon, MakeupDate: day(27), Kind: "lab",
	})
	if err == nil {
		t.Fatal("a makeup was filed for a lab sitting that already has submitted hours")
	}
}

// A TA may only file a makeup for a period a holiday cancelled; the "กรณีอื่น"
// path belongs to the course.
func TestAddMakeup_TACannotCancelOrdinaryDay(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	err := f.qaTeaching().AddMakeup(f.ctx, f.TAID, f.SectionID, MakeupSchedule{
		OriginalDate: firstMonday(), MakeupDate: day(27), Kind: "lab",
	})
	if err != ErrForbidden {
		t.Fatalf("TA makeup for a non-holiday: err=%v, want ErrForbidden", err)
	}
}

// When the lecturer does cancel a sitting, the TA's unsent draft for it is
// removed so it cannot ride along into the next submit.
func TestAddMakeup_ClearsDraftOfCancelledSitting(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mon := firstMonday()
	draft := f.insertLog(mon, "lab", "draft", "13:00", "16:00")
	if err := f.qaTeaching().AddMakeup(f.ctx, f.LecturerID, f.SectionID, MakeupSchedule{
		OriginalDate: mon, MakeupDate: day(27), Kind: "lab",
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM work_logs WHERE id = $1`, draft).Scan(&n); err != nil || n != 0 {
		t.Fatalf("draft for the cancelled sitting survived: n=%d err=%v", n, err)
	}
}

// Exporting one month used to lock makeups for the whole course (and answer a
// bare 500). Only the months the makeup touches are locked now.
func TestAddMakeup_LockIsPerMonth(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	// Some OTHER month of the course is exported.
	other := monthStart().AddDate(0, -1, 0).Format("01")
	f.addSubmissionPeriod(other, openDueDate(), "exported", false)
	f.exec(`UPDATE teaching_courses SET exported_at = NOW() WHERE id = $1`, f.CourseID)

	if err := f.qaTeaching().AddMakeup(f.ctx, f.LecturerID, f.SectionID, MakeupSchedule{
		OriginalDate: firstMonday(), MakeupDate: day(27), Kind: "lab",
	}); err != nil {
		t.Fatalf("makeup in an un-exported month refused: %v", err)
	}

	// The current month exported: now refused, with a readable 409.
	f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "exported", false)
	err := f.qaTeaching().AddMakeup(f.ctx, f.LecturerID, f.SectionID, MakeupSchedule{
		OriginalDate: firstMonday(), MakeupDate: day(27), Kind: "lecture",
	})
	var ue *UserError
	if !errors.As(err, &ue) || ue.Status != 409 {
		t.Fatalf("makeup in an exported month: err=%v, want 409", err)
	}
}

// ---------------------------------------------------------------------------
// Holidays added after the fact
// ---------------------------------------------------------------------------

func TestHolidayCreate_RefusesSentClassesAndClearsDrafts(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	hs := &HolidayService{pool: f.Pool, aud: audit.New(f.Pool)}
	mon := firstMonday()

	f.insertLog(mon, "lab", "approved", "13:00", "16:00")
	if _, err := hs.Create(f.ctx, f.StaffID, HolidayInput{HolidayDate: mon, NameTH: "ทดสอบ"}); err == nil {
		t.Fatal("a holiday was added over an approved class sitting")
	}

	next := monthStart().AddDate(0, 0, firstMondayOffset()+7).Format("2006-01-02")
	draft := f.insertLog(next, "lab", "draft", "13:00", "16:00")
	if _, err := hs.Create(f.ctx, f.StaffID, HolidayInput{HolidayDate: next, NameTH: "ทดสอบ"}); err != nil {
		t.Fatalf("holiday over a draft only: %v", err)
	}
	var n int
	if err := f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM work_logs WHERE id = $1`, draft).Scan(&n); err != nil || n != 0 {
		t.Fatalf("draft on the new holiday survived: n=%d err=%v", n, err)
	}
}

// ---------------------------------------------------------------------------
// Admin accounts are out of staff's reach
// ---------------------------------------------------------------------------

func TestStaffCannotManageAdmin(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	us := &UserService{pool: f.Pool, aud: audit.New(f.Pool)}
	admin := f.insertUser("admin", "admin")

	var ue *UserError
	if err := us.Deactivate(f.ctx, f.StaffID, admin); !errors.As(err, &ue) || ue.Status != 403 {
		t.Fatalf("staff deactivated an admin: err=%v", err)
	}
	email := "hijack@example.test"
	if _, err := us.Update(f.ctx, f.StaffID, admin, UpdateUserInput{Email: &email}); !errors.As(err, &ue) || ue.Status != 403 {
		t.Fatalf("staff rewrote an admin's email: err=%v", err)
	}
	// Staff still manage ordinary accounts.
	if err := us.Deactivate(f.ctx, f.StaffID, f.LecturerID); err != nil {
		t.Fatalf("staff could not deactivate a lecturer: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Grad-special lump: one per TA per course
// ---------------------------------------------------------------------------

func TestBudget_GradSpecialLumpCountedOncePerTA(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	grad := f.insertUser("ta", "grad")
	f.exec(`UPDATE users SET study_level = 'phd' WHERE id = $1`, grad)
	req := uuid.New()
	f.exec(`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at, decided_at)
	        VALUES ($1, $2, $3, 'both', 'approved', NOW(), NOW())`, req, f.CourseID, f.LecturerID)
	for _, sec := range []string{"03", "04"} {
		sid := uuid.New()
		f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track) VALUES ($1, $2, $3, 'special')`,
			sid, f.CourseID, sec)
		f.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
		        VALUES (gen_random_uuid(), $1, $2, $3, 'phd')`, req, sid, grad)
	}
	var lump float64
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT LEAST(graduate_special_lumpsum, grad_special_term_cap) FROM `+payRatesInForce).Scan(&lump); err != nil {
		t.Fatal(err)
	}
	snap, err := (&BudgetService{pool: f.Pool}).Compute(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.UsedBahtSpecial != lump {
		t.Fatalf("special used = %.2f, want one lump %.2f (was counted per section)", snap.UsedBahtSpecial, lump)
	}
}

// ---------------------------------------------------------------------------
// Profile validation
// ---------------------------------------------------------------------------

func TestValidThaiCitizenID(t *testing.T) {
	for id, want := range map[string]bool{
		"1234567890121": true,
		"1234567890123": false, // one digit off
		"1101700230708": true,
		"12345":         false,
	} {
		if got := validThaiCitizenID(id); got != want {
			t.Errorf("validThaiCitizenID(%s) = %v, want %v", id, got, want)
		}
	}
}

func TestSafeSignatureSVG(t *testing.T) {
	ok := `<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 10 10'><path d='M0 0L10 10' stroke='#111'/></svg>`
	if !safeSignatureSVG(ok) {
		t.Error("the signature pad's own output was refused")
	}
	for _, bad := range []string{
		`<svg onload="alert(1)"><path d="M0 0"/></svg>`,
		`<svg><script>alert(1)</script></svg>`,
		`<svg><a href="javascript:alert(1)"><path d="M0 0"/></a></svg>`,
		`<img src=x>`,
	} {
		if safeSignatureSVG(bad) {
			t.Errorf("accepted unsafe signature: %s", bad)
		}
	}
}

var _ = time.Now // keep the import set stable if helpers above change

// ---------------------------------------------------------------------------
// Review round 2 (27/09/2026)
// ---------------------------------------------------------------------------

// Rewriting a period's month would carry its sign-off and lock rows onto
// another month and leave the signed-off month with no period at all.
func TestPeriodUpsert_RefusesMonthRewriteAfterSignoff(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	pid := f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "staff_reviewed", false)
	other := monthStart().AddDate(0, 1, 0).Format("01")
	_, err := f.Periods.Upsert(f.ctx, f.StaffID, SubmissionPeriod{
		ID: pid, TermID: f.TermID, YearMonth: fmt.Sprintf("%d-%s", f.AcademicYear, other),
		StartsOn: day(1), DueDate: openDueDate(), Label: "ย้ายเดือน",
	})
	var ue *UserError
	if !errors.As(err, &ue) || ue.Status != 409 {
		t.Fatalf("month of a signed-off period rewritten: err=%v, want 409", err)
	}
}

// Any change to a signed-off month's rows sends it back to pending, so the
// export cannot bill hours staff never checked.
func TestWorklogChange_ResetsStaffReview(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	pid := f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "staff_reviewed", false)
	f.insertLog(firstMonday(), "lab", "approved", "13:00", "16:00")
	var st string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT status FROM submission_period_status WHERE submission_period_id = $1`, pid).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != "pending" {
		t.Fatalf("status after a new approved row = %q, want pending", st)
	}
}

// The export lock holds at the row itself, whatever the timing of the check
// in Go.
func TestWorklogTrigger_RefusesWriteInExportedMonth(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "exported", false)
	_, err := f.Pool.Exec(f.ctx, `
		INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
		VALUES (gen_random_uuid(), $1, $2::date, '13:00', '16:00', 3, 'lab', 'draft')`,
		f.AssignmentID, firstMonday())
	if err == nil {
		t.Fatal("a work_log was inserted into an exported month")
	}
}

// The file is built before the lock; if rows moved in between, nothing locks.
func TestMarkCourseExportedAsBuilt_RefusesChangedRows(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.insertLog(firstMonday(), "lab", "approved", "13:00", "16:00")
	fp, err := f.Periods.CourseWorklogFingerprint(f.ctx, f.CourseID, nil)
	if err != nil {
		t.Fatal(err)
	}
	next := monthStart().AddDate(0, 0, firstMondayOffset()+7).Format("2006-01-02")
	f.insertLog(next, "lab", "approved", "13:00", "16:00")
	_, err = f.Periods.MarkCourseExportedAsBuilt(f.ctx, f.StaffID, f.CourseID, nil, fp)
	var ue *UserError
	if !errors.As(err, &ue) || ue.Status != 409 {
		t.Fatalf("export locked after rows changed under the file: err=%v, want 409", err)
	}
}

// Waiving over a scheduled makeup nulled its date in place and skipped every
// check DeleteMakeup makes.
func TestWaiveMakeup_RefusesOverScheduledMakeup(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mon := firstMonday()
	ts := f.qaTeaching()
	if err := ts.AddMakeup(f.ctx, f.LecturerID, f.SectionID, MakeupSchedule{
		OriginalDate: mon, MakeupDate: day(27), Kind: "lab",
	}); err != nil {
		t.Fatal(err)
	}
	err := ts.WaiveMakeup(f.ctx, f.LecturerID, f.SectionID, WaiveMakeupRequest{OriginalDate: mon, Kind: "lab"})
	var ue *UserError
	if !errors.As(err, &ue) || ue.Status != 409 {
		t.Fatalf("waived over a scheduled makeup: err=%v, want 409", err)
	}
}

// Widening a holiday's hours cancels more sittings, exactly like a new one.
func TestHolidayPatch_WideningClearsDrafts(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	hs := &HolidayService{pool: f.Pool, aud: audit.New(f.Pool)}
	mon := firstMonday()
	st, et := "08:00", "12:00"
	id, err := hs.Create(f.ctx, f.StaffID, HolidayInput{HolidayDate: mon, NameTH: "ครึ่งวัน", StartTime: &st, EndTime: &et})
	if err != nil {
		t.Fatal(err)
	}
	draft := f.insertLog(mon, "lab", "draft", "13:00", "16:00")
	if err := hs.Patch(f.ctx, f.StaffID, id, "ทั้งวัน", nil, nil, nil, nil); err != nil {
		t.Fatalf("widen to whole day: %v", err)
	}
	var n int
	if err := f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM work_logs WHERE id = $1`, draft).Scan(&n); err != nil || n != 0 {
		t.Fatalf("draft inside the widened holiday survived: n=%d err=%v", n, err)
	}
}

// SSO signs in by email, so rewriting a staff account's email is a takeover.
func TestStaffCannotRewriteStaffEmail(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	us := &UserService{pool: f.Pool, aud: audit.New(f.Pool)}
	other := f.insertUser("staff", "staff2")
	email := "takeover@example.test"
	var ue *UserError
	if _, err := us.Update(f.ctx, f.StaffID, other, UpdateUserInput{Email: &email}); !errors.As(err, &ue) || ue.Status != 403 {
		t.Fatalf("staff rewrote another staff account's email: err=%v", err)
	}
	// A TA's email is still staff's to fix.
	taEmail := "fixed-ta@example.test"
	if _, err := us.Update(f.ctx, f.StaffID, f.TAID, UpdateUserInput{Email: &taEmail}); err != nil {
		t.Fatalf("staff could not fix a TA's email: %v", err)
	}
}

// An SSO account whose temporary password was retired has password_hash NULL;
// a step-up password check must answer in Thai, not fail the row scan (500).
func TestVerifyUserPassword_NoLocalPassword(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	u := f.insertUser("lecturer", "ssoonly")
	f.exec(`UPDATE users SET password_hash = NULL WHERE id = $1`, u)
	err := VerifyUserPassword(f.ctx, f.Pool, u, "anything")
	var ue *UserError
	if !errors.As(err, &ue) || ue.Status != 401 {
		t.Fatalf("NULL password hash: err=%v, want a 401 UserError", err)
	}
}
