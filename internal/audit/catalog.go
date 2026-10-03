package audit

import (
	"sort"
	"strings"
)

// catalog.go is the one place that says what an audit action MEANS: the words
// a person reads for it, which part of the system it belongs to, how loudly it
// should read, and whether it records a success or a refusal.
//
// This used to live in the frontend (app/staff/audit/vocabulary.ts), next to
// the screen. That left the server unable to answer any question phrased in
// those terms — "only the failures", "only money", "what needs a second look"
// — because the server did not know which actions those were, and it meant a
// newly audited action appeared on screen as its raw identifier until somebody
// remembered the other repository. About 120 of the action names in this
// codebase had no wording of their own.
//
// TestCatalogCoversEveryAuditedAction reads the source tree and fails when an
// action is written without an entry here, so the two cannot drift again.

// Category is the part of the system an action belongs to, in the terms the
// people reading the trail use.
type Category string

const (
	CatAccess  Category = "access"  // signing in and out
	CatAccount Category = "account" // accounts, roles, credentials, PDPA
	CatHours   Category = "hours"   // work logs and the timetable that prices them
	CatPayment Category = "payment" // periods, exports, rates, budgets, sign-off
	CatDocs    Category = "docs"    // TA profile and documents
	CatCourse  Category = "course"  // courses, sections, terms, requests
	CatComms   Category = "comms"   // announcements and reminders
	CatView    Category = "view"    // somebody looked at or downloaded something
	CatSystem  Category = "system"  // settings, housekeeping, the trail itself
)

// Severity is how loudly a row should read.
//
//   - danger: someone was refused or locked out, or something was destroyed.
//   - warn:   a decision already taken was undone or overridden, or access
//     to an account changed. Worth a second look, not a failure.
//   - notice: a sensitive disclosure — somebody read personal data.
//   - info:   ordinary work.
type Severity string

const (
	SevInfo   Severity = "info"
	SevNotice Severity = "notice"
	SevWarn   Severity = "warn"
	SevDanger Severity = "danger"
)

// Outcome is whether the thing the row describes actually happened.
type Outcome string

const (
	OutcomeOK     Outcome = "ok"
	OutcomeFailed Outcome = "failed" // attempted and did not succeed
	OutcomeDenied Outcome = "denied" // refused for lack of permission
)

// ActionInfo is everything the catalog knows about one action.
type ActionInfo struct {
	// Label is a verb phrase that completes "<somebody> …".
	Label    string   `json:"label"`
	Category Category `json:"category"`
	Severity Severity `json:"severity"`
	Outcome  Outcome  `json:"outcome"`
	// System marks actions the system raises by itself (a scheduled job, an
	// automatic decision). A row with no actor is "the system" only for these;
	// for anything else a missing actor means nobody recorded one.
	System bool `json:"system,omitempty"`
	// Known is false for an action that has no entry of its own and was
	// described from its family prefix.
	Known bool `json:"-"`
}

func ok(label string, c Category) ActionInfo {
	return ActionInfo{Label: label, Category: c, Severity: SevInfo, Outcome: OutcomeOK, Known: true}
}
func warn(label string, c Category) ActionInfo {
	return ActionInfo{Label: label, Category: c, Severity: SevWarn, Outcome: OutcomeOK, Known: true}
}
func danger(label string, c Category) ActionInfo {
	return ActionInfo{Label: label, Category: c, Severity: SevDanger, Outcome: OutcomeOK, Known: true}
}
func failed(label string, c Category) ActionInfo {
	return ActionInfo{Label: label, Category: c, Severity: SevDanger, Outcome: OutcomeFailed, Known: true}
}
func view(label string) ActionInfo {
	return ActionInfo{Label: label, Category: CatView, Severity: SevInfo, Outcome: OutcomeOK, Known: true}
}
func sensitive(label string) ActionInfo {
	return ActionInfo{Label: label, Category: CatView, Severity: SevNotice, Outcome: OutcomeOK, Known: true}
}
func system(i ActionInfo) ActionInfo { i.System = true; return i }

