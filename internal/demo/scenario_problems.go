// scenario_problems.go is the "เคสที่ต้องรับมือ" (problem cases) half of the
// simulator panel — see docs/PLAN-demo-sandbox.md §5. Where scenario_steps.go
// walks the happy path, these events deliberately trigger real business-rule
// REFUSALS through the same service calls, so staff can see the exact Thai
// message a TA/lecturer would hit, without needing to wait for someone to
// actually make the mistake in production.
//
// worklog_over_cap and worklog_reject reuse the happy path's course 1 / TA 1
// (they need an appointed assignment to exist — see their own "run step N
// first" messages). budget_over_cap and submission_deadline_missed are each
// self-contained in their OWN dedicated course, both assigned to TA 4 — the
// one seed.go's demoAccounts deliberately leaves unassigned by the happy
// path — on different weekdays so the two never collide with each other on
// TA 4's own recurring schedule. Neither touches course 1/2/3.
package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/auth"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/timeutil"
)

// Every problem case is written from staff's own point of view — "here's
// what staff sees when X goes wrong" — even the ones a lecturer or TA
// technically triggers in the narrative (worklog_reject, submission_
// deadline_missed), since RelatedPath always lands on the staff screen that
// shows the consequence, not the other party's own page. See ScenarioEvent
// .ActorRole's doc comment.
var problemEvents = []ScenarioEvent{
	{Key: "worklog_over_cap", Label: "TA ลงเวลาเกินเพดานชั่วโมงต่อวัน", Description: "ให้ TA วิชาที่ 1 บันทึกเวลา 9 ชม. ในวันเดียว (เกินเพดาน 7 ชม./วัน) — ต้องทำขั้นตอนที่ 5 ในเส้นทางหลักก่อน", RelatedPath: "/staff/payouts", ActorRole: "staff"},
	{Key: "budget_over_cap", Label: "งบประมาณของรายวิชาเกินวงเงิน", Description: "สร้างวิชาจำลองที่มีนักศึกษาแค่ 1 คน (วงเงินตามสูตรจึงต่ำมาก) แล้วให้ TA ทำงานจริงจนเกินวงเงินนั้น — ไม่แก้ไขวิชาในเส้นทางหลัก แต่ทำให้ขั้นตอนที่ 12 (ปะหน้าจ่ายตรง) ติดชั่วคราวจนกว่าจะแก้ไขวิชาทดสอบนี้ให้จบ หรือกดรีเซ็ตห้องทดลอง — เพราะปะหน้าจ่ายตรงต้องรอทุกวิชาในภาคเรียนพร้อมกัน เหมือนของจริง", RelatedPath: "/staff/budget-summary", ActorRole: "staff"},
	{Key: "worklog_reject", Label: "อาจารย์ตีกลับบันทึกเวลา", Description: "อาจารย์วิชาที่ 1 ตีกลับบันทึกเวลาของ TA กลับไปแก้ไข — ต้องทำขั้นตอนที่ 9 ในเส้นทางหลักก่อน (ถ้าอนุมัติไปแล้วจะดึงกลับมาให้ตีกลับเอง) แต่ถ้าส่งออกไฟล์/ส่งการเงินแล้ว (ขั้นตอนที่ 11) จะแก้ไขย้อนหลังไม่ได้ตามกติกาจริง", RelatedPath: "/staff/payouts", ActorRole: "staff"},
	{Key: "submission_deadline_missed", Label: "TA ไม่ส่งเอกสารจนงวดปิด", Description: "จำลองเดือนก่อนหน้าที่ปิดรอบไปแล้ว แล้วให้ TA พยายามส่งเวลาย้อนหลัง — ระบบต้องปฏิเสธ ไม่มีการอนุโลม (มีผลข้างเคียงกับขั้นตอนที่ 12 เหมือนเคสงบเกินวงเงินด้านบน)", RelatedPath: "/staff/settings?tab=calendar", ActorRole: "staff"},
	{Key: "docs_reject", Label: "เจ้าหน้าที่ตีกลับเอกสาร TA", Description: "เจ้าหน้าที่ตีกลับเอกสาร 1 ฉบับของ TA วิชาที่ 1 (เช่น ไฟล์ไม่ชัด) — ต้องทำขั้นตอนที่ 7 ในเส้นทางหลักก่อน (ถ้าอนุมัติไปแล้วจะดึงกลับมาให้ตีกลับเอง) TA ต้องอัปโหลดใหม่แล้วให้ทำขั้นตอนที่ 8 ซ้ำ", RelatedPath: "/staff/review", ActorRole: "staff"},
	{Key: "staff_cannot_unlock", Label: "เจ้าหน้าที่ปลดล็อกวิชาที่ส่งออกแล้วไม่ได้", Description: "จำลองการเรียก API ปลดล็อกวิชาที่ 1 (ต้องทำขั้นตอนที่ 11 ก่อน) ด้วยสิทธิ์เจ้าหน้าที่ — ต้องถูกปฏิเสธ แล้วลองซ้ำด้วยสิทธิ์ผู้ดูแลระบบ — ต้องสำเร็จ", RelatedPath: "/staff/payouts", ActorRole: "staff"},
}

