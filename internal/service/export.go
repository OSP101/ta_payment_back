package service

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"

	"ta-payment-back/internal/storage"
)

type ExportService struct {
	pool  *pgxpool.Pool
	store storage.Store
}

// BuildCourseZip builds a zip archive containing per-TA .xlsx files for a course.
// Each xlsx has three sheets:
//   Sheet 1: "หน้าปะ" (coversheet) — reimbursement summary
//   Sheet 2: "บันทึกเวลา" (work log)
//   Sheet 3: "ตารางสอน+งาน" (schedule)
func (s *ExportService) BuildCourseZip(ctx context.Context, teachingCourseID uuid.UUID) ([]byte, string, error) {
	type row struct {
		taID       uuid.UUID
		fullName   string
		email      string
		nationalID string
		bankAcct   string
		track      string
		level      string
		hoursTotal float64
		payBaht    float64
	}
	var courseCode, courseName string
	if err := s.pool.QueryRow(ctx, `
		SELECT fc.code, fc.name_th FROM teaching_courses tc
		JOIN faculty_courses fc ON fc.id = tc.faculty_course_id
		WHERE tc.id=$1`, teachingCourseID).Scan(&courseCode, &courseName); err != nil {
		return nil, "", err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT a.ta_id, u.first_name||' '||u.last_name, u.email,
		       COALESCE(p.national_id,''), COALESCE(p.bank_name,'')||' '||COALESCE(p.account_no,''),
		       sec.track::text, a.level::text,
		       COALESCE(SUM(wl.hours) FILTER (WHERE wl.status='approved'), 0)
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status='approved'
		JOIN users u ON u.id = a.ta_id
		LEFT JOIN ta_profiles p ON p.user_id = a.ta_id
		JOIN sections sec ON sec.id = a.section_id
		LEFT JOIN work_logs wl ON wl.assignment_id = a.id
		WHERE r.teaching_course_id = $1
		GROUP BY a.ta_id, u.first_name, u.last_name, u.email, p.national_id, p.bank_name, p.account_no, sec.track, a.level
		ORDER BY u.first_name`, teachingCourseID)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var pr PayRate
	_ = s.pool.QueryRow(ctx,
		`SELECT undergrad_regular, undergrad_special, graduate_regular, graduate_special_lumpsum, term_months
		 FROM pay_rates ORDER BY effective_from DESC LIMIT 1`).Scan(
		&pr.UndergradRegular, &pr.UndergradSpecial, &pr.GraduateRegular, &pr.GraduateSpecialLumpsum, &pr.TermMonths)
	// Prefer per-term months (source of truth); fall back to pay_rates.term_months.
	var perTermMonths int
	_ = s.pool.QueryRow(ctx, `
		SELECT t.months FROM academic_terms t
		JOIN teaching_courses tc ON tc.term_id = t.id WHERE tc.id = $1`, teachingCourseID).Scan(&perTermMonths)
	if perTermMonths > 0 {
		pr.TermMonths = perTermMonths
	}

	var records []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.taID, &r.fullName, &r.email, &r.nationalID, &r.bankAcct, &r.track, &r.level, &r.hoursTotal); err != nil {
			return nil, "", err
		}
		// Undergrad: hourly × recorded hours.
		// Graduate: flat monthly lump-sum × term months (recorded hours are informational only).
		termMonths := pr.TermMonths
		if termMonths == 0 { termMonths = 4 }
		switch {
		case r.level == "undergrad" && r.track == "regular":
			r.payBaht = pr.UndergradRegular * r.hoursTotal
		case r.level == "undergrad" && r.track == "special":
			r.payBaht = pr.UndergradSpecial * r.hoursTotal
		case (r.level == "master" || r.level == "phd") && r.track == "regular":
			r.payBaht = pr.GraduateRegular * float64(termMonths)
		case (r.level == "master" || r.level == "phd") && r.track == "special":
			r.payBaht = pr.GraduateSpecialLumpsum * float64(termMonths)
		}
		records = append(records, r)
	}

	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	for _, r := range records {
		f, err := s.buildPerTAWorkbook(ctx, teachingCourseID, courseCode, courseName, r)
		if err != nil {
			return nil, "", err
		}
		w, err := zw.Create(fmt.Sprintf("%s/%s.xlsx", courseCode, sanitize(r.fullName)))
		if err != nil {
			return nil, "", err
		}
		if _, err := io.Copy(w, bytes.NewReader(f)); err != nil {
			return nil, "", err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	name := fmt.Sprintf("%s_%s.zip", courseCode, time.Now().Format("20060102_150405"))
	return buf.Bytes(), name, nil
}

func (s *ExportService) buildPerTAWorkbook(ctx context.Context, tcID uuid.UUID, code, name string, r struct {
	taID       uuid.UUID
	fullName   string
	email      string
	nationalID string
	bankAcct   string
	track      string
	level      string
	hoursTotal float64
	payBaht    float64
}) ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()

	// Sheet 1: coversheet
	cover := "หน้าปะ"
	f.SetSheetName("Sheet1", cover)
	f.SetCellValue(cover, "A1", "ใบปะหน้าเบิกจ่ายค่าตอบแทนผู้ช่วยสอน")
	f.SetCellValue(cover, "A3", "รหัสวิชา")
	f.SetCellValue(cover, "B3", code)
	f.SetCellValue(cover, "A4", "ชื่อวิชา")
	f.SetCellValue(cover, "B4", name)
	f.SetCellValue(cover, "A6", "ชื่อ-นามสกุล TA")
	f.SetCellValue(cover, "B6", r.fullName)
	f.SetCellValue(cover, "A7", "เลขบัตรประชาชน")
	f.SetCellValue(cover, "B7", r.nationalID)
	f.SetCellValue(cover, "A8", "ระดับ / ภาค")
	f.SetCellValue(cover, "B8", r.level+" / "+r.track)
	f.SetCellValue(cover, "A9", "ธนาคาร / บัญชี")
	f.SetCellValue(cover, "B9", r.bankAcct)
	f.SetCellValue(cover, "A11", "รวมชั่วโมง (อนุมัติ)")
	f.SetCellValue(cover, "B11", r.hoursTotal)
	f.SetCellValue(cover, "A12", "รวมเงินค่าตอบแทน (บาท)")
	f.SetCellValue(cover, "B12", r.payBaht)

	// Sheet 2: work log
	log := "บันทึกเวลา"
	f.NewSheet(log)
	f.SetSheetRow(log, "A1", &[]interface{}{"วันที่", "เริ่ม", "สิ้นสุด", "ชั่วโมง", "กิจกรรม", "ห้อง", "หมายเหตุ"})
	rowNum := 2
	rows, _ := s.pool.Query(ctx, `
		SELECT TO_CHAR(wl.work_date,'YYYY-MM-DD'), wl.start_time::text, wl.end_time::text, wl.hours,
		       wl.activity, COALESCE(wl.room,''), COALESCE(wl.note,'')
		FROM work_logs wl JOIN ta_request_assignments a ON a.id=wl.assignment_id
		JOIN ta_requests req ON req.id = a.request_id
		WHERE req.teaching_course_id=$1 AND a.ta_id=$2 AND wl.status IN ('submitted','approved')
		ORDER BY wl.work_date`, tcID, r.taID)
	for rows.Next() {
		var date, start, end, activity, room, note string
		var hours float64
		if err := rows.Scan(&date, &start, &end, &hours, &activity, &room, &note); err == nil {
			cell := fmt.Sprintf("A%d", rowNum)
			f.SetSheetRow(log, cell, &[]interface{}{date, start, end, hours, activity, room, note})
			rowNum++
		}
	}
	rows.Close()

	// Sheet 3: schedule
	sched := "ตารางสอน+งาน"
	f.NewSheet(sched)
	f.SetSheetRow(sched, "A1", &[]interface{}{"วัน", "เริ่ม", "สิ้นสุด", "ประเภท", "ห้อง"})
	rowNum = 2
	rows, _ = s.pool.Query(ctx, `
		SELECT ss.day_of_week, ss.start_time::text, ss.end_time::text, ss.kind, COALESCE(ss.room, '')
		FROM section_schedules ss
		JOIN sections sec ON sec.id = ss.section_id
		JOIN ta_request_assignments a ON a.section_id = sec.id
		JOIN ta_requests req ON req.id = a.request_id
		WHERE req.teaching_course_id = $1 AND a.ta_id = $2
		ORDER BY ss.day_of_week, ss.start_time`, tcID, r.taID)
	dayName := []string{"อาทิตย์", "จันทร์", "อังคาร", "พุธ", "พฤหัส", "ศุกร์", "เสาร์"}
	for rows.Next() {
		var day int
		var start, end, kind, room string
		if err := rows.Scan(&day, &start, &end, &kind, &room); err == nil {
			cell := fmt.Sprintf("A%d", rowNum)
			f.SetSheetRow(sched, cell, &[]interface{}{dayName[day], start, end, kind, room})
			rowNum++
		}
	}
	rows.Close()

	f.SetActiveSheet(0)
	var out bytes.Buffer
	if err := f.Write(&out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func sanitize(s string) string {
	out := []rune{}
	for _, c := range s {
		switch c {
		case '/', '\\', '?', '*', ':', '"', '<', '>', '|':
			out = append(out, '_')
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
