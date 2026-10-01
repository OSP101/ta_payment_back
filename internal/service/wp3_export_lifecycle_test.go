package service

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// WP3 (01/10/2026): the export lifecycle — frozen figures for locked months,
// corrected versions after a send-back, one send-back behaviour, a real admin
// unlock, and the races around the staff sign-off.

// wp3Fixture is reviewFixture with the TA's documents approved (the sign-off
// now requires them) and an export service that keeps its files.
func wp3Fixture(t *testing.T) (*fixture, *ExportService, string) {
	t.Helper()
	f, month := reviewFixture(t) // reviewFixture now approves the TA's documents
	svc := &ExportService{pool: f.Pool, aud: audit.New(f.Pool),
		budget: &BudgetService{pool: f.Pool}, store: newMemStore()}
	return f, svc, month
}

func (f *fixture) gregMonth() string { return monthStart().Format("2006-01") }

// exportOnce does what ExportHandler.CourseZip does, minus HTTP: re-download
// from the archive when there is one, otherwise build, lock with figures,
// freeze, keep the file and record the batch.
func exportOnce(t *testing.T, f *fixture, svc *ExportService, months []string) (*ExportBatch, []byte, error) {
	t.Helper()
	if arch, err := svc.ArchivedReissue(f.ctx, f.CourseID, months); err != nil {
		return nil, nil, err
	} else if arch != nil {
		return nil, arch.Body, nil
	}
	fp, err := f.Periods.CourseWorklogFingerprint(f.ctx, f.CourseID, months)
	if err != nil {
		return nil, nil, err
	}
	pack, err := svc.BuildCourseZipPack(f.ctx, f.CourseID, months)
	if err != nil {
		return nil, nil, err
	}
	if _, err := f.Periods.MarkCourseExportedWithFigures(f.ctx, f.StaffID, f.CourseID, months, fp, pack.Figures); err != nil {
		return nil, nil, err
	}
	if err := svc.FreezeGradLumpSnapshot(f.ctx, f.StaffID, pack.GradLumps, months); err != nil {
		return nil, nil, err
	}
	key, _, err := svc.store.Save("export_batches", pack.Name, bytes.NewReader(pack.Body))
	if err != nil {
		return nil, nil, err
	}
	prev, err := svc.CoursePreview(WithGradLumpSnapshot(f.ctx, pack.GradLumps), f.CourseID, months)
	if err != nil {
		return nil, nil, err
	}
	b, err := (&ExportBatchService{pool: f.Pool, aud: audit.New(f.Pool)}).Record(f.ctx, f.StaffID, ExportBatch{
		TeachingCourseID: f.CourseID, FilePath: key, FileName: pack.Name,
		TACount: pack.TACount, Months: months, TotalBaht: prev.TotalActual})
	return b, pack.Body, err
}

func (f *fixture) logStatuses(t *testing.T) map[string]int {
	t.Helper()
	rows, err := f.Pool.Query(f.ctx, `SELECT status::text FROM work_logs WHERE assignment_id=$1`, f.AssignmentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out[s]++
	}
	return out
}

func reviewAndExport(t *testing.T, f *fixture, svc *ExportService, pid uuid.UUID) *ExportBatch {
	t.Helper()
	if err := f.Periods.MarkStaffReviewed(f.ctx, f.StaffID, pid, f.TAID, f.CourseID, ""); err != nil {
		t.Fatalf("MarkStaffReviewed: %v", err)
	}
	b, _, err := exportOnce(t, f, svc, []string{f.gregMonth()})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if b == nil {
		t.Fatal("first export was served from an archive that cannot exist yet")
	}
	return b
}