var catalog = map[string]ActionInfo{
	// ── Signing in ──────────────────────────────────────────────────────────
	"auth.login":                 ok("เข้าสู่ระบบ", CatAccess),
	"auth.login_failed":          failed("เข้าสู่ระบบไม่สำเร็จ (รหัสผ่านผิด)", CatAccess),
	"auth.login_unknown_account": failed("พยายามเข้าสู่ระบบด้วยอีเมลที่ไม่มีในระบบ", CatAccess),
	"auth.login_locked":          failed("ถูกล็อกบัญชีชั่วคราว (รหัสผ่านผิดหลายครั้ง)", CatAccess),
	"auth.2fa_ok":                ok("ยืนยันตัวตน 2 ชั้นผ่าน", CatAccess),
	"auth.2fa_failed":            failed("ยืนยันตัวตน 2 ชั้นไม่ผ่าน (รหัสผิด)", CatAccess),
	"auth.2fa_locked":            failed("ถูกล็อกการยืนยัน 2 ชั้น (รหัสผิดหลายครั้ง)", CatAccess),
	"auth.2fa_recovery_used":     warn("เข้าสู่ระบบด้วยรหัสสำรองแทนการยืนยัน 2 ชั้น", CatAccess),
	"auth.sso_matched":           ok("เข้าสู่ระบบผ่าน KKU SSO", CatAccess),
	"auth.sso_rejected":          failed("เข้าสู่ระบบผ่าน KKU SSO ไม่สำเร็จ", CatAccess),
	"auth.sso_unknown_account":   failed("พยายามเข้าผ่าน KKU SSO ด้วยบัญชีที่ไม่มีในระบบ", CatAccess),
	"access.denied": {Label: "พยายามใช้งานส่วนที่ไม่มีสิทธิ์", Category: CatAccess,
		Severity: SevDanger, Outcome: OutcomeDenied, Known: true},

	// ── Accounts, roles, credentials ────────────────────────────────────────
	"user.create":                   ok("สร้างบัญชีผู้ใช้", CatAccount),
	"user.update":                   warn("แก้ไขข้อมูลบัญชีหรือสิทธิ์", CatAccount),
	"user.activate":                 ok("เปิดใช้งานบัญชี", CatAccount),
	"user.deactivate":               warn("ปิดใช้งานบัญชี", CatAccount),
	"user.reset_password":           warn("รีเซ็ตรหัสผ่านให้ผู้ใช้", CatAccount),
	"user.temp_password_retired":    ok("ตั้งรหัสผ่านใหม่แทนรหัสชั่วคราว", CatAccount),
	"user.password_gate_unlock":     ok("ยืนยันรหัสผ่านเพื่อปลดล็อกข้อมูลอ่อนไหว", CatAccount),
	"user.2fa_enabled":              ok("เปิดการยืนยันตัวตน 2 ชั้น", CatAccount),
	"user.2fa_disabled":             warn("ปิดการยืนยันตัวตน 2 ชั้น", CatAccount),
	"user.2fa_reset":                warn("ล้างการยืนยันตัวตน 2 ชั้นของผู้ใช้", CatAccount),
	"user.2fa_recovery_regenerated": ok("สร้างรหัสสำรองชุดใหม่", CatAccount),
	"user.avatar.set":               ok("เปลี่ยนรูปโปรไฟล์", CatAccount),
	"user.avatar.clear":             ok("ลบรูปโปรไฟล์", CatAccount),
	"user.pdpa_consent":             ok("ให้ความยินยอมตามนโยบายข้อมูลส่วนบุคคล", CatAccount),
	"user.pdpa_erasure":             danger("ลบข้อมูลส่วนบุคคลตามคำขอ", CatAccount),
	"user.pdpa_erasure_incomplete": {Label: "ลบข้อมูลส่วนบุคคลไม่ครบถ้วน", Category: CatAccount,
		Severity: SevDanger, Outcome: OutcomeFailed, Known: true},
	"user.data_export":                    ok("ดาวน์โหลดข้อมูลส่วนตัวของตนเอง", CatAccount),
	"ta_deletion_request.submit":          ok("ยื่นคำขอลบข้อมูลส่วนบุคคล", CatAccount),
	"ta_deletion_request.approve":         warn("อนุมัติคำขอลบข้อมูลส่วนบุคคล", CatAccount),
	"ta_deletion_request.reject":          ok("ปฏิเสธคำขอลบข้อมูลส่วนบุคคล", CatAccount),
	"admin_officer.reassign":              warn("เปลี่ยนตัวผู้ดำรงตำแหน่งบริหาร", CatAccount),
	"ta_enrollment.transition":            ok("บันทึกการเปลี่ยนระดับการศึกษาของผู้ช่วยสอน", CatAccount),
	"ta_profile.identity_correct":         warn("แก้รหัสนักศึกษา/เลขบัตร/คำนำหน้าที่ผู้ช่วยสอนกรอกผิด", CatAccount),
	"ta_profile.creditor_form_regenerate": warn("สร้างแบบฟอร์มเจ้าหนี้ใหม่จากข้อมูลที่แก้ไข", CatAccount),

	// ── Hours ───────────────────────────────────────────────────────────────
	"worklog.create":             ok("เพิ่มรายการชั่วโมง", CatHours),
	"worklog.update":             ok("แก้ไขรายการชั่วโมง", CatHours),
	"worklog.delete":             ok("ลบรายการชั่วโมง", CatHours),
	"worklog.generate":           ok("สร้างรายการชั่วโมงจากตารางสอน", CatHours),
	"worklog.submit":             ok("ส่งชั่วโมงให้อาจารย์ตรวจ", CatHours),
	"worklog.approve":            ok("อนุมัติชั่วโมง", CatHours),
	"worklog.reject":             ok("ตีกลับชั่วโมงให้แก้ไข", CatHours),
	"worklog.review_cut":         warn("ตัดรายการชั่วโมงระหว่างตรวจ", CatHours),
	"worklog.review_edit":        warn("แก้ชั่วโมงระหว่างตรวจ", CatHours),
	"worklog.lecturer_edit":      warn("อาจารย์แก้ชั่วโมงที่อนุมัติแล้ว", CatHours),
	"worklog.lecturer_delete":    warn("อาจารย์ลบชั่วโมงที่อนุมัติแล้ว", CatHours),
	"worklog.staff_edit":         warn("เจ้าหน้าที่แก้ชั่วโมง", CatHours),
	"worklog.staff_delete":       warn("เจ้าหน้าที่ลบชั่วโมง", CatHours),
	"worklog.staff_edit_batch":   warn("เจ้าหน้าที่แก้ชั่วโมงหลายรายการ", CatHours),
	"ta_class_schedule.replace":  ok("ผู้ช่วยสอนแก้ตารางเรียนของตนเอง", CatHours),
	"ta_review_schedule.add":     ok("เพิ่มตารางงานตรวจของผู้ช่วยสอน", CatHours),
	"ta_review_schedule.update":  ok("แก้ตารางงานตรวจของผู้ช่วยสอน", CatHours),
	"ta_review_schedule.delete":  ok("ลบตารางงานตรวจของผู้ช่วยสอน", CatHours),
	"review_date.add":            ok("เพิ่มวันตรวจงาน", CatHours),
	"makeup.add":                 ok("เพิ่มคาบชดเชย", CatHours),
	"makeup.delete":              ok("ลบคาบชดเชย", CatHours),
	"makeup.waive":               ok("ยกเว้นคาบชดเชย", CatHours),
	"makeup.unwaive":             warn("ยกเลิกการยกเว้นคาบชดเชย", CatHours),
	"makeup.auto_fill_from_tdbm": system(ok("เติมคาบชดเชยอัตโนมัติจากตารางสอนชดเชย (TDBM)", CatHours)),

	// ── Payment paperwork ───────────────────────────────────────────────────
	"submission.staff_reviewed":           ok("เจ้าหน้าที่ตรวจผ่าน", CatPayment),
	"submission_period.upsert":            ok("สร้างหรือแก้ไขงวดส่ง", CatPayment),
	"submission_period.bulk_create":       ok("สร้างงวดส่งทั้งภาคการศึกษา", CatPayment),
	"submission_period.delete":            warn("ลบงวดส่ง", CatPayment),
	"submission_period.exported":          ok("ส่งออกเอกสารเบิกจ่าย", CatPayment),
	"submission_period.finance_sent":      ok("ส่งเอกสารให้การเงิน", CatPayment),
	"submission_period.finance_revert":    warn("ดึงเอกสารกลับจากการเงิน", CatPayment),
	"submission_period.unlocked":          warn("ปลดล็อกเดือนที่ส่งออกแล้วเพื่อแก้ไข", CatPayment),
	"submission_period.sent_back":         warn("ตีกลับเอกสารให้แก้ไข", CatPayment),
	"course.unexport":                     warn("ปลดล็อกวิชาที่ส่งออกแล้ว", CatPayment),
	"course.settlement_mode":              warn("เปลี่ยนวิธีแบ่งงบของวิชา", CatPayment),
	"export_batch.record":                 ok("บันทึกชุดเอกสารที่ส่งออก", CatPayment),
	"export.transfer_cover":               ok("ออกใบปะหน้าโอนเงิน", CatPayment),
	"export.transfer_cover.reprint":       ok("พิมพ์ใบปะหน้าโอนเงินซ้ำ", CatPayment),
	"export.course_summary":               ok("ออกสรุปงบรายวิชา", CatPayment),
	"export.course_summary.reprint":       ok("พิมพ์สรุปงบรายวิชาซ้ำ", CatPayment),
	"document_progress.set_stage":         ok("เปลี่ยนขั้นความคืบหน้าเอกสาร", CatPayment),
	"document_progress_share_link.create": ok("สร้างลิงก์แชร์ความคืบหน้าเอกสาร", CatPayment),
	"document_progress_share_link.revoke": ok("ยกเลิกลิงก์แชร์ความคืบหน้าเอกสาร", CatPayment),
	"signature_checklist.toggle":          ok("ติ๊กหรือยกเลิกการลงนาม", CatPayment),
	"pay_rate.create":                     warn("เปลี่ยนอัตราค่าตอบแทน", CatPayment),
	"pay_rate.delete_scheduled":           warn("ยกเลิกอัตราค่าตอบแทนที่ตั้งล่วงหน้า", CatPayment),
	"budget_cap.create":                   warn("เปลี่ยนเพดานงบ", CatPayment),
	"term.set_budget":                     warn("เปลี่ยนงบประมาณของภาคการศึกษา", CatPayment),
	"term.set_certifier":                  ok("กำหนดผู้รับรองเอกสารของภาคการศึกษา", CatPayment),
	"appointment_order.build":             ok("ออกคำสั่งแต่งตั้ง", CatPayment),
	"appointment_order.reprint":           ok("พิมพ์คำสั่งแต่งตั้งซ้ำ", CatPayment),

	// ── TA profile and documents ────────────────────────────────────────────
	"ta_profile.submit":         ok("ส่งประวัติและเอกสารให้ตรวจ", CatDocs),
	"ta_profile.review":         ok("ตรวจประวัติผู้ช่วยสอน", CatDocs),
	"ta_profile.approve_all":    ok("อนุมัติเอกสารผู้ช่วยสอนทั้งชุด", CatDocs),
	"ta_profile.auto_approved":  system(ok("ระบบอนุมัติประวัติผู้ช่วยสอนอัตโนมัติ", CatDocs)),
	"ta_doc.upload":             ok("อัปโหลดเอกสาร", CatDocs),
	"ta_doc.review":             ok("ตรวจเอกสารผู้ช่วยสอน", CatDocs),
	"ta_doc.expire":             system(ok("ลบไฟล์เอกสารที่พ้นกำหนดเก็บ", CatDocs)),
	"ta_docs.reject_batch":      ok("ตีกลับเอกสารผู้ช่วยสอนหลายรายการ", CatDocs),
	"ta_doc.upload_infected":    failed("อัปโหลดเอกสารไม่สำเร็จ (พบไวรัสในไฟล์)", CatDocs),
	"ta_doc.upload_scan_failed": failed("อัปโหลดเอกสารไม่สำเร็จ (ตรวจไวรัสไม่ได้)", CatDocs),
	"upload.infected":           failed("อัปโหลดไฟล์ไม่สำเร็จ (พบไวรัสในไฟล์)", CatDocs),
	"upload.scan_failed":        failed("อัปโหลดไฟล์ไม่สำเร็จ (ตรวจไวรัสไม่ได้)", CatDocs),

	// ── Courses, sections, terms, requests ──────────────────────────────────
	"teaching_course.create":            ok("เพิ่มรายวิชา", CatCourse),
	"teaching_course.update_info":       ok("แก้ไขข้อมูลรายวิชา", CatCourse),
	"teaching_course.update_settings":   ok("แก้ไขการตั้งค่ารายวิชา", CatCourse),
	"teaching_course.num_students":      ok("แก้จำนวนนักศึกษาของรายวิชา", CatCourse),
	"teaching_course.lecturers.replace": ok("เปลี่ยนอาจารย์ผู้สอนของรายวิชา", CatCourse),
	"teaching_course.merge_code":        ok("รวมรหัสวิชาที่สอนร่วมกัน", CatCourse),
	"teaching_course.fold_section":      warn("รวม section ของรหัสที่รวมไว้เข้ากับ section ที่เรียนด้วยกัน", CatCourse),
	"teaching_course.delete":            warn("ลบรายวิชา", CatCourse),
	"section.add":                       ok("เพิ่มกลุ่มเรียน", CatCourse),
	"section.update":                    ok("แก้ไขกลุ่มเรียน", CatCourse),
	"section.delete":                    warn("ลบกลุ่มเรียน", CatCourse),
	"section.schedules.replace":         ok("แก้ตารางสอนของกลุ่มเรียน", CatCourse),
	"section.curriculum.update":         ok("เปลี่ยนหลักสูตรของกลุ่มเรียน", CatCourse),
	"curriculum.update":                 ok("แก้ไขข้อมูลหลักสูตร", CatCourse),
	"schedule.import":                   ok("นำเข้าตารางสอน", CatCourse),
	"term.create":                       ok("สร้างภาคการศึกษา", CatCourse),
	"academic_term.budget_cap":          warn("กำหนดเพดานงบรายวิชาของภาคเรียน", CatCourse),
	"term.update":                       ok("แก้ไขภาคการศึกษา", CatCourse),
	"term.delete":                       warn("ลบภาคการศึกษา", CatCourse),
	"holiday.create":                    ok("เพิ่มวันหยุด", CatCourse),
	"holiday.bulk_create":               ok("เพิ่มวันหยุดหลายรายการ", CatCourse),
	"holiday.patch":                     ok("แก้ไขวันหยุด", CatCourse),
	"holiday.delete":                    ok("ลบวันหยุด", CatCourse),
	"holiday.sync_bot":                  system(ok("ดึงวันหยุดจากธนาคารแห่งประเทศไทย", CatCourse)),
	"ta_window.upsert":                  ok("ตั้งช่วงเปิดรับคำขอผู้ช่วยสอน", CatCourse),
	"ta_window.delete":                  ok("ลบช่วงเปิดรับคำขอผู้ช่วยสอน", CatCourse),
	"ta_request.add_sections":           ok("เพิ่มกลุ่มเรียนในคำขอผู้ช่วยสอน", CatCourse),
	"ta_request.pending_schedule":       ok("ยื่นคำขอผู้ช่วยสอนโดยรอตารางสอน", CatCourse),
	"ta_request.update_workload":        ok("แก้ภาระงานในคำขอผู้ช่วยสอน", CatCourse),
	"ta_request.cancel":                 warn("ยกเลิกคำขอผู้ช่วยสอน", CatCourse),
	"ta_request.auto_decide":            system(ok("ระบบพิจารณาคำขอผู้ช่วยสอนอัตโนมัติ", CatCourse)),

	// ── Announcements and reminders ─────────────────────────────────────────
	"announce.upsert":              ok("สร้างหรือแก้ไขประกาศ", CatComms),
	"announce.publish":             ok("เผยแพร่ประกาศ", CatComms),
	"announce.unpublish":           ok("ยกเลิกการเผยแพร่ประกาศ", CatComms),
	"announce.delete":              ok("ลบประกาศ", CatComms),
	"announce.remind":              ok("ส่งเตือนให้อ่านประกาศ", CatComms),
	"announce.background_add":      ok("เพิ่มพื้นหลังรูปประกาศ", CatComms),
	"announce.background_delete":   ok("ลบพื้นหลังรูปประกาศ", CatComms),
	"holiday.remind":               ok("เตือนอาจารย์ให้นัดคาบชดเชย", CatComms),
	"payout.remind_lecturer":       ok("เตือนอาจารย์ให้ตรวจชั่วโมง", CatComms),
	"signature_checklist.remind":   ok("เตือนให้ลงนามเอกสาร", CatComms),
	"appointment.remind_timetable": ok("เตือนผู้ช่วยสอนให้กรอกตารางเรียน", CatComms),

	// ── Looking at things ───────────────────────────────────────────────────
	"users.list.view":               sensitive("เปิดดูรายชื่อผู้ใช้ทั้งหมด"),
	"user.record.view":              sensitive("เปิดดูประวัติผู้ใช้"),
	"dashboard.executive.view":      view("เปิดดูแดชบอร์ดงบประมาณ"),
	"payout.queue.view":             view("เปิดดูรายการเบิกจ่าย"),
	"export.course.preview":         view("ดูตัวอย่างเอกสารเบิกจ่าย"),
	"export.course_summary.preview": view("ดูตัวอย่างสรุปงบรายวิชา"),
	"export.course":                 view("ดาวน์โหลดเอกสารเบิกจ่าย"),
	"export.batch_download":         view("ดาวน์โหลดชุดเอกสารเบิกจ่าย"),
	"ta_doc.view":                   sensitive("เปิดดูเอกสารของผู้ช่วยสอน"),
	"ta_doc.view_watermarked":       sensitive("เปิดดูเอกสารของผู้ช่วยสอน (มีลายน้ำ)"),
	"ta_docs.download":              sensitive("ดาวน์โหลดเอกสารของผู้ช่วยสอน"),
	"ta_docs.download_all":          sensitive("ดาวน์โหลดเอกสารของผู้ช่วยสอนทั้งชุด"),
	"ta_docs.redownload_verify":     sensitive("ยืนยันตัวตนเพื่อดาวน์โหลดเอกสารซ้ำ"),
	"ta_profile.citizen_id.reveal":  sensitive("เปิดดูเลขบัตรประชาชน"),
	"audit_log.search":              view("ค้นหาในบันทึกการใช้งาน"),
	"audit_log.export":              sensitive("ส่งออกบันทึกการใช้งานเป็นไฟล์"),

	// ── Settings and housekeeping ───────────────────────────────────────────
	"mail_settings.update": warn("แก้ไขการตั้งค่าอีเมลของระบบ", CatSystem),
	"mail.check":           ok("ทดสอบการเชื่อมต่อเซิร์ฟเวอร์อีเมล", CatSystem),
	"mail.test_send":       ok("ส่งอีเมลทดสอบ", CatSystem),
	"system.db_restore":    danger("กู้คืนฐานข้อมูลจากไฟล์สำรอง", CatSystem),
	"audit_log.purge":      system(ok("ย้ายบันทึกที่พ้นอายุเก็บ 5 ปีเข้าคลังถาวร", CatSystem)),
}

