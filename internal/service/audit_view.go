// audit_view.go turns a stored audit row into something a person can read.
//
// The trail was always complete; what it lacked was a reading. A row came back
// as an action identifier, an eight-character id, a `key=value` note written
// for grep and a JSON blob, and the screen did what it could with those. The
// people who open that screen are finance officers. Everything here exists so
// the server can hand them a sentence — who did what, to what, whether it
// worked — while the raw fields stay in the row for the administrator who
// needs them.
//
// Nothing here changes what is stored. It is derived at read time, which is
// the only way it can apply to rows already in an append-only table.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/timeutil"
)

// AuditChange is one field of a before/after pair, in words.
//
// Before and After are pointers because "was empty" and "was not recorded" are
// different things: a create has no Before at all, a cleared field has an
// After of "".
type AuditChange struct {
	Key    string  `json:"key"`
	Label  string  `json:"label"`
	Before *string `json:"before"`
	After  *string `json:"after"`
}

// AuditRef is the reference a person quotes for one row — in an email, in a
// memo, to the administrator. It is the row id, padded so it reads as a
// document number rather than as a count.
func AuditRef(id int64) string { return fmt.Sprintf("AL-%06d", id) }

var auditRefPattern = regexp.MustCompile(`(?i)^\s*AL-?0*([0-9]{1,18})\s*$`)

// ParseAuditRef reads a reference back into a row id.
func ParseAuditRef(s string) (int64, bool) {
	m := auditRefPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	return id, err == nil && id > 0
}

// decorate fills in everything about a row that is derived rather than stored.
func decorate(r *AuditRow) {
	info := audit.Describe(r.Action)
	r.Ref = AuditRef(r.ID)
	r.Label = info.Label
	r.Category = string(info.Category)
	r.CategoryLabel = audit.CategoryLabel(info.Category)
	r.Severity = string(info.Severity)
	r.Outcome = string(info.Outcome)
	r.Automatic = info.System
	if r.Count == 0 {
		r.Count = 1
	}

	// Who was behind it. A missing actor means three different things and the
	// screen used to call all of them "ระบบ".
	switch {
	case r.ActorID != nil:
		r.ActorKind = "user"
	case info.System:
		r.ActorKind = "system"
	case r.RequestID != nil:
		// A request came in but nobody was signed in — a failed login is the
		// usual case.
		r.ActorKind = "anonymous"
	default:
		r.ActorKind = "unknown"
	}

	if r.UserAgent != nil {
		r.Device = audit.Device(*r.UserAgent)
	}
	if r.IP != nil {
		r.Network = audit.Network(*r.IP)
	}

	before, after := jsonObject(r.Before), jsonObject(r.After)
	r.Changes = auditChanges(before, after)
	if r.SubjectName == "" {
		// The thing the row was about no longer exists (or never had a name the
		// resolver knows). The row's own images are the last place its name is
		// written down.
		r.SubjectName = subjectFromImages(before, after)
	}
	r.Details = dropRestatedChanges(auditDetails(r), r.Changes)
}

