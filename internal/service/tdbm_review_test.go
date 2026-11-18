package service

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"ta-payment-back/internal/audit"
)

func (f *fixture) reviewHoliday(date string) {
	f.exec(`INSERT INTO public_holidays (id, holiday_date, name_th, source)
	 VALUES (gen_random_uuid(), $1::date, 'วันหยุดทดสอบ', 'custom')`, date)
}

func (f *fixture) reviewEntry(id int, date, start, end string) {
	f.exec(`INSERT INTO tdbm_extra_teachings (extra_class_id, academic_year, semester, class_date,
	 start_time, end_time, duration_minutes, opt_status, section_id, teaching_course_id)
	 VALUES ($1,$2,1,$3::date,$4::time,$5::time,120,'D',$6,$7)`, id, f.AcademicYear, date, start, end, f.SectionID, f.CourseID)
}

func (f *fixture) reviewService() *TDBMService {
	return &TDBMService{pool: f.Pool, aud: audit.New(f.Pool)}
}

func (f *fixture) makeupCount() int {
	var n int
	if err := f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM makeup_schedules`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) reviewInput(items ...TDBMMatchItem) TDBMMatchInput {
	return TDBMMatchInput{AcademicYear: f.AcademicYear, Semester: 1, Items: items}
}

func TestTDBMReview_MultipleHolidaysExplicitPairingAndSplitDays(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	s := f.reviewService()
	mon, _ := time.Parse("2006-01-02", firstMonday())
	var items []TDBMMatchItem
	for i := 0; i < 3; i++ {
		original := mon.AddDate(0, 0, 7*i).Format("2006-01-02")
		f.reviewHoliday(original)
		// Two periods on each holiday move to two different days. The oldest
		// holiday deliberately moves to the newest makeup date.
		for j, kind := range []string{"lecture", "lab"} {
			id := 930000 + i*2 + j
			makeup := mon.AddDate(0, 0, 35-i*7+j).Format("2006-01-02")
			f.reviewEntry(id, makeup, "10:00", "12:00")
			items = append(items, TDBMMatchItem{ExtraClassID: id, OriginalDate: original, Kind: kind})
		}
	}
	if n, err := s.AutoFillMakeupSchedules(f.ctx, f.AcademicYear, 1); err != nil || n != 0 {
		t.Fatalf("ambiguous days must await review: n=%d err=%v", n, err)
	}
	rows, err := s.ListExtraTeachings(f.ctx, f.AcademicYear, 1)
	if err != nil || len(rows) != 6 {
		t.Fatalf("review rows=%d err=%v", len(rows), err)
	}
	for _, r := range rows {
		if r.ResolutionReason != "มีวันหยุดหรือวันชดเชยหลายวัน ต้องเลือกคาบเดิมเพื่อป้องกันการจับคู่ผิดวัน" || len(r.MatchOptions) != 6 {
			t.Fatalf("missing reason or period options: %+v", r)
		}
	}
	if n, err := s.MatchMakeups(f.ctx, f.StaffID, f.reviewInput(items...)); err != nil || n != 6 {
		t.Fatalf("bulk split-day pairing: n=%d err=%v", n, err)
	}
	rows, err = s.ListExtraTeachings(f.ctx, f.AcademicYear, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		want := items[r.ExtraClassID-930000]
		if r.OriginalHolidayDate == nil || *r.OriginalHolidayDate != want.OriginalDate || r.OriginalKind == nil || *r.OriginalKind != want.Kind || r.MatchMethod != "manual" {
			t.Fatalf("wrong explicit pairing: %+v want %+v", r, want)
		}
	}
	if n, err := s.AutoFillMakeupSchedules(f.ctx, f.AcademicYear, 1); err != nil || n != 0 || f.makeupCount() != 6 {
		t.Fatalf("sync changed reviewed links: n=%d err=%v", n, err)
	}
	var auditRows int
	if err := f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM audit_logs WHERE action = 'makeup.match_from_tdbm' AND actor_id = $1`, f.StaffID).Scan(&auditRows); err != nil || auditRows != 6 {
		t.Fatalf("audit rows=%d err=%v", auditRows, err)
	}
}

