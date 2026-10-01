package service

import (
	"testing"
	"time"
)

// A makeup filed before AddMakeup checked the course window can point years
// past the term (one on the dev DB pointed at 2045-12-31). Generate used to
// move the period there, producing a draft the TA could never submit
// ("วันที่ทำงานต้องอยู่ในช่วงภาคการศึกษา") and that silently ate the period.
func TestGenerate_SkipsMakeupOutsideTheCourse(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	// First Monday of the term — the fixture teaches on Mondays.
	d := monthStart()
	for d.Weekday() != time.Monday {
		d = d.AddDate(0, 0, 1)
	}
	far := d.AddDate(10, 0, 0).Format("2006-01-02")
	f.exec(`INSERT INTO makeup_schedules (id, section_id, original_date, makeup_date, note, kind)
	        VALUES (gen_random_uuid(), $1, $2::date, $3::date, 'ข้อมูลเก่านอกเทอม', 'lecture')`,
		f.SectionID, d.Format("2006-01-02"), far)

	res, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, e := range res.Entries {
		if e.WorkDate == far {
			t.Fatalf("generated a row on %s, outside the course — the TA can never submit it", far)
		}
	}
	var n int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM work_logs WHERE assignment_id = $1 AND work_date = $2::date`,
		f.AssignmentID, far).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d rows stored on %s", n, far)
	}
}