// dropRestatedChanges removes a detail line that only repeats a change already
// listed. Some writers put "old → new" in the note AND record the images; the
// reader then saw the same transition twice, once in each section.
func dropRestatedChanges(details []string, changes []AuditChange) []string {
	if len(details) == 0 || len(changes) == 0 {
		return details
	}
	restated := map[string]bool{}
	for _, c := range changes {
		if c.Before != nil && c.After != nil {
			restated[*c.Before+" → "+*c.After] = true
		}
	}
	out := details[:0:0]
	for _, d := range details {
		if !restated[d] {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func jsonObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// auditFieldLabels names the columns and payload keys that turn up in
// before/after images. A key with no entry is shown as itself — still
// readable by an administrator, and a visible prompt to add it here.
var auditFieldLabels = map[string]string{
	"hours": "ชั่วโมง", "status": "สถานะ", "activity": "กิจกรรม", "work_date": "วันที่ทำงาน",
	"start_time": "เวลาเริ่ม", "end_time": "เวลาสิ้นสุด", "room": "ห้อง", "note": "หมายเหตุ",
	"count": "จำนวนรายการ", "rows": "รายการ", "reason": "เหตุผล", "reject_reason": "เหตุผลที่ตีกลับ",
	"budget_baht": "งบประมาณ (บาท)", "budget_note": "หมายเหตุงบประมาณ",
	"settlement_mode": "วิธีแบ่งงบ", "reissue_months": "เดือนที่ออกเอกสารใหม่",
	"email": "อีเมล", "phone": "โทรศัพท์", "title": "คำนำหน้าหรือหัวเรื่อง",
	"first_name": "ชื่อ", "last_name": "นามสกุล", "roles": "บทบาท", "role": "บทบาท",
	"is_active": "สถานะการใช้งาน", "study_level": "ระดับการศึกษา", "study_year": "ชั้นปี",
	"student_id": "รหัสนักศึกษา", "department": "สาขา", "password_hash": "รหัสผ่าน",
	"totp_secret_enc": "กุญแจยืนยัน 2 ชั้น", "citizen_id_enc": "เลขบัตรประชาชน", "payee_enc": "บัญชีธนาคารและที่อยู่",
	"must_change_password": "ต้องเปลี่ยนรหัสผ่าน", "admin_position": "ตำแหน่งบริหาร",
	"round": "รอบ", "signed": "การลงนาม", "stage": "ขั้นความคืบหน้า",
	"kind": "ประเภท", "filename": "ชื่อไฟล์", "file_name": "ชื่อไฟล์",
	"code": "รหัสวิชา", "name_th": "ชื่อวิชา (ไทย)", "name_en": "ชื่อวิชา (อังกฤษ)",
	"level": "ระดับ", "credits": "หน่วยกิต", "lecture_hrs": "ชั่วโมงบรรยาย", "lab_hrs": "ชั่วโมงปฏิบัติการ",
	"self_hrs": "ชั่วโมงศึกษาด้วยตนเอง", "num_students": "จำนวนนักศึกษา",
	"num_students_regular": "นักศึกษาภาคปกติ", "num_students_special": "นักศึกษาโครงการพิเศษ",
	"alt_codes":                    "รหัสวิชาที่สอนร่วม",
	"num_students_regular_entered": "กรอกจำนวนนักศึกษาภาคปกติแล้ว",
	"num_students_special_entered": "กรอกจำนวนนักศึกษาโครงการพิเศษแล้ว", "curriculum": "หลักสูตร", "sec_no": "กลุ่มเรียน",
	"sections": "กลุ่มเรียน", "assignments": "ผู้ช่วยสอนที่มอบหมาย", "day_of_week": "วันในสัปดาห์",
	"original_date": "วันเดิม", "makeup_date": "วันชดเชย", "source": "ที่มา",
	"months": "เดือน", "year_month": "เดือน", "label": "ชื่องวด", "due_date": "กำหนดส่ง",
	"starts_on": "วันเริ่ม", "ends_on": "วันสิ้นสุด", "is_closed": "ปิดงวด",
	"remind_days_before": "เตือนล่วงหน้า (วัน)", "total_baht": "ยอดรวม (บาท)",
	"ta_count": "จำนวนผู้ช่วยสอน", "sheet_count": "จำนวนแผ่นงาน", "doc_count": "จำนวนเอกสาร",
	"order_no": "เลขที่คำสั่ง", "academic_year": "ปีการศึกษา", "semester": "ภาคการศึกษา",
	"midterm_starts_on": "เริ่มสอบกลางภาค", "midterm_ends_on": "สิ้นสุดสอบกลางภาค",
	"final_starts_on": "เริ่มสอบปลายภาค", "final_ends_on": "สิ้นสุดสอบปลายภาค",
	"full_name": "ชื่อ-นามสกุล", "academic_prefix": "ตำแหน่งทางวิชาการ",
	"is_dean": "เป็นคณบดี", "is_head": "เป็นหัวหน้าสาขา", "linked_email": "อีเมลที่ผูกบัญชี",
	"linked_active": "บัญชีที่ผูกใช้งานอยู่", "reimburse_scope": "ประเภทการเบิก",
	"is_open": "เปิดรับ", "opens_at": "เปิดรับตั้งแต่", "closes_at": "ปิดรับเมื่อ",
	"created_count": "สร้างใหม่", "merged_count": "รวมเข้ากับของเดิม", "skipped_count": "ข้าม",
	"requested_count": "ที่ขอ", "error_count": "ผิดพลาด", "warning_count": "คำเตือน", "row_count": "จำนวนแถว",
	"prep_hrs": "ชั่วโมงเตรียมสอน", "grade_hrs": "ชั่วโมงตรวจงาน", "check_work_hrs": "ชั่วโมงตรวจการบ้าน",
	"attendance_hrs": "ชั่วโมงเช็กชื่อ", "help_teach_hrs": "ชั่วโมงช่วยสอน", "other_hrs": "ชั่วโมงอื่น ๆ",
	"lab_other_hrs": "ชั่วโมงปฏิบัติการอื่น ๆ", "ug_other_hrs": "ชั่วโมงอื่น ๆ (ป.ตรี)",
	"prep_desc": "รายละเอียดเตรียมสอน", "grade_desc": "รายละเอียดตรวจงาน",
	"help_teach_desc": "รายละเอียดช่วยสอน", "other_desc": "รายละเอียดอื่น ๆ",
	"lab_other_desc": "รายละเอียดปฏิบัติการอื่น ๆ", "ug_other_desc": "รายละเอียดอื่น ๆ (ป.ตรี)",
	"effective_from": "มีผลตั้งแต่", "expires_at": "หมดอายุ", "published_at": "เผยแพร่เมื่อ",
	"exported_at": "ส่งออกเมื่อ", "body": "เนื้อหา", "category": "หมวด", "pinned": "ปักหมุด",
	"settlement_mode_at": "เปลี่ยนวิธีแบ่งงบเมื่อ",
}

// auditFieldOrder puts the fields a reader looks for first at the top; the
// rest follow alphabetically.
var auditFieldOrder = []string{
	"status", "hours", "count", "work_date", "start_time", "end_time", "activity",
	"total_baht", "budget_baht", "settlement_mode", "roles", "role", "is_active",
	"code", "name_th", "first_name", "last_name", "email", "kind", "filename", "reason", "reject_reason",
}

// auditValueLabels translates the enum values that appear in the images and
// notes. Keyed by value alone: the same word means the same thing wherever it
// turns up in this system.
var auditValueLabels = map[string]string{
	"draft": "ร่าง", "submitted": "ส่งแล้ว รอตรวจ", "approved": "อนุมัติแล้ว", "rejected": "ตีกลับ",
	"pending": "รอดำเนินการ", "exported": "ส่งออกแล้ว", "finance_sent": "ส่งการเงินแล้ว",
	"cancelled": "ยกเลิก", "expired": "หมดอายุ",
	"lecture": "บรรยาย", "lab": "ปฏิบัติการ", "review": "ตรวจงาน",
	"chronological": "จ่ายตามลำดับเวลา", "spread": "เฉลี่ยทุกเดือน",
	"admin": "ผู้ดูแลระบบ", "staff": "เจ้าหน้าที่", "lecturer": "อาจารย์", "ta": "ผู้ช่วยสอน",
	"executive": "ผู้บริหาร",
	"undergrad": "ปริญญาตรี", "graduate": "บัณฑิตศึกษา", "master": "ปริญญาโท", "doctoral": "ปริญญาเอก",
	"regular": "ภาคปกติ", "special": "โครงการพิเศษ",
	"bank_book": "สมุดบัญชีธนาคาร", "national_id": "บัตรประชาชน", "creditor_form": "แบบฟอร์มเจ้าหนี้",
	"passport": "Passport", "foreign": "ต่างชาติ",
	"both": "บรรยาย + ปฏิบัติการ", "phd": "ปริญญาเอก", "needs_fix": "ต้องแก้ไข",
	"import": "นำเข้าจากไฟล์", "other_lecture": "งานอื่น ๆ ของบรรยาย", "other_lab": "งานอื่น ๆ ของปฏิบัติการ",
	"sso": "ผ่าน KKU SSO", "manual": "กรอกเอง", "tdbm": "ระบบตารางสอน (TDBM)", "generated": "สร้างจากตารางสอน",
}

// auditHiddenKey reports whether a key is plumbing rather than content. These
// stay in the raw JSON; they are only kept out of the readable list, where an
// id is noise and a storage path is worse.
func auditHiddenKey(k string) bool {
	switch k {
	case "id", "file_path", "storage_key", "updated_at", "created_at", "generated_at",
		"locked_cells", "document", "trigger_doc", "decision_checks", "link_id", "counts", "rows":
		return true
	}
	return strings.HasSuffix(k, "_id") || strings.HasSuffix(k, "_ids") ||
		strings.HasSuffix(k, "_by") || strings.HasSuffix(k, "_key") || strings.HasSuffix(k, "_key_version")
}

const auditMaxChanges = 14

// auditChanges lists what moved between the two images, in words.
func auditChanges(before, after map[string]any) []AuditChange {
	if before == nil && after == nil {
		return nil
	}
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	rank := map[string]int{}
	for i, k := range auditFieldOrder {
		rank[k] = i + 1
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		if !auditHiddenKey(k) {
			ordered = append(ordered, k)
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		ri, rj := rank[ordered[i]], rank[ordered[j]]
		switch {
		case ri != 0 && rj != 0:
			return ri < rj
		case ri != 0:
			return true
		case rj != 0:
			return false
		}
		return ordered[i] < ordered[j]
	})

	out := []AuditChange{}
	for _, k := range ordered {
		c := AuditChange{Key: k, Label: k}
		if l, found := auditFieldLabels[k]; found {
			c.Label = l
		}
		bv, hasB := before[k]
		av, hasA := after[k]
		if hasB {
			if s, shown := auditValue(k, bv); shown {
				c.Before = &s
			}
		}
		if hasA {
			if s, shown := auditValue(k, av); shown {
				c.After = &s
			}
		}
		if c.Before == nil && c.After == nil {
			continue
		}
		// An update where neither side has anything to show for this key (both
		// empty) says nothing.
		if c.Before != nil && c.After != nil && *c.Before == *c.After {
			continue
		}
		out = append(out, c)
		if len(out) == auditMaxChanges {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// auditValue renders one JSON value as a person would write it. The second
// result is false for a value there is nothing useful to say about (a nested
// object), which drops that side of the pair.
func auditValue(key string, v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", true
	case bool:
		switch key {
		case "signed":
			return map[bool]string{true: "ลงนามแล้ว", false: "ยังไม่ลงนาม"}[x], true
		case "is_active", "linked_active":
			return map[bool]string{true: "ใช้งาน", false: "ปิดใช้งาน"}[x], true
		}
		return map[bool]string{true: "ใช่", false: "ไม่ใช่"}[x], true
	case float64:
		if key == "day_of_week" {
			days := []string{"อาทิตย์", "จันทร์", "อังคาร", "พุธ", "พฤหัสบดี", "ศุกร์", "เสาร์"}
			if d := int(x); float64(d) == x && d >= 0 && d < 7 {
				return days[d], true
			}
		}
		if x == float64(int64(x)) {
			switch key {
			case "academic_year", "year", "semester", "round", "study_year", "sec_no":
				// A year is not a quantity: 2569, never 2,569.
				return strconv.FormatInt(int64(x), 10), true
			}
			return groupThousands(int64(x)), true
		}
		return strconv.FormatFloat(x, 'f', 2, 64), true
	case string:
		return auditString(x), true
	case []any:
		if len(x) == 0 {
			return "", true
		}
		parts := make([]string, 0, len(x))
		for _, e := range x {
			switch s := e.(type) {
			case string:
				parts = append(parts, auditString(s))
			case float64, bool:
				p, _ := auditValue("", s)
				parts = append(parts, p)
			default:
				// A list of records: say how many, the detail is in the raw view.
				return fmt.Sprintf("%d รายการ", len(x)), true
			}
		}
		return truncateRunes(strings.Join(parts, ", "), 160), true
	}
	return "", false
}

var (
	reISODate   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	reISOMonth  = regexp.MustCompile(`^\d{4}-\d{2}$`)
	reISOStamp  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}`)
	reClockTime = regexp.MustCompile(`^(\d{2}:\d{2}):\d{2}(\.\d+)?$`)
)

// auditString renders a string value: enum words are translated, dates are
// written the way the rest of this system writes them (day first, Buddhist
// year), secrets say that they are secrets.
func auditString(s string) string {
	s = strings.TrimSpace(s)
	if s == auditRedacted {
		return "(ข้อมูลลับ ไม่แสดง)"
	}
	if l, found := auditValueLabels[s]; found {
		return l
	}
	switch {
	case reISODate.MatchString(s):
		if t, err := time.Parse("2006-01-02", s); err == nil {
			return auditDate(t)
		}
	case reISOMonth.MatchString(s):
		if t, err := time.Parse("2006-01", s); err == nil {
			return fmt.Sprintf("%02d/%d", int(t.Month()), buddhistYear(t.Year()))
		}
	case reISOStamp.MatchString(s):
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			t = t.In(timeutil.Bangkok)
			return auditDate(t) + " " + t.Format("15:04")
		}
	case reClockTime.MatchString(s):
		return reClockTime.FindStringSubmatch(s)[1]
	}
	return truncateRunes(s, 160)
}

// auditDate writes a date day-first with the Buddhist year, zero-padded so a
// column of them lines up.
func auditDate(t time.Time) string {
	return fmt.Sprintf("%02d/%02d/%d", t.Day(), int(t.Month()), buddhistYear(t.Year()))
}

// buddhistYear converts a Gregorian year, and leaves alone one that is already
// Buddhist: submission_periods.year_month is stored as "2569-08", and adding
// 543 to it printed the month as 08/3112.
func buddhistYear(y int) int {
	if y >= 2400 {
		return y
	}
	return y + 543
}

func groupThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// subjectFromImages names the subject from the row's own before/after, for a
// thing that has since been deleted.
func subjectFromImages(before, after map[string]any) string {
	for _, m := range []map[string]any{before, after} {
		if m == nil {
			continue
		}
		str := func(k string) string {
			s, _ := m[k].(string)
			return strings.TrimSpace(s)
		}
		if code := str("code"); code != "" {
			return strings.TrimSpace(code + " " + str("name_th"))
		}
		if name := strings.TrimSpace(str("first_name") + " " + str("last_name")); name != "" {
			return name
		}
		for _, k := range []string{"full_name", "filename", "file_name", "label"} {
			if v := str(k); v != "" {
				return truncateRunes(v, 80)
			}
		}
	}
	return ""
}

// ── Notes ───────────────────────────────────────────────────────────────────
//
// `note` is free text and its writers used it three ways: a reason a person
// typed, a short machine summary (`fetched=19 inserted=0`), and the query
// string of a page somebody looked at. Only the first is a sentence. The other
// two are parsed for the parts worth saying and the rest is left to the raw
// view.

var auditNoteKeyLabels = map[string]string{
	"q": "คำค้น", "months": "เดือน", "through_month": "ถึงเดือน", "month": "เดือน",
	"year": "ปี", "fetched": "ดึงมา", "inserted": "เพิ่มใหม่", "updated": "แก้ไข", "skipped": "ข้าม",
	"domain": "โดเมนอีเมล", "original": "วันเดิม", "kind": "ประเภทคาบ", "makeup": "วันชดเชย",
	"status": "สถานะ", "route": "ส่วนที่พยายามใช้", "rows": "จำนวนแถว",
}

var (
	reNoteMachine = regexp.MustCompile(`(^|[\s&(])[A-Za-z_]+=`)
	reUUIDAny     = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	reInserted    = regexp.MustCompile(`^inserted (\d+)/(\d+)$`)
	reThai        = regexp.MustCompile(`[\x{0E00}-\x{0E7F}]`)
)

// parseNotePairs reads `k=v` pairs out of a machine note. It accepts both the
// query-string form (`a=1&b=2`) and the space-separated form the services
// write, where a value may itself contain spaces (`makeup=2026-10-11 13:00-15:00`).
func parseNotePairs(note string) [][2]string {
	var out [][2]string
	if strings.Contains(note, "&") || !strings.Contains(note, " ") {
		for _, part := range strings.Split(note, "&") {
			k, v, found := strings.Cut(part, "=")
			if !found {
				continue
			}
			if dec, err := url.QueryUnescape(v); err == nil {
				v = dec
			}
			out = append(out, [2]string{k, v})
		}
		return out
	}
	for _, tok := range strings.Fields(note) {
		if k, v, found := strings.Cut(tok, "="); found && k != "" && !strings.ContainsAny(k, "()") {
			out = append(out, [2]string{k, v})
		} else if len(out) > 0 && !strings.ContainsAny(tok, "()") {
			out[len(out)-1][1] += " " + tok
		}
	}
	return out
}

// auditDetails is the line or two under the headline: the reason, the size of
// the thing, the month it was about.
func auditDetails(r *AuditRow) []string {
	if r.Note == nil {
		return nil
	}
	note := strings.TrimSpace(*r.Note)
	if note == "" {
		return nil
	}
	if l, found := auditValueLabels[note]; found {
		return []string{l}
	}
	if m := reInserted.FindStringSubmatch(note); m != nil {
		return []string{fmt.Sprintf("เพิ่ม %s จาก %s รายการ", m[1], m[2])}
	}
	if left, right, found := strings.Cut(note, " → "); found {
		return []string{auditString(left) + " → " + auditString(right)}
	}
	if !reNoteMachine.MatchString(note) {
		if reISODate.MatchString(note) || reISOMonth.MatchString(note) {
			return []string{auditString(note)}
		}
		// "สถานะเดิม: approved" — a sentence that ends in one of the system's
		// own words.
		if i := strings.LastIndex(note, ": "); i >= 0 {
			if l, found := auditValueLabels[note[i+2:]]; found {
				return []string{note[:i+2] + l}
			}
		}
		return []string{truncateRunes(note, 240)}
	}

	var out []string
	for _, kv := range parseNotePairs(note) {
		label, found := auditNoteKeyLabels[kv[0]]
		// An id means nothing to the reader, and has already been turned into
		// the row's subject where it could be.
		if !found || kv[1] == "" || reUUIDAny.MatchString(kv[1]) {
			continue
		}
		vals := strings.Split(kv[1], ",")
		for i, v := range vals {
			// Word by word, so "2026-10-11 13:00-15:00" has its date read as a
			// date and its time range left alone.
			words := strings.Fields(v)
			for j, w := range words {
				words[j] = auditString(w)
			}
			vals[i] = strings.Join(words, " ")
		}
		out = append(out, label+" "+strings.Join(vals, ", "))
	}
	// Something a person typed can contain an "=" too ("แก้เพราะ x=3"). If the
	// machine reading found nothing to say and the note has Thai in it, it was
	// a sentence all along — show it rather than swallow somebody's reason.
	if len(out) == 0 && reThai.MatchString(note) {
		return []string{truncateRunes(note, 240)}
	}
	return out
}

// noteSubjects finds what a row was about from ids its writer put in the note
// rather than in entity_id: the read-audit middleware records `term_id=…` for
// term-wide screens, a targeted search of the trail records `actor=…`, and the
// staff sign-off records `ta=… course=…`.
func noteSubjects(r *AuditRow) [][2]string {
	if r.Note == nil {
		return nil
	}
	var out [][2]string
	for _, kv := range parseNotePairs(*r.Note) {
		id := reUUIDAny.FindString(kv[1])
		if id == "" {
			continue
		}
		switch kv[0] {
		case "term_id":
			out = append(out, [2]string{"academic_term", id})
		case "actor", "ta":
			out = append(out, [2]string{"user", id})
		case "course":
			out = append(out, [2]string{"teaching_course", id})
		}
	}
	return out
}

// resolveVirtualSubjects names the rows the main query could not: the id was in
// the note, or entity_id pointed at something the resolver has no name for.
// One lookup per distinct id, and there are only ever a handful on a page.
func (s *AuditService) resolveVirtualSubjects(ctx context.Context, rows []AuditRow) error {
	cache := map[[2]string]string{}
	for i := range rows {
		if rows[i].SubjectName != "" {
			continue
		}
		var parts []string
		for _, k := range noteSubjects(&rows[i]) {
			label, done := cache[k]
			if !done {
				if err := s.pool.QueryRow(ctx,
					`SELECT COALESCE(audit_subject_label($1, $2), '')`, k[0], k[1]).Scan(&label); err != nil {
					return err
				}
				cache[k] = label
			}
			if label != "" {
				parts = append(parts, label)
			}
		}
		rows[i].SubjectName = strings.Join(parts, " · ")
	}
	return nil
}
