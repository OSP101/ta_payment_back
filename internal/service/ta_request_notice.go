package service

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/timeutil"
)

// ta_request_notice.go mails lecturers about a TA request window: once when it
// opens, and once more a few days before it closes.
//
// # WHY A SWEEP AND NOT "SEND ON SAVE"
//
// Staff often open the window before the course data is complete — the
// registrar import matches lecturers by first name and leaves some courses with
// nobody attached, and TDBM or a manual fix fills them in days later. A mail
// sent on save would tell a lecturer "requests are open" while their home page
// shows none of their courses. So the rule is: a lecturer is mailed only once
// they are attached to at least one course in the window's term, and the
// hourly sweep picks up everyone who becomes eligible later. A window whose
// opens_at is in the future is likewise mailed when it actually opens.
//
// The ta_window_notices primary key is the de-duplication: a row is claimed
// before the mail goes out, so the hourly tick, the post-save sweep and the
// post-bind sweep can overlap without anyone getting a second copy.

// closingNoticeLead is how long before closes_at the "deadline near" mail goes.
const closingNoticeLead = 3 * 24 * time.Hour

// closingAfterOpenGap keeps the two mails from arriving back to back: a
// lecturer who was only just told the window is open (a short window, or a
// course attached late) does not need "the deadline is near" an hour later.
const closingAfterOpenGap = 48 * time.Hour

// windowNoticeFooter ends every notice. Staff asked for it to say plainly
// that this is automatic and that no action is needed from a lecturer who
// does not want a TA, since most courses do not request one.
const windowNoticeFooter = "ข้อความนี้เป็นการแจ้งเตือนอัตโนมัติจากระบบ ขออภัยหากเป็นการรบกวน " +
	"หากท่านไม่ประสงค์ขอผู้ช่วยสอนสำหรับรายวิชาใด ไม่ต้องดำเนินการใด ๆ"

// noticeWindow is a window currently eligible for notices.
type noticeWindow struct {
	id       uuid.UUID
	termID   uuid.UUID
	closesAt time.Time
	term     string // "ภาคการศึกษาที่ 1 ปีการศึกษา 2569"
}

// noticeCourse is one of a lecturer's courses as the mail lists it.
type noticeCourse struct {
	code, name string
	sections   int
	requested  bool
}

// SweepWindowNotices sends every open and closing notice that is due. Safe to
// call at any time and as often as wanted. Returns the number of mails sent.
func (s *TARequestService) SweepWindowNotices(ctx context.Context) (int, error) {
	return s.sweepWindowNotices(ctx, nil)
}

// SweepWindowNoticesFor is the sweep narrowed to some lecturers — used right
// after staff attach lecturers to a course, so the mail goes now rather than at
// the next hourly tick without re-scanning everyone else.
func (s *TARequestService) SweepWindowNoticesFor(ctx context.Context, lecturerIDs []uuid.UUID) (int, error) {
	if len(lecturerIDs) == 0 {
		return 0, nil
	}
	return s.sweepWindowNotices(ctx, lecturerIDs)
}

