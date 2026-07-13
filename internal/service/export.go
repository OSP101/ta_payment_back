package service

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"

	"ta-payment-back/internal/pdfgen"
	"ta-payment-back/internal/storage"
)

type ExportService struct {
	pool    *pgxpool.Pool
	store   storage.Store
	budget  *BudgetService
	fontDir string // path to Sarabun-Regular/Bold TTFs, used by the PDF renderer
}

// exportRow is one TA's aggregated numbers used by both the coversheet and
// the PDF renderer. actualPaid ≤ payBaht when the course is over budget and
// pro-rata scaling was applied. isReturning is the "เก่า/ใหม่" badge.
type exportRow struct {
	taID        uuid.UUID
	fullName    string
	email       string
	nationalID  string
	bankAcct    string
	track       string
	level       string
	hoursTotal  float64
	payBaht     float64
	actualPaid  float64
	isReturning bool
}

// applyProrataCap scales actualPaid on each row when Σ payBaht > budgetMax
// so that Σ actualPaid == budgetMax exactly (rounding drift lands on the last
// row). Returns true if scaling was applied. Pure — no I/O, DB-free.
//
// Semantics:
//   - budgetMax ≤ 0 → no-op (unlimited).
//   - Σ payBaht ≤ budgetMax → no-op (each actualPaid stays at payBaht).
//   - Σ payBaht > budgetMax → each actualPaid = round2(payBaht × k) with
//     k = budgetMax / Σ payBaht; residual off the last row so it sums exactly.
func applyProrataCap(records []exportRow, budgetMax float64) bool {
	if budgetMax <= 0 || len(records) == 0 {
		return false
	}
	var totalRaw float64
	for _, r := range records {
		totalRaw += r.payBaht
	}
	if totalRaw <= budgetMax+0.01 {
		return false
	}
	k := budgetMax / totalRaw
	var scaledSum float64
	for i := range records {
		records[i].actualPaid = math.Round(records[i].payBaht*k*100) / 100
		scaledSum += records[i].actualPaid
	}
	// Unconditionally place any residual (positive or negative) on the last row
	// so Σ actualPaid == budgetMax to the cent. Adding zero is a no-op.
	records[len(records)-1].actualPaid = math.Round((records[len(records)-1].actualPaid+(budgetMax-scaledSum))*100) / 100
	return true
}

