package service

import (
	"strings"
	"testing"
)

// UAT DEF-004: once part of a month is sent, a new row used to be refused for
// the whole month. With Submit only sending days that have happened (DEF-003)
// that closed every remaining day of the month, and a makeup the lecturer
// filed after the TA sent the month could never be logged. Now: a day after
// the month's last sent day, or a makeup day of the section, may be added;
// back-filling before the sent days is still refused.
func TestUpsert_NewRowInAPartlySentMonth(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}

	if _, err := f.upsert(f.entry(day(12), "09:00", "10:00", 1)); err != nil {
		t.Errorf("a day after the last sent day must be accepted: %v", err)
	}

	_, err := f.upsert(f.entry(day(5), "09:00", "10:00", 1))
	if err == nil || !strings.Contains(err.Error(), "เพิ่มย้อนหลังไม่ได้") {
		t.Errorf("back-filling before the sent days: err = %v, want refusal", err)
	}

	// The lecturer files a makeup on day 4 after the month was sent.
	f.exec(`INSERT INTO makeup_schedules (id, section_id, original_date, makeup_date, start_time, end_time, kind)
	        VALUES (gen_random_uuid(), $1, $2::date, $3::date, '13:00', '15:00', 'lecture')`,
		f.SectionID, day(3), day(4))
	if _, err := f.upsert(f.entry(day(4), "09:00", "10:00", 1)); err != nil {
		t.Errorf("a makeup day must be accepted even before the sent days: %v", err)
	}
}