// ProblemEvents lists every problem-case event, for the panel's second list.
func ProblemEvents() []ScenarioEvent { return problemEvents }

// RunProblemEvent dispatches by key, same shape as RunScenarioEvent.
// app/basePath are only needed by staff_cannot_unlock, which is the one
// problem case that has to go through a real HTTP round trip to observe an
// HTTP-layer permission check (RequireRole on the route) — every other
// event calls its service method directly like scenario_steps.go does. See
// problemStaffCannotUnlock's doc comment for why that one case is different.
func RunProblemEvent(ctx context.Context, svc *service.Container, app *fiber.App, tokens *auth.TokenService, basePath, key string) (string, error) {
	switch key {
	case "worklog_over_cap":
		return problemWorkLogOverCap(ctx, svc)
	case "budget_over_cap":
		return problemBudgetOverCap(ctx, svc)
	case "worklog_reject":
		return problemWorkLogReject(ctx, svc)
	case "submission_deadline_missed":
		return problemDeadlineMissed(ctx, svc)
	case "docs_reject":
		return problemDocsReject(ctx, svc)
	case "staff_cannot_unlock":
		return problemStaffCannotUnlock(ctx, svc, app, tokens, basePath)
	default:
		return "", fmt.Errorf("demo: unknown problem event %q", key)
	}
}

// ---- 1. TA logs hours over the daily cap -----------------------------

func problemWorkLogOverCap(ctx context.Context, svc *service.Container) (string, error) {
	taID, err := userIDByEmail(ctx, svc, courseSeeds[0].taEmail)
	if err != nil {
		return "", err
	}
	assignmentID, err := assignmentIDFor(ctx, svc, courseSeeds[0].code, taID)
	if err != nil {
		return "", err
	}

	// A date certain not to collide with the happy path's own generated
	// entries (those land on Mondays only — see stepCourses) — any Tuesday
	// inside the course's date range is free.
	var workDate string
	if err := svc.Pool.QueryRow(ctx, `
		SELECT to_char(d, 'YYYY-MM-DD') FROM (
			SELECT generate_series(tc.starts_on, tc.ends_on, interval '1 day') AS d
			FROM ta_request_assignments a
			JOIN sections sec ON sec.id = a.section_id
			JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
			WHERE a.id = $1
		) days
		WHERE EXTRACT(ISODOW FROM d) = 2
		  AND NOT EXISTS (SELECT 1 FROM work_logs WHERE assignment_id = $1 AND work_date = d)
		ORDER BY d LIMIT 1`, assignmentID,
	).Scan(&workDate); err != nil {
		return "", fmt.Errorf("demo: หาไม่พบวันว่างสำหรับทดลอง (%w) — ลองรีเซ็ตห้องทดลอง", err)
	}

	w := service.WorkLog{
		AssignmentID: assignmentID,
		WorkDate:     workDate,
		StartTime:    "08:00",
		EndTime:      "17:00", // 9 hours, straddling a lunch break the UI would normally warn about too
		Hours:        9,
		Activity:     "lecture",
	}
	_, err = svc.WorkLog.Upsert(ctx, taID, w)
	if err == nil {
		return "", errors.New("demo: ระบบยอมรับบันทึกเวลาที่เกินเพดานโดยไม่ควร — นี่คือบั๊ก ไม่ใช่ผลลัพธ์ที่ถูกต้อง")
	}
	return fmt.Sprintf("ระบบปฏิเสธตามที่ควร: %s", errMessageText(err)), nil
}