// families describe an action that has no entry of its own, from its prefix,
// so a row written by code newer than this file still reads as a sentence and
// still lands in a category. Longest prefix wins.
var families = []struct {
	prefix string
	label  string
	cat    Category
}{
	{"auth.", "การเข้าสู่ระบบ", CatAccess},
	{"access.", "การเข้าถึงระบบ", CatAccess},
	{"user.", "จัดการบัญชีผู้ใช้", CatAccount},
	{"users.", "รายชื่อผู้ใช้", CatView},
	{"admin_officer.", "ตำแหน่งบริหาร", CatAccount},
	{"ta_deletion_request.", "คำขอลบข้อมูล", CatAccount},
	{"ta_enrollment.", "การศึกษาของผู้ช่วยสอน", CatAccount},
	{"worklog.", "รายการชั่วโมง", CatHours},
	{"makeup.", "คาบชดเชย", CatHours},
	{"review_date.", "วันตรวจงาน", CatHours},
	{"ta_review_schedule.", "ตารางงานผู้ช่วยสอน", CatHours},
	{"ta_class_schedule.", "ตารางเรียนของผู้ช่วยสอน", CatHours},
	{"submission_period.", "งวดส่งเอกสาร", CatPayment},
	{"submission.", "การตรวจของเจ้าหน้าที่", CatPayment},
	{"export.", "การส่งออกเอกสาร", CatPayment},
	{"export_batch.", "ชุดเอกสารที่ส่งออก", CatPayment},
	{"payout.", "การเบิกจ่าย", CatPayment},
	{"pay_rate.", "อัตราค่าตอบแทน", CatPayment},
	{"budget_cap.", "เพดานงบ", CatPayment},
	{"signature_checklist.", "รายการลงนาม", CatPayment},
	{"document_progress", "ความคืบหน้าเอกสาร", CatPayment},
	{"appointment_order.", "คำสั่งแต่งตั้ง", CatPayment},
	{"appointment.", "การแต่งตั้ง", CatPayment},
	{"ta_doc", "เอกสารผู้ช่วยสอน", CatDocs},
	{"ta_profile.", "ประวัติผู้ช่วยสอน", CatDocs},
	{"upload.", "การอัปโหลดไฟล์", CatDocs},
	{"teaching_course.", "ข้อมูลรายวิชา", CatCourse},
	{"course.", "รายวิชา", CatCourse},
	{"section.", "กลุ่มเรียน", CatCourse},
	{"term.", "ภาคการศึกษา", CatCourse},
	{"holiday.", "วันหยุด", CatCourse},
	{"schedule.", "ตารางสอน", CatCourse},
	{"curriculum.", "หลักสูตร", CatCourse},
	{"ta_request.", "คำขอผู้ช่วยสอน", CatCourse},
	{"ta_window.", "ช่วงเปิดรับคำขอ", CatCourse},
	{"announce.", "ประกาศ", CatComms},
	{"dashboard.", "แดชบอร์ด", CatView},
	{"audit_log.", "บันทึกการใช้งาน", CatSystem},
	{"mail", "การตั้งค่าอีเมล", CatSystem},
	{"system.", "งานของระบบ", CatSystem},
}

