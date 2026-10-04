package service

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/docxgen"
	"ta-payment-back/internal/mail"
)

// appointment_lecturer_notice.go tells each lecturer that the คำสั่งแต่งตั้ง
// carrying their TAs has been issued (05/10/2026). Lecturers heard about each
// verdict at approval time, but never that the order itself went out, nor saw
// in one place who was appointed to which of their courses.
//
// One notice per lecturer per round, covering only the pairs on that round: a
// late round carries the stragglers, so a lecturer whose course was split over
// two rounds gets two notices, each about its own names. Reprint sends nothing;
// it is a copy, not a new order.
//
// The order travels as the same .docx staff download. A PDF was tried: the
// gopdf renderer has no Thai shaping (tone marks stack on "คำสั่ง", "ที่") and
// wraps lines differently from Word, so it would read as a different document.

const docxContentType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

// appointedTA is one TA on one lecturer's course in an issued round.
type appointedTA struct {
	name, studentID, level string
	studyYear              int
	sections               []string
}

// appointedCourse is one of a lecturer's courses on the round.
type appointedCourse struct {
	label string // "CP362104 Advanced Topics"
	code  string
	tas   []appointedTA
}

// lecturerAppointNotice is what one lecturer is told.
type lecturerAppointNotice struct {
	lecturerID uuid.UUID
	courses    []appointedCourse
}

// notifyLecturersAppointed mails every lecturer with a TA on the round just
// issued. Best-effort like every notice: Build has already handed staff the
// document, and a mail that did not go out must not undo the order.
func (s *AppointmentOrderService) notifyLecturersAppointed(
	ctx context.Context, termID uuid.UUID, round int, pairs []AppointmentCandidate,
	doc docxgen.AppointmentOrderData, file []byte, fileName string,
) {
	notices, err := s.lecturerAppointNotices(ctx, termID, pairs)
	if err != nil {
		log.Printf("appointment order round %d: lecturer notices: %v", round, err)
		return
	}
	att := []mail.Attachment{{Name: fileName, ContentType: docxContentType, Data: file}}
	for _, n := range notices {
		title, body, layout := lecturerAppointContent(doc, round, n.courses)
		layout.Attachments = att
		s.notify.SendLaidOut(ctx, n.lecturerID, title, body, "/lecturer", false, layout)
	}
}

