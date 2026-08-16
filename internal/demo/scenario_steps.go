package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/service"
	"ta-payment-back/internal/timeutil"
)

func strPtr(s string) *string { return &s }

// activeTermID returns the term stepTerm created. Every later step needs
// this, so a clear "run step 1 first" message beats a confusing nil-uuid
// failure three calls deep.
func activeTermID(ctx context.Context, svc *service.Container) (uuid.UUID, error) {
	var id uuid.UUID
	// UpsertTerm demotes every other active term on create, so at most one
	// row ever has is_active=true — no ORDER BY needed (academic_terms has
	// no created_at column to order by anyway).
	err := svc.Pool.QueryRow(ctx, `SELECT id FROM academic_terms WHERE is_active LIMIT 1`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, errors.New("demo: ยังไม่ได้สร้างปีการศึกษา/ภาคเรียน — กรุณาทำขั้นตอนที่ 1 ก่อน")
	}
	return id, err
}

// ---- 1. Term ---------------------------------------------------------

func stepTerm(ctx context.Context, svc *service.Container) (string, error) {
	var existing int
	if err := svc.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM academic_terms WHERE is_active`).Scan(&existing); err != nil {
		return "", err
	}
	if existing > 0 {
		return "มีปีการศึกษา/ภาคเรียนที่เปิดใช้งานอยู่แล้ว ข้ามไปขั้นตอนถัดไปได้เลย", nil
	}
	now := timeutil.Now()
	beYear := now.Year() + 543
	adminID, err := userIDByEmail(ctx, svc, "admin@demo.local")
	if err != nil {
		return "", err
	}
	in := service.Term{
		AcademicYear:    beYear,
		Semester:        1,
		StartsOn:        strPtr(isoDate(now.AddDate(0, 0, -14))),
		EndsOn:          strPtr(isoDate(now.AddDate(0, 4, 0))),
		MidtermStartsOn: strPtr(isoDate(now.AddDate(0, 0, 45))),
		MidtermEndsOn:   strPtr(isoDate(now.AddDate(0, 0, 48))),
		FinalStartsOn:   strPtr(isoDate(now.AddDate(0, 0, 110))),
		FinalEndsOn:     strPtr(isoDate(now.AddDate(0, 0, 113))),
		Months:          4,
		IsActive:        true,
	}
	if _, err := svc.Teaching.UpsertTerm(ctx, adminID, in); err != nil {
		return "", err
	}
	return fmt.Sprintf("สร้างภาคเรียนที่ 1 ปีการศึกษา %d เรียบร้อย (ช่วงสอบกลางภาค/ปลายภาคตั้งอยู่ล่วงหน้า ไม่ชนกับเดือนที่กำลังจำลอง)", beYear), nil
}

// ---- 2. Courses --------------------------------------------------------

func stepCourses(ctx context.Context, svc *service.Container) (string, error) {
	termID, err := activeTermID(ctx, svc)
	if err != nil {
		return "", err
	}
	adminID, err := userIDByEmail(ctx, svc, "admin@demo.local")
	if err != nil {
		return "", err
	}

	now := timeutil.Now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, timeutil.Bangkok)
	monthEnd := monthStart.AddDate(0, 1, -1)
	// Monday, in the middle of the working day — arbitrary but safely clear
	// of the TA's own personal-class slot (Friday afternoon, see
	// stepTASchedules) so no schedule ever collides with another.
	lectureDay := 1

	created, skipped := 0, 0
	for _, cs := range courseSeeds {
		var exists int
		if err := svc.Pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM teaching_courses WHERE code=$1 AND term_id=$2`, cs.code, termID,
		).Scan(&exists); err != nil {
			return "", err
		}
		if exists > 0 {
			skipped++
			continue
		}
		lecturerID, err := userIDByEmail(ctx, svc, cs.lecturerEmail)
		if err != nil {
			return "", err
		}

		// CreateTeachingCourseInput.Sections is an ANONYMOUS struct type (see
		// teaching.go) — round-tripping through the same JSON shape the real
		// HTTP API binds from (Bind→json.Unmarshal) is more robust than
		// hand-copying that struct's field tags verbatim into a literal here,
		// and breaks the same way the real API would if the shape ever changes.
		raw, err := json.Marshal(map[string]any{
			"term_id":      termID,
			"code":         cs.code,
			"name_th":      cs.nameTH,
			"level":        "undergrad",
			"credits":      3,
			"lecture_hrs":  3,
			"lab_hrs":      0,
			"self_hrs":     6,
			"starts_on":    isoDate(monthStart),
			"ends_on":      isoDate(monthEnd),
			"num_students": 30,
			"lecturer_ids": []uuid.UUID{lecturerID},
			"sections": []map[string]any{
				{
					"sec_no":       "1",
					"track":        "regular",
					"num_students": 30,
					"schedules": []map[string]any{
						{"kind": "lecture", "day_of_week": lectureDay, "start_time": "09:00", "end_time": "12:00"},
					},
				},
			},
		})
		if err != nil {
			return "", err
		}
		var in service.CreateTeachingCourseInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return "", err
		}
		if _, err := svc.Teaching.Create(ctx, adminID, in); err != nil {
			return "", fmt.Errorf("เปิดรายวิชา %s: %w", cs.code, err)
		}
		created++
	}
	if created == 0 {
		return "เปิดรายวิชาไว้ครบทั้ง 3 วิชาแล้ว ข้ามไปขั้นตอนถัดไปได้เลย", nil
	}
	return fmt.Sprintf("เปิดรายวิชาใหม่ %d วิชา (ข้ามที่มีอยู่แล้ว %d วิชา) — แต่ละวิชามีตารางบรรยายทุกวันจันทร์ 09:00-12:00 ตลอดเดือนนี้", created, skipped), nil
}

