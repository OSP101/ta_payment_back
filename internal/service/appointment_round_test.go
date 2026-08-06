package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

// The คำสั่งแต่งตั้ง is issued in rounds (24/07/2026 meeting). TA requests never
// close, so a course still waiting on a timetable is skipped and picked up
// later — and the later round must carry ONLY the stragglers.
//
// Build() used to be stateless: it read every approved assignment in the term
// and rendered them, so running it twice appointed the same people twice on
// paper. These tests pin the ledger that prevents that.

type apptFixture struct {
	t    *testing.T
	svc  *AppointmentOrderService
	ctx  context.Context
	term uuid.UUID
	in   AppointmentOrderInput
}

func newApptFixture(t *testing.T) *apptFixture {
	t.Helper()
	pool := testutil.NewPool(t)
	ctx := context.Background()
	svc := &AppointmentOrderService{
		pool:    pool,
		aud:     audit.New(pool),
		fontDir: filepath.Join(repoRoot(t), "assets", "fonts"),
	}

	term := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO academic_terms (id, academic_year, semester, is_active) VALUES ($1, 2569, 1, TRUE)`,
		term); err != nil {
		t.Fatalf("insert term: %v", err)
	}
	// Build requires a real signer from the executive roster.
	signer := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO admin_officers (id, full_name, title, academic_prefix, is_active)
		 VALUES ($1, 'ผู้ลงนาม ทดสอบ', 'คณบดี', 'รศ.ดร.', TRUE)`, signer); err != nil {
		t.Fatalf("insert signer: %v", err)
	}

	f := &apptFixture{t: t, svc: svc, ctx: ctx, term: term}
	f.in = AppointmentOrderInput{
		TermID: term, OrderNo: "6/2569",
		OrderDate: "29 กรกฎาคม 2569", EffectiveDate: "1 สิงหาคม 2569",
		SignerOfficerID: signer,
	}
	return f
}