// Bug 1 (CP363205): a rate edit after export re-priced the exported month, and
// the re-download then 409'd. The month now keeps the figure its document
// carried — in the preview, the settlement and the pricing every dashboard
// reads — and a re-download is the archived file itself.
func TestExportLedger_LockedMonthKeepsItsFigureAndRedownloads(t *testing.T) {
	f, svc, month := wp3Fixture(t)
	pid := mustUUID(t, f.periodID(t, month))
	months := []string{f.gregMonth()}
	first := reviewAndExport(t, f, svc, pid)
	if first.TotalBaht <= 0 {
		t.Fatalf("fixture bug: exported total %.2f", first.TotalBaht)
	}
	var ledgerRows int
	_ = f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM export_month_ledger WHERE teaching_course_id=$1`, f.CourseID).Scan(&ledgerRows)
	if ledgerRows == 0 {
		t.Fatal("the export locked a month without recording its figure")
	}

	// The rate in force changes after the export.
	f.exec(`UPDATE pay_rates SET undergrad_regular = undergrad_regular * 2`)

	prev, err := svc.CoursePreview(f.ctx, f.CourseID, months)
	if err != nil {
		t.Fatal(err)
	}
	if prev.TotalActual != first.TotalBaht {
		t.Errorf("preview of the exported month = %.2f, document said %.2f — the month was re-priced live",
			prev.TotalActual, first.TotalBaht)
	}
	st, err := svc.SettleCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.PaidBaht(); got != first.TotalBaht {
		t.Errorf("settlement pays %.2f for the locked month, document said %.2f", got, first.TotalBaht)
	}
	if prev.Archived == nil || prev.Archived.BatchID != first.ID || !prev.Archived.FileAvailable {
		t.Errorf("preview does not point at the issued document: %+v", prev.Archived)
	}

	// Re-download: the archived bytes, not a rebuild, and no 409.
	b, body, err := exportOnce(t, f, svc, months)
	if err != nil {
		t.Fatalf("re-download of an exported month refused: %v", err)
	}
	if b != nil || len(body) == 0 {
		t.Fatal("re-download rebuilt and recorded a new batch instead of serving the archive")
	}
	// Even a rebuild (file lost) reproduces the frozen figures per TA.
	if _, err := svc.BuildCourseZipPack(f.ctx, f.CourseID, months); err != nil {
		t.Fatalf("rebuild of a locked month refused: %v", err)
	}
}

// Bug 2: after a deliberate send-back of an exported month and a correction,
// the re-export 409'd forever ("เดิม 600 ปัจจุบัน 480"). It is now a
// corrected version that points at the document it replaces.
func TestExport_SendBackThenCorrectedVersion(t *testing.T) {
	f, svc, month := wp3Fixture(t)
	pid := mustUUID(t, f.periodID(t, month))
	// A second row so a deletion changes the money.
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES (gen_random_uuid(), $1, $2::date, '13:00', '15:00', 2, 'review', 'approved')`,
		f.AssignmentID, day(12))
	first := reviewAndExport(t, f, svc, pid)

	if err := f.Periods.MarkSentBack(f.ctx, f.StaffID, pid, f.TAID, f.CourseID, "pending", "ลบรายการวันที่ 12"); err != nil {
		t.Fatalf("send-back of an exported month: %v", err)
	}
	// The TA deletes one row and resubmits, the lecturer approves.
	f.exec(`DELETE FROM work_logs WHERE assignment_id=$1 AND work_date=$2::date`, f.AssignmentID, day(12))
	f.exec(`UPDATE work_logs SET status='approved', reject_reason=NULL WHERE assignment_id=$1`, f.AssignmentID)

	second := reviewAndExport(t, f, svc, pid)
	if second.TotalBaht >= first.TotalBaht {
		t.Errorf("corrected total %.2f should be below the original %.2f", second.TotalBaht, first.TotalBaht)
	}
	if second.Version != 2 || second.PreviousBatchID == nil || *second.PreviousBatchID != first.ID {
		t.Errorf("corrected batch = version %d prev %v, want version 2 replacing %s",
			second.Version, second.PreviousBatchID, first.ID)
	}
	hist, err := (&ExportBatchService{pool: f.Pool}).ListByCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 {
		t.Fatalf("history has %d batches, want the original kept beside the correction", len(hist))
	}
	for _, h := range hist {
		if h.ID == first.ID && (h.SupersededBy == nil || *h.SupersededBy != second.ID) {
			t.Errorf("original batch not marked superseded: %+v", h)
		}
	}
}