// ---- 3. Submission periods ---------------------------------------------

// stepSubmissionPeriods deliberately does NOT call
// SubmissionPeriodService.BulkCreateForTerm — that template's due dates
// follow the REAL faculty calendar (see submission_period.go), which for
// most months of the year has already passed by the time anyone runs this
// sandbox. A demo has to be walkable on any day it's opened, so this opens
// exactly the current month with a due date safely in the future instead.
func stepSubmissionPeriods(ctx context.Context, svc *service.Container) (string, error) {
	termID, err := activeTermID(ctx, svc)
	if err != nil {
		return "", err
	}
	adminID, err := userIDByEmail(ctx, svc, "admin@demo.local")
	if err != nil {
		return "", err
	}
	ym := currentYearMonth()
	var existing int
	if err := svc.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM submission_periods WHERE term_id=$1 AND year_month=$2`, termID, ym,
	).Scan(&existing); err != nil {
		return "", err
	}
	if existing > 0 {
		return fmt.Sprintf("เปิดรอบส่งของเดือน %s ไว้แล้ว ข้ามไปขั้นตอนถัดไปได้เลย", ym), nil
	}
	now := timeutil.Now()
	in := service.SubmissionPeriod{
		TermID:           termID,
		YearMonth:        ym,
		StartsOn:         isoDate(time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, timeutil.Bangkok)),
		DueDate:          isoDate(now.AddDate(0, 0, 30)),
		Label:            fmt.Sprintf("รอบส่งเดือน %s (จำลอง)", ym),
		RemindDaysBefore: 3,
		IsClosed:         false,
	}
	if _, err := svc.SubmissionPeriods.Upsert(ctx, adminID, in); err != nil {
		return "", err
	}
	return fmt.Sprintf("เปิดรอบส่งค่าตอบแทนเดือน %s แล้ว กำหนดส่งอีก 30 วันข้างหน้า", ym), nil
}

// ---- 4. TA class schedules ---------------------------------------------

func stepTASchedules(ctx context.Context, svc *service.Container) (string, error) {
	termID, err := activeTermID(ctx, svc)
	if err != nil {
		return "", err
	}
	taEmails := []string{"ta1@demo.local", "ta2@demo.local", "ta3@demo.local", "ta4@demo.local"}
	done, skipped := 0, 0
	for _, email := range taEmails {
		taID, err := userIDByEmail(ctx, svc, email)
		if err != nil {
			return "", err
		}
		var has bool
		if err := svc.Pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM ta_class_schedules WHERE user_id=$1 AND term_id=$2)`, taID, termID,
		).Scan(&has); err != nil {
			return "", err
		}
		if has {
			skipped++
			continue
		}
		// A class the TA takes as a STUDENT — free text, unrelated to any
		// teaching_courses row. Friday afternoon: clear of every seeded
		// course's Monday-morning lecture slot, so no conflict check ever
		// has a real clash to report for this scenario.
		block := service.ClassBlock{
			ID:         "demo-own-class",
			TermID:     termID,
			CourseCode: "GE100001",
			CourseName: "วิชาศึกษาทั่วไป (จำลอง)",
			Kind:       "lecture",
			SecNo:      "1",
			DayOfWeek:  5,
			StartTime:  "15:00",
			EndTime:    "16:00",
		}
		if err := svc.Workload.ReplaceClasses(ctx, taID, termID, []service.ClassBlock{block}); err != nil {
			return "", fmt.Errorf("บันทึกตารางเรียนของ %s: %w", email, err)
		}
		done++
	}
	if done == 0 {
		return "TA ทั้ง 4 คนบันทึกตารางเรียนไว้แล้ว ข้ามไปขั้นตอนถัดไปได้เลย", nil
	}
	return fmt.Sprintf("TA บันทึกตารางเรียนของตัวเองแล้ว %d คน (มีอยู่ก่อนแล้ว %d คน)", done, skipped), nil
}

