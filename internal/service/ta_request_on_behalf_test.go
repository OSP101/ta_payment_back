package service

import (
	"strings"
	"testing"
)

// Staff file a request for a lecturer who does not do it themselves
// (03/10/2026). The request is the lecturer's; the officer is recorded as the
// sender, written as the audit actor, and the lecturer is told.
func TestCreateOnBehalf_RecordsTheOfficerAndTellsTheLecturer(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	// A timetable, so the request is decided (ta_request.auto_decide) rather
	// than resting on a missing one.
	rf.addClassFor(rf.TAID)

	res, err := rf.Req.CreateOnBehalf(rf.ctx, rf.StaffID, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("CreateOnBehalf: %v", err)
	}
	var lecturer, submittedBy string
	if err := rf.Pool.QueryRow(rf.ctx,
		`SELECT lecturer_id::text, COALESCE(submitted_by::text, '') FROM ta_requests WHERE id = $1`, res.ID).
		Scan(&lecturer, &submittedBy); err != nil {
		t.Fatal(err)
	}
	if lecturer != rf.LecturerID.String() {
		t.Errorf("lecturer_id = %s, want the lecturer the request is for", lecturer)
	}
	if submittedBy != rf.StaffID.String() {
		t.Errorf("submitted_by = %q, want the officer", submittedBy)
	}

	var actor string
	if err := rf.Pool.QueryRow(rf.ctx,
		`SELECT actor_id::text FROM audit_logs WHERE entity_id = $1 AND action = 'ta_request.auto_decide'`,
		res.ID.String()).Scan(&actor); err != nil {
		t.Fatalf("no audit row: %v", err)
	}
	if actor != rf.StaffID.String() {
		t.Errorf("audit actor = %s, want the officer who pressed send", actor)
	}

	var title string
	if err := rf.Pool.QueryRow(rf.ctx,
		`SELECT title FROM notifications WHERE user_id = $1 AND channel = 'in_app' AND title LIKE '%แทนท่าน%'`,
		rf.LecturerID).Scan(&title); err != nil {
		t.Fatalf("lecturer was not told: %v", err)
	}

	rows, err := rf.Req.ListForLecturer(rf.ctx, rf.LecturerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SubmittedByName == "" {
		t.Fatalf("lecturer's list = %+v, want the request naming who sent it", rows)
	}
}

// A lecturer's own request carries no sender — the list shows nothing extra.
func TestCreate_OwnRequestHasNoSubmittedBy(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatal(err)
	}
	var isNull bool
	if err := rf.Pool.QueryRow(rf.ctx,
		`SELECT submitted_by IS NULL FROM ta_requests WHERE id = $1`, res.ID).Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Error("submitted_by set on the lecturer's own request")
	}
}

// The lecturer picked must actually teach the course.
func TestCreateOnBehalf_RefusesALecturerWhoDoesNotTeachTheCourse(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	other := rf.insertUser("lecturer", "other")
	_, err := rf.Req.CreateOnBehalf(rf.ctx, rf.StaffID, other, rf.createInput())
	if err == nil || !strings.Contains(err.Error(), "อาจารย์ที่เลือกไม่ได้เป็นผู้สอน") {
		t.Fatalf("err = %v, want the not-their-course refusal", err)
	}
}

