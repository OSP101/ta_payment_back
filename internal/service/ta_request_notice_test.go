package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/testutil"
)

// Staff open the TA-request window, lecturers get one mail listing their
// courses — but only once they actually HAVE a course in the term, because the
// course data often lands after the window opens and a lecturer told
// "requests are open" who then finds nothing on their home page is the
// complaint this feature exists to avoid.

type noticeFixture struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	svc  *TARequestService
	term uuid.UUID
}

func newNoticeFixture(t *testing.T) *noticeFixture {
	t.Helper()
	pool := testutil.NewPool(t)
	f := &noticeFixture{
		t: t, ctx: context.Background(), pool: pool,
		svc: &TARequestService{
			pool: pool, aud: audit.New(pool),
			notify: &NotifyService{pool: pool, mailer: mail.New(config.Config{})},
		},
		term: uuid.New(),
	}
	f.exec(`INSERT INTO academic_terms (id, academic_year, semester, starts_on, ends_on, is_active)
	        VALUES ($1, 2569, 1, '2026-06-01', '2026-10-31', TRUE)`, f.term)
	return f
}

func (f *noticeFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", strings.Fields(sql)[0], err)
	}
}

func (f *noticeFixture) lecturer(name string) uuid.UUID {
	id := uuid.New()
	f.exec(`INSERT INTO users (id, email, first_name, last_name, is_active) VALUES ($1,$2,$3,'ทดสอบ',TRUE)`,
		id, "notice-"+id.String()+"@example.test", name)
	f.exec(`INSERT INTO user_roles (user_id, role) VALUES ($1, 'lecturer')`, id)
	return id
}