// ---- shared: provision a dedicated, isolated problem course -------------

// provisionProblemCourse creates (once) a course outside courseSeeds — code
// must never collide with the happy path's CP100001-3 — with the given
// enrollment and date window, appoints TA 4 to it (same TARequest.Create
// auto-decide path the happy path uses, not a shortcut), and returns the
// resulting course + assignment. dayOfWeek must differ between every problem
// course that uses this helper, since TA 4's own recurring weekly schedule
// is shared across all of them for the whole term regardless of which
// specific dates each course spans.
func provisionProblemCourse(
	ctx context.Context, svc *service.Container,
	code, nameTH string, numStudents, dayOfWeek int, monthsAgo int,
) (courseID, assignmentID uuid.UUID, err error) {
	termID, err := activeTermID(ctx, svc)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	adminID, err := userIDByEmail(ctx, svc, "admin@demo.local")
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	lecturerID, err := userIDByEmail(ctx, svc, courseSeeds[0].lecturerEmail)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	taID, err := userIDByEmail(ctx, svc, "ta4@demo.local")
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}

	err = svc.Pool.QueryRow(ctx,
		`SELECT id FROM teaching_courses WHERE code=$1 AND term_id=$2`, code, termID,
	).Scan(&courseID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, err
	}
	if courseID == uuid.Nil {
		now := timeutil.Now()
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, timeutil.Bangkok).AddDate(0, -monthsAgo, 0)
		monthEnd := monthStart.AddDate(0, 1, -1)

		raw, jerr := json.Marshal(map[string]any{
			"term_id":      termID,
			"code":         code,
			"name_th":      nameTH,
			"level":        "undergrad",
			"credits":      3,
			"lecture_hrs":  3,
			"lab_hrs":      0,
			"self_hrs":     6,
			"starts_on":    isoDate(monthStart),
			"ends_on":      isoDate(monthEnd),
			"num_students": numStudents,
			"lecturer_ids": []uuid.UUID{lecturerID},
			"sections": []map[string]any{
				{
					"sec_no":       "1",
					"track":        "regular",
					"num_students": numStudents,
					"schedules": []map[string]any{
						{"kind": "lecture", "day_of_week": dayOfWeek, "start_time": "09:00", "end_time": "12:00"},
					},
				},
			},
		})
		if jerr != nil {
			return uuid.Nil, uuid.Nil, jerr
		}
		var in service.CreateTeachingCourseInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return uuid.Nil, uuid.Nil, err
		}
		courseID, err = svc.Teaching.Create(ctx, adminID, in)
		if err != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("สร้างวิชาทดสอบ %s: %w", code, err)
		}
	}

	// TA 4 needs its own class-schedule row before a TA request naming it can
	// auto-approve — same checkTASchedule gate stepTARequests relies on.
	var hasSchedule bool
	if err := svc.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM ta_class_schedules WHERE user_id=$1 AND term_id=$2)`, taID, termID,
	).Scan(&hasSchedule); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if !hasSchedule {
		block := service.ClassBlock{
			ID: "demo-own-class", TermID: termID,
			CourseCode: "GE100001", CourseName: "วิชาศึกษาทั่วไป (จำลอง)",
			Kind: "lecture", SecNo: "1", DayOfWeek: 6, StartTime: "15:00", EndTime: "16:00",
		}
		if err := svc.Workload.ReplaceClasses(ctx, taID, termID, []service.ClassBlock{block}); err != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("บันทึกตารางเรียนของ TA 4: %w", err)
		}
	}

	assignmentID, err = assignmentIDFor(ctx, svc, code, taID)
	if err == nil {
		return courseID, assignmentID, nil
	}

	var sectionID uuid.UUID
	if err := svc.Pool.QueryRow(ctx,
		`SELECT id FROM sections WHERE teaching_course_id=$1 LIMIT 1`, courseID,
	).Scan(&sectionID); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	res, err := svc.TARequest.Create(ctx, lecturerID, service.CreateTARequestInput{
		TeachingCourseID: courseID,
		ReimburseScope:   "lecture",
		Assignments: []service.AssignmentInput{{
			SectionIDs: []uuid.UUID{sectionID},
			TAID:       taID,
			Level:      "undergrad",
			Workload:   service.WorkloadInput{CheckWorkHrs: 2, AttendanceHrs: 3},
		}},
	})
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("เสนอชื่อ TA 4 ให้วิชา %s: %w", code, err)
	}
	if res.Status != "approved" {
		return uuid.Nil, uuid.Nil, fmt.Errorf("demo: คำขอ TA สำหรับวิชา %s ไม่ได้รับอนุมัติ (สถานะ %s) — รีเซ็ตห้องทดลองแล้วลองใหม่", code, res.Status)
	}
	assignmentID, err = assignmentIDFor(ctx, svc, code, taID)
	return courseID, assignmentID, err
}

// ---- 2. Course budget over cap -----------------------------------------

// budgetProblemCourse is dated the CURRENT month (monthsAgo=0) — unlike
// deadlineProblemCourse below — because this event needs a normal, OPEN
// submission window to actually submit+approve real hours through; the
// overage it demonstrates comes from an unrealistically tiny enrollment
// (1 student, so the formula-derived cap — see budget.go's Compute doc
// comment, "no more manual budget_caps setting" — comes out tiny too), not
// from a closed period.
const budgetProblemCourse = "CP999001"

func problemBudgetOverCap(ctx context.Context, svc *service.Container) (string, error) {
	courseID, assignmentID, err := provisionProblemCourse(
		ctx, svc, budgetProblemCourse, "วิชาทดสอบงบประมาณเกิน (จำลอง)", 1, 1, 0)
	if err != nil {
		return "", err
	}
	taID, err := userIDByEmail(ctx, svc, "ta4@demo.local")
	if err != nil {
		return "", err
	}
	lecturerID, err := userIDByEmail(ctx, svc, courseSeeds[0].lecturerEmail)
	if err != nil {
		return "", err
	}

	var already int
	if err := svc.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM work_logs WHERE assignment_id=$1`, assignmentID,
	).Scan(&already); err != nil {
		return "", err
	}
	if already == 0 {
		if _, err := svc.WorkLog.Generate(ctx, taID, assignmentID); err != nil {
			return "", fmt.Errorf("สร้างบันทึกเวลาให้วิชาทดสอบ: %w", err)
		}
		if err := svc.WorkLog.Submit(ctx, taID, assignmentID); err != nil {
			return "", fmt.Errorf("ส่งอนุมัติบันทึกเวลาของวิชาทดสอบ: %w", err)
		}
		if err := svc.WorkLog.Approve(ctx, lecturerID, assignmentID, "", false); err != nil {
			return "", fmt.Errorf("อนุมัติบันทึกเวลาของวิชาทดสอบ: %w", err)
		}
	}

	snap, err := svc.Budget.Compute(ctx, courseID)
	if err != nil {
		return "", err
	}
	if snap.OverBudget {
		return fmt.Sprintf(
			"เกินวงเงินจริง: วิชาทดสอบ (1 คน) ใช้ไปแล้ว %.0f บาท จากวงเงินตามสูตรที่คำนวณได้เพียง %.0f บาท (เกิน %.0f บาท) — เจ้าหน้าที่จะเห็นคำเตือนนี้ที่หน้า “สรุปงบและเบิกจ่ายจ่ายตรง”",
			snap.UsedBaht, snap.PerCourseMaxBaht, snap.UsedBaht-snap.PerCourseMaxBaht,
		), nil
	}
	return fmt.Sprintf(
		"วิชาทดสอบ (1 คน) ใช้ไป %.0f บาท จากวงเงิน %.0f บาท — ยังไม่เกิน คงเหลือ %.0f บาท (ลองกดซ้ำอีกครั้งหลังจากนี้ เผื่อชั่วโมงสะสมเพิ่ม)",
		snap.UsedBaht, snap.PerCourseMaxBaht, snap.RemainingBaht,
	), nil
}

