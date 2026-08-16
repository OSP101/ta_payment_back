// Package demo's scenario engine drives one academic term through its ENTIRE
// real lifecycle — term → courses → TA nomination → appointment → documents
// → worklog → approval → export → transfer cover — using nothing but the
// same service.* calls the real UI uses. See docs/PLAN-demo-sandbox.md §5.
//
// Every step is idempotent-ish: run twice and the second call reports "ทำไป
// แล้ว" instead of erroring, because the simulator panel has no way to know
// what a visitor already clicked in a previous session. Steps look up their
// inputs by querying the slot's own data (by email, by course code, by
// year-month) rather than threading IDs between calls — a page reload or an
// out-of-order click must not lose the engine's place.
package demo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/service"
	"ta-payment-back/internal/timeutil"
)

// ScenarioEvent is one button in the simulator panel.
type ScenarioEvent struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
	// RelatedPath is the staff page that shows this step's real effect —
	// e.g. after "appointment_order" runs, /staff/appointments now has a
	// document to show. Optional (empty for steps with no dedicated staff
	// screen, like a TA's own schedule confirmation): the frontend only
	// renders a link when it is set. This is the "แผงเกิดอะไรขึ้น" half of
	// the explanation layer — the result message says what changed in
	// words, this link lets staff go SEE it in the actual product instead
	// of taking the message's word for it, which is the whole point of a
	// sandbox that reuses real screens instead of drawing its own.
	RelatedPath string `json:"related_path,omitempty"`
	// ActorRole is which seeded role's account this step is written from
	// the point of view of — "staff", "lecturer", or "ta" (never "admin":
	// every staff-actor step is equally an admin one, since admin's own
	// roles already include "staff" — see seed.go's demoAccounts). Every
	// RelatedPath above happens to be a staff-only page today (staff can
	// see the effect of anyone's step), so this matters specifically for a
	// lecturer- or TA-tier tester (see tiers.go): the frontend compares
	// this against whichever account they're actually logged in as, and
	// hides the "go see the real page" navigation for a step they hold no
	// access to — offering it anyway would just send them into a 403 on a
	// screen that was never going to be theirs to see this session.
	ActorRole string `json:"actor_role"`
}

