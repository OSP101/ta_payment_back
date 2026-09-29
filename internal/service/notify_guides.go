package service

// notify_guides.go names the manual pages e-mails link to. The pages live in
// ta_payment_front content/docs/pages ("slug" field) and are served at
// /docs/<audience>/<slug>; renaming a slug there breaks the link here, so
// keep this list in step. The manual's index (/docs/<audience>) always exists
// and is listed last as the fallback.

var (
	guideLecturerStart = MailGuide{
		Label: "เริ่มใช้งานครั้งแรก (อาจารย์)", Path: "/docs/lecturer/start",
		Note: "ภาพรวมขั้นตอนตั้งแต่ส่งคำขอผู้ช่วยสอน อนุมัติบันทึกเวลา จนถึงลงนามเอกสารเบิกจ่าย",
	}
	guideLecturerRequest = MailGuide{
		Label: "วิธีส่งคำขอผู้ช่วยสอน", Path: "/docs/lecturer/request/overview",
		Note: "เลือกรูปแบบคำขอ เพิ่มผู้ช่วยสอน กำหนดกลุ่มเรียนและชั่วโมง แล้วส่งคำขอ",
	}
	guideLecturerAll = MailGuide{Label: "คู่มือการใช้งานทั้งหมดสำหรับอาจารย์", Path: "/docs/lecturer"}

	guideTAStart = MailGuide{
		Label: "เริ่มใช้งานครั้งแรก (ผู้ช่วยสอน)", Path: "/docs/ta/start",
		Note: "ภาพรวมขั้นตอนตั้งแต่ส่งเอกสาร บันทึกตารางเรียน ลงเวลาปฏิบัติงาน จนถึงรับค่าตอบแทน",
	}
	guideTASchedule = MailGuide{
		Label: "วิธีบันทึกตารางเรียน", Path: "/docs/ta/schedule/overview",
		Note: "นำเข้าหรือวาดตารางเรียนของภาคการศึกษา ซึ่งจำเป็นต่อการพิจารณาคำขอแต่งตั้ง",
	}
	guideTAWorklog = MailGuide{
		Label: "วิธีลงเวลาปฏิบัติงานและส่งอนุมัติ", Path: "/docs/ta/worklog/rules",
		Note: "กติกาการลงเวลา การสร้างรายการอัตโนมัติ และการส่งให้อาจารย์อนุมัติก่อนครบกำหนด",
	}
	guideTAAll = MailGuide{Label: "คู่มือการใช้งานทั้งหมดสำหรับผู้ช่วยสอน", Path: "/docs/ta"}
)

// Guide sets per first-contact notice.
var (
	lecturerRequestGuides = []MailGuide{guideLecturerStart, guideLecturerRequest, guideLecturerAll}
	lecturerStartGuides   = []MailGuide{guideLecturerStart, guideLecturerAll}
	taAppointedGuides     = []MailGuide{guideTAStart, guideTAWorklog, guideTAAll}
	taTimetableGuides     = []MailGuide{guideTAStart, guideTASchedule, guideTAAll}
)