// ---- 3. Lecturer rejects a submitted worklog ---------------------------

func problemWorkLogReject(ctx context.Context, svc *service.Container) (string, error) {
	lecturerID, err := userIDByEmail(ctx, svc, courseSeeds[0].lecturerEmail)
	if err != nil {
		return "", err
	}
	taID, err := userIDByEmail(ctx, svc, courseSeeds[0].taEmail)
	if err != nil {
		return "", err
	}
	assignmentID, err := assignmentIDFor(ctx, svc, courseSeeds[0].code, taID)
	if err != nil {
		return "", err
	}
	var pending int
	if err := svc.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM work_logs WHERE assignment_id=$1 AND status='submitted'`, assignmentID,
	).Scan(&pending); err != nil {
		return "", err
	}
	note := ""
	var revertedID uuid.UUID
	// A visitor who already ran the WHOLE happy path (steps 9-10) has
	// nothing left in 'submitted' — step 10 approved every one of them. Undo
	// exactly one row's approval directly (there is no service method for
	// this; it is not a real operation, only this demo's way of getting back
	// to the state step 10 started from) so the event still has something to
	// reject regardless of click order. Reject below still re-validates
	// through the real service call, so if the month has since been
	// exported/sent to finance (step 11), assertNoFinanceLockedRows still
	// correctly refuses — that lock is real and this demo does not bypass
	// it; on that refusal the revert below is undone so this event never
	// leaves a row sitting in 'submitted' while its month is finance-locked,
	// a state the real system never produces on its own.
	if pending == 0 {
		if err := svc.Pool.QueryRow(ctx, `
			UPDATE work_logs SET status='submitted'
			WHERE id = (SELECT id FROM work_logs WHERE assignment_id=$1 AND status='approved' LIMIT 1)
			RETURNING id`,
			assignmentID,
		).Scan(&revertedID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", errors.New("demo: ไม่มีบันทึกเวลาของวิชาที่ 1 เลยให้ตีกลับ — ทำขั้นตอนที่ 9 ในเส้นทางหลักก่อน")
			}
			return "", err
		}
		note = " (ดึงรายการที่อนุมัติไปแล้ว 1 รายการกลับมาเป็น “รออนุมัติ” ก่อน เพื่อให้มีของให้ตีกลับ)"
	}
	if err := svc.WorkLog.Reject(ctx, lecturerID, assignmentID, "กรุณาตรวจสอบชั่วโมงให้ตรงกับตารางสอนอีกครั้ง (เหตุผลจำลอง)", "", false); err != nil {
		if revertedID != uuid.Nil {
			if _, undoErr := svc.Pool.Exec(ctx, `UPDATE work_logs SET status='approved' WHERE id=$1`, revertedID); undoErr != nil {
				return "", fmt.Errorf("ตีกลับไม่สำเร็จ (%s) และคืนสถานะเดิมไม่สำเร็จซ้อน (%s) — ลองรีเซ็ตห้องทดลอง", errMessageText(err), undoErr)
			}
			return "", fmt.Errorf("%s — วิชาที่ 1 ถูกส่งออกไฟล์/ส่งการเงินไปแล้ว จึงแก้ไขย้อนหลังไม่ได้ตามกติกาจริง ลองรีเซ็ตห้องทดลองแล้วทำใหม่โดยไม่ต้องทำถึงขั้นตอนที่ 11 ก่อน", errMessageText(err))
		}
		return "", err
	}
	return "อาจารย์ตีกลับบันทึกเวลาแล้ว 1 รายการ" + note + " — สถานะเปลี่ยนเป็น “ถูกตีกลับ” TA ต้องแก้ไขแล้วกดส่งอนุมัติใหม่ (ทำขั้นตอนที่ 9 ซ้ำได้ทันที เพราะ Submit ส่งรายการที่ถูกตีกลับซ้ำได้)", nil
}

// ---- 4. TA misses the submission deadline ------------------------------

// deadlineProblemCourse is dated ENTIRELY in the previous calendar month
// (monthsAgo=1) — a different weekday from budgetProblemCourse (Wednesday
// vs Monday) so TA 4's recurring weekly schedule never collides between the
// two problem courses.
const deadlineProblemCourse = "CP999002"

func problemDeadlineMissed(ctx context.Context, svc *service.Container) (string, error) {
	termID, err := activeTermID(ctx, svc)
	if err != nil {
		return "", err
	}
	courseID, assignmentID, err := provisionProblemCourse(
		ctx, svc, deadlineProblemCourse, "วิชาทดสอบพ้นกำหนดส่ง (จำลอง)", 30, 3, 1)
	if err != nil {
		return "", err
	}
	taID, err := userIDByEmail(ctx, svc, "ta4@demo.local")
	if err != nil {
		return "", err
	}
	adminID, err := userIDByEmail(ctx, svc, "admin@demo.local")
	if err != nil {
		return "", err
	}

	// Generate FIRST, while the month's submission period does not exist yet
	// (WorkLogService.Generate silently skips any month whose period is
	// already closed/locked — see loadBlockedMonths — so generating AFTER
	// closing the period below would always produce zero rows). This order
	// mirrors the real sequence anyway: the TA had the whole month to log
	// hours; only afterward does staff close the books on it.
	var already int
	if err := svc.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM work_logs WHERE assignment_id=$1`, assignmentID,
	).Scan(&already); err != nil {
		return "", err
	}
	if already == 0 {
		if _, err := svc.WorkLog.Generate(ctx, taID, assignmentID); err != nil {
			return "", fmt.Errorf("สร้างบันทึกเวลาย้อนหลัง (จำลอง): %w", err)
		}
	}

	now := timeutil.Now()
	lastMonth := now.AddDate(0, -1, 0)
	ym := fmt.Sprintf("%d-%02d", lastMonth.Year()+543, int(lastMonth.Month()))
	monthStart := time.Date(lastMonth.Year(), lastMonth.Month(), 1, 0, 0, 0, 0, timeutil.Bangkok)

	var periodID uuid.UUID
	err = svc.Pool.QueryRow(ctx,
		`SELECT id FROM submission_periods WHERE term_id=$1 AND year_month=$2`, termID, ym,
	).Scan(&periodID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if periodID == uuid.Nil {
		// Already overdue AND closed — this event exists to show the
		// forfeiture rule, not to open a real editable window.
		if _, err := svc.SubmissionPeriods.Upsert(ctx, adminID, service.SubmissionPeriod{
			TermID:           termID,
			YearMonth:        ym,
			StartsOn:         isoDate(monthStart),
			DueDate:          isoDate(monthStart.AddDate(0, 0, 5)),
			Label:            fmt.Sprintf("รอบส่งเดือน %s (จำลอง — ปิดแล้ว)", ym),
			RemindDaysBefore: 3,
			IsClosed:         true,
		}); err != nil {
			return "", fmt.Errorf("เปิดรอบส่งย้อนหลัง (จำลอง): %w", err)
		}
	}

	err = svc.WorkLog.Submit(ctx, taID, assignmentID)
	if err == nil {
		return "", fmt.Errorf("demo: ระบบยอมให้ส่งย้อนหลังในงวดที่ปิดแล้วโดยไม่ควร — นี่คือบั๊ก ไม่ใช่ผลลัพธ์ที่ถูกต้อง (courseID=%s)", courseID)
	}
	return fmt.Sprintf("ระบบปฏิเสธตามที่ควร: %s", errMessageText(err)), nil
}