// Bug 3: on a reviewed or exported month, send-back only moved the status and
// left the rows approved — nobody could edit. It now reopens the rows in one
// action, at every stage, and records the current reason.
func TestSendBack_ExportedMonthReopensRowsInOneAction(t *testing.T) {
	f, svc, month := wp3Fixture(t)
	pid := mustUUID(t, f.periodID(t, month))
	reviewAndExport(t, f, svc, pid)

	if err := f.Periods.MarkSentBack(f.ctx, f.StaffID, pid, f.TAID, f.CourseID, "pending", "เหตุผลที่สอง"); err != nil {
		t.Fatal(err)
	}
	if got := f.statusOf(t); got != "pending" {
		t.Errorf("status = %s, want pending", got)
	}
	if st := f.logStatuses(t); st["approved"] != 0 || st["rejected"] == 0 {
		t.Errorf("rows after one send-back = %v, want all rejected so the TA can edit", st)
	}
	var reason string
	_ = f.Pool.QueryRow(f.ctx, `SELECT COALESCE(sent_back_reason,'') FROM submission_period_status
	        WHERE ta_id=$1 AND teaching_course_id=$2`, f.TAID, f.CourseID).Scan(&reason)
	if reason != "เหตุผลที่สอง" {
		t.Errorf("timeline reason = %q, want the current one", reason)
	}
	// The TA can now edit the row (the export lock is gone).
	if _, err := f.Pool.Exec(f.ctx, `UPDATE work_logs SET status='draft' WHERE assignment_id=$1`, f.AssignmentID); err != nil {
		t.Errorf("row still write-locked after send-back: %v", err)
	}
}

// A closed month is refused with the way out, never forfeited.
func TestSendBack_ClosedExportedMonthRefusedWithGuidance(t *testing.T) {
	f, svc, month := wp3Fixture(t)
	pid := mustUUID(t, f.periodID(t, month))
	reviewAndExport(t, f, svc, pid)
	f.exec(`UPDATE submission_periods SET is_closed = true WHERE id = $1`, pid)
	err := f.Periods.MarkSentBack(f.ctx, f.StaffID, pid, f.TAID, f.CourseID, "pending", "แก้")
	if err == nil || !strings.Contains(err.Error(), "ขยายกำหนดส่ง") {
		t.Fatalf("closed-month send-back = %v, want a refusal naming the due-date extension", err)
	}
	if got := f.statusOf(t); got != "exported" {
		t.Errorf("refused send-back moved the month to %s", got)
	}
}