// ---- 5. TA requests (lecturer nominates a TA) --------------------------

func stepTARequests(ctx context.Context, svc *service.Container) (string, error) {
	termID, err := activeTermID(ctx, svc)
	if err != nil {
		return "", err
	}
	approved, rejected, skipped := 0, 0, 0
	var notes []string
	for _, cs := range courseSeeds {
		var courseID uuid.UUID
		if err := svc.Pool.QueryRow(ctx,
			`SELECT id FROM teaching_courses WHERE code=$1 AND term_id=$2`, cs.code, termID,
		).Scan(&courseID); err != nil {
			return "", fmt.Errorf("demo: วิชา %s ยังไม่ถูกสร้าง — ทำขั้นตอนที่ 2 ก่อน", cs.code)
		}
		var existingReq int
		if err := svc.Pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM ta_requests WHERE teaching_course_id=$1`, courseID,
		).Scan(&existingReq); err != nil {
			return "", err
		}
		if existingReq > 0 {
			skipped++
			continue
		}
		lecturerID, err := userIDByEmail(ctx, svc, cs.lecturerEmail)
		if err != nil {
			return "", err
		}
		taID, err := userIDByEmail(ctx, svc, cs.taEmail)
		if err != nil {
			return "", err
		}
		var sectionID uuid.UUID
		if err := svc.Pool.QueryRow(ctx,
			`SELECT id FROM sections WHERE teaching_course_id=$1 LIMIT 1`, courseID,
		).Scan(&sectionID); err != nil {
			return "", err
		}
		in := service.CreateTARequestInput{
			TeachingCourseID: courseID,
			ReimburseScope:   "lecture",
			Assignments: []service.AssignmentInput{
				{
					SectionIDs: []uuid.UUID{sectionID},
					TAID:       taID,
					Level:      "undergrad",
					// HelpTeachHrs/PrepHrs/GradeHrs only count toward the grad
					// (master/phd) 10–12 hr/week rule — autoDecide's workload
					// check for an UNDERGRAD TA sums CheckWorkHrs+AttendanceHrs+
					// UGOtherHrs+LabHrs+LabOtherHrs instead (ta_request.go), so
					// those are the fields that have to be non-zero here.
					Workload: service.WorkloadInput{
						CheckWorkHrs:  2,
						AttendanceHrs: 3,
					},
				},
			},
		}
		res, err := svc.TARequest.Create(ctx, lecturerID, in)
		if err != nil {
			return "", fmt.Errorf("เสนอชื่อ TA สำหรับวิชา %s: %w", cs.code, err)
		}
		switch res.Status {
		case "approved":
			approved++
		case "rejected":
			rejected++
			notes = append(notes, fmt.Sprintf("%s: %s", cs.code, res.RejectReason))
		default:
			notes = append(notes, fmt.Sprintf("%s: สถานะ %s", cs.code, res.Status))
		}
	}
	msg := fmt.Sprintf("ยื่นคำขอ TA แล้ว — อนุมัติอัตโนมัติ %d วิชา, ปฏิเสธ %d วิชา, ข้ามที่มีอยู่แล้ว %d วิชา", approved, rejected, skipped)
	if len(notes) > 0 {
		msg += " | " + strings.Join(notes, "; ")
	}
	return msg, nil
}

// ---- 6. Appointment order ------------------------------------------------

func stepAppointmentOrder(ctx context.Context, svc *service.Container) (string, error) {
	termID, err := activeTermID(ctx, svc)
	if err != nil {
		return "", err
	}
	adminID, err := userIDByEmail(ctx, svc, "admin@demo.local")
	if err != nil {
		return "", err
	}
	var signerID uuid.UUID
	if err := svc.Pool.QueryRow(ctx, `SELECT id FROM admin_officers WHERE is_active LIMIT 1`).Scan(&signerID); err != nil {
		return "", errors.New("demo: ยังไม่มีผู้ลงนามในระบบ (admin_officers ว่าง) — รีเซ็ตห้องทดลองแล้วลองใหม่")
	}
	now := timeutil.Now()
	in := service.AppointmentOrderInput{
		TermID:          termID,
		OrderNo:         fmt.Sprintf("ทดลอง-%s", isoDate(now)),
		OrderDate:       thaiLongDate(now),
		EffectiveDate:   thaiLongDate(now),
		SignerOfficerID: signerID,
	}
	body, name, err := svc.Appointment.Build(ctx, adminID, in)
	if err != nil {
		// Build's own message already distinguishes "no approved TA yet" from
		// "everyone already has an order this term" — surface it as-is rather
		// than wrapping it, since either one IS the useful answer here.
		return "", err
	}
	return fmt.Sprintf("ออกคำสั่งแต่งตั้งสำเร็จ (%s, %d KB) — ดูเอกสารจริงได้ที่หน้า “ใบแต่งตั้งคำสั่ง” ของเจ้าหน้าที่", name, len(body)/1024+1), nil
}

// ---- 7. TA docs -----------------------------------------------------------

func stepTADocs(ctx context.Context, svc *service.Container) (string, error) {
	done, skipped := 0, 0
	for i, cs := range courseSeeds {
		taID, err := userIDByEmail(ctx, svc, cs.taEmail)
		if err != nil {
			return "", err
		}
		var hasProfile bool
		if err := svc.Pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM ta_profiles WHERE user_id=$1 AND completed_at IS NOT NULL)`, taID,
		).Scan(&hasProfile); err != nil {
			return "", err
		}
		if hasProfile {
			skipped++
			continue
		}
		// validateProfileInput requires exactly "XXXXXXXXX-X" (9 digits, a
		// dash, 1 check-like digit) — see docs.go.
		profile := service.TAProfile{
			StudentID:    fmt.Sprintf("64500000%d-%d", i+1, i+1),
			Prefix:       "นาย",
			Phone:        "0800000000",
			NationalID:   "1234567890123",
			BankName:     "ธนาคารกรุงไทย",
			BankBranch:   "สาขามหาวิทยาลัยขอนแก่น",
			BranchCode:   "0000",
			AccountNo:    "1234567890",
			AccountName:  "บัญชีทดลอง",
			SignatureSVG: minimalSignatureSVG,
		}
		if err := svc.Docs.UpsertProfile(ctx, taID, profile); err != nil {
			return "", fmt.Errorf("บันทึกโปรไฟล์ %s: %w", cs.taEmail, err)
		}
		for _, kind := range []string{"national_id", "bank_book", "creditor_form"} {
			if _, err := svc.Docs.Upload(ctx, taID, kind, kind+".pdf", "application/pdf",
				int64(len(minimalPDF)), bytes.NewReader([]byte(minimalPDF))); err != nil {
				return "", fmt.Errorf("อัปโหลดเอกสาร %s ของ %s: %w", kind, cs.taEmail, err)
			}
		}
		done++
	}
	if done == 0 {
		return "TA ที่ได้รับแต่งตั้งกรอกโปรไฟล์และส่งเอกสารครบแล้ว ข้ามไปขั้นตอนถัดไปได้เลย", nil
	}
	return fmt.Sprintf("TA ส่งโปรไฟล์และเอกสาร 3 ฉบับแล้ว %d คน (มีอยู่ก่อนแล้ว %d คน)", done, skipped), nil
}