// lecturerAppointNotices groups the round's (course × TA) pairs by the
// lecturer whose approved request put the TA there. Sections come from that
// lecturer's own requests, so two lecturers sharing a course each see the
// sections they asked for.
func (s *AppointmentOrderService) lecturerAppointNotices(ctx context.Context, termID uuid.UUID, pairs []AppointmentCandidate) ([]lecturerAppointNotice, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	courseIDs := make([]uuid.UUID, len(pairs))
	taIDs := make([]uuid.UUID, len(pairs))
	for i, p := range pairs {
		courseIDs[i], taIDs[i] = p.TeachingCourseID, p.TAID
	}
	var termYear int
	if err := s.pool.QueryRow(ctx,
		`SELECT academic_year FROM academic_terms WHERE id = $1`, termID).Scan(&termYear); err != nil {
		return nil, err
	}

	secLabel := `(` + PrintSecNoSQL("sec") + `) || CASE WHEN sec.track = 'special' THEN ' (ภาคพิเศษ)' ELSE '' END`
	rows, err := s.pool.Query(ctx, `
		SELECT r.lecturer_id,
		       `+CourseCodesSQL("tc")+`,
		       COALESCE(NULLIF(tc.name_th, ''), tc.name_en, ''),
		       TRIM(COALESCE(NULLIF(tp.prefix, ''), NULLIF(u.title, ''), '') || u.first_name || ' ' || u.last_name),
		       COALESCE(MAX(a.student_id_snapshot), u.student_id, ''),
		       MAX(a.level::text),
		       COALESCE(u.study_year, 0),
		       ARRAY_AGG(DISTINCT `+secLabel+` ORDER BY `+secLabel+`)
		  FROM ta_request_assignments a
		  JOIN ta_requests r       ON r.id = a.request_id AND r.status = 'approved'
		  JOIN users lu            ON lu.id = r.lecturer_id AND lu.is_active
		  JOIN sections sec        ON sec.id = a.section_id
		  JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		  JOIN users u             ON u.id = a.ta_id
		  LEFT JOIN ta_profiles tp ON tp.user_id = u.id
		 WHERE tc.term_id = $1
		   AND a.state <> 'dropped'
		   AND (tc.id, a.ta_id) IN (SELECT * FROM UNNEST($2::uuid[], $3::uuid[]))
		 GROUP BY r.lecturer_id, tc.id, tc.code, tc.alt_codes, tc.name_th, tc.name_en,
		          u.id, tp.prefix, u.title, u.first_name, u.last_name, u.student_id, u.study_year
		 ORDER BY r.lecturer_id, tc.code, u.first_name, u.last_name`,
		termID, courseIDs, taIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []lecturerAppointNotice
	for rows.Next() {
		var lecID uuid.UUID
		var code, name string
		var t appointedTA
		var storedYear int
		if err := rows.Scan(&lecID, &code, &name, &t.name, &t.studentID, &t.level, &storedYear, &t.sections); err != nil {
			return nil, err
		}
		t.studyYear = storedYear
		if t.level == "undergrad" {
			// Year at the order's term, not today's: the order is about that term.
			if y := deriveStudyYear(t.studentID, termYear); y > 0 {
				t.studyYear = y
			}
		}
		if len(out) == 0 || out[len(out)-1].lecturerID != lecID {
			out = append(out, lecturerAppointNotice{lecturerID: lecID})
		}
		n := &out[len(out)-1]
		if len(n.courses) == 0 || n.courses[len(n.courses)-1].code != code {
			n.courses = append(n.courses, appointedCourse{
				label: strings.TrimSpace(code + " " + name), code: code,
			})
		}
		c := &n.courses[len(n.courses)-1]
		c.tas = append(c.tas, t)
	}
	return out, rows.Err()
}

// levelYearTH is the TA's level and, for undergraduates, year: "ป.ตรี ปี 3".
func (t appointedTA) levelYearTH() string {
	label := studyLevelTH(t.level)
	if t.level == "undergrad" && t.studyYear > 0 {
		label += " ปี " + strconv.Itoa(t.studyYear)
	}
	return label
}

// lecturerAppointContent builds one lecturer's notice: the in-app body (plain
// lines) and the e-mail with a table per course.
func lecturerAppointContent(doc docxgen.AppointmentOrderData, round int, courses []appointedCourse) (string, string, MailLayout) {
	codes := make([]string, len(courses))
	// รายชื่อ, not คน: one TA on two courses is two appointments on the order,
	// the same unit the appointments page counts in.
	taCount := 0
	for i, c := range courses {
		codes[i] = c.code
		taCount += len(c.tas)
	}
	// Order number and codes in the title: unread notices with the same
	// title and link fold into one, and two rounds must stay two notices.
	title := "ออกคำสั่งแต่งตั้งผู้ช่วยสอนแล้ว ที่ " + doc.OrderNo + " " + strings.Join(codes, " ")

	kind := "คำสั่ง"
	if round > 1 {
		kind = "คำสั่งเพิ่มเติม"
	}
	intro := fmt.Sprintf("ตามที่ท่านได้ยื่นคำขอผู้ช่วยสอน ประจำ%s ปีการศึกษา %s นั้น "+
		"บัดนี้วิทยาลัยการคอมพิวเตอร์ได้ออก%sแต่งตั้งผู้ช่วยสอนในรายวิชาของท่านเรียบร้อยแล้ว "+
		"รายละเอียดดังนี้", doc.SemesterLabel, doc.AcademicYear, kind)

	layout := MailLayout{
		Intro: intro,
		Facts: []MailFact{
			{"คำสั่งวิทยาลัยการคอมพิวเตอร์ที่", doc.OrderNo},
			{"สั่ง ณ วันที่", doc.OrderDate},
			{"มีผลตั้งแต่วันที่", doc.EffectiveDate},
			{"จำนวน", fmt.Sprintf("%d รายวิชา %d รายชื่อ", len(courses), taCount)},
		},
		ButtonLabel: "เข้าสู่ระบบ",
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("วิทยาลัยการคอมพิวเตอร์ได้ออก%sที่ %s ลงวันที่ %s แต่งตั้งผู้ช่วยสอนในรายวิชาของท่าน มีผลตั้งแต่วันที่ %s",
		kind, doc.OrderNo, doc.OrderDate, doc.EffectiveDate))
	for _, c := range courses {
		t := MailTable{
			Title: c.label,
			Head:  []string{"ชื่อ-สกุล", "รหัสนักศึกษา", "ระดับ/ชั้นปี", "กลุ่มเรียน"},
		}
		lines := make([]string, 0, len(c.tas))
		for _, ta := range c.tas {
			secs := strings.Join(ta.sections, ", ")
			t.Rows = append(t.Rows, []string{ta.name, ta.studentID, ta.levelYearTH(), secs})
			lines = append(lines, fmt.Sprintf("%s รหัส %s %s กลุ่มเรียนที่ %s", ta.name, ta.studentID, ta.levelYearTH(), secs))
		}
		layout.Tables = append(layout.Tables, t)
		b.WriteString("\n\n" + c.label + "\n" + numberedLines(lines))
	}

	after := "ได้แนบสำเนาคำสั่งแต่งตั้ง (ไฟล์ Word) มาพร้อมอีเมลฉบับนี้"
	if round > 1 {
		after += " รายชื่อข้างต้นเป็นเฉพาะผู้ช่วยสอนที่อยู่ในคำสั่งฉบับนี้ ผู้ที่ได้รับแต่งตั้งในคำสั่งฉบับก่อนหน้าไม่ได้แสดงซ้ำ"
	}
	layout.After = after
	b.WriteString("\n\nสำเนาคำสั่งแต่งตั้งได้ส่งไปทางอีเมลของท่านแล้ว")
	return title, b.String(), layout
}

// lecturerNoticeTimeout bounds the background send. SMTP to the campus relay
// can stall; the goroutine must not outlive the process's patience forever.
const lecturerNoticeTimeout = 10 * time.Minute