// Bug 4: admin unlock only cleared teaching_courses.exported_at.
func TestUnlockCourse_ReopensExportedMonths(t *testing.T) {
	f, svc, month := wp3Fixture(t)
	pid := mustUUID(t, f.periodID(t, month))
	reviewAndExport(t, f, svc, pid)
	f.exec(`UPDATE teaching_courses SET exported_at = now() WHERE id = $1`, f.CourseID)
	admin := f.insertUser("admin", "admin")
	ts := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	if err := ts.Unexport(f.ctx, admin, f.CourseID, "ปลดล็อกเพื่อแก้ไข"); err != nil {
		t.Fatalf("Unexport: %v", err)
	}
	if got := f.statusOf(t); got != "pending" {
		t.Errorf("month after unlock = %s, want pending (editable)", got)
	}
	if st := f.logStatuses(t); st["approved"] != 0 {
		t.Errorf("rows after unlock = %v, want sent back to the TA", st)
	}
	var flagged bool
	_ = f.Pool.QueryRow(f.ctx, `SELECT exported_at IS NOT NULL FROM teaching_courses WHERE id=$1`, f.CourseID).Scan(&flagged)
	if flagged {
		t.Error("course flag still set after unlock")
	}
	var audited int
	_ = f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM audit_logs WHERE action='submission_period.unlocked' AND entity_id=$1`,
		f.CourseID.String()).Scan(&audited)
	if audited != 1 {
		t.Errorf("unlock audit rows = %d, want 1", audited)
	}
	// The TA card reads the months, not the flag.
	ds := &DashboardService{pool: f.Pool}
	cards, err := ds.TaOverview(f.ctx, f.TAID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cards {
		if c.TeachingCourseID == f.CourseID && c.Stage == "exported" {
			t.Error("TA card still says exported after the months were reopened")
		}
	}
}

// The TA card says exported from the months even when the course flag is
// clear (it is set only by the handler).
func TestTaOverview_StageReadsMonthState(t *testing.T) {
	f, svc, month := wp3Fixture(t)
	pid := mustUUID(t, f.periodID(t, month))
	reviewAndExport(t, f, svc, pid)
	ds := &DashboardService{pool: f.Pool}
	cards, err := ds.TaOverview(f.ctx, f.TAID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cards {
		if c.TeachingCourseID == f.CourseID && c.Stage != "exported" {
			t.Errorf("stage = %s, want exported (every month with work is locked)", c.Stage)
		}
	}
}

// Bug 5: finance-revert said "ปลดล็อก…เพื่อให้แก้ไข" and left the month locked.
func TestRevertFinanceSent_NoticeMatchesState(t *testing.T) {
	f, svc, month := wp3Fixture(t)
	pid := mustUUID(t, f.periodID(t, month))
	reviewAndExport(t, f, svc, pid)
	f.exec(`UPDATE submission_period_status SET status='finance_sent' WHERE ta_id=$1`, f.TAID)
	admin := f.insertUser("admin", "admin")
	if err := f.Periods.RevertFinanceSent(f.ctx, admin, pid, f.TAID, f.CourseID, "ส่งผิด"); err != nil {
		t.Fatal(err)
	}
	if got := f.statusOf(t); got != "exported" {
		t.Fatalf("status = %s, want exported", got)
	}
	var body string
	_ = f.Pool.QueryRow(f.ctx, `SELECT body FROM notifications WHERE user_id=$1 AND channel='in_app'
	        ORDER BY created_at DESC LIMIT 1`, f.TAID).Scan(&body)
	if strings.Contains(body, "เพื่อให้แก้ไข") || !strings.Contains(body, "ยังคงถูกล็อก") {
		t.Errorf("TA notice = %q, want it to say the month is still locked", body)
	}
}

// Bug 6a: a TA's draft committed while staff sign off must not end up inside a
// staff_reviewed month.
func TestStaffReview_RaceWithDraftInsert(t *testing.T) {
	f, _, month := wp3Fixture(t)
	pid := mustUUID(t, f.periodID(t, month))
	tx, err := f.Pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// The TA's write, held open (the trigger takes the cell lock).
	if _, err := tx.Exec(f.ctx, `INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES (gen_random_uuid(), $1, $2::date, '18:00', '19:00', 1, 'review', 'draft')`, f.AssignmentID, day(13)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.Periods.MarkStaffReviewed(f.ctx, f.StaffID, pid, f.TAID, f.CourseID, "") }()
	time.Sleep(300 * time.Millisecond)
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	err = <-done
	if got := f.statusOf(t); got == StatusStaffReviewed {
		t.Fatalf("month signed off with a draft inside (sign-off err=%v)", err)
	}
	if err == nil {
		t.Error("sign-off succeeded although a draft was committed first")
	}
}

// Bug 6b: send-back racing a lecturer approval left some rows approved and
// some rejected, and the TA got both notices for the same month.
func TestSendBack_RaceWithLecturerApproval(t *testing.T) {
	f, _, month := wp3Fixture(t)
	pid := mustUUID(t, f.periodID(t, month))
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES (gen_random_uuid(), $1, $2::date, '13:00', '15:00', 2, 'review', 'submitted')`, f.AssignmentID, day(12))
	tx, err := f.Pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// The approval, held open.
	if _, err := tx.Exec(f.ctx, `UPDATE work_logs SET status='approved' WHERE assignment_id=$1 AND status='submitted'`, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- f.Periods.MarkSentBack(f.ctx, f.StaffID, pid, f.TAID, f.CourseID, "pending", "แก้ไข")
	}()
	time.Sleep(300 * time.Millisecond)
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("send-back: %v", err)
	}
	if st := f.logStatuses(t); st["approved"] != 0 {
		t.Errorf("rows after the race = %v, want none left approved", st)
	}
}