// ---- 8. Docs approve ------------------------------------------------------

func stepDocsApprove(ctx context.Context, svc *service.Container) (string, error) {
	staffID, err := userIDByEmail(ctx, svc, "staff@demo.local")
	if err != nil {
		return "", err
	}
	approved, skipped := 0, 0
	for _, cs := range courseSeeds {
		taID, err := userIDByEmail(ctx, svc, cs.taEmail)
		if err != nil {
			return "", err
		}
		var status string
		if err := svc.Pool.QueryRow(ctx, `SELECT status::text FROM ta_profiles WHERE user_id=$1`, taID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", fmt.Errorf("demo: %s ยังไม่ได้ส่งเอกสาร — ทำขั้นตอนที่ 7 ก่อน", cs.taEmail)
			}
			return "", err
		}
		if status == "approved" {
			skipped++
			continue
		}
		if _, err := svc.Docs.ApproveAll(ctx, staffID, taID); err != nil {
			return "", fmt.Errorf("อนุมัติเอกสารของ %s: %w", cs.taEmail, err)
		}
		approved++
	}
	if approved == 0 {
		return "อนุมัติเอกสารของ TA ทุกคนไว้แล้ว ข้ามไปขั้นตอนถัดไปได้เลย", nil
	}
	return fmt.Sprintf("อนุมัติเอกสารแล้ว %d คน (มีอยู่ก่อนแล้ว %d คน) — พร้อมสำหรับการเบิกจ่ายแล้ว", approved, skipped), nil
}