// Describe returns what the catalog knows about an action. An action with no
// entry is described from its family; one with no family either keeps its own
// name as the label, which is ugly on purpose — it is the visible sign that the
// catalog is missing an entry.
func Describe(action string) ActionInfo {
	if i, found := catalog[action]; found {
		return i
	}
	out := ActionInfo{Label: action, Category: CatSystem, Severity: SevInfo, Outcome: OutcomeOK}
	best := 0
	for _, f := range families {
		if len(f.prefix) > best && strings.HasPrefix(action, f.prefix) {
			best = len(f.prefix)
			out.Label, out.Category = f.label, f.cat
		}
	}
	// The suffix conventions the read-audit middleware and the upload paths
	// already follow, so an unregistered row still sorts sensibly.
	switch {
	case strings.HasSuffix(action, ".view"), strings.HasSuffix(action, ".preview"):
		out.Category = CatView
	case strings.HasSuffix(action, "_failed"), strings.HasSuffix(action, "_locked"):
		out.Severity, out.Outcome = SevDanger, OutcomeFailed
	}
	return out
}

// CategoryInfo is one category as the screen lists it.
type CategoryInfo struct {
	ID    Category `json:"id"`
	Label string   `json:"label"`
}

// Categories lists every category in the order the screen shows them.
func Categories() []CategoryInfo {
	return []CategoryInfo{
		{CatAccess, "การเข้าสู่ระบบ"},
		{CatHours, "ชั่วโมงทำงาน"},
		{CatPayment, "เบิกจ่ายและงบประมาณ"},
		{CatDocs, "เอกสารผู้ช่วยสอน"},
		{CatAccount, "บัญชีและสิทธิ์"},
		{CatCourse, "รายวิชาและตาราง"},
		{CatComms, "ประกาศและการเตือน"},
		{CatView, "การเปิดดูข้อมูล"},
		{CatSystem, "ระบบและการตั้งค่า"},
	}
}