// errMessageText extracts the Thai text callers should show from a service
// error — the same UserError/plain-error shapes handler.ErrorHandler
// unwraps for an HTTP response, needed here because these two problem
// events treat "the system correctly refused" as their OWN success message,
// not a failure to propagate as-is.
func errMessageText(err error) string {
	var ue *service.UserError
	if errors.As(err, &ue) {
		return ue.Msg
	}
	return err.Error()
}

// ---- 5. Staff rejects a TA's submitted document ------------------------

func problemDocsReject(ctx context.Context, svc *service.Container) (string, error) {
	staffID, err := userIDByEmail(ctx, svc, "staff@demo.local")
	if err != nil {
		return "", err
	}
	taID, err := userIDByEmail(ctx, svc, courseSeeds[0].taEmail)
	if err != nil {
		return "", err
	}
	var docID uuid.UUID
	var kind string
	err = svc.Pool.QueryRow(ctx, `
		SELECT id, kind FROM ta_documents
		WHERE user_id=$1 AND status='submitted' AND superseded_at IS NULL LIMIT 1`,
		taID,
	).Scan(&docID, &kind)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	note := ""
	if docID == uuid.Nil {
		// Every doc already approved (step 8 ran) — undo exactly one so
		// there is something to reject, same reasoning as
		// problemWorkLogReject's revert-then-reject.
		if err := svc.Pool.QueryRow(ctx, `
			UPDATE ta_documents SET status='submitted'
			WHERE id = (SELECT id FROM ta_documents WHERE user_id=$1 AND status='approved' AND superseded_at IS NULL LIMIT 1)
			RETURNING id, kind`,
			taID,
		).Scan(&docID, &kind); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", errors.New("demo: TA วิชาที่ 1 ยังไม่มีเอกสารเลย — ทำขั้นตอนที่ 7 ในเส้นทางหลักก่อน")
			}
			return "", err
		}
		note = " (ดึงเอกสารที่อนุมัติไปแล้ว 1 ฉบับกลับมาก่อน เพื่อให้มีของให้ตีกลับ)"
	}
	if err := svc.Docs.RejectBatch(ctx, staffID, taID, []service.RejectItem{
		{DocID: docID, Reason: "ไฟล์ไม่ชัดเจน กรุณาอัปโหลดใหม่ (เหตุผลจำลอง)"},
	}); err != nil {
		return "", err
	}
	return fmt.Sprintf("เจ้าหน้าที่ตีกลับเอกสาร %s ของ TA วิชาที่ 1 แล้ว%s — สถานะโปรไฟล์เปลี่ยนเป็น “ต้องแก้ไข” TA ต้องอัปโหลดใหม่ก่อนเจ้าหน้าที่จะอนุมัติซ้ำได้ (ทำขั้นตอนที่ 8 ซ้ำหลัง TA อัปโหลดแล้ว)", kindLabelDemo(kind), note), nil
}