// ---- 9. Worklog generate + submit -----------------------------------------

func stepWorkLogSubmit(ctx context.Context, svc *service.Container) (string, error) {
	generated, submitted, skipped := 0, 0, 0
	for _, cs := range courseSeeds {
		taID, err := userIDByEmail(ctx, svc, cs.taEmail)
		if err != nil {
			return "", err
		}
		assignmentID, err := assignmentIDFor(ctx, svc, cs.code, taID)
		if err != nil {
			return "", err
		}
		var count int
		if err := svc.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM work_logs WHERE assignment_id=$1`, assignmentID).Scan(&count); err != nil {
			return "", err
		}
		if count == 0 {
			if _, err := svc.WorkLog.Generate(ctx, taID, assignmentID); err != nil {
				return "", fmt.Errorf("สร้างบันทึกเวลาให้ %s: %w", cs.taEmail, err)
			}
			generated++
		}
		var pending int
		if err := svc.Pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM work_logs WHERE assignment_id=$1 AND status IN ('draft','rejected')`, assignmentID,
		).Scan(&pending); err != nil {
			return "", err
		}
		if pending == 0 {
			skipped++
			continue
		}
		if err := svc.WorkLog.Submit(ctx, taID, assignmentID); err != nil {
			return "", fmt.Errorf("ส่งอนุมัติบันทึกเวลาของ %s: %w", cs.taEmail, err)
		}
		submitted++
	}
	return fmt.Sprintf("สร้างบันทึกเวลาให้ %d คน และส่งอนุมัติแล้ว %d คน (ส่งไปแล้วก่อนหน้า %d คน) ตามตารางบรรยายของเดือนนี้", generated, submitted, skipped), nil
}

