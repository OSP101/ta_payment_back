package service

import (
	"context"
	"testing"
)

// A lecturer bounced the TA's month, the TA did not fix it before the period
// closed, and the rows became forfeited: the TA can no longer edit, resend or
// delete them. Generate used to count every rejected row as "in review" and
// refuse, so that TA could never auto-generate the rest of the term again —
// and nothing on screen could clear the block.
func TestGenerate_IgnoresRejectedRowsInClosedMonth(t *testing.T) {
	f := scheduleFixture(t)
	f.addOwnClassInTerm(3, "08:00", "09:00")
	ctx := context.Background()

	pid := f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "", false)
	f.exec(`INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity, status, reject_reason)
	        VALUES ($1, $2, '09:00', '10:00', 1, 'review', 'rejected', 'ไม่ถูกต้อง')`,
		f.AssignmentID, day(12))

	// Still inside an open period the TA can fix it, so regenerating stays
	// refused — it would mint a fresh draft beside the rejected slot.
	if _, err := f.Svc.Generate(ctx, f.TAID, f.AssignmentID); err == nil {
		t.Fatal("a rejected row in an OPEN month must still block Generate")
	}

	f.exec(`UPDATE submission_periods SET is_closed = TRUE WHERE id = $1`, pid)
	if _, err := f.Svc.Generate(ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("a forfeited rejected row (period closed) must not block Generate: %v", err)
	}

	// Nothing may be generated into the closed month, and the forfeited row stays.
	var inClosed, rejected int
	if err := f.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FILTER (WHERE status = 'draft'), COUNT(*) FILTER (WHERE status = 'rejected')
		   FROM work_logs WHERE assignment_id = $1 AND to_char(work_date,'MM') = $2`,
		f.AssignmentID, currentMonthMM()).Scan(&inClosed, &rejected); err != nil {
		t.Fatal(err)
	}
	if inClosed != 0 {
		t.Errorf("Generate wrote %d drafts into the closed month", inClosed)
	}
	if rejected != 1 {
		t.Errorf("rejected rows in the closed month = %d, want the 1 forfeited row kept", rejected)
	}
}

// The ฿300/day cap is per TA per day across every course (rule 6a). Generate
// used to count only the rows it was writing, so a TA already carrying a day in
// another course got a generated row that pushed it over — and the lecturer's
// approval then failed on a day their own course priced well under the cap.
//
// A session that would cross the cap is shortened to what still fits, in
// 30-minute steps, rather than dropped: the TA claims as much as the rule
// allows. Only when not even 30 minutes fit is the session left out.
func TestGenerate_DailyBahtCountsOtherCourses(t *testing.T) {
	cases := []struct {
		name         string
		otherHours   string // hours the other course already holds that day, at 40฿
		otherEnd     string
		wantRows     int
		wantEnd      string
		wantHours    float64
		wantSkipped  float64
		wantExisting float64
	}{
		// 280฿ + a 1-hour session at 40฿ = 320฿: 20฿ of room = 0.5 ชม.
		{"trimmed to fit", "7", "15:00", 1, "17:30", 0.5, 0.5, 280},
		// 300฿ already: no room at all, the session is left out.
		{"no room left", "7.5", "15:30", 0, "", 0, 1, 300},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{Rates: rateOverrides{UndergradRegular: 40, DailyPayCapBaht: 300}, Workload: workloadHours{CheckWork: 2}})
			f.addDutySlot(DutyReview, 3, "17:00", "18:00") // Wednesday evening

			rows, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			var target string
			for _, r := range rows.Entries {
				if r.Activity == "review" {
					target = r.WorkDate
					break
				}
			}
			if target == "" {
				t.Fatal("fixture generated no review row to collide with")
			}

			other := f.secondCourseAssignment(fixtureOpts{})
			f.exec(`INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity, status)
			        VALUES ($1, $2, '08:00', $3::time, $4::numeric, 'review', 'approved')`,
				other, target, tc.otherEnd, tc.otherHours)

			res, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID)
			if err != nil {
				t.Fatalf("regenerate: %v", err)
			}

			var got []WorkLog
			for _, r := range res.Entries {
				if r.WorkDate == target {
					got = append(got, r)
				}
			}
			if len(got) != tc.wantRows {
				t.Fatalf("rows on %s = %d, want %d (%+v)", target, len(got), tc.wantRows, got)
			}
			if tc.wantRows == 1 {
				if got[0].Hours != tc.wantHours || hhmmPrefix(got[0].EndTime) != tc.wantEnd || hhmmPrefix(got[0].StartTime) != "17:00" {
					t.Errorf("row = %s–%s %.2f ชม., want 17:00–%s %.1f ชม.",
						got[0].StartTime, got[0].EndTime, got[0].Hours, tc.wantEnd, tc.wantHours)
				}
			}

			// ...and the TA is told, so a short day has a reason attached.
			var reported *DailyCapSkip
			for i := range res.SkippedDailyBaht {
				if res.SkippedDailyBaht[i].Date == target {
					reported = &res.SkippedDailyBaht[i]
				}
			}
			if reported == nil {
				t.Fatalf("skipped_daily_baht = %+v, want %s listed", res.SkippedDailyBaht, target)
			}
			if reported.Existing != tc.wantExisting || reported.Hours != tc.wantSkipped {
				t.Errorf("reported %+v, want existing_baht %v and %v ชม. left out", *reported, tc.wantExisting, tc.wantSkipped)
			}
			if res.DailyBahtCap != 300 {
				t.Errorf("daily_baht_cap = %v, want 300", res.DailyBahtCap)
			}
		})
	}
}