// kindLabelDemo mirrors service.kindLabel (unexported, ta_documents.kind →
// Thai display name) — duplicated rather than exported from service purely
// for this one result message; not worth widening that package's surface
// for a label string.
func kindLabelDemo(kind string) string {
	switch kind {
	case "national_id":
		return "บัตรประชาชน"
	case "bank_book":
		return "สมุดบัญชี"
	case "creditor_form":
		return "แบบฟอร์มเจ้าหนี้"
	}
	return kind
}

// ---- 6. Staff cannot reopen an exported course; admin can --------------

// problemStaffCannotUnlock is the one problem case that is NOT a direct
// service call: the rule it demonstrates — "only admin may reopen an
// exported course" — lives in the HTTP route's RequireRole(rbac.RoleAdmin)
// middleware (internal/handler/router.go), not inside
// TeachingService.Unexport itself (services here enforce business rules;
// routes enforce role). Every other problem case in this file calls its
// service method directly, same as scenario_steps.go, and would never see
// that middleware at all. Demonstrating it faithfully means actually going
// through it: fire two real HTTP requests at this slot's own mounted routes
// via Fiber's in-process app.Test (no socket, no separate process) — one
// with a staff token, one with an admin token — and show that only the
// second one gets through.
//
// Both tokens are minted for THIS run only, each backed by its own throwaway
// sessions row inserted directly (not via Sessions.CreateAndSupersede, which
// would revoke every OTHER active session for that user — including
// whichever real device is currently logged in as staff@demo.local or
// admin@demo.local, quite possibly the very browser tab that clicked this
// button). Both rows are revoked again before returning, success or not.
func problemStaffCannotUnlock(ctx context.Context, svc *service.Container, app *fiber.App, tokens *auth.TokenService, basePath string) (string, error) {
	if app == nil {
		return "", errors.New("demo: ไม่พร้อมใช้งานเคสนี้ในสภาพแวดล้อมนี้")
	}
	termID, err := activeTermID(ctx, svc)
	if err != nil {
		return "", err
	}
	var courseID uuid.UUID
	var exported bool
	if err := svc.Pool.QueryRow(ctx,
		`SELECT id, exported_at IS NOT NULL FROM teaching_courses WHERE term_id=$1 AND code=$2`,
		termID, courseSeeds[0].code,
	).Scan(&courseID, &exported); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errors.New("demo: ยังไม่ได้เปิดวิชาที่ 1 — ทำขั้นตอนที่ 2 ในเส้นทางหลักก่อน")
		}
		return "", err
	}
	if !exported {
		return "", errors.New("demo: วิชาที่ 1 ยังไม่ได้ส่งออกไฟล์ — ทำขั้นตอนที่ 11 ในเส้นทางหลักก่อน (ต้องมีอะไรให้ปลดล็อกก่อน)")
	}

	staffID, err := userIDByEmail(ctx, svc, "staff@demo.local")
	if err != nil {
		return "", err
	}
	adminID, err := userIDByEmail(ctx, svc, "admin@demo.local")
	if err != nil {
		return "", err
	}

	path := fmt.Sprintf("%s/exports/course/%s/unlock", basePath, courseID)

	staffStatus, staffBody, err := unlockAs(ctx, svc, app, tokens, path, staffID, []string{"staff"})
	if err != nil {
		return "", err
	}
	if staffStatus != http.StatusForbidden {
		return "", fmt.Errorf("demo: คาดว่าเจ้าหน้าที่จะถูกปฏิเสธ (403) แต่ได้ %d แทน (%s) — นี่คือบั๊ก ไม่ใช่ผลลัพธ์ที่ถูกต้อง", staffStatus, staffBody)
	}

	adminStatus, adminBody, err := unlockAs(ctx, svc, app, tokens, path, adminID, []string{"admin", "staff"})
	if err != nil {
		return "", err
	}
	if adminStatus != http.StatusOK {
		return "", fmt.Errorf("demo: คาดว่าผู้ดูแลระบบจะปลดล็อกสำเร็จ (200) แต่ได้ %d แทน (%s) — นี่คือบั๊ก ไม่ใช่ผลลัพธ์ที่ถูกต้อง", adminStatus, adminBody)
	}

	return "เจ้าหน้าที่พยายามปลดล็อกวิชาที่ 1 — ถูกปฏิเสธ (403 ไม่มีสิทธิ์) ตามที่ควร แล้วผู้ดูแลระบบลองซ้ำ — สำเร็จ วิชาที่ 1 ถูกปลดล็อกจริงแล้ว (ต้องทำขั้นตอนที่ 11 ซ้ำถ้าต้องการส่งออกใหม่)", nil
}