// addCourseWithTA wires one approved (or pending) TA on a new course and
// returns the course id and TA id.
func (f *apptFixture) addCourseWithTA(code, taFirst, status string) (uuid.UUID, uuid.UUID) {
	f.t.Helper()
	exec := func(sql string, args ...any) {
		if _, err := f.svc.pool.Exec(f.ctx, sql, args...); err != nil {
			f.t.Fatalf("fixture exec: %v\nSQL: %s", err, sql)
		}
	}
	lec, ta := uuid.New(), uuid.New()
	exec(`INSERT INTO users (id,email,first_name,last_name,is_active) VALUES ($1,$2,'อาจารย์','ทดสอบ',TRUE)`,
		lec, "lec-"+lec.String()+"@example.test")
	exec(`INSERT INTO users (id,email,title,first_name,last_name,student_id,is_active)
	      VALUES ($1,$2,'นาย',$3,'ผู้ช่วย','6530000-0',TRUE)`,
		ta, "ta-"+ta.String()+"@example.test", taFirst)

	tc, sec, req, asg := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO teaching_courses (id,term_id,code,name_th,level,credits,lecture_hrs,lab_hrs,self_hrs,num_students)
	      VALUES ($1,$2,$3,'วิชาทดสอบ','undergrad',3,2,2,5,40)`, tc, f.term, code)
	exec(`INSERT INTO teaching_lecturers (teaching_course_id,lecturer_id,is_primary) VALUES ($1,$2,TRUE)`, tc, lec)
	exec(`INSERT INTO sections (id,teaching_course_id,sec_no,track) VALUES ($1,$2,'01','regular')`, sec, tc)
	exec(`INSERT INTO ta_requests (id,teaching_course_id,lecturer_id,reimburse_scope,status,submitted_at)
	      VALUES ($1,$2,$3,'both',$4::ta_request_status,NOW())`, req, tc, lec, status)
	exec(`INSERT INTO ta_request_assignments (id,request_id,section_id,ta_id,level)
	      VALUES ($1,$2,$3,$4,'undergrad')`, asg, req, sec, ta)
	return tc, ta
}

func TestAppointmentRounds_SecondRoundExcludesAlreadyIssued(t *testing.T) {
	f := newApptFixture(t)
	f.addCourseWithTA("CP101", "หนึ่ง", "approved")

	p1, err := f.svc.Preview(f.ctx, f.term)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if p1.NextRound != 1 || p1.IsLate {
		t.Fatalf("first round = %d (late=%v), want round 1 on time", p1.NextRound, p1.IsLate)
	}
	if len(p1.Include) != 1 {
		t.Fatalf("round 1 should include the approved TA, got %d", len(p1.Include))
	}

	if _, _, err := f.svc.Build(f.ctx, uuid.Nil, f.in); err != nil {
		t.Fatalf("Build round 1: %v", err)
	}

	// A second course becomes ready only after round 1 was printed.
	f.addCourseWithTA("CP202", "สอง", "approved")

	p2, err := f.svc.Preview(f.ctx, f.term)
	if err != nil {
		t.Fatalf("Preview round 2: %v", err)
	}
	if p2.NextRound != 2 || !p2.IsLate {
		t.Fatalf("second round = %d (late=%v), want round 2 flagged late", p2.NextRound, p2.IsLate)
	}
	if len(p2.Include) != 1 {
		t.Fatalf("round 2 must carry ONLY the straggler, got %d names", len(p2.Include))
	}
	if p2.Include[0].CourseCode != "CP202" {
		t.Errorf("round 2 includes %q, want the course that was not ready earlier", p2.Include[0].CourseCode)
	}
	if p2.AlreadyIssued != 1 {
		t.Errorf("already issued = %d, want 1", p2.AlreadyIssued)
	}
}

// Running Build twice with nothing new must refuse rather than reprint.
func TestAppointmentRounds_RefusesWhenEveryoneAppointed(t *testing.T) {
	f := newApptFixture(t)
	f.addCourseWithTA("CP101", "หนึ่ง", "approved")

	if _, _, err := f.svc.Build(f.ctx, uuid.Nil, f.in); err != nil {
		t.Fatalf("Build round 1: %v", err)
	}
	_, _, err := f.svc.Build(f.ctx, uuid.Nil, f.in)
	if err == nil {
		t.Fatal("a second identical round must be refused, not reprinted")
	}
	if got := err.Error(); got == "" || !contains(got, "ครบทุกคนแล้ว") {
		t.Errorf("message should say everyone is already appointed, got: %v", err)
	}
}

// A course whose TA request has not been decided is not appointable, and must
// be named so staff can chase it rather than wonder why it vanished.
func TestAppointmentRounds_SkipsUndecidedRequests(t *testing.T) {
	f := newApptFixture(t)
	f.addCourseWithTA("CP101", "หนึ่ง", "approved")
	f.addCourseWithTA("CP999", "รอตาราง", "submitted")

	p, err := f.svc.Preview(f.ctx, f.term)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(p.Include) != 1 || p.Include[0].CourseCode != "CP101" {
		t.Fatalf("only the approved course may be included, got %+v", p.Include)
	}
	if len(p.Skipped) != 1 {
		t.Fatalf("the undecided course must be reported as skipped, got %d", len(p.Skipped))
	}
	if p.Skipped[0].CourseCode != "CP999" {
		t.Errorf("skipped %q, want CP999", p.Skipped[0].CourseCode)
	}
	if len(p.Skipped[0].PendingTAs) == 0 {
		t.Error("skipped course should name who is holding it up")
	}
}

// The ledger is the record of what was printed; the history list is how staff
// see it later.
func TestAppointmentRounds_HistoryRecordsEachRound(t *testing.T) {
	f := newApptFixture(t)
	f.addCourseWithTA("CP101", "หนึ่ง", "approved")
	if _, _, err := f.svc.Build(f.ctx, uuid.Nil, f.in); err != nil {
		t.Fatalf("Build: %v", err)
	}
	f.addCourseWithTA("CP202", "สอง", "approved")
	if _, _, err := f.svc.Build(f.ctx, uuid.Nil, f.in); err != nil {
		t.Fatalf("Build round 2: %v", err)
	}

	rounds, err := f.svc.ListRounds(f.ctx, f.term)
	if err != nil {
		t.Fatalf("ListRounds: %v", err)
	}
	if len(rounds) != 2 {
		t.Fatalf("expected 2 rounds, got %d", len(rounds))
	}
	if rounds[0].RoundNo != 2 || !rounds[0].IsLate {
		t.Errorf("newest round = %d (late=%v), want 2 flagged late", rounds[0].RoundNo, rounds[0].IsLate)
	}
	if rounds[1].RoundNo != 1 || rounds[1].IsLate {
		t.Errorf("first round = %d (late=%v), want 1 on time", rounds[1].RoundNo, rounds[1].IsLate)
	}
	for _, r := range rounds {
		if r.TACount != 1 {
			t.Errorf("round %d recorded %d TAs, want 1", r.RoundNo, r.TACount)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// The sidebar badge and the page's "จะออกคำสั่งให้ N รายชื่อ" read the same
// predicate — this pins that the count moves exactly with the preview list:
// grows when a request is approved, falls to zero once the round is printed,
// and never counts a request still waiting for a decision.
func TestAppointmentPendingCountMatchesPreview(t *testing.T) {
	f := newApptFixture(t)
	f.addCourseWithTA("CP101", "หนึ่ง", "approved")
	f.addCourseWithTA("CP202", "สอง", "approved")
	f.addCourseWithTA("CP303", "สาม", "submitted") // ยังไม่อนุมัติ ต้องไม่ถูกนับ

	p, err := f.svc.Preview(f.ctx, f.term)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	n, err := f.svc.PendingCount(f.ctx, f.term)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if n != len(p.Include) {
		t.Fatalf("badge says %d but the page would print %d names", n, len(p.Include))
	}
	if n != 2 {
		t.Fatalf("pending = %d, want 2 (the submitted request is not appointable)", n)
	}

	if _, _, err := f.svc.Build(f.ctx, uuid.Nil, f.in); err != nil {
		t.Fatalf("Build: %v", err)
	}
	n, err = f.svc.PendingCount(f.ctx, f.term)
	if err != nil {
		t.Fatalf("PendingCount after build: %v", err)
	}
	if n != 0 {
		t.Fatalf("badge must clear once the order is printed, got %d", n)
	}
}