// assignmentIDFor looks up the ta_request_assignments row for (course code,
// TA), which every downstream worklog/approval/export step keys on.
func assignmentIDFor(ctx context.Context, svc *service.Container, courseCode string, taID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := svc.Pool.QueryRow(ctx, `
		SELECT a.id FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id
		JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		WHERE tc.code = $1 AND a.ta_id = $2 AND a.state <> 'dropped'
		ORDER BY a.id LIMIT 1`, courseCode, taID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("demo: ยังไม่มีการแต่งตั้ง TA ในวิชา %s — ทำขั้นตอนที่ 5 ก่อน (อาจถูกปฏิเสธอัตโนมัติ ลองรีเซ็ตห้องทดลอง)", courseCode)
	}
	return id, err
}

// ---- 10. Worklog approve (lecturer) ---------------------------------------

func stepWorkLogApprove(ctx context.Context, svc *service.Container) (string, error) {
	approved, skipped := 0, 0
	for _, cs := range courseSeeds {
		lecturerID, err := userIDByEmail(ctx, svc, cs.lecturerEmail)
		if err != nil {
			return "", err
		}
		taID, err := userIDByEmail(ctx, svc, cs.taEmail)
		if err != nil {
			return "", err
		}
		assignmentID, err := assignmentIDFor(ctx, svc, cs.code, taID)
		if err != nil {
			return "", err
		}
		var pending int
		if err := svc.Pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM work_logs WHERE assignment_id=$1 AND status='submitted'`, assignmentID,
		).Scan(&pending); err != nil {
			return "", err
		}
		if pending == 0 {
			skipped++
			continue
		}
		if err := svc.WorkLog.Approve(ctx, lecturerID, assignmentID, "", false); err != nil {
			return "", fmt.Errorf("อนุมัติบันทึกเวลาวิชา %s: %w", cs.code, err)
		}
		approved++
	}
	if approved == 0 {
		return "ไม่มีบันทึกเวลาที่รออนุมัติ (อาจยังไม่ถึงขั้นตอนที่ 9 หรืออนุมัติไปแล้วทั้งหมด)", nil
	}
	return fmt.Sprintf("อาจารย์อนุมัติบันทึกเวลาแล้ว %d วิชา (ไม่มีรายการค้าง %d วิชา)", approved, skipped), nil
}

// ---- 11. Staff review + export + lock -------------------------------------

func stepStaffReviewExport(ctx context.Context, svc *service.Container) (string, error) {
	termID, err := activeTermID(ctx, svc)
	if err != nil {
		return "", err
	}
	staffID, err := userIDByEmail(ctx, svc, "staff@demo.local")
	if err != nil {
		return "", err
	}
	ym := currentYearMonth()
	var periodID uuid.UUID
	if err := svc.Pool.QueryRow(ctx,
		`SELECT id FROM submission_periods WHERE term_id=$1 AND year_month=$2`, termID, ym,
	).Scan(&periodID); err != nil {
		return "", errors.New("demo: ยังไม่ได้เปิดรอบส่งของเดือนนี้ — ทำขั้นตอนที่ 3 ก่อน")
	}

	var reviewed, exported, financeSent int
	var problems []string
	for _, cs := range courseSeeds {
		var courseID uuid.UUID
		if err := svc.Pool.QueryRow(ctx,
			`SELECT id FROM teaching_courses WHERE code=$1 AND term_id=$2`, cs.code, termID,
		).Scan(&courseID); err != nil {
			return "", err
		}
		taID, err := userIDByEmail(ctx, svc, cs.taEmail)
		if err != nil {
			return "", err
		}

		if err := svc.SubmissionPeriods.MarkStaffReviewed(ctx, staffID, periodID, taID, courseID, "ตรวจสอบโดยห้องทดลอง"); err != nil {
			// Already reviewed, or nothing approved yet for this TA/month — both
			// are normal re-run states, not failures worth stopping the whole
			// step over; note it and keep going with the other courses.
			problems = append(problems, fmt.Sprintf("%s: %s", cs.code, err.Error()))
		} else {
			reviewed++
		}

		// BuildCourseZip has no "already exported" guard of its own — staff can
		// legitimately re-export a course for a later round, so that refusal
		// isn't the service layer's job. It IS this step's job: without this
		// check, re-running this event after a full run built and RECORDED a
		// fresh (duplicate) export_batches row every time, even though the
		// actual lock (MarkCourseExported) below correctly no-ops on a second
		// call — the exports history page would grow one identical entry per
		// click.
		var alreadyExported bool
		if err := svc.Pool.QueryRow(ctx,
			`SELECT exported_at IS NOT NULL FROM teaching_courses WHERE id=$1`, courseID,
		).Scan(&alreadyExported); err != nil {
			return "", err
		}
		if alreadyExported {
			exported++
			financeSent++
			continue
		}

		months, err := svc.Export.ResolveCourseMonths(ctx, courseID, nil)
		if err != nil {
			return "", err
		}
		body, name, taCount, err := svc.Export.BuildCourseZip(ctx, courseID, months)
		if err != nil {
			problems = append(problems, fmt.Sprintf("ส่งออก %s: %s", cs.code, err.Error()))
			continue
		}
		if _, err := svc.SubmissionPeriods.MarkCourseExported(ctx, staffID, courseID, months); err != nil {
			return "", fmt.Errorf("ล็อกเดือนของวิชา %s: %w", cs.code, err)
		}
		_ = svc.Teaching.MarkExported(ctx, courseID)
		if prev, perr := svc.Export.CoursePreview(ctx, courseID, months); perr == nil {
			_, _ = svc.ExportBatches.Record(ctx, staffID, service.ExportBatch{
				TeachingCourseID: courseID,
				FilePath:         name,
				FileName:         name,
				TACount:          taCount,
				Months:           months,
				TotalBaht:        prev.TotalActual,
			})
		}
		exported++
		_ = body // bytes not persisted — the demo doesn't need the file on disk, only the state change it locks in

		if err := svc.SubmissionPeriods.MarkFinanceSent(ctx, staffID, periodID, taID, courseID, "ส่งการเงินโดยห้องทดลอง"); err != nil {
			problems = append(problems, fmt.Sprintf("ส่งการเงิน %s: %s", cs.code, err.Error()))
		} else {
			financeSent++
		}
	}
	msg := fmt.Sprintf("ตรวจสอบแล้ว %d วิชา, ส่งออกไฟล์และล็อกเดือนแล้ว %d วิชา, ส่งการเงินแล้ว %d วิชา", reviewed, exported, financeSent)
	if len(problems) > 0 {
		msg += " | ข้อสังเกต: " + strings.Join(problems, "; ")
	}
	return msg, nil
}

// ---- 12. Transfer cover ----------------------------------------------------

func stepTransferCover(ctx context.Context, svc *service.Container) (string, error) {
	termID, err := activeTermID(ctx, svc)
	if err != nil {
		return "", err
	}
	adminID, err := userIDByEmail(ctx, svc, "admin@demo.local")
	if err != nil {
		return "", err
	}
	// BuildTransferCoverWorkbook's months param is Gregorian (see
	// currentGregorianYearMonth's doc comment) — NOT the Buddhist
	// submission_periods key currentYearMonth() returns.
	ym := currentGregorianYearMonth()
	body, files, err := svc.Export.BuildTransferCoverWorkbook(ctx, adminID, termID, []string{ym}, "undergrad")
	if err != nil {
		// BuildTransferCoverWorkbook's own message already names which course
		// hasn't reached finance_sent yet — pass it through unchanged.
		return "", err
	}
	return fmt.Sprintf("ออกเอกสารปะหน้าจ่ายตรงของเดือน %s แล้ว (%d ไฟล์, %d KB) — ขั้นตอนสุดท้ายของเส้นทางหลักเสร็จสมบูรณ์", ym, len(files), len(body)/1024+1), nil
}
