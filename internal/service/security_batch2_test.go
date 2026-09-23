package service

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// Batch 2a: the document write path (DocsService.Upload) enforces PDPA consent
// and approved-profile immutability for EVERY caller, and rejecting a single
// document reopens an approved profile so the TA is never left unable to fix it.

func uploadPDF(svc *DocsService, uid uuid.UUID, kind string) (uuid.UUID, error) {
	b := pdfBytes()
	return svc.Upload(context.Background(), uid, kind, kind+".pdf", "application/pdf", int64(len(b)), bytes.NewReader(b))
}

func userErrStatus(err error) int {
	var ue *UserError
	if errors.As(err, &ue) {
		return ue.Status
	}
	return 0
}

func TestUpload_RefusesWithoutPdpaConsent(t *testing.T) {
	svc, store, _ := avFixture(t, nil)
	ctx := context.Background()

	// A second TA who never accepted the notice.
	other := uuid.New()
	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active) VALUES ($1,$2,'ไม่','ยินยอม',TRUE)`,
		other, "noconsent-"+other.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}

	for _, kind := range []string{"national_id", "bank_book", "creditor_form"} {
		_, err := uploadPDF(svc, other, kind)
		if userErrStatus(err) != 403 {
			t.Fatalf("%s: expected 403 without consent, got %v", kind, err)
		}
	}
	if store.saved != 0 {
		t.Fatalf("nothing may reach storage without consent, got %d saves", store.saved)
	}
}

func TestUpload_RefusesWhenProfileApproved(t *testing.T) {
	svc, _, uid := avFixture(t, nil)
	ctx := context.Background()
	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO ta_profiles (user_id, status, current_round) VALUES ($1,'approved',1)`, uid); err != nil {
		t.Fatal(err)
	}

	_, err := uploadPDF(svc, uid, "creditor_form")
	if userErrStatus(err) != 409 {
		t.Fatalf("expected 409 replacing a document on an approved profile, got %v", err)
	}
}

