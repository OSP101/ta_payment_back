package service

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/config"
	"ta-payment-back/internal/docxgen"
	"ta-payment-back/internal/mail"
)

// When a round of the คำสั่งแต่งตั้ง is issued, each lecturer hears once, about
// their own courses only, with every TA's name, student id, year and sections.
func TestLecturerAppointNotice_OnePerLecturerWithTheirTAs(t *testing.T) {
	f := newApptFixture(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.svc.pool.Exec(f.ctx, sql, args...); err != nil {
			t.Fatalf("fixture exec: %v\nSQL: %s", err, sql)
		}
	}
	user := func(first, studentID string) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO users (id,email,title,first_name,last_name,student_id,is_active)
		      VALUES ($1,$2,'นาย',$3,'ทดสอบ',NULLIF($4,''),TRUE)`, id, id.String()+"@example.test", first, studentID)
		return id
	}
	course := func(code string, lec uuid.UUID) (uuid.UUID, uuid.UUID) {
		tc, req := uuid.New(), uuid.New()
		exec(`INSERT INTO teaching_courses (id,term_id,code,name_th,level,credits,lecture_hrs,lab_hrs,self_hrs,num_students)
		      VALUES ($1,$2,$3,'วิชา '||$3,'undergrad',3,2,2,5,40)`, tc, f.term, code)
		exec(`INSERT INTO ta_requests (id,teaching_course_id,lecturer_id,reimburse_scope,status,submitted_at)
		      VALUES ($1,$2,$3,'both','approved',NOW())`, req, tc, lec)
		return tc, req
	}
	section := func(tc uuid.UUID, no, track string) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO sections (id,teaching_course_id,sec_no,track) VALUES ($1,$2,$3,$4::section_track)`, id, tc, no, track)
		return id
	}
	assign := func(req, sec, ta uuid.UUID, level, sid, state string) {
		exec(`INSERT INTO ta_request_assignments (id,request_id,section_id,ta_id,level,student_id_snapshot,state)
		      VALUES (gen_random_uuid(),$1,$2,$3,$4::study_level,$5,$6::ta_assignment_state)`, req, sec, ta, level, sid, state)
	}

	lecA, lecB := user("อาจารย์เอ", ""), user("อาจารย์บี", "")
	ug, grad := user("ตรี", "663380555-8"), user("โท", "")

	c1, r1 := course("CP101", lecA)
	s01, s02, s03 := section(c1, "01", "regular"), section(c1, "02", "special"), section(c1, "03", "regular")
	assign(r1, s01, ug, "undergrad", "663380555-8", "active")
	assign(r1, s02, ug, "undergrad", "663380555-8", "active")
	assign(r1, s03, ug, "undergrad", "663380555-8", "dropped") // clash: not appointed there
	assign(r1, s01, grad, "master", "675020001-1", "active")

	c2, r2 := course("CP202", lecA)
	assign(r2, section(c2, "01", "regular"), ug, "undergrad", "663380555-8", "active")

	c3, r3 := course("CP303", lecB)
	assign(r3, section(c3, "01", "regular"), grad, "master", "675020001-1", "active")

	// CP303 is not on this round: lecturer B must hear nothing.
	pairs := []AppointmentCandidate{
		{TeachingCourseID: c1, TAID: ug}, {TeachingCourseID: c1, TAID: grad},
		{TeachingCourseID: c2, TAID: ug},
	}
	_ = c3

	f.svc.notify = &NotifyService{pool: f.svc.pool, mailer: mail.New(config.Config{})}
	doc := docxgen.AppointmentOrderData{OrderNo: "6/2569", AcademicYear: "2569", SemesterLabel: "ภาคต้น",
		OrderDate: "29 กรกฎาคม พ.ศ. 2569", EffectiveDate: "1 สิงหาคม 2569"}
	f.svc.notifyLecturersAppointed(f.ctx, f.term, 1, pairs, doc, []byte("docx"), "appointment-order-6-2569.docx")

	type row struct{ channel, title, body string }
	read := func(u uuid.UUID) []row {
		rows, err := f.svc.pool.Query(f.ctx, `SELECT channel::text, title, body FROM notifications WHERE user_id = $1 ORDER BY channel`, u)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.channel, &r.title, &r.body); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}

	if got := read(lecB); len(got) != 0 {
		t.Errorf("lecturer B has no course on the round, got %+v", got)
	}
	got := read(lecA)
	if len(got) != 2 { // one e-mail record, one bell row
		t.Fatalf("lecturer A notices = %+v, want one e-mail and one in-app", got)
	}
	n := got[1]
	if n.channel != "in_app" {
		n = got[0]
	}
	if !strings.Contains(n.title, "6/2569") || !strings.Contains(n.title, "CP101") || !strings.Contains(n.title, "CP202") {
		t.Errorf("title = %q, want order number and both codes", n.title)
	}
	for _, want := range []string{
		"นายตรี ทดสอบ รหัส 663380555-8 ป.ตรี ปี 4 กลุ่มเรียนที่ 01, 02 (ภาคพิเศษ)",
		"นายโท ทดสอบ รหัส 675020001-1 ป.โท กลุ่มเรียนที่ 01",
	} {
		if !strings.Contains(n.body, want) {
			t.Errorf("body lacks %q:\n%s", want, n.body)
		}
	}
	if strings.Contains(n.body, "03") {
		t.Errorf("dropped section 03 listed:\n%s", n.body)
	}
}