func (f *noticeFixture) course(code string, lecturers ...uuid.UUID) uuid.UUID {
	id := uuid.New()
	f.exec(`INSERT INTO teaching_courses (id, term_id, code, name_th, level, credits, lecture_hrs, lab_hrs, num_students)
	        VALUES ($1,$2,$3,$4,'undergrad',3,2,2,0)`, id, f.term, code, "วิชา "+code)
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no) VALUES (gen_random_uuid(), $1, '1')`, id)
	for i, l := range lecturers {
		f.bind(id, l, i == 0)
	}
	return id
}

func (f *noticeFixture) bind(course, lecturer uuid.UUID, primary bool) {
	f.exec(`INSERT INTO teaching_lecturers (teaching_course_id, lecturer_id, is_primary) VALUES ($1,$2,$3)`,
		course, lecturer, primary)
}

func (f *noticeFixture) window(opens, closes time.Time, notify bool) uuid.UUID {
	w, err := f.svc.UpsertWindow(f.ctx, uuid.Nil, Window{
		TermID: f.term, OpensAt: opens, ClosesAt: closes, IsOpen: true, NotifyLecturers: &notify,
	})
	if err != nil {
		f.t.Fatalf("UpsertWindow: %v", err)
	}
	return w.ID
}

func (f *noticeFixture) sweep() int {
	f.t.Helper()
	n, err := f.svc.SweepWindowNotices(f.ctx)
	if err != nil {
		f.t.Fatalf("SweepWindowNotices: %v", err)
	}
	return n
}

// inbox returns the in-app notices a user holds — the same title/body the
// e-mail carries.
func (f *noticeFixture) inbox(user uuid.UUID) []Notification {
	f.t.Helper()
	out, err := f.svc.notify.List(f.ctx, user, 50, false)
	if err != nil {
		f.t.Fatalf("List: %v", err)
	}
	return out
}

func TestWindowNotice_OneMailListingEveryCourse_OnlyForLecturersWithACourse(t *testing.T) {
	f := newNoticeFixture(t)
	busy := f.lecturer("มีสองวิชา")
	idle := f.lecturer("ยังไม่มีวิชา")
	f.course("CP111", busy)
	f.course("CP222", busy)
	f.window(time.Now().Add(-time.Hour), time.Now().Add(30*24*time.Hour), true)

	if n := f.sweep(); n != 1 {
		t.Fatalf("sent %d, want 1 (only the lecturer who has courses)", n)
	}
	got := f.inbox(busy)
	if len(got) != 1 {
		t.Fatalf("busy lecturer has %d notices, want 1 (one mail, not one per course)", len(got))
	}
	for _, want := range []string{"CP111", "CP222", "จำนวน 2 รายวิชา", "แจ้งเตือนอัตโนมัติ", "ขออภัยหากเป็นการรบกวน", "ไม่ประสงค์ขอผู้ช่วยสอน"} {
		if !strings.Contains(got[0].Body, want) {
			t.Errorf("body missing %q:\n%s", want, got[0].Body)
		}
	}
	if !strings.Contains(got[0].Title, "ภาคการศึกษาที่ 1 ปีการศึกษา 2569") {
		t.Errorf("title %q should name the term", got[0].Title)
	}
	if len(f.inbox(idle)) != 0 {
		t.Error("a lecturer with no course in the term must not be told requests are open")
	}

	// Re-running (the hourly tick, or staff editing the window) sends nothing.
	if n := f.sweep(); n != 0 {
		t.Fatalf("second sweep sent %d, want 0", n)
	}
}

func TestWindowNotice_CourseAttachedLaterIsPickedUpBySweep(t *testing.T) {
	f := newNoticeFixture(t)
	late := f.lecturer("วิชามาช้า")
	f.window(time.Now().Add(-time.Hour), time.Now().Add(30*24*time.Hour), true)
	if n := f.sweep(); n != 0 {
		t.Fatalf("sent %d before the lecturer had any course", n)
	}

	f.course("CP333", late)
	if n := f.sweep(); n != 1 {
		t.Fatalf("sent %d after the course arrived, want 1", n)
	}
	if got := f.inbox(late); len(got) != 1 || !strings.Contains(got[0].Body, "CP333") {
		t.Fatalf("late lecturer inbox = %+v", got)
	}
}

func TestWindowNotice_NothingForFutureOrSwitchedOffWindows(t *testing.T) {
	f := newNoticeFixture(t)
	l := f.lecturer("รอเปิด")
	f.course("CP444", l)

	f.window(time.Now().Add(24*time.Hour), time.Now().Add(30*24*time.Hour), true)
	if n := f.sweep(); n != 0 {
		t.Fatalf("sent %d for a window that has not opened yet", n)
	}

	f2 := newNoticeFixture(t)
	l2 := f2.lecturer("ปิดแจ้ง")
	f2.course("CP555", l2)
	f2.window(time.Now().Add(-time.Hour), time.Now().Add(30*24*time.Hour), false)
	if n := f2.sweep(); n != 0 {
		t.Fatalf("sent %d with notify_lecturers off", n)
	}
}

func TestWindowNotice_ClosingReminderListsOnlyCoursesWithoutARequest(t *testing.T) {
	f := newNoticeFixture(t)
	partial := f.lecturer("ส่งไปบางวิชา")
	done := f.lecturer("ส่งครบแล้ว")
	fresh := f.lecturer("เพิ่งได้แจ้งเปิด")
	sent := f.course("CP601", partial)
	f.course("CP602", partial)
	doneCourse := f.course("CP603", done)
	f.course("CP604", fresh)

	w := f.window(time.Now().Add(-10*24*time.Hour), time.Now().Add(2*24*time.Hour), true)
	// partial and done were told the window was open a week ago; fresh an hour ago.
	f.exec(`INSERT INTO ta_window_notices (window_id, lecturer_id, kind, sent_at) VALUES
	        ($1,$2,'open',NOW() - INTERVAL '7 days'),
	        ($1,$3,'open',NOW() - INTERVAL '7 days'),
	        ($1,$4,'open',NOW() - INTERVAL '1 hour')`, w, partial, done, fresh)
	for _, c := range []uuid.UUID{sent, doneCourse} {
		f.exec(`INSERT INTO ta_requests (teaching_course_id, window_id, lecturer_id, reimburse_scope, status)
		        VALUES ($1,$2,(SELECT lecturer_id FROM teaching_lecturers WHERE teaching_course_id=$1),'both','submitted')`, c, w)
	}

	if n := f.sweep(); n != 1 {
		t.Fatalf("sent %d closing reminders, want 1 (only the lecturer with a course still missing)", n)
	}
	got := f.inbox(partial)
	if len(got) != 1 || !strings.Contains(got[0].Title, "ใกล้ครบกำหนด") {
		t.Fatalf("partial inbox = %+v", got)
	}
	if !strings.Contains(got[0].Body, "CP602") || strings.Contains(got[0].Body, "CP601") {
		t.Errorf("reminder should list CP602 only:\n%s", got[0].Body)
	}
	if !strings.Contains(got[0].Body, "แจ้งเตือนอัตโนมัติ") {
		t.Errorf("reminder must say it is automatic:\n%s", got[0].Body)
	}
	if len(f.inbox(done)) != 0 {
		t.Error("a lecturer who already requested every course must not be reminded")
	}
	if len(f.inbox(fresh)) != 0 {
		t.Error("a lecturer told the window opened an hour ago must not get the reminder right after")
	}
	if n := f.sweep(); n != 0 {
		t.Fatalf("second sweep sent %d, want 0", n)
	}
}

func TestWindowNotice_ExistingWindowKeepsNotifyFlagWhenClientOmitsIt(t *testing.T) {
	f := newNoticeFixture(t)
	id := f.window(time.Now(), time.Now().Add(24*time.Hour), true)
	// An older client re-saves the window without knowing about the field.
	out, err := f.svc.UpsertWindow(f.ctx, uuid.Nil, Window{
		ID: id, TermID: f.term, OpensAt: time.Now(), ClosesAt: time.Now().Add(48 * time.Hour), IsOpen: true,
	})
	if err != nil {
		t.Fatalf("UpsertWindow: %v", err)
	}
	if out.NotifyLecturers == nil || !*out.NotifyLecturers {
		t.Fatalf("notify_lecturers = %v, want still true", out.NotifyLecturers)
	}
	// And a brand-new window defaults to on.
	nw, err := f.svc.UpsertWindow(f.ctx, uuid.Nil, Window{
		TermID: f.term, OpensAt: time.Now(), ClosesAt: time.Now().Add(24 * time.Hour), IsOpen: true,
	})
	if err != nil {
		t.Fatalf("UpsertWindow new: %v", err)
	}
	if nw.NotifyLecturers == nil || !*nw.NotifyLecturers {
		t.Fatalf("new window notify_lecturers = %v, want default true", nw.NotifyLecturers)
	}
}

func TestWindowReadiness_ShowsTheGaps(t *testing.T) {
	f := newNoticeFixture(t)
	ready := f.lecturer("พร้อม")
	f.lecturer("ไม่มีวิชา")
	f.course("CP701", ready)
	f.course("CP702") // nobody attached — registrar name did not match

	r, err := f.svc.WindowReadiness(f.ctx, f.term)
	if err != nil {
		t.Fatalf("WindowReadiness: %v", err)
	}
	if r.CoursesTotal != 2 || r.LecturersReady != 1 {
		t.Errorf("courses=%d ready=%d, want 2 and 1", r.CoursesTotal, r.LecturersReady)
	}
	if len(r.CoursesWithoutLecturer) != 1 || r.CoursesWithoutLecturer[0].Code != "CP702" {
		t.Errorf("courses without lecturer = %+v", r.CoursesWithoutLecturer)
	}
	found := false
	for _, l := range r.LecturersWithoutCourse {
		if strings.Contains(l.Name, "ไม่มีวิชา") {
			found = true
		}
		if strings.Contains(l.Name, "พร้อม") {
			t.Errorf("lecturer with a course listed as without: %+v", l)
		}
	}
	if !found {
		t.Errorf("lecturers without course = %+v", r.LecturersWithoutCourse)
	}
}

func TestAbsoluteLink(t *testing.T) {
	cases := []struct{ base, link, want string }{
		{"https://ta.example.ac.th/", "/lecturer", "https://ta.example.ac.th/lecturer"},
		{"https://ta.example.ac.th", "/announcements/x", "https://ta.example.ac.th/announcements/x"},
		{"", "/lecturer", "/lecturer"},
		{"https://ta.example.ac.th", "", ""},
		{"https://ta.example.ac.th", "https://other/x", "https://other/x"},
		{"https://ta.example.ac.th", "//evil.example/x", "//evil.example/x"},
	}
	for _, c := range cases {
		if got := absoluteLink(c.base, c.link); got != c.want {
			t.Errorf("absoluteLink(%q,%q) = %q, want %q", c.base, c.link, got, c.want)
		}
	}
}

// Staff attaching a lecturer while the window is live: the lecturer gets the
// "requests are open" mail listing every course, not that AND a separate
// "you were added to CP801" mail about the same course.
func TestWindowNotice_LecturerAttachedMidWindowGetsOneMail(t *testing.T) {
	f := newNoticeFixture(t)
	staff := uuid.New()
	f.exec(`INSERT INTO users (id, email, first_name, last_name, is_active) VALUES ($1,$2,'จนท','ทดสอบ',TRUE)`,
		staff, "notice-staff-"+staff.String()+"@example.test")
	f.exec(`INSERT INTO user_roles (user_id, role) VALUES ($1, 'staff')`, staff)
	first := f.lecturer("อาจารย์เดิม")
	added := f.lecturer("อาจารย์ใหม่")
	tc := f.course("CP801", first)
	f.window(time.Now().Add(-time.Hour), time.Now().Add(30*24*time.Hour), true)
	f.sweep() // first lecturer's open notice

	teach := &TeachingService{pool: f.pool, aud: audit.New(f.pool), notify: f.svc.notify, requests: f.svc}
	if err := teach.ReplaceLecturers(f.ctx, staff, tc, []uuid.UUID{first, added}, first); err != nil {
		t.Fatalf("ReplaceLecturers: %v", err)
	}
	got := f.inbox(added)
	if len(got) != 1 || !strings.Contains(got[0].Title, "เปิดรับคำขอ") {
		t.Fatalf("added lecturer inbox = %+v, want exactly the open notice", got)
	}
	if n := f.sweep(); n != 0 {
		t.Fatalf("hourly sweep re-sent %d after the bind already mailed", n)
	}
}

// personName reads the TA prefix from the profile before the account title,
// the same order the transfer cover uses.
func TestPersonName_UsesTitleTheSystemHolds(t *testing.T) {
	f := newNoticeFixture(t)
	lect := f.lecturer("สมชาย")
	f.exec(`UPDATE users SET title = 'ผศ. ดร.' WHERE id = $1`, lect)
	if got := personName(f.ctx, f.pool, lect); got != "ผศ. ดร.สมชาย ทดสอบ" {
		t.Errorf("lecturer = %q", got)
	}
	ta := f.lecturer("สมหญิง")
	f.exec(`INSERT INTO ta_profiles (user_id, prefix, status, current_round) VALUES ($1, 'นางสาว', 'pending', 1)`, ta)
	if got := personName(f.ctx, f.pool, ta); got != "นางสาวสมหญิง ทดสอบ" {
		t.Errorf("ta = %q", got)
	}
}