// The reject-one-document path must reopen the profile. Otherwise the guard
// above would strand a TA whom staff asked to fix a document.
func TestReviewReject_ReopensApprovedProfileSoTACanReupload(t *testing.T) {
	svc, _, uid := avFixture(t, nil)
	ctx := context.Background()
	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO ta_profiles (user_id, status, current_round) VALUES ($1,'submitted',1)`, uid); err != nil {
		t.Fatal(err)
	}
	docID, err := uploadPDF(svc, uid, "bank_book")
	if err != nil {
		t.Fatalf("initial upload: %v", err)
	}
	// Staff later approved the whole profile.
	if _, err := svc.pool.Exec(ctx, `UPDATE ta_profiles SET status='approved' WHERE user_id=$1`, uid); err != nil {
		t.Fatal(err)
	}

	if err := svc.Review(ctx, uid, docID, false, "สมุดบัญชีไม่ชัด"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	var status string
	if err := svc.pool.QueryRow(ctx, `SELECT status::text FROM ta_profiles WHERE user_id=$1`, uid).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "needs_fix" {
		t.Fatalf("rejecting a document must reopen the profile, status=%q", status)
	}
	if _, err := uploadPDF(svc, uid, "bank_book"); err != nil {
		t.Fatalf("TA must be able to re-upload after a rejection, got %v", err)
	}
}

// ---- Batch 2b: the printed appointment order is enforced on WRITE paths ----
//
// The review queue already hid un-appointed pairs; these assert the forward
// write transitions agree, so such a pair can never be reviewed, sent to
// finance, or locked into an exported claim.

func TestMarkStaffReviewed_RefusesUnappointedPair(t *testing.T) {
	f, month := reviewFixtureWithoutOrder(t)
	staff := f.insertUser("staff", "officer")
	pid := mustUUID(t, f.periodID(t, month))

	err := f.Periods.MarkStaffReviewed(f.ctx, staff, pid, f.TAID, f.CourseID, "")
	if userErrStatus(err) != 400 {
		t.Fatalf("expected un-appointed pair to be refused, got %v (status now %q)", err, f.statusOf(t))
	}
	if st := f.statusOf(t); st == StatusStaffReviewed {
		t.Fatal("un-appointed pair reached staff_reviewed")
	}
}

// A colleague whose approved work is NOT on a printed order cannot be signed off,
// so their month stays unreviewed and the course's claim stays blocked until the
// order is printed. Blocking (rather than silently leaving them out) keeps the
// claim file honest: the claim workbook lists people from the assignments, so a
// pair left out of the gate would still be billed in the file finance receives.
func TestExport_UnappointedColleagueBlocksUntilAppointed(t *testing.T) {
	f, month := reviewFixture(t) // appointed TA, approved work
	other := f.secondTAOnSameCourse()
	staff := f.insertUser("staff", "officer")
	pid := mustUUID(t, f.periodID(t, month))
	if err := f.Periods.MarkStaffReviewed(f.ctx, staff, pid, f.TAID, f.CourseID, ""); err != nil {
		t.Fatalf("MarkStaffReviewed (appointed TA): %v", err)
	}
	if err := f.Periods.MarkStaffReviewed(f.ctx, staff, pid, other, f.CourseID, ""); userErrStatus(err) != 400 {
		t.Fatalf("the un-appointed colleague must not be signed off, got %v", err)
	}
	blockers, err := exportSvcFor(f).CourseExportBlockers(f.ctx, f.CourseID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(blockers) == 0 {
		t.Fatal("an un-appointed colleague's unreviewed approved work must keep the claim blocked")
	}
	// The reason must say what to DO: issue the order. "unreviewed" would send
	// staff to a review queue that does not list this TA.
	for _, bl := range blockers {
		if bl.Kind != "not_appointed" {
			t.Errorf("blocker kind = %q, want not_appointed (the queue cannot review this TA)", bl.Kind)
		}
	}
}

// A pair reviewed BEFORE the write-side check existed can still be sitting at
// staff_reviewed; the lock must not freeze it into an exported claim.
func TestMarkCourseExported_DoesNotLockUnappointedPair(t *testing.T) {
	f, month := reviewFixtureWithoutOrder(t)
	pid := mustUUID(t, f.periodID(t, month))
	staff := f.insertUser("staff", "officer")
	f.exec(`INSERT INTO submission_period_status (id, submission_period_id, ta_id, teaching_course_id, status)
	        VALUES (gen_random_uuid(), $1, $2, $3, 'staff_reviewed')`,
		pid, f.TAID, f.CourseID)

	if _, err := f.Periods.MarkCourseExported(f.ctx, staff, f.CourseID, nil); err != nil {
		t.Fatalf("MarkCourseExported: %v", err)
	}
	if st := f.statusOf(t); st == "exported" {
		t.Fatal("un-appointed pair was locked as exported")
	}
}

// ---- Batch 2c: who may be written into authority-bearing rows ----

func TestConfirmCourseGroup_RejectsMemberFromAnotherTerm(t *testing.T) {
	svc, ctx, pool, term := newCourseGroupFixture(t)
	otherTerm := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO academic_terms (id, academic_year, semester) VALUES ($1, 2569, 2)`, otherTerm); err != nil {
		t.Fatal(err)
	}
	here := cgInsertCourse(t, pool, term, "CP111111", "X")
	foreign := cgInsertCourse(t, pool, otherTerm, "SC222222", "X")

	_, err := svc.ConfirmCourseGroup(ctx, cgActor(t, pool), term, here, []uuid.UUID{here, foreign}, "CS")
	if userErrStatus(err) != 400 {
		t.Fatalf("a member from another term must be refused, got %v", err)
	}
}

func TestAssertActiveLecturers_RejectsNonLecturerAndInactive(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	ta := f.insertUser("ta", "ta-only")
	gone := f.insertUser("lecturer", "left")
	f.exec(`UPDATE users SET is_active = FALSE WHERE id = $1`, gone)

	if err := assertActiveLecturers(f.ctx, f.Pool, []uuid.UUID{f.LecturerID}); err != nil {
		t.Fatalf("an active lecturer must pass: %v", err)
	}
	if err := assertActiveLecturers(f.ctx, f.Pool, []uuid.UUID{f.LecturerID, ta}); userErrStatus(err) != 400 {
		t.Fatalf("a TA-only account must be refused, got %v", err)
	}
	if err := assertActiveLecturers(f.ctx, f.Pool, []uuid.UUID{gone}); userErrStatus(err) != 400 {
		t.Fatalf("a deactivated lecturer must be refused, got %v", err)
	}
	// Repeating an id is not a reason to refuse a valid list.
	if err := assertActiveLecturers(f.ctx, f.Pool, []uuid.UUID{f.LecturerID, f.LecturerID}); err != nil {
		t.Fatalf("duplicate valid ids must pass: %v", err)
	}
}

