// Package handler's request bodies were, until this file, parsed with a bare
// c.BodyParser(&in) and nothing else — whatever JSON arrived became the
// struct, with no check that a required field was present, a string was
// within a sane length, or a code was one of the values the database (or the
// business rule two lines later) actually accepts. Malformed input mostly
// surfaced as a Postgres constraint violation, or a business-rule error from
// deep inside a service method — both work, but late, in a way that: (a)
// exercises a DB round trip for something that was never going to succeed,
// and (b) can produce a validation-adjacent report that reads as "this field
// is wrong" when it should read as "this field is missing".
//
// Bind is the fix: c.BodyParser(&in), then validator.Struct(&in) against the
// struct's own `validate:"..."` tags. It replaces the bodyparser call, not
// the business logic after it — a struct tag can express "not empty" or "one
// of these three codes", not "this course isn't over budget", so every
// service-layer check downstream is unchanged and still runs.
package handler

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
)

// validate is safe for concurrent use across requests — the package doc for
// go-playground/validator states struct validation itself does not mutate
// shared state, only the one-time tag/translator registration below does,
// and that runs once at package init.
var validate = validator.New()

func init() {
	// Field names in validation errors read as the JSON key ("email"), not
	// the Go struct field ("Email") — the caller sent JSON, so the error
	// should talk about the JSON they sent.
	validate.RegisterTagNameFunc(func(f reflect.StructField) string {
		name := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
		if name == "-" || name == "" {
			return f.Name
		}
		return name
	})
}

// Bind parses c's JSON body into dst and validates it against dst's
// `validate:"..."` struct tags in one step. Handlers should call this
// instead of c.BodyParser for any request body carrying user-editable
// fields — GET-only handlers and handlers with no body (id-only actions)
// have nothing to bind and don't need it.
//
// Both failure modes return the same 400 shape the rest of this codebase
// already uses ({"error": "..."}), so no frontend change was needed to
// surface either kind of rejection.
func Bind(c *fiber.Ctx, dst any) error {
	if err := c.BodyParser(dst); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "รูปแบบข้อมูลที่ส่งมาไม่ถูกต้อง")
	}
	if err := validate.Struct(dst); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, humanizeValidationError(err))
	}
	return nil
}

// humanizeValidationError turns the first failing field into a short Thai
// sentence naming the field the way the form labels it ("จำนวนนักศึกษา
// ต้องไม่น้อยกว่า 0"). It used to answer in validator's English ("num_students
// must be 0 or greater"); the frontend only shows server text that contains
// Thai, so every one of those surfaced as a bare "ทำรายการไม่สำเร็จ (รหัส
// 400)" and the user never learned which field was wrong. Only the first
// failure is reported — naming the single most useful one beats a list.
func humanizeValidationError(err error) string {
	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) || len(verrs) == 0 {
		return "ข้อมูลที่ส่งมาไม่ถูกต้อง"
	}
	e := verrs[0]
	field := fieldLabel(e.Field())
	isString := e.Kind() == reflect.String
	// slice/map: min/max count items, not characters.
	isList := e.Kind() == reflect.Slice || e.Kind() == reflect.Array || e.Kind() == reflect.Map
	switch e.Tag() {
	case "required", "required_if", "required_unless", "required_with", "required_without":
		return fmt.Sprintf("กรุณาระบุ%s", field)
	case "min":
		switch {
		case isString:
			return fmt.Sprintf("%s ต้องมีอย่างน้อย %s ตัวอักษร", field, e.Param())
		case isList:
			return fmt.Sprintf("%s ต้องเลือกอย่างน้อย %s รายการ", field, e.Param())
		}
		return fmt.Sprintf("%s ต้องไม่น้อยกว่า %s", field, e.Param())
	case "max":
		switch {
		case isString:
			return fmt.Sprintf("%s ยาวได้ไม่เกิน %s ตัวอักษร", field, e.Param())
		case isList:
			return fmt.Sprintf("%s เลือกได้ไม่เกิน %s รายการ", field, e.Param())
		}
		return fmt.Sprintf("%s ต้องไม่เกิน %s", field, e.Param())
	case "len":
		if isString {
			return fmt.Sprintf("%s ต้องมี %s ตัวอักษร", field, e.Param())
		}
		return fmt.Sprintf("%s ต้องมี %s รายการ", field, e.Param())
	case "email":
		return fmt.Sprintf("%s ไม่ใช่รูปแบบอีเมลที่ถูกต้อง", field)
	case "oneof":
		return fmt.Sprintf("%s ต้องเป็นค่าใดค่าหนึ่งต่อไปนี้: %s", field, strings.ReplaceAll(e.Param(), " ", ", "))
	case "gte":
		return fmt.Sprintf("%s ต้องไม่น้อยกว่า %s", field, e.Param())
	case "lte":
		return fmt.Sprintf("%s ต้องไม่เกิน %s", field, e.Param())
	case "gt":
		return fmt.Sprintf("%s ต้องมากกว่า %s", field, e.Param())
	case "lt":
		return fmt.Sprintf("%s ต้องน้อยกว่า %s", field, e.Param())
	case "uuid4", "uuid":
		return fmt.Sprintf("%s ไม่ใช่รหัสอ้างอิงที่ถูกต้อง", field)
	case "datetime":
		return fmt.Sprintf("%s ไม่ใช่รูปแบบวันที่/เวลาที่ถูกต้อง (%s)", field, e.Param())
	case "numeric", "number":
		return fmt.Sprintf("%s ต้องเป็นตัวเลข", field)
	case "dive":
		return fmt.Sprintf("%s มีรายการที่ไม่ถูกต้อง", field)
	default:
		return fmt.Sprintf("%s ไม่ถูกต้อง", field)
	}
}