// scenarioEvents is the "เส้นทางหลัก" (happy path) — see plan doc §5's numbered
// table. Order matters for display only; each step re-derives its own
// prerequisites from the database rather than trusting the caller ran them
// in order, so an out-of-sequence click fails with a clear Thai message
// instead of a confusing crash.
var scenarioEvents = []ScenarioEvent{
	{Key: "term", Label: "1. สร้างปีการศึกษา / ภาคเรียน", Description: "เปิดภาคเรียนใหม่ พร้อมช่วงสอบกลางภาค/ปลายภาค", RelatedPath: "/staff/settings?tab=terms", ActorRole: "staff"},
	{Key: "courses", Label: "2. เปิดรายวิชา + ตารางสอน (อาจารย์ 3 คน)", Description: "สร้าง 3 รายวิชา คนละ 1 วิชา พร้อมตารางบรรยายของแต่ละวิชา", RelatedPath: "/staff/teaching", ActorRole: "staff"},
	{Key: "submission_periods", Label: "3. เปิดรอบส่งค่าตอบแทนเดือนนี้", Description: "เปิดรอบเบิกจ่ายของเดือนปัจจุบัน กำหนดส่งยังไม่ถึง", RelatedPath: "/staff/settings?tab=calendar", ActorRole: "staff"},
	{Key: "ta_schedules", Label: "4. TA บันทึกตารางเรียนของตัวเอง (4 คน)", Description: "TA ทั้ง 4 คนยืนยันว่าตัวเองมีตารางเรียนในภาคเรียนนี้แล้ว", RelatedPath: "/staff/teaching", ActorRole: "ta"},
	{Key: "ta_requests", Label: "5. อาจารย์เสนอชื่อ TA (3 วิชา)", Description: "แต่ละวิชาเสนอชื่อ TA 1 คน ระบบจะตัดสินอนุมัติ/ปฏิเสธอัตโนมัติ", RelatedPath: "/staff/approvals", ActorRole: "lecturer"},
	{Key: "appointment_order", Label: "6. ออกคำสั่งแต่งตั้ง TA", Description: "ออกคำสั่งแต่งตั้งสำหรับ TA ที่ได้รับอนุมัติทั้งหมดในภาคเรียนนี้", RelatedPath: "/staff/appointments", ActorRole: "staff"},
	{Key: "ta_docs", Label: "7. TA ส่งเอกสาร + ข้อมูลส่วนตัว", Description: "TA ที่ได้รับแต่งตั้งกรอกโปรไฟล์และแนบเอกสาร 3 ฉบับ", RelatedPath: "/staff/review", ActorRole: "ta"},
	{Key: "docs_approve", Label: "8. เจ้าหน้าที่อนุมัติเอกสาร", Description: "อนุมัติเอกสารทั้งหมดของ TA ที่ส่งมาในขั้นตอนก่อนหน้า", RelatedPath: "/staff/review", ActorRole: "staff"},
	{Key: "worklog_submit", Label: "9. TA ลงเวลาและส่งอนุมัติ", Description: "สร้างบันทึกเวลาปฏิบัติงานตามตารางสอนของเดือนนี้ แล้วส่งอนุมัติ", RelatedPath: "/staff/payouts", ActorRole: "ta"},
	{Key: "worklog_approve", Label: "10. อาจารย์อนุมัติบันทึกเวลา", Description: "อาจารย์อนุมัติบันทึกเวลาที่ TA ส่งมา", RelatedPath: "/staff/payouts", ActorRole: "lecturer"},
	{Key: "staff_review_export", Label: "11. เจ้าหน้าที่ตรวจสอบ + ส่งออกเอกสาร + ปิดงวด", Description: "ตรวจสอบ, ส่งออกไฟล์จ่ายเงินรายวิชา, ล็อกเดือน, และส่งเรื่องให้การเงิน", RelatedPath: "/staff/payouts", ActorRole: "staff"},
	{Key: "transfer_cover", Label: "12. สรุปงบและออกปะหน้าจ่ายตรง", Description: "ออกเอกสารปะหน้าจ่ายตรงของเดือนนี้ทั้งภาคเรียน", RelatedPath: "/staff/budget-summary", ActorRole: "staff"},
}

// courseSeeds is the fixed 1:1:1 lecturer/course/TA mapping the scenario
// engine drives — see seed.go's demoAccounts doc comment. TA 4 is
// deliberately left out: an unassigned TA is itself a state worth seeing.
var courseSeeds = []struct {
	code, nameTH  string
	lecturerEmail string
	taEmail       string
}{
	{"CP100001", "หลักการเขียนโปรแกรมเบื้องต้น (จำลอง)", "lecturer1@demo.local", "ta1@demo.local"},
	{"CP100002", "โครงสร้างข้อมูลและขั้นตอนวิธี (จำลอง)", "lecturer2@demo.local", "ta2@demo.local"},
	{"CP100003", "ระบบฐานข้อมูล (จำลอง)", "lecturer3@demo.local", "ta3@demo.local"},
}

// RunScenarioEvent dispatches by key. actorEmail identifies who is
// "pretending" to perform the step (an admin for staff-only steps works for
// all of them) — kept separate from the per-step actual actor below because
// several steps (TA docs, worklog, lecturer approval) MUST run as the
// specific TA/lecturer the action belongs to, not whoever clicked the panel
// button, or the service layer's ownership checks (assertTAOwnsAssignment,
// courseAccess, ...) would reject it as someone acting on another person's
// behalf.
func RunScenarioEvent(ctx context.Context, svc *service.Container, key string) (string, error) {
	switch key {
	case "term":
		return stepTerm(ctx, svc)
	case "courses":
		return stepCourses(ctx, svc)
	case "submission_periods":
		return stepSubmissionPeriods(ctx, svc)
	case "ta_schedules":
		return stepTASchedules(ctx, svc)
	case "ta_requests":
		return stepTARequests(ctx, svc)
	case "appointment_order":
		return stepAppointmentOrder(ctx, svc)
	case "ta_docs":
		return stepTADocs(ctx, svc)
	case "docs_approve":
		return stepDocsApprove(ctx, svc)
	case "worklog_submit":
		return stepWorkLogSubmit(ctx, svc)
	case "worklog_approve":
		return stepWorkLogApprove(ctx, svc)
	case "staff_review_export":
		return stepStaffReviewExport(ctx, svc)
	case "transfer_cover":
		return stepTransferCover(ctx, svc)
	default:
		return "", fmt.Errorf("demo: unknown scenario event %q", key)
	}
}