// unlockAs mints a throwaway session+token for userID/roles, fires the
// unlock request through app.Test, and revokes the session again before
// returning — see problemStaffCannotUnlock's doc comment.
//
// This request goes through AccountGuard like any other, including its
// mandatory-2FA check — but svc here is always slot.Container (see
// RunProblemEvent's caller in handlers.go), whose Cfg.IsDemoSlot is true, so
// that check is skipped the same way it is for every other request in this
// slot. Seeded demo admin/staff accounts never enrol in TOTP, so this would
// break the moment IsDemoSlot stopped being threaded through correctly —
// worth knowing if this stops working after a refactor of how slot Configs
// get built.
func unlockAs(ctx context.Context, svc *service.Container, app *fiber.App, tokens *auth.TokenService, path string, userID uuid.UUID, roles []string) (int, string, error) {
	sessionID := uuid.New()
	if _, err := svc.Pool.Exec(ctx, `
		INSERT INTO sessions (id, user_id, created_at, last_activity_at, expires_at)
		VALUES ($1, $2, NOW(), NOW(), NOW() + INTERVAL '5 minutes')`,
		sessionID, userID); err != nil {
		return 0, "", err
	}
	defer func() { _ = svc.Sessions.Revoke(ctx, sessionID, "demo_scenario_cleanup") }()

	token, err := tokens.Issue(userID, roles, sessionID, 5*time.Minute)
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequest(http.MethodPost, path, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req, 10000)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
}