func TestTDBMReview_RejectsWholeBatchOnStaleOrInvalidSelection(t *testing.T) {
	for _, scenario := range []string{"duplicate_period", "duplicate_entry", "cancelled", "wrong_term", "wrong_day", "taught", "already_matched", "alias_reused", "out_of_window", "exported"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{})
			s := f.reviewService()
			mon := firstMonday()
			f.reviewHoliday(mon)
			f.reviewEntry(940001, day(27), "10:00", "12:00")
			f.reviewEntry(940002, day(28), "13:00", "15:00")
			items := []TDBMMatchItem{{ExtraClassID: 940001, OriginalDate: mon, Kind: "lecture"}, {ExtraClassID: 940002, OriginalDate: mon, Kind: "lab"}}
			switch scenario {
			case "duplicate_period":
				items[1].Kind = "lecture"
			case "duplicate_entry":
				items[1].ExtraClassID = items[0].ExtraClassID
			case "cancelled":
				f.exec(`UPDATE tdbm_extra_teachings SET opt_status = 'C' WHERE extra_class_id = 940002`)
			case "wrong_term":
				f.exec(`UPDATE tdbm_extra_teachings SET semester = 2 WHERE extra_class_id = 940002`)
			case "wrong_day":
				d, _ := time.Parse("2006-01-02", mon)
				items[1].OriginalDate = d.AddDate(0, 0, 1).Format("2006-01-02")
			case "taught":
				f.insertLog(mon, "lab", "submitted", "13:00", "16:00")
			case "already_matched":
				if _, err := s.MatchMakeups(f.ctx, f.StaffID, f.reviewInput(items[1])); err != nil {
					t.Fatal(err)
				}
			case "alias_reused":
				f.exec(`UPDATE tdbm_extra_teachings SET class_date = $1::date, start_time = '10:00', end_time = '12:00' WHERE extra_class_id = 940002`, day(27))
			case "out_of_window":
				f.exec(`UPDATE tdbm_extra_teachings SET class_date = '2045-12-31' WHERE extra_class_id = 940002`)
			case "exported":
				f.exec(`UPDATE teaching_courses SET exported_at = now() WHERE id = $1`, f.CourseID)
			}
			before := f.makeupCount()
			if _, err := s.MatchMakeups(f.ctx, f.StaffID, f.reviewInput(items...)); err == nil {
				t.Fatal("invalid batch succeeded")
			}
			if got := f.makeupCount(); got != before {
				t.Fatalf("partial batch committed: before=%d after=%d", before, got)
			}
		})
	}
}

func TestTDBMReview_AliasesAndOverlappingHolidaysDoNotDuplicate(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	s := f.reviewService()
	mon := firstMonday()
	f.reviewHoliday(mon)
	f.exec(`INSERT INTO public_holidays (id, holiday_date, name_th, source, start_time, end_time)
	 VALUES (gen_random_uuid(), $1::date, 'วันหยุดซ้อน', 'faculty', '09:00', '12:00')`, mon)
	f.reviewEntry(950001, day(27), "10:00", "12:00")
	f.reviewEntry(950002, day(27), "10:00", "12:00")
	rows, err := s.ListExtraTeachings(f.ctx, f.AcademicYear, 1)
	if err != nil || len(rows) != 2 || len(rows[0].MatchOptions) != 2 || rows[0].ResolutionReason != "จำนวนคาบวันหยุดกับรายการชดเชยไม่ตรงกัน กรุณาเลือกจับคู่รายคาบ" {
		t.Fatalf("overlapping holidays duplicated options: rows=%+v err=%v", rows, err)
	}
	if n, err := s.MatchMakeups(f.ctx, f.StaffID, f.reviewInput(TDBMMatchItem{ExtraClassID: 950001, OriginalDate: mon, Kind: "lecture"})); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	var linked int
	if err := f.Pool.QueryRow(f.ctx, `SELECT count(DISTINCT applied_makeup_id) FROM tdbm_extra_teachings WHERE extra_class_id IN (950001,950002)`).Scan(&linked); err != nil || linked != 1 {
		t.Fatalf("aliases not linked together: n=%d err=%v", linked, err)
	}
}

func TestTDBMReview_ConcurrentReviewsOnlyConsumeSittingOnce(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	s := f.reviewService()
	mon := firstMonday()
	f.reviewHoliday(mon)
	f.reviewEntry(960001, day(27), "10:00", "12:00")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, kind := range []string{"lecture", "lab"} {
		wg.Add(1)
		go func(kind string) {
			defer wg.Done()
			_, err := s.MatchMakeups(f.ctx, f.StaffID, f.reviewInput(TDBMMatchItem{ExtraClassID: 960001, OriginalDate: mon, Kind: kind}))
			results <- err
		}(kind)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 || f.makeupCount() != 1 {
		t.Fatal(fmt.Sprintf("concurrent reviews: successes=%d makeups=%d", success, f.makeupCount()))
	}
}

func TestTDBMReview_ExportedMonthBlocksManualMatch(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mon := firstMonday()
	f.reviewHoliday(mon)
	f.reviewEntry(970001, day(27), "10:00", "12:00")
	f.addSubmissionPeriod(mon[5:7], day(28), "exported", false)
	if _, err := f.reviewService().MatchMakeups(f.ctx, f.StaffID, f.reviewInput(TDBMMatchItem{
		ExtraClassID: 970001, OriginalDate: mon, Kind: "lecture",
	})); err == nil || f.makeupCount() != 0 {
		t.Fatalf("exported month changed: err=%v", err)
	}
}

func TestTDBMAutoFill_ExactDateDoesNotDisambiguateMultipleHolidays(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mon := firstMonday()
	next, _ := time.Parse("2006-01-02", mon)
	f.reviewHoliday(mon)
	f.reviewHoliday(next.AddDate(0, 0, 7).Format("2006-01-02"))
	f.reviewEntry(980001, mon, "09:00", "12:00")
	f.reviewEntry(980002, mon, "13:00", "16:00")
	if n, err := f.reviewService().AutoFillMakeupSchedules(f.ctx, f.AcademicYear, 1); err != nil || n != 0 {
		t.Fatalf("a makeup on a holiday could replace either original day: n=%d err=%v", n, err)
	}
}
