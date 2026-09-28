package service

import (
	"context"
	"strings"
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

	// Still inside an open period the TA can fix it, so the month is kept as
	// it is — a fresh draft beside the rejected slot would be sent twice.
	res, err := f.Svc.Generate(ctx, f.TAID, f.AssignmentID)
	if err != nil {
		t.Fatalf("Generate with a fixable rejected row: %v", err)
	}
	if len(res.KeptMonths) != 1 || res.KeptMonths[0] != day(12)[:7] {
		t.Errorf("kept_months = %v, want [%s]", res.KeptMonths, day(12)[:7])
	}
	for _, e := range res.Entries {
		if e.WorkDate[:7] == day(12)[:7] {
			t.Fatalf("Generate wrote %s into the month holding a fixable rejected row", e.WorkDate)
		}
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

// A lecturer approved one month and bounced another. The TA deletes the
// bounced rows to regenerate them — Generate used to refuse the whole run over
// the approved month, so the only way back was typing the month by hand.
// Months in review are now kept untouched and only the rest is regenerated.
func TestGenerate_KeepsReviewedMonthsAndRegeneratesTheRest(t *testing.T) {
	f := newFixture(t, fixtureOpts{Workload: workloadHours{CheckWork: 2}})
	f.addDutySlot(DutyReview, 3, "17:00", "18:00") // Wednesday evening

	first, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	m0 := monthStart().Format("2006-01")
	m1 := monthStart().AddDate(0, 1, 0).Format("2006-01")
	m2 := monthStart().AddDate(0, 2, 0).Format("2006-01")
	perMonth := map[string]int{}
	for _, e := range first.Entries {
		perMonth[e.WorkDate[:7]]++
	}
	if perMonth[m0] == 0 || perMonth[m1] == 0 || perMonth[m2] == 0 {
		t.Fatalf("fixture must generate into all three months, got %v", perMonth)
	}

	count := func(ym, status string) int {
		t.Helper()
		var n int
		if err := f.Pool.QueryRow(f.ctx,
			`SELECT COUNT(*) FROM work_logs WHERE assignment_id=$1 AND to_char(work_date,'YYYY-MM')=$2 AND status=$3`,
			f.AssignmentID, ym, status).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1 AND to_char(work_date,'YYYY-MM')=$2`, f.AssignmentID, m0)
	f.exec(`UPDATE work_logs SET status='rejected', reject_reason='แก้ไข' WHERE assignment_id=$1 AND to_char(work_date,'YYYY-MM')=$2`, f.AssignmentID, m1)
	f.exec(`DELETE FROM work_logs WHERE assignment_id=$1 AND to_char(work_date,'YYYY-MM')=$2`, f.AssignmentID, m2)

	res, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID)
	if err != nil {
		t.Fatalf("Generate must run beside an approved and a bounced month: %v", err)
	}
	if got := strings.Join(res.KeptMonths, ","); got != m0+","+m1 {
		t.Errorf("kept_months = %s, want %s,%s", got, m0, m1)
	}
	if count(m0, "approved") != perMonth[m0] || count(m0, "draft") != 0 {
		t.Errorf("approved month changed: approved %d (want %d), drafts %d", count(m0, "approved"), perMonth[m0], count(m0, "draft"))
	}
	if count(m1, "rejected") != perMonth[m1] || count(m1, "draft") != 0 {
		t.Errorf("bounced month must be left for the TA to fix: rejected %d (want %d), drafts %d", count(m1, "rejected"), perMonth[m1], count(m1, "draft"))
	}
	if count(m2, "draft") != perMonth[m2] {
		t.Errorf("free month regenerated %d drafts, want %d", count(m2, "draft"), perMonth[m2])
	}

	// Deleting the bounced rows releases the month.
	f.exec(`DELETE FROM work_logs WHERE assignment_id=$1 AND status='rejected'`, f.AssignmentID)
	if _, err := f.Svc.Generate(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatalf("Generate after clearing the bounced month: %v", err)
	}
	if count(m1, "draft") != perMonth[m1] {
		t.Errorf("cleared month regenerated %d drafts, want %d", count(m1, "draft"), perMonth[m1])
	}
	if count(m0, "approved") != perMonth[m0] {
		t.Errorf("approved month changed on the second run")
	}
}