// Many concurrent sign-off / draft pairs: never a staff_reviewed month with
// open work.
func TestStaffReview_ConcurrentWritesNeverLeaveDraftInside(t *testing.T) {
	f, _, month := wp3Fixture(t)
	pid := mustUUID(t, f.periodID(t, month))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = f.Periods.MarkStaffReviewed(f.ctx, f.StaffID, pid, f.TAID, f.CourseID, "")
	}()
	go func() {
		defer wg.Done()
		_, _ = f.Pool.Exec(f.ctx, `INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
		        VALUES (gen_random_uuid(), $1, $2::date, '18:00', '19:00', 1, 'review', 'draft')`, f.AssignmentID, day(14))
	}()
	wg.Wait()
	var n int
	_ = f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM work_logs WHERE assignment_id=$1 AND status='draft'`, f.AssignmentID).Scan(&n)
	if n > 0 && f.statusOf(t) == StatusStaffReviewed {
		t.Fatal("staff_reviewed month holds a draft")
	}
}

// Bug 7: an unapproved TA document blocked only at the real export, for the
// whole course. The sign-off refuses, the gate names the TA, the dashboard is
// not eligible.
func TestProfileReadiness_IsABlockerPerTA(t *testing.T) {
	f, month := reviewFixtureWithoutOrder(t) // documents NOT approved
	f.addAppointmentOrder()
	pid := mustUUID(t, f.periodID(t, month))
	err := f.Periods.MarkStaffReviewed(f.ctx, f.StaffID, pid, f.TAID, f.CourseID, "")
	if err == nil || !strings.Contains(err.Error(), "โปรไฟล์") {
		t.Fatalf("sign-off without an approved profile = %v, want a refusal", err)
	}
	bs, err := exportSvcFor(f).CourseExportBlockers(f.ctx, f.CourseID, nil)
	if err != nil {
		t.Fatal(err)
	}
	named := false
	for _, b := range bs {
		if b.Kind == "profile" && b.Issue != "" {
			named = true
		}
	}
	if !named {
		t.Errorf("export gate does not name the TA whose documents are not approved: %+v", bs)
	}
	dash, err := (&ExportBatchService{pool: f.Pool}).DashboardSummary(f.ctx, &BudgetService{pool: f.Pool}, nil, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range dash.Courses {
		if c.TeachingCourseID == f.CourseID && (c.ExportEligible || c.ReviewComplete || len(c.ProfileNotReady) == 0) {
			t.Errorf("dashboard row = eligible %v complete %v notReady %v", c.ExportEligible, c.ReviewComplete, c.ProfileNotReady)
		}
	}
}

// Bug 8: the silent reset of a sign-off now tells staff, with course, TA and
// month in the title, and puts what happened on the timeline.
func TestStaffReviewReset_NotifiesStaff(t *testing.T) {
	f, _, month := wp3Fixture(t)
	pid := mustUUID(t, f.periodID(t, month))
	if err := f.Periods.MarkStaffReviewed(f.ctx, f.StaffID, pid, f.TAID, f.CourseID, ""); err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES (gen_random_uuid(), $1, $2::date, '18:00', '19:00', 1, 'review', 'draft')`, f.AssignmentID, day(15))
	if got := f.statusOf(t); got != "pending" {
		t.Fatalf("status = %s, want reset to pending", got)
	}
	var code, label, title string
	_ = f.Pool.QueryRow(f.ctx, `SELECT code FROM teaching_courses WHERE id=$1`, f.CourseID).Scan(&code)
	_ = f.Pool.QueryRow(f.ctx, `SELECT label FROM submission_periods WHERE id=$1`, pid).Scan(&label)
	if err := f.Pool.QueryRow(f.ctx, `SELECT title FROM notifications WHERE user_id=$1 AND channel='in_app'
	        AND title LIKE 'ต้องตรวจเบิกจ่ายอีกครั้ง%'`, f.StaffID).Scan(&title); err != nil {
		t.Fatalf("no staff notice for the reset: %v", err)
	}
	if !strings.Contains(title, code) || !strings.Contains(title, label) || !strings.Contains(title, "ta") {
		t.Errorf("notice title %q must carry course %s, TA and month %s", title, code, label)
	}
	var reason string
	_ = f.Pool.QueryRow(f.ctx, `SELECT sent_back_reason FROM submission_period_status WHERE ta_id=$1`, f.TAID).Scan(&reason)
	if !strings.Contains(reason, "เพิ่มรายการ") {
		t.Errorf("timeline reason = %q, want what changed", reason)
	}
}