// CategoryLabel is the Thai name of a category, or the id itself if unknown.
func CategoryLabel(c Category) string {
	for _, i := range Categories() {
		if i.ID == c {
			return i.Label
		}
	}
	return string(c)
}

// ActionsWhere lists the registered actions matching a predicate, sorted. This
// is how a question phrased in the catalog's terms ("only money", "only the
// failures") becomes a list the action index can answer — which also makes it
// work for rows written before the catalog existed.
func ActionsWhere(match func(ActionInfo) bool) []string {
	out := []string{}
	for name, info := range catalog {
		if match(info) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ActionsLabelled lists the registered actions whose Thai label contains q, so
// a search typed in the words the screen shows ("อนุมัติ") finds the rows.
func ActionsLabelled(q string) []string {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	return ActionsWhere(func(i ActionInfo) bool {
		return strings.Contains(strings.ToLower(i.Label), q)
	})
}

// CatalogEntry is one registered action, for the screen's filter.
type CatalogEntry struct {
	Action string `json:"action"`
	ActionInfo
}

// Catalog lists every registered action, sorted by category then label.
func Catalog() []CatalogEntry {
	out := make([]CatalogEntry, 0, len(catalog))
	for name, info := range catalog {
		out = append(out, CatalogEntry{Action: name, ActionInfo: info})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		if out[i].Label != out[j].Label {
			return out[i].Label < out[j].Label
		}
		return out[i].Action < out[j].Action
	})
	return out
}