// ---- helpers -------------------------------------------------------------

func userIDByEmail(ctx context.Context, svc *service.Container, email string) (uuid.UUID, error) {
	var id uuid.UUID
	err := svc.Pool.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, email).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("demo: seed account %s not found — reset the workspace and try again", email)
	}
	return id, err
}

// isoDate formats t as "YYYY-MM-DD" in Bangkok civil time — the string shape
// every *string date field in this codebase's inputs expects.
func isoDate(t time.Time) string {
	return t.In(timeutil.Bangkok).Format("2006-01-02")
}

// thaiLongDate formats t as "2 มกราคม 2569" — what AppointmentOrderInput's
// OrderDate/EffectiveDate fields expect (see appointment_order.go's
// thaiGovDate, which reformats an ISO date the UI's date picker would send;
// the raw Thai form also passes straight through unchanged).
var thaiMonths = [...]string{"", "มกราคม", "กุมภาพันธ์", "มีนาคม", "เมษายน", "พฤษภาคม", "มิถุนายน",
	"กรกฎาคม", "สิงหาคม", "กันยายน", "ตุลาคม", "พฤศจิกายน", "ธันวาคม"}

func thaiLongDate(t time.Time) string {
	t = t.In(timeutil.Bangkok)
	return fmt.Sprintf("%d %s %d", t.Day(), thaiMonths[int(t.Month())], t.Year()+543)
}

// currentYearMonth returns the Buddhist-era "YYYY-MM" submission_periods key
// for "now" — every submission_period/finance step (MarkStaffReviewed,
// MarkCourseExported, MarkFinanceSent) keys ITS PERIOD LOOKUP on this. See
// submission_period.go's BulkCreateForTerm.
func currentYearMonth() string {
	n := timeutil.Now()
	return fmt.Sprintf("%d-%02d", n.Year()+543, int(n.Month()))
}

// currentGregorianYearMonth is "now" in the OTHER calendar this codebase
// speaks — see term_months.go's package doc: work_logs.work_date,
// export_batches.months, and BuildTransferCoverWorkbook's months param are
// all plain Gregorian "YYYY-MM", never the Buddhist submission_periods key.
// Passing currentYearMonth()'s value where this one belongs looks right (a
// title-cased year does not visibly announce itself as wrong) but resolves
// to a month 543 years from now that no course ever claims a cent in.
func currentGregorianYearMonth() string {
	n := timeutil.Now()
	return fmt.Sprintf("%04d-%02d", n.Year(), int(n.Month()))
}

// minimalPDF is a tiny but structurally valid single-page PDF — enough to
// pass DocsService.Upload's PDF check and be openable by anything that later
// inspects it (e.g. pdfcpu merges, internal/watermark.Apply on the TA-review
// preview path — see watermark_middleware.go), without shipping a real
// scanned document into a sandbox that must never carry anything resembling
// real PII. Needs the xref table and startxref/trailer below even though
// most casual PDF viewers tolerate their absence via brute-force object
// scanning — pdfcpu does not: without them it fails to resolve the page
// tree with "cannot dereference pageNodeDict", turning every seeded TA
// document into one the review-preview watermark endpoint 400s on.
// Confirmed against internal/watermark.Apply directly, not just "looks
// right" — see that package's own tests for the same shape used here.
const minimalPDF = "%PDF-1.4\n" +
	"1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n" +
	"2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n" +
	"3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 200]>>endobj\n" +
	"xref\n" +
	"0 4\n" +
	"0000000000 65535 f \n" +
	"0000000009 00000 n \n" +
	"0000000052 00000 n \n" +
	"0000000101 00000 n \n" +
	"trailer<</Size 4/Root 1 0 R>>\n" +
	"startxref\n" +
	"164\n" +
	"%%EOF"

// minimalSignatureSVG is a placeholder signature — DocsService.UpsertProfile
// requires SignatureSVG non-empty but never validates it's a real signature.
const minimalSignatureSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 200 60"><path d="M10 40 Q 60 10 100 40 T 190 40" stroke="black" fill="none" stroke-width="2"/></svg>`