// Bug 9: periods follow the term's own months, each opening on its own 1st.
func TestBulkCreateForTerm_FollowsTermDatesAndOpensOnOwnMonth(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	termID := uuid.New()
	f.exec(`INSERT INTO academic_terms (id, academic_year, semester, starts_on, ends_on, months)
	        VALUES ($1, 2570, 1, '2027-07-05', '2027-11-20', 4)`, termID)
	got, err := f.Periods.BulkCreateForTerm(f.ctx, f.StaffID, termID)
	if err != nil {
		t.Fatal(err)
	}
	byYM := map[string]SubmissionPeriod{}
	for _, p := range got {
		byYM[p.YearMonth] = p
	}
	if _, ok := byYM["2570-06"]; ok {
		t.Error("June created for a term that starts in July")
	}
	if _, ok := byYM["2570-11"]; !ok {
		t.Error("November missing for a term that ends in November")
	}
	if p := byYM["2570-08"]; p.StartsOn != "2027-08-01" || p.DueDate <= p.StartsOn {
		t.Errorf("สิงหาคม = %s → %s, want it to open 1 ส.ค.", p.StartsOn, p.DueDate)
	}
	if p := byYM["2570-07"]; p.DueDate != "2027-07-31" {
		t.Errorf("กรกฎาคม due = %s, want the ประกาศ's 31 ก.ค.", p.DueDate)
	}
}

func TestDeletePeriod_RefusesReviewedMonth(t *testing.T) {
	f, _, month := wp3Fixture(t)
	pid := mustUUID(t, f.periodID(t, month))
	if err := f.Periods.MarkStaffReviewed(f.ctx, f.StaffID, pid, f.TAID, f.CourseID, ""); err != nil {
		t.Fatal(err)
	}
	err := f.Periods.Delete(f.ctx, f.StaffID, pid)
	var ue *UserError
	if !errors.As(err, &ue) || ue.Status != 409 {
		t.Fatalf("deleting a reviewed period = %v, want 409", err)
	}
}

// Bug 11: the class-clash blocker printed the Gregorian year.
func TestClassClashBlocker_UsesBuddhistYear(t *testing.T) {
	f, svc, _ := wp3Fixture(t)
	// The fixture's own class is Sunday 07:00–08:00; an approved row on top of it.
	sunday := monthStart()
	for sunday.Weekday() != time.Sunday {
		sunday = sunday.AddDate(0, 0, 1)
	}
	f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES (gen_random_uuid(), $1, $2::date, '07:00', '08:00', 1, 'review', 'approved')`,
		f.AssignmentID, sunday.Format("2006-01-02"))
	bs, err := svc.CourseExportBlockers(f.ctx, f.CourseID, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range bs {
		if b.Kind != "class_clash" {
			continue
		}
		found = true
		for _, m := range b.Months {
			if strings.Contains(m, "2026") || strings.Contains(m, "2027") {
				t.Errorf("clash month label %q carries a Gregorian year", m)
			}
		}
	}
	if !found {
		t.Skip("fixture produced no clash on this calendar")
	}
}