// fieldLabels names request fields the way the screens label them. A field
// missing here falls back to its JSON key — still specific, and still wrapped
// in a Thai sentence so the frontend shows it instead of a bare status code.
var fieldLabels = map[string]string{
	"email": "อีเมล", "confirm_email": "อีเมลยืนยัน", "password": "รหัสผ่าน",
	"first_name": "ชื่อ", "last_name": "นามสกุล", "prefix": "คำนำหน้า", "title": "หัวข้อ",
	"name": "ชื่อ", "name_th": "ชื่อภาษาไทย", "full_name_th": "ชื่อ-นามสกุลภาษาไทย",
	"phone": "เบอร์โทรศัพท์", "student_id": "รหัสนักศึกษา", "national_id": "เลขประจำตัวประชาชน",
	"study_level": "ระดับการศึกษา", "level": "ระดับ", "role": "บทบาท", "roles": "บทบาท",
	"note": "หมายเหตุ", "reason": "เหตุผล", "comment": "ความคิดเห็น", "body": "เนื้อหา",
	"code": "รหัสวิชา", "credits": "หน่วยกิต", "sec_no": "หมายเลข section", "room": "ห้อง",
	"num_students": "จำนวนนักศึกษา", "num_students_regular": "จำนวนนักศึกษาภาคปกติ",
	"num_students_special": "จำนวนนักศึกษาภาคพิเศษ",
	"lecturer_ids":         "อาจารย์ผู้สอน", "ta_id": "TA", "ta_ids": "TA", "user_id": "ผู้ใช้", "user_ids": "ผู้ใช้",
	"section_id": "section", "section_ids": "section", "assignments": "รายชื่อ TA", "counts": "จำนวน TA ต่อ section",
	"undergrad_count": "จำนวน TA ป.ตรี", "graduate_count": "จำนวน TA บัณฑิตศึกษา",
	"teaching_course_id": "รายวิชา", "course_ids": "รายวิชา", "term_id": "ภาคเรียน",
	"academic_year": "ปีการศึกษา", "semester": "ภาคเรียน",
	"day_of_week": "วัน", "start_time": "เวลาเริ่ม", "end_time": "เวลาสิ้นสุด",
	"work_date": "วันที่ปฏิบัติงาน", "hours": "จำนวนชั่วโมง", "year_month": "เดือน", "months": "เดือน",
	"opens_at": "วันเปิด", "closes_at": "วันปิด", "due_date": "วันครบกำหนด", "starts_on": "วันเริ่ม",
	"effective_date": "วันที่มีผล", "effective_from": "วันที่มีผล", "order_date": "วันที่ออกคำสั่ง",
	"order_no": "เลขที่คำสั่ง", "holiday_date": "วันหยุด", "makeup_date": "วันสอนชดเชย",
	"original_date": "วันเดิม", "review_date": "วันที่ตรวจ",
	"bank_name": "ธนาคาร", "bank_branch": "สาขาธนาคาร", "account_no": "เลขที่บัญชี", "account_name": "ชื่อบัญชี",
	"activity": "กิจกรรม", "category": "หมวดหมู่", "label": "ชื่อ", "kind": "ประเภท", "stage": "ขั้นตอน",
	"to_status": "สถานะ", "attachments": "ไฟล์แนบ", "items": "รายการ", "audience": "กลุ่มผู้รับ",
	"signer_id": "ผู้ลงนาม", "signer_officer_id": "ผู้ลงนาม", "officer_id": "เจ้าหน้าที่",
}

func fieldLabel(field string) string {
	// Fields inside a slice arrive as "section_ids[0]"; label the list itself.
	if i := strings.IndexByte(field, '['); i >= 0 {
		field = field[:i]
	}
	if l, ok := fieldLabels[field]; ok {
		return l
	}
	return field
}