// PendingOpenNotice reports whether the sweep would still send this lecturer
// an "open" notice for a course in the given term. TeachingService uses it to
// skip its own "you were added to course X" mail when the open notice — which
// lists every course anyway — is about to go out.
func (s *TARequestService) PendingOpenNotice(ctx context.Context, termID, lecturerID uuid.UUID) bool {
	var pending bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM (`+noticeWindowsSQL+`) w
			 WHERE w.term_id = $1 AND `+noticeWindowLive+`
			   AND NOT EXISTS (SELECT 1 FROM ta_window_notices n
			                    WHERE n.window_id = w.id AND n.lecturer_id = $2 AND n.kind = 'open'))`,
		termID, lecturerID).Scan(&pending)
	return err == nil && pending
}

// noticeWindowsSQL is the latest-closing window of each term — the one
// CreateRequest measures lateness against, so the only one notices speak for.
const noticeWindowsSQL = `
	SELECT DISTINCT ON (w.term_id) w.id, w.term_id, w.closes_at, w.is_open, w.notify_lecturers, w.opens_at
	  FROM ta_request_windows w
	 ORDER BY w.term_id, w.closes_at DESC`

// noticeWindowLive filters noticeWindowsSQL (aliased w) to windows that are
// switched on and currently between opens_at and closes_at.
const noticeWindowLive = `w.is_open AND w.notify_lecturers AND w.opens_at <= NOW() AND w.closes_at > NOW()`

func (s *TARequestService) sweepWindowNotices(ctx context.Context, only []uuid.UUID) (int, error) {
	if s.notify == nil {
		return 0, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT w.id, w.term_id, w.closes_at,
		       'ภาคการศึกษาที่ ' || t.semester::text || ' ปีการศึกษา ' || t.academic_year::text
		  FROM (`+noticeWindowsSQL+`) w
		  JOIN academic_terms t ON t.id = w.term_id
		 WHERE `+noticeWindowLive)
	if err != nil {
		return 0, err
	}
	var windows []noticeWindow
	for rows.Next() {
		var w noticeWindow
		if err := rows.Scan(&w.id, &w.termID, &w.closesAt, &w.term); err != nil {
			rows.Close()
			return 0, err
		}
		windows = append(windows, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	sent := 0
	for _, w := range windows {
		n, err := s.sendNotices(ctx, w, "open", only)
		sent += n
		if err != nil {
			return sent, err
		}
		if time.Until(w.closesAt) <= closingNoticeLead {
			n, err := s.sendNotices(ctx, w, "closing", only)
			sent += n
			if err != nil {
				return sent, err
			}
		}
	}
	return sent, nil
}

// sendNotices mails every lecturer who is due a notice of this kind for w.
func (s *TARequestService) sendNotices(ctx context.Context, w noticeWindow, kind string, only []uuid.UUID) (int, error) {
	// A course counts as requested once a request for it is in flight or
	// approved — by ANY of its lecturers, since one request covers a co-taught
	// course. Drafts, rejections and cancellations do not count.
	q := `
		SELECT tl.lecturer_id, tc.code, tc.name_th,
		       (SELECT COUNT(*) FROM sections sc WHERE sc.teaching_course_id = tc.id),
		       EXISTS (SELECT 1 FROM ta_requests r
		                WHERE r.teaching_course_id = tc.id AND r.status IN ('submitted', 'approved'))
		  FROM teaching_lecturers tl
		  JOIN teaching_courses tc ON tc.id = tl.teaching_course_id AND tc.term_id = $1
		  JOIN users u ON u.id = tl.lecturer_id AND u.is_active AND u.deleted_at IS NULL
		 WHERE NOT EXISTS (SELECT 1 FROM ta_window_notices n
		                    WHERE n.window_id = $2 AND n.lecturer_id = tl.lecturer_id AND n.kind = $3)
		   AND ($4::uuid[] IS NULL OR tl.lecturer_id = ANY($4))`
	args := []any{w.termID, w.id, kind, only}
	if kind == "closing" {
		q += `
		   AND NOT EXISTS (SELECT 1 FROM ta_window_notices n
		                    WHERE n.window_id = $2 AND n.lecturer_id = tl.lecturer_id AND n.kind = 'open'
		                      AND n.sent_at > NOW() - $5::interval)`
		args = append(args, fmt.Sprintf("%d seconds", int(closingAfterOpenGap.Seconds())))
	}
	q += ` ORDER BY tl.lecturer_id, tc.code`

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	byLecturer := map[uuid.UUID][]noticeCourse{}
	var order []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		var c noticeCourse
		if err := rows.Scan(&id, &c.code, &c.name, &c.sections, &c.requested); err != nil {
			rows.Close()
			return 0, err
		}
		if _, ok := byLecturer[id]; !ok {
			order = append(order, id)
		}
		byLecturer[id] = append(byLecturer[id], c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	sent := 0
	for _, id := range order {
		courses := byLecturer[id]
		var title, body string
		var layout MailLayout
		if kind == "open" {
			title, body = openNoticeText(w, courses)
			layout = openNoticeLayout(w, courses)
		} else {
			// Only the courses still without a request; nothing left means
			// nothing to remind about.
			var pending []noticeCourse
			for _, c := range courses {
				if !c.requested {
					pending = append(pending, c)
				}
			}
			if len(pending) == 0 {
				continue
			}
			title, body = closingNoticeText(w, pending)
			layout = closingNoticeLayout(w, pending)
		}
		// Claim first, then send: a lost race means someone else is sending.
		tag, err := s.pool.Exec(ctx, `
			INSERT INTO ta_window_notices (window_id, lecturer_id, kind)
			VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, w.id, id, kind)
		if err != nil {
			return sent, err
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		s.notify.SendLaidOut(ctx, id, title, body, "/lecturer", kind == "closing", layout)
		sent++
	}
	if sent > 0 {
		log.Printf("ta_window_notice: window=%s kind=%s sent=%d", w.id, kind, sent)
	}
	return sent, nil
}

func openNoticeText(w noticeWindow, courses []noticeCourse) (string, string) {
	title := "เปิดรับคำขอผู้ช่วยสอน " + w.term
	var b strings.Builder
	b.WriteString("ด้วยวิทยาลัยการคอมพิวเตอร์ได้เปิดรับคำขอผู้ช่วยสอนสำหรับ" + w.term +
		" แล้ว โดยกำหนดให้ยื่นคำขอภายใน" + thaiDeadline(w.closesAt) + "\n\n")
	fmt.Fprintf(&b, "ในภาคการศึกษานี้ ท่านเป็นอาจารย์ผู้สอน จำนวน %d รายวิชา ดังนี้\n", len(courses))
	b.WriteString(noticeCourseList(courses) + "\n\n")
	b.WriteString("ทั้งนี้ หากยื่นคำขอภายหลังกำหนด ระบบยังคงรับคำขอ แต่การเบิกจ่ายค่าตอบแทนจะล่าช้ากว่ากำหนด " +
		"และหากรายวิชาข้างต้นไม่ครบถ้วนหรือไม่ถูกต้อง กรุณาแจ้งเจ้าหน้าที่\n\n")
	b.WriteString(windowNoticeFooter)
	return title, b.String()
}

func closingNoticeText(w noticeWindow, pending []noticeCourse) (string, string) {
	title := "ใกล้ครบกำหนดยื่นคำขอผู้ช่วยสอน " + w.term
	var b strings.Builder
	b.WriteString("ตามที่วิทยาลัยการคอมพิวเตอร์ได้เปิดรับคำขอผู้ช่วยสอนสำหรับ" + w.term +
		" นั้น จะครบกำหนดยื่นคำขอใน" + thaiDeadline(w.closesAt) + "\n\n")
	fmt.Fprintf(&b, "ขณะนี้ยังไม่มีคำขอผู้ช่วยสอนสำหรับรายวิชาของท่าน จำนวน %d รายวิชา ดังนี้\n", len(pending))
	b.WriteString(noticeCourseList(pending) + "\n\n")
	b.WriteString("ทั้งนี้ หากยื่นคำขอภายหลังกำหนด ระบบยังคงรับคำขอ แต่การเบิกจ่ายค่าตอบแทนจะล่าช้ากว่ากำหนด\n\n")
	b.WriteString(windowNoticeFooter)
	return title, b.String()
}

// openNoticeLayout is the e-mail form of the open notice: the deadline in the
// highlight box and the courses as a table, where the bell gets them as text.
func openNoticeLayout(w noticeWindow, courses []noticeCourse) MailLayout {
	return MailLayout{
		Intro: "ด้วยวิทยาลัยการคอมพิวเตอร์ได้เปิดรับคำขอผู้ช่วยสอนสำหรับ" + w.term +
			" แล้ว จึงขอแจ้งรายละเอียดและรายวิชาที่ท่านเป็นอาจารย์ผู้สอนในภาคการศึกษานี้ ดังนี้",
		Facts: []MailFact{
			{"ภาคการศึกษา", w.term},
			{"จำนวนรายวิชาของท่าน", fmt.Sprintf("%d รายวิชา", len(courses))},
		},
		Highlight: &MailFact{"กำหนดยื่นคำขอผู้ช่วยสอนภายใน", thaiDeadline(w.closesAt)},
		Table:     noticeCourseTable("รายวิชาของท่านในภาคการศึกษานี้", courses),
		After: "หากยื่นคำขอภายหลังกำหนด ระบบยังคงรับคำขอ แต่การเบิกจ่ายค่าตอบแทนจะล่าช้ากว่ากำหนด " +
			"และหากรายวิชาข้างต้นไม่ครบถ้วนหรือไม่ถูกต้อง กรุณาแจ้งเจ้าหน้าที่\n\n" + windowNoticeFooter,
		ButtonLabel: "ยื่นคำขอผู้ช่วยสอน",
	}
}

func closingNoticeLayout(w noticeWindow, pending []noticeCourse) MailLayout {
	return MailLayout{
		Intro: "ตามที่วิทยาลัยการคอมพิวเตอร์ได้เปิดรับคำขอผู้ช่วยสอนสำหรับ" + w.term +
			" นั้น ขณะนี้ใกล้ครบกำหนดยื่นคำขอแล้ว และยังไม่มีคำขอผู้ช่วยสอนสำหรับรายวิชาของท่านตามรายการด้านล่าง",
		Highlight: &MailFact{"ครบกำหนดยื่นคำขอผู้ช่วยสอนใน", thaiDeadline(w.closesAt)},
		Table:     noticeCourseTable("รายวิชาที่ยังไม่มีคำขอผู้ช่วยสอน", pending),
		After: "หากยื่นคำขอภายหลังกำหนด ระบบยังคงรับคำขอ แต่การเบิกจ่ายค่าตอบแทนจะล่าช้ากว่ากำหนด\n\n" +
			windowNoticeFooter,
		ButtonLabel: "ยื่นคำขอผู้ช่วยสอน",
	}
}

func noticeCourseTable(title string, courses []noticeCourse) *MailTable {
	t := &MailTable{
		Title: title,
		Head:  []string{"รหัสวิชา", "ชื่อรายวิชา", "กลุ่มเรียน", "สถานะ"},
		Align: []string{"left", "left", "center", "left"},
	}
	for _, c := range courses {
		status := "ยังไม่ยื่นคำขอ"
		if c.requested {
			status = "ยื่นคำขอแล้ว"
		}
		t.Rows = append(t.Rows, []string{c.code, c.name, fmt.Sprintf("%d", c.sections), status})
	}
	return t
}

func noticeCourseList(courses []noticeCourse) string {
	items := make([]string, 0, len(courses))
	for _, c := range courses {
		it := c.code + " " + c.name
		if c.sections > 0 {
			it += fmt.Sprintf(" จำนวน %d กลุ่มเรียน", c.sections)
		}
		if c.requested {
			it += " (ยื่นคำขอแล้ว)"
		}
		items = append(items, it)
	}
	return numberedLines(items)
}

// thaiDeadline renders closes_at as "วันที่ 23 ตุลาคม 2569 เวลา 16.30 น." in
// Bangkok time.
func thaiDeadline(t time.Time) string {
	d := t.In(timeutil.Bangkok)
	return fmt.Sprintf("วันที่ %d %s %d เวลา %02d.%02d น.",
		d.Day(), thaiMonthNames[d.Month()], d.Year()+543, d.Hour(), d.Minute())
}

// WindowReadiness is what staff see before (and while) a window mails
// lecturers: who will get the mail, and the gaps that would leave a lecturer
// opening the site to find their course missing.
type WindowReadiness struct {
	CoursesTotal int `json:"courses_total"`
	// LecturersReady have at least one course in the term — the mail's audience.
	LecturersReady int `json:"lecturers_ready"`
	// CoursesWithoutLecturer is the usual culprit: a registrar row whose
	// officer names did not match an account. Nobody is told about these.
	CoursesWithoutLecturer []ReadinessCourse `json:"courses_without_lecturer"`
	// LecturersWithoutCourse are held back until a course is attached. Often
	// legitimate (on leave, not teaching this term), so informational only.
	LecturersWithoutCourse []ReadinessLecturer `json:"lecturers_without_course"`
}

type ReadinessCourse struct {
	ID     uuid.UUID `json:"id"`
	Code   string    `json:"code"`
	NameTH string    `json:"name_th"`
}

type ReadinessLecturer struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Email string    `json:"email"`
}

func (s *TARequestService) WindowReadiness(ctx context.Context, termID uuid.UUID) (*WindowReadiness, error) {
	out := &WindowReadiness{
		CoursesWithoutLecturer: []ReadinessCourse{},
		LecturersWithoutCourse: []ReadinessLecturer{},
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT (SELECT COUNT(*) FROM teaching_courses WHERE term_id = $1),
		       (SELECT COUNT(DISTINCT tl.lecturer_id)
		          FROM teaching_lecturers tl
		          JOIN teaching_courses tc ON tc.id = tl.teaching_course_id AND tc.term_id = $1
		          JOIN users u ON u.id = tl.lecturer_id AND u.is_active AND u.deleted_at IS NULL)`,
		termID).Scan(&out.CoursesTotal, &out.LecturersReady); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT tc.id, tc.code, tc.name_th
		  FROM teaching_courses tc
		 WHERE tc.term_id = $1
		   AND NOT EXISTS (SELECT 1 FROM teaching_lecturers tl
		                    JOIN users u ON u.id = tl.lecturer_id AND u.is_active AND u.deleted_at IS NULL
		                   WHERE tl.teaching_course_id = tc.id)
		 ORDER BY tc.code`, termID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c ReadinessCourse
		if err := rows.Scan(&c.ID, &c.Code, &c.NameTH); err != nil {
			rows.Close()
			return nil, err
		}
		out.CoursesWithoutLecturer = append(out.CoursesWithoutLecturer, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.pool.Query(ctx, `
		SELECT u.id, TRIM(COALESCE(u.title, '') || ' ' || u.first_name || ' ' || u.last_name), u.email
		  FROM users u
		 WHERE u.is_active AND u.deleted_at IS NULL
		   AND EXISTS (SELECT 1 FROM user_roles ur WHERE ur.user_id = u.id AND ur.role = 'lecturer')
		   AND NOT EXISTS (SELECT 1 FROM teaching_lecturers tl
		                    JOIN teaching_courses tc ON tc.id = tl.teaching_course_id AND tc.term_id = $1
		                   WHERE tl.lecturer_id = u.id)
		 ORDER BY u.first_name, u.last_name`, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var l ReadinessLecturer
		if err := rows.Scan(&l.ID, &l.Name, &l.Email); err != nil {
			return nil, err
		}
		out.LecturersWithoutCourse = append(out.LecturersWithoutCourse, l)
	}
	return out, rows.Err()
}
