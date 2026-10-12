package i18n

import "testing"

func TestTranslate(t *testing.T) {
	once.Do(func() {}) // skip the embedded catalog; use this test's own
	exact = map[string]string{"ข้อมูลไม่ถูกต้อง": "Invalid data", "ภาคปกติ": "regular program"}
	catalogJSON = nil
	patterns = nil
	for th, en := range map[string]string{
		"เหลือ %d ชั่วโมง":         "%d hours left",
		"วิชา %s ยังไม่มี %s":      "course %s has no %s",
		"ส่ง %[2]s ให้ %[1]s แล้ว": "sent %[1]s to %[2]s",
	} {
		addPattern(th, en)
	}
	cases := map[string]string{
		"ข้อมูลไม่ถูกต้อง":            "Invalid data",
		"เหลือ 12 ชั่วโมง":            "12 hours left",
		"วิชา CP101 ยังไม่มี ภาคปกติ": "course CP101 has no regular program",
		"ไม่รู้จัก":                   "ไม่รู้จัก",
		"ข้อมูลไม่ถูกต้อง: ภาคปกติ":   "Invalid data: regular program",
		"already english": "already english",
	}
	for in, want := range cases {
		if got := T("en", in); got != want {
			t.Errorf("T(en, %q) = %q, want %q", in, got, want)
		}
	}
	if got := T("th", "ข้อมูลไม่ถูกต้อง"); got != "ข้อมูลไม่ถูกต้อง" {
		t.Errorf("th must be unchanged, got %q", got)
	}
}

func TestDatesAndGluedCodes(t *testing.T) {
	SetForTest(map[string]string{"บันทึกเวลาปฏิบัติงานได้รับการอนุมัติ": "Work log approved"})
	for in, want := range map[string]string{
		"มิถุนายน 2569":      "June 2026",
		"15 ก.ค. 2569":       "15 Jul 2026",
		"รอบ 1 (มิ.ย.–ก.ย.)": "รอบ 1 (Jun–Sep)",
		"ปีการศึกษา 2568":    "ปีการศึกษา 2568",
		"บันทึกเวลาปฏิบัติงานได้รับการอนุมัติ CP321002": "Work log approved CP321002",
	} {
		if got := T("en", in); got != want {
			t.Errorf("T(en, %q) = %q, want %q", in, got, want)
		}
	}
}

func TestNoHalfTranslation(t *testing.T) {
	SetForTest(nil)
	addPattern("บันทึกเวลาปฏิบัติงานรายวิชา %s", "Work log for course %s")
	in := "บันทึกเวลาปฏิบัติงานรายวิชา CP321002 ของท่านได้รับการอนุมัติจากอาจารย์ผู้สอนเรียบร้อยแล้ว"
	if got := T("en", in); got != in {
		t.Errorf("half translation: %q", got)
	}
	if got := T("en", "บันทึกเวลาปฏิบัติงานรายวิชา CP321002 Data Science"); got != "Work log for course CP321002 Data Science" {
		t.Errorf("short arg should still translate: %q", got)
	}
}

func TestGluedTailIsTranslatedWhole(t *testing.T) {
	SetForTest(map[string]string{"เปิดรับคำขอผู้ช่วยสอน": "TA requests open"})
	addPattern("ภาคการศึกษาที่ %d ปีการศึกษา %d", "semester %d, academic year %d")
	if got := T("en", "เปิดรับคำขอผู้ช่วยสอน ภาคการศึกษาที่ 2 ปีการศึกษา 2568"); got != "TA requests open semester 2, academic year 2568" {
		t.Errorf("got %q", got)
	}
}

// A notice body is translated line by line: a one-line key must not swallow
// the following lines through its last value, each line's own values are still
// translated, and a month split from its year by a format keeps the CE year.
func TestNoticeBodyLines(t *testing.T) {
	SetForTest(map[string]string{"กันยายน": "September"})
	addPattern("มีผลตั้งแต่วันที่ %s", "effective %s.")
	addPattern("%d. %s รอตารางเรียน", "%d. %s, waiting for a class schedule")
	addPattern("%s เวลา %s น.", "%s at %s")
	addPattern("ส่งเมื่อ %s", "sent %s")
	addPattern("ส่งกลับ %s %s", "returned: %s %s")
	in := "มีผลตั้งแต่วันที่ 24 กันยายน 2569\n\n1. Ann รอตารางเรียน\nส่งเมื่อ 1 กันยายน 2569 เวลา 10.30 น."
	want := "effective 24 September 2026.\n\n1. Ann, waiting for a class schedule\nsent 1 September 2026 at 10.30"
	if got := T("en", in); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if got := T("en", "ส่งกลับ CP1 กันยายน 2569"); got != "returned: CP1 September 2026" {
		t.Errorf("month and year split: %q", got)
	}
}