// A late round says it is additional and that earlier names are not repeated.
func TestLecturerAppointContent_LateRound(t *testing.T) {
	doc := docxgen.AppointmentOrderData{OrderNo: "9/2569", SemesterLabel: "ภาคต้น", AcademicYear: "2569"}
	courses := []appointedCourse{{label: "CP101 วิชา", code: "CP101",
		tas: []appointedTA{{name: "นายก ข", studentID: "1", level: "undergrad", studyYear: 2, sections: []string{"01"}}}}}

	_, _, onTime := lecturerAppointContent(doc, 1, courses)
	if strings.Contains(onTime.Intro, "เพิ่มเติม") || strings.Contains(onTime.After, "ฉบับก่อนหน้า") {
		t.Errorf("round 1 reads as late: %q / %q", onTime.Intro, onTime.After)
	}
	_, _, late := lecturerAppointContent(doc, 2, courses)
	if !strings.Contains(late.Intro, "คำสั่งเพิ่มเติม") || !strings.Contains(late.After, "ฉบับก่อนหน้า") {
		t.Errorf("round 2 does not say it is additional: %q / %q", late.Intro, late.After)
	}
	if len(late.Tables) != 1 || late.Tables[0].Rows[0][2] != "ป.ตรี ปี 2" {
		t.Errorf("tables = %+v", late.Tables)
	}
}

// Build itself sends the notice, in the background, once per round; a reprint
// of that round sends nothing.
func TestBuild_NotifiesLecturerOnceAndReprintDoesNot(t *testing.T) {
	f := newApptFixture(t)
	f.svc.notify = &NotifyService{pool: f.svc.pool, mailer: mail.New(config.Config{})}
	tc, _ := f.addCourseWithTA("CP777", "หนึ่ง", "approved")
	var lec uuid.UUID
	if err := f.svc.pool.QueryRow(f.ctx, `SELECT lecturer_id FROM ta_requests WHERE teaching_course_id = $1`, tc).Scan(&lec); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var n int
		_ = f.svc.pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM notifications WHERE user_id = $1 AND channel = 'email' AND title LIKE 'ออกคำสั่งแต่งตั้ง%CP777%'`, lec).Scan(&n)
		return n
	}
	waitFor := func(want int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for count() != want && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if got := count(); got != want {
			t.Fatalf("lecturer e-mails = %d, want %d", got, want)
		}
	}

	if _, _, err := f.svc.Build(f.ctx, uuid.Nil, f.in); err != nil {
		t.Fatalf("Build: %v", err)
	}
	waitFor(1)

	var orderID uuid.UUID
	if err := f.svc.pool.QueryRow(f.ctx, `SELECT id FROM appointment_orders WHERE term_id = $1`, f.term).Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Reprint(f.ctx, uuid.Nil, orderID); err != nil {
		t.Fatalf("Reprint: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	waitFor(1)
}