// Staff may withdraw a lecturer's request too; it says who did, the audit
// names the officer, and the lecturer is told.
func TestCancelOnBehalf_NamesTheOfficerAndTellsTheLecturer(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatal(err)
	}
	if err := rf.Req.CancelOnBehalf(rf.ctx, rf.StaffID, res.ID); err != nil {
		t.Fatalf("CancelOnBehalf: %v", err)
	}
	var status, reason, decidedBy string
	if err := rf.Pool.QueryRow(rf.ctx,
		`SELECT status::text, COALESCE(reject_reason,''), COALESCE(decided_by::text,'') FROM ta_requests WHERE id = $1`, res.ID).
		Scan(&status, &reason, &decidedBy); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || !strings.Contains(reason, "เจ้าหน้าที่") || decidedBy != rf.StaffID.String() {
		t.Errorf("status=%q reason=%q decided_by=%s, want cancelled by the officer, named", status, reason, decidedBy)
	}
	var n int
	if err := rf.Pool.QueryRow(rf.ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id = $1 AND channel = 'in_app' AND title LIKE '%ยกเลิกคำขอผู้ช่วยสอนแทนท่าน%'`,
		rf.LecturerID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("lecturer notices = %d, want 1", n)
	}
}

// Another lecturer still cannot cancel a request that is not theirs.
func TestCancel_OtherLecturerStillRefused(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatal(err)
	}
	other := rf.insertUser("lecturer", "other")
	wantUserErr(t, rf.Req.Cancel(rf.ctx, other, res.ID), 403)
}

func TestEditOnBehalf_PreservesLecturerAndRecordsOfficer(t *testing.T) {
	f := newRequestFixture(t, fixtureOpts{})
	f.addClassFor(f.TAID)
	old, err := f.Req.Create(f.ctx, f.LecturerID, f.createInput())
	if err != nil {
		t.Fatal(err)
	}
	in, err := f.Req.EditInputOnBehalf(f.ctx, f.StaffID, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	in.Assignments[0].SectionWorkloads[0].Workload.CheckWorkHrs = 1
	edited, err := f.Req.EditOnBehalf(f.ctx, f.StaffID, old.ID, *in)
	if err != nil {
		t.Fatal(err)
	}
	if f.status(old.ID) != "cancelled" || f.status(edited.ID) != "approved" {
		t.Fatal("replacement not committed")
	}
	var owner, sender, actor string
	if err := f.Pool.QueryRow(f.ctx, `SELECT lecturer_id::text, submitted_by::text FROM ta_requests WHERE id=$1`, edited.ID).Scan(&owner, &sender); err != nil {
		t.Fatal(err)
	}
	if owner != f.LecturerID.String() || sender != f.StaffID.String() {
		t.Fatalf("owner=%s sender=%s", owner, sender)
	}
	if err := f.Pool.QueryRow(f.ctx, `SELECT actor_id::text FROM audit_logs WHERE entity_id=$1 AND action='ta_request.edit'`, old.ID.String()).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	if actor != f.StaffID.String() {
		t.Fatalf("audit actor=%s", actor)
	}
	var notices int
	if err := f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM notifications WHERE user_id=$1 AND channel='in_app' AND title LIKE '%แก้ไขคำขอผู้ช่วยสอนแทนท่าน%'`, f.LecturerID).Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if notices != 1 {
		t.Fatalf("lecturer notices=%d", notices)
	}
	// The normal lecturer entry points still enforce ownership.
	other := f.insertUser("lecturer", "other")
	_, err = f.Req.EditInput(f.ctx, other, edited.ID)
	wantUserErr(t, err, 403)
	_, err = f.Req.Edit(f.ctx, other, edited.ID, *in)
	wantUserErr(t, err, 403)
	// Invalid replacement must leave the original active even for an officer.
	in.Assignments[0].SectionWorkloads[0].Workload = WorkloadInput{}
	if _, err := f.Req.EditOnBehalf(f.ctx, f.StaffID, edited.ID, *in); err == nil {
		t.Fatal("empty workload accepted")
	}
	if f.status(edited.ID) != "approved" {
		t.Fatal("failed edit cancelled original")
	}
}

func TestEditOnBehalf_IssuedRequestRemainsLocked(t *testing.T) {
	f := newRequestFixture(t, fixtureOpts{})
	f.addClassFor(f.TAID)
	old, err := f.Req.Create(f.ctx, f.LecturerID, f.createInput())
	if err != nil {
		t.Fatal(err)
	}
	in, err := f.Req.EditInputOnBehalf(f.ctx, f.StaffID, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO appointment_orders(id,term_id,round_no,order_no,order_date,effective_date,ta_count) VALUES($1,$2,1,'1','today','today',1)`, old.ID, f.TermID)
	f.exec(`INSERT INTO appointment_order_items(id,appointment_order_id,teaching_course_id,ta_id) VALUES(gen_random_uuid(),$1,$2,$3)`, old.ID, f.CourseID, f.TAID)
	if _, err := f.Req.EditOnBehalf(f.ctx, f.StaffID, old.ID, *in); err == nil {
		t.Fatal("officer edited issued request")
	}
	if err := f.Req.CancelOnBehalf(f.ctx, f.StaffID, old.ID); err == nil {
		t.Fatal("officer cancelled issued request")
	}
	if f.status(old.ID) != "approved" {
		t.Fatal("issued request changed")
	}
}
