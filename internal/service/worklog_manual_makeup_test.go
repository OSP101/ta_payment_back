package service

import (
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestManualWorklogMakeup_OriginalDatePreviewAndSameDay(t *testing.T) {
	for _, scenario := range []string{"different_day", "target_holiday", "same_day", "same_day_changed_time", "stale_date", "wrong_kind", "outside_window"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{})
			orig := firstMonday()
			d, _ := time.Parse("2006-01-02", orig)
			target := d.AddDate(0, 0, 5).Format("2006-01-02")
			start, end := "09:00", "12:00"
			if scenario == "same_day" || scenario == "same_day_changed_time" {
				target = orig
			}
			if scenario == "same_day_changed_time" {
				start, end = "13:00", "16:00"
			}
			f.reviewHoliday(orig)
			if scenario == "target_holiday" {
				f.reviewHoliday(target)
			}
			id := uuid.New()
			f.exec(`INSERT INTO makeup_schedules(id,section_id,original_date,makeup_date,kind,start_time,end_time) VALUES($1,$2,$3::date,$4::date,'lecture',$5::time,$6::time)`, id, f.SectionID, orig, target, start, end)
			w := WorkLog{AssignmentID: f.AssignmentID, WorkDate: target, OriginalWorkDate: &orig, MakeupID: &id, StartTime: start, EndTime: end, Hours: 3, Activity: "lecture"}
			switch scenario {
			case "stale_date":
				f.exec(`UPDATE makeup_schedules SET makeup_date=makeup_date+1 WHERE id=$1`, id)
			case "wrong_kind":
				w.Activity = "lab"
			case "outside_window":
				w.StartTime = "08:00"
				w.EndTime = "11:00"
			}
			row, err := f.upsert(w)
			rejected := scenario == "stale_date" || scenario == "wrong_kind" || scenario == "outside_window"
			if (err != nil) != rejected {
				t.Fatalf("save err=%v rejected=%v", err, rejected)
			}
			if !rejected {
				var got string
				if err = f.Pool.QueryRow(f.ctx, `SELECT work_date::text FROM work_logs WHERE id=$1`, row).Scan(&got); err != nil || got != target {
					t.Fatalf("saved original date instead of actual: %s %v", got, err)
				}
			}
		})
	}
}

func TestManualWorklogMakeup_OnlyTheCancelledKindIsBlocked(t *testing.T) {
	for _, waived := range []bool{false, true} {
		t.Run(map[bool]string{false: "moved", true: "waived"}[waived], func(t *testing.T) {
			f := newFixture(t, fixtureOpts{})
			orig := firstMonday()
			id := uuid.New()
			if waived {
				f.exec(`INSERT INTO makeup_schedules(id,section_id,original_date,kind,waived) VALUES($1,$2,$3::date,'lecture',true)`, id, f.SectionID, orig)
			} else {
				f.exec(`INSERT INTO makeup_schedules(id,section_id,original_date,makeup_date,kind,start_time,end_time) VALUES($1,$2,$3::date,$4::date,'lecture','09:00','12:00')`, id, f.SectionID, orig, day(27))
			}
			if _, err := f.upsert(WorkLog{AssignmentID: f.AssignmentID, WorkDate: orig, Activity: "lab", StartTime: "13:00", EndTime: "16:00", Hours: 3}); err != nil {
				t.Fatalf("lecture cancellation blocked normal lab: %v", err)
			}
		})
	}
}

func TestManualWorklogMakeup_SameDayOldTimeAndMissingMakeupAreRefused(t *testing.T) {
	for _, hasMakeup := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_makeup", true: "old_window"}[hasMakeup], func(t *testing.T) {
			f := newFixture(t, fixtureOpts{})
			orig := firstMonday()
			f.reviewHoliday(orig)
			if hasMakeup {
				f.exec(`INSERT INTO makeup_schedules(section_id,original_date,makeup_date,kind,start_time,end_time) VALUES($1,$2::date,$2::date,'lecture','13:00','16:00')`, f.SectionID, orig)
			}
			if _, err := f.upsert(WorkLog{AssignmentID: f.AssignmentID, WorkDate: orig, Activity: "lecture", StartTime: "09:00", EndTime: "12:00", Hours: 3}); err == nil {
				t.Fatal("holiday or superseded original time was accepted")
			}
		})
	}
}