// isReturningTA reports whether the TA had an approved assignment in any prior
// academic term. Used by exports to badge each row as "เก่า/ใหม่". Q&A rule:
// "old" = held any approved ta_request_assignment in a term whose start_date
// is earlier than the current course's term.
func (s *ExportService) isReturningTA(ctx context.Context, taID, currentTermID uuid.UUID) bool {
	var yes bool
	_ = s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM ta_request_assignments a
			JOIN ta_requests r ON r.id=a.request_id AND r.status='approved'
			JOIN sections sec ON sec.id=a.section_id
			JOIN teaching_courses tc ON tc.id=sec.teaching_course_id
			JOIN academic_terms t  ON t.id=tc.term_id
			JOIN academic_terms cur ON cur.id=$2
			WHERE a.ta_id=$1 AND (t.academic_year < cur.academic_year
			     OR (t.academic_year = cur.academic_year AND t.semester < cur.semester))
		)`, taID, currentTermID).Scan(&yes)
	return yes
}

// BuildCourseZip builds a zip archive containing per-TA .xlsx files for a course.
// Each xlsx has three sheets:
//   Sheet 1: "หน้าปะ" (coversheet) — reimbursement summary
//   Sheet 2: "บันทึกเวลา" (work log)
//   Sheet 3: "ตารางสอน+งาน" (schedule)
//
// Payment model (ประกาศ 731/2565 + 1080/2565 + Q&A 2026):
//   ป.ตรี ปกติ  → 40 ฿ × approved hours
//   ป.ตรี พิเศษ → 50 ฿ × approved hours
//   บัณฑิต ปกติ → 50 ฿ × approved hours (hourly per Q&A rule 6c, was lump-sum)
//   บัณฑิต พิเศษ → min(4,000 × term_months, 12,000) — flat monthly capped per term
//
// Regular-first-then-special quota (Q&A rule 5): when a TA holds BOTH a regular
// and a special ป.ตรี section in the same course, consume the regular quota
// (= scheduled hours/week × weeks_in_term) at the regular rate first; any hours
// beyond the regular quota "spill" onto the special rate. Grad tiers don't spill.
func (s *ExportService) BuildCourseZip(ctx context.Context, teachingCourseID uuid.UUID) ([]byte, string, error) {
	var courseCode, courseName string
	if err := s.pool.QueryRow(ctx, `
		SELECT fc.code, fc.name_th FROM teaching_courses tc
		JOIN faculty_courses fc ON fc.id = tc.faculty_course_id
		WHERE tc.id=$1`, teachingCourseID).Scan(&courseCode, &courseName); err != nil {
		return nil, "", err
	}

	var pr PayRate
	_ = s.pool.QueryRow(ctx,
		`SELECT undergrad_regular, undergrad_special, graduate_regular, graduate_special_lumpsum,
		        graduate_regular_hourly, grad_special_term_cap, term_months
		 FROM pay_rates ORDER BY effective_from DESC LIMIT 1`).Scan(
		&pr.UndergradRegular, &pr.UndergradSpecial, &pr.GraduateRegular, &pr.GraduateSpecialLumpsum,
		&pr.GraduateRegularHourly, &pr.GradSpecialTermCap, &pr.TermMonths)
	// Prefer per-term months (source of truth); fall back to pay_rates.term_months.
	var perTermMonths int
	_ = s.pool.QueryRow(ctx, `
		SELECT t.months FROM academic_terms t
		JOIN teaching_courses tc ON tc.term_id = t.id WHERE tc.id = $1`, teachingCourseID).Scan(&perTermMonths)
	if perTermMonths > 0 {
		pr.TermMonths = perTermMonths
	}
	termMonths := pr.TermMonths
	if termMonths == 0 {
		termMonths = 4
	}
	weeksInTerm := float64(termMonths) * 4.0

	// Pull per-assignment rows ordered regular-first per TA so we can consume
	// the regular quota before spilling onto special.
	assignRows, err := s.pool.Query(ctx, `
		SELECT a.id, a.ta_id, u.first_name||' '||u.last_name, u.email,
		       COALESCE(p.national_id,''), COALESCE(p.bank_name,'')||' '||COALESCE(p.account_no,''),
		       sec.track::text, a.level::text,
		       COALESCE(SUM(wl.hours) FILTER (WHERE wl.status='approved'), 0) AS approved_hrs,
		       (SELECT COALESCE(SUM(EXTRACT(EPOCH FROM (end_time-start_time))/3600),0)
		        FROM section_schedules WHERE section_id=sec.id) AS sched_hrs_per_week
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status='approved'
		JOIN users u ON u.id = a.ta_id
		LEFT JOIN ta_profiles p ON p.user_id = a.ta_id
		JOIN sections sec ON sec.id = a.section_id
		LEFT JOIN work_logs wl ON wl.assignment_id = a.id
		WHERE r.teaching_course_id = $1
		GROUP BY a.id, a.ta_id, u.first_name, u.last_name, u.email,
		         p.national_id, p.bank_name, p.account_no, sec.track, sec.id, a.level, sec.sec_no
		ORDER BY u.first_name, a.ta_id, (sec.track='regular') DESC, sec.sec_no`, teachingCourseID)
	if err != nil {
		return nil, "", err
	}
	defer assignRows.Close()

	// Per-TA accumulator so we can (a) apply regular-first spillover for
	// undergrad and (b) aggregate all of a TA's assignments into ONE workbook row
	// (matches the pre-refactor UX where each TA yields a single .xlsx).
	type taAgg struct {
		taID       uuid.UUID
		fullName   string
		email      string
		nationalID string
		bankAcct   string
		// display: "regular" if TA has any regular assignment else "special";
		// display: "undergrad" if any undergrad assignment else "master"/"phd".
		track      string
		level      string
		hoursTotal float64
		payBaht    float64
		ugRegularQuotaLeft float64 // scheduled regular hours × weeks_in_term
	}
	byTA := map[uuid.UUID]*taAgg{}
	order := []uuid.UUID{}

	for assignRows.Next() {
		var (
			assignID          uuid.UUID
			taID              uuid.UUID
			fullName, email   string
			nationalID, bank  string
			track, level      string
			approvedHrs       float64
			schedHrsPerWeek   float64
		)
		if err := assignRows.Scan(&assignID, &taID, &fullName, &email, &nationalID, &bank,
			&track, &level, &approvedHrs, &schedHrsPerWeek); err != nil {
			return nil, "", err
		}
		agg, ok := byTA[taID]
		if !ok {
			agg = &taAgg{
				taID: taID, fullName: fullName, email: email,
				nationalID: nationalID, bankAcct: bank,
				track: track, level: level,
			}
			byTA[taID] = agg
			order = append(order, taID)
		}
		// Regular assignments come first (per ORDER BY), so their scheduled hours
		// seed the regular quota that a later special assignment may spill onto.
		if track == "regular" && level == "undergrad" {
			agg.ugRegularQuotaLeft += schedHrsPerWeek * weeksInTerm
		}
		// Display-track precedence: "regular" over "special" so the coversheet
		// shows the more informative label when a TA has both.
		if track == "regular" {
			agg.track = "regular"
		}
		// Display-level precedence: undergrad wins over grad only if this TA
		// happens to hold both (unusual — study_level is fixed per user).
		if level == "undergrad" {
			agg.level = "undergrad"
		}
		agg.hoursTotal += approvedHrs

		switch {
		case level == "undergrad" && track == "regular":
			// Bill approved hours at regular rate up to the quota; grad-regular
			// spillover never applies here (this branch never spills).
			agg.payBaht += approvedHrs * pr.UndergradRegular
			agg.ugRegularQuotaLeft -= approvedHrs
			if agg.ugRegularQuotaLeft < 0 {
				// TA exceeded scheduled regular hours — treat overflow as
				// counted-against-regular still (rare; sched hours are advisory).
				agg.ugRegularQuotaLeft = 0
			}

		case level == "undergrad" && track == "special":
			// Q&A rule 5: consume any leftover regular quota FIRST at the regular
			// rate, THEN bill the remainder at the special rate.
			billedAtRegular := math.Min(approvedHrs, agg.ugRegularQuotaLeft)
			spill := approvedHrs - billedAtRegular
			agg.payBaht += billedAtRegular*pr.UndergradRegular + spill*pr.UndergradSpecial
			agg.ugRegularQuotaLeft -= billedAtRegular

		case (level == "master" || level == "phd") && track == "regular":
			// Q&A rule 6c: บัณฑิต regular is HOURLY (50฿/hr per ประกาศ).
			agg.payBaht += approvedHrs * pr.GraduateRegularHourly

		case (level == "master" || level == "phd") && track == "special":
			// Q&A rule 6b: บัณฑิต special is flat 4,000฿/เดือน capped at 12,000฿/term.
			raw := pr.GraduateSpecialLumpsum * float64(termMonths)
			termCap := pr.GradSpecialTermCap
			if termCap > 0 && raw > termCap {
				raw = termCap
			}
			agg.payBaht += raw
		}
	}

	// Look up the current term for old/new detection.
	var currentTermID uuid.UUID
	_ = s.pool.QueryRow(ctx, `SELECT term_id FROM teaching_courses WHERE id=$1`, teachingCourseID).Scan(&currentTermID)

	// Freeze to the shape the workbook + PDF builders share.
	var records []exportRow
	for _, taID := range order {
		agg := byTA[taID]
		records = append(records, exportRow{
			taID: agg.taID, fullName: agg.fullName, email: agg.email,
			nationalID: agg.nationalID, bankAcct: agg.bankAcct,
			track: agg.track, level: agg.level,
			hoursTotal:  agg.hoursTotal,
			payBaht:     agg.payBaht,
			actualPaid:  agg.payBaht, // will be scaled below if over budget
			isReturning: currentTermID != uuid.Nil && s.isReturningTA(ctx, agg.taID, currentTermID),
		})
	}

	// Pro-rata cap: if the sum of raw payBaht exceeds the course budget,
	// scale each TA's actualPaid so the total lands exactly on the budget.
	var budgetMax float64
	if s.budget != nil {
		if snap, err := s.budget.Compute(ctx, teachingCourseID); err == nil {
			budgetMax = snap.PerCourseMaxBaht
		}
	}
	prorata := applyProrataCap(records, budgetMax)

	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	// Folder convention per ประกาศ 2569:
	//   {course_code}/{TA name}/{course_code} - {TA name}.xlsx
	//   {course_code}/{TA name}/{course_code} - {TA name}.pdf
	// (Phase 3 will add /{month} between name and file when the export scopes
	// to a specific submission period.)
	for _, r := range records {
		safeName := sanitize(r.fullName)
		folder := fmt.Sprintf("%s/%s", courseCode, safeName)
		fileBase := fmt.Sprintf("%s - %s", courseCode, safeName)

		xlsx, err := s.buildPerTAWorkbook(ctx, teachingCourseID, courseCode, courseName, r, budgetMax, prorata)
		if err != nil {
			return nil, "", err
		}
		w, err := zw.Create(folder + "/" + fileBase + ".xlsx")
		if err != nil {
			return nil, "", err
		}
		if _, err := io.Copy(w, bytes.NewReader(xlsx)); err != nil {
			return nil, "", err
		}

		// PDF is best-effort: without a fontDir configured we skip it rather
		// than fail the whole export. The zip still ships the .xlsx.
		if s.fontDir != "" {
			pdfBytes, perr := s.buildPerTAPDF(ctx, teachingCourseID, courseCode, courseName, "", r)
			if perr == nil {
				pw, err := zw.Create(folder + "/" + fileBase + ".pdf")
				if err != nil {
					return nil, "", err
				}
				if _, err := io.Copy(pw, bytes.NewReader(pdfBytes)); err != nil {
					return nil, "", err
				}
			}
		}
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	name := fmt.Sprintf("%s_%s.zip", courseCode, time.Now().Format("20060102_150405"))
	return buf.Bytes(), name, nil
}

// buildPerTAPDF assembles the monthly worklog PDF for one TA. The
// yearMonthLabel is optional (empty = "รวมทุกเดือน") and is used to filter
// the approved worklog entries embedded on the sheet.
func (s *ExportService) buildPerTAPDF(ctx context.Context, tcID uuid.UUID,
	code, name, yearMonthLabel string,
	r exportRow) ([]byte, error) {

	// Human-friendly level/track labels for the coversheet.
	levelTH := r.level
	switch r.level {
	case "undergrad":
		levelTH = "ปริญญาตรี"
	case "master":
		levelTH = "ปริญญาโท"
	case "phd":
		levelTH = "ปริญญาเอก"
	}
	trackTH := r.track
	switch r.track {
	case "regular":
		trackTH = "ภาคปกติ"
	case "special":
		trackTH = "ภาคพิเศษ"
	}

	// Schedule (Block B) — combine all sections this TA holds for the course.
	sched := []pdfgen.ScheduleRow{}
	dayName := []string{"อาทิตย์", "จันทร์", "อังคาร", "พุธ", "พฤหัส", "ศุกร์", "เสาร์"}
	rows, _ := s.pool.Query(ctx, `
		SELECT ss.day_of_week, ss.start_time::text, ss.end_time::text, ss.kind, COALESCE(ss.room,'')
		FROM section_schedules ss
		JOIN sections sec ON sec.id = ss.section_id
		JOIN ta_request_assignments a ON a.section_id = sec.id
		JOIN ta_requests req ON req.id = a.request_id AND req.status='approved'
		WHERE req.teaching_course_id = $1 AND a.ta_id = $2
		ORDER BY ss.day_of_week, ss.start_time`, tcID, r.taID)
	for rows.Next() {
		var day int
		var start, end, kind, room string
		if err := rows.Scan(&day, &start, &end, &kind, &room); err == nil {
			kindTH := kind
			if kind == "lecture" {
				kindTH = "บรรยาย"
			} else if kind == "lab" {
				kindTH = "ปฏิบัติการ"
			}
			d := ""
			if day >= 0 && day < len(dayName) {
				d = dayName[day]
			}
			sched = append(sched, pdfgen.ScheduleRow{
				DayTH: d, StartTime: start, EndTime: end, Kind: kindTH, Room: room,
			})
		}
	}
	rows.Close()

	// Approved worklog entries (Block C).
	entries := []pdfgen.WorklogRow{}
	entryRows, _ := s.pool.Query(ctx, `
		SELECT TO_CHAR(wl.work_date,'YYYY-MM-DD'), wl.start_time::text, wl.end_time::text,
		       wl.hours, wl.activity, COALESCE(wl.room,''), COALESCE(wl.note,'')
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		JOIN ta_requests req ON req.id = a.request_id
		WHERE req.teaching_course_id = $1 AND a.ta_id = $2 AND wl.status = 'approved'
		ORDER BY wl.work_date, wl.start_time`, tcID, r.taID)
	for entryRows.Next() {
		var date, start, end, activity, room, note string
		var hours float64
		if err := entryRows.Scan(&date, &start, &end, &hours, &activity, &room, &note); err == nil {
			label := activity
			switch activity {
			case "lecture":
				label = "บรรยาย"
			case "lab":
				label = "ปฏิบัติการ"
			case "review":
				label = "ตรวจงาน"
			case "makeup":
				label = "ชดเชย"
			case "other":
				label = "อื่นๆ"
			}
			entries = append(entries, pdfgen.WorklogRow{
				Date: date, Start: start, End: end, Hours: hours,
				Activity: label, Room: room, Note: note,
			})
		}
	}
	entryRows.Close()

	// TA signature (best-effort).
	var taSig []byte
	_ = s.pool.QueryRow(ctx,
		`SELECT COALESCE(decode(signature_png_b64, 'base64'), '') FROM ta_profiles WHERE user_id=$1`, r.taID).Scan(&taSig)

	if yearMonthLabel == "" {
		yearMonthLabel = "รวมทุกเดือน"
	}

	return pdfgen.BuildWorklogPDF(pdfgen.WorklogPDFInput{
		FontDir: s.fontDir,
		Data: pdfgen.WorklogPDFData{
			CourseCode: code, CourseName: name,
			YearMonthLabel: yearMonthLabel,
			FullName:       r.fullName,
			NationalID:     r.nationalID,
			Email:          r.email,
			BankAcct:       r.bankAcct,
			Level:          levelTH,
			Track:          trackTH,
			HoursTotal:     r.hoursTotal,
			PayBaht:        r.payBaht,
			Schedule:       sched,
			Entries:        entries,
			TASignaturePNG: taSig,
		},
	})
}

func (s *ExportService) buildPerTAWorkbook(ctx context.Context, tcID uuid.UUID, code, name string, r exportRow, budgetMax float64, prorata bool) ([]byte, error) {
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
	f.SetCellValue(cover, "A7", "สถานะ TA")
	if r.isReturning {
		f.SetCellValue(cover, "B7", "เก่า (เคยเป็น TA ในเทอมก่อน)")
	} else {
		f.SetCellValue(cover, "B7", "ใหม่")
	}
	f.SetCellValue(cover, "A8", "เลขบัตรประชาชน")
	f.SetCellValue(cover, "B8", r.nationalID)
	f.SetCellValue(cover, "A9", "ระดับ / ภาค")
	f.SetCellValue(cover, "B9", r.level+" / "+r.track)
	f.SetCellValue(cover, "A10", "ธนาคาร / บัญชี")
	f.SetCellValue(cover, "B10", r.bankAcct)
	f.SetCellValue(cover, "A12", "รวมชั่วโมง (อนุมัติ)")
	f.SetCellValue(cover, "B12", r.hoursTotal)
	f.SetCellValue(cover, "A13", "ยอดที่ควรได้ (บาท)")
	f.SetCellValue(cover, "B13", r.payBaht)
	f.SetCellValue(cover, "A14", "ยอดจริงที่จ่าย (บาท)")
	f.SetCellValue(cover, "B14", r.actualPaid)
	if budgetMax > 0 {
		f.SetCellValue(cover, "A16", "งบรายวิชา (บาท)")
		f.SetCellValue(cover, "B16", budgetMax)
	}
	if prorata {
		f.SetCellValue(cover, "A18", "* ยอดถูกเฉลี่ยตามสัดส่วน (pro-rata) เนื่องจากยอดรวมเกินงบวิชา")
	}

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