func TestSignerAuthority_RefusesIneligibleOrVacantSeat(t *testing.T) {
	pool, ids := signerFixture(t,
		[2]string{"ธุรการ ทดสอบ", "เจ้าหน้าที่ธุรการ"},
		[2]string{"รอง ทดสอบ", "รองคณบดีฝ่ายวิชาการ"},
		[2]string{"ผู้ช่วย ทดสอบ", "ผู้ช่วยคณบดีฝ่ายดิจิทัล"},
	)
	ctx := context.Background()

	if _, err := loadSignerAuthority(ctx, pool, ids[0]); userErrStatus(err) != 400 {
		t.Fatalf("a clerical seat must not sign for the dean, got %v", err)
	}
	a, err := loadSignerAuthority(ctx, pool, ids[1])
	if err != nil {
		t.Fatalf("a vice dean must be able to act for the dean: %v", err)
	}
	if a.ActingFor == "" {
		t.Fatal("a vice dean signs as ACTING — the acting line must be present")
	}
	if _, err := loadSignerAuthority(ctx, pool, ids[2]); err != nil {
		t.Fatalf("an assistant dean must be able to act for the dean: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE admin_officers SET is_active = FALSE WHERE id = $1`, ids[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSignerAuthority(ctx, pool, ids[1]); userErrStatus(err) != 400 {
		t.Fatalf("a vacant seat must not sign, got %v", err)
	}
}

func TestCertifierEligibility(t *testing.T) {
	for title, want := range map[string]bool{
		"หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์": true,
		"คณบดีวิทยาลัยการคอมพิวเตอร์":        true,
		"รองคณบดีฝ่ายบริหาร":                 true,
		"ผู้ช่วยคณบดีฝ่ายแผนและประกันคุณภาพ": true,
		"เจ้าหน้าที่ธุรการ":                  false,
		"รองหัวหน้าสาขาวิชา":                 false, // prefix, not substring
	} {
		if got := CanCertifyForHead(title); got != want {
			t.Errorf("CanCertifyForHead(%q) = %v, want %v", title, got, want)
		}
	}
	if CanSignForDean("หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์") {
		t.Error("the head of department is not on the executive team and must not sign for the dean")
	}
}

// ---- Batch 2d: editing an APPROVED work log needs password + reason and tells the lecturers ----

func approvedWorkLog(t *testing.T) (*fixture, uuid.UUID) {
	t.Helper()
	f := newFixture(t, fixtureOpts{})
	id := f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='approved' WHERE id=$1`, id)
	return f, id
}

func TestStaffUpsert_ApprovedRowRequiresStepUp(t *testing.T) {
	f, id := approvedWorkLog(t)
	edit := f.entry(day(10), "09:00", "12:00", 3)
	edit.ID = id
	good := "แก้ชั่วโมงตามใบลงชื่อจริงของวันนั้น"

	for name, su := range map[string]*EditStepUp{
		"no step-up":     nil,
		"short reason":   {Password: fixturePassword, Reason: "สั้น"},
		"wrong password": {Password: "not-the-password", Reason: good},
	} {
		if _, err := f.Svc.StaffUpsert(f.ctx, f.StaffID, true, edit, su); err == nil {
			t.Fatalf("%s: editing an approved row must be refused", name)
		}
	}
	var hours float64
	if err := f.Pool.QueryRow(f.ctx, `SELECT hours FROM work_logs WHERE id=$1`, id).Scan(&hours); err != nil {
		t.Fatal(err)
	}
	if hours != 2 {
		t.Fatalf("a refused edit changed the row: hours=%v", hours)
	}

	if _, err := f.Svc.StaffUpsert(f.ctx, f.StaffID, true, edit, &EditStepUp{Password: fixturePassword, Reason: good}); err != nil {
		t.Fatalf("a correctly stepped-up edit must succeed: %v", err)
	}
	var status string
	if err := f.Pool.QueryRow(f.ctx, `SELECT hours, status::text FROM work_logs WHERE id=$1`, id).Scan(&hours, &status); err != nil {
		t.Fatal(err)
	}
	if hours != 3 || status != "approved" {
		t.Fatalf("after the edit: hours=%v status=%q, want 3 approved", hours, status)
	}
	var notified int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id=$1 AND title LIKE 'มีการแก้ไขบันทึกเวลาที่อนุมัติแล้ว%'`,
		f.LecturerID).Scan(&notified); err != nil {
		t.Fatal(err)
	}
	if notified == 0 {
		t.Fatal("the course lecturer must be told when a row they approved is changed")
	}
}

// Rows that are not approved yet keep the old one-click behaviour.
func TestStaffUpsert_UnapprovedRowNeedsNoStepUp(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	id := f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	edit := f.entry(day(10), "09:00", "12:00", 3)
	edit.ID = id
	if _, err := f.Svc.StaffUpsert(f.ctx, f.StaffID, true, edit, nil); err != nil {
		t.Fatalf("editing a draft row must not require a step-up: %v", err)
	}
}
