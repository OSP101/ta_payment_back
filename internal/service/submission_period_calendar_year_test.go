package service

import "testing"

func TestNormalizePeriodYearMonth(t *testing.T) {
	for _, tc := range []struct {
		name, ym, start, want string
		valid                 bool
	}{
		{"January calendar year", "2570-01", "2027-01-01", "2569-01", true},
		{"March calendar year", "2570-03", "2027-03-01", "2569-03", true},
		{"academic key", "2569-01", "2027-01-01", "2569-01", true},
		{"legacy different opening month", "2569-08", "2026-07-01", "2569-08", true},
		{"December", "2569-12", "2026-12-01", "2569-12", true},
		{"wrong calendar month", "2570-01", "2027-02-01", "2570-01", false},
		{"wrong calendar year", "2570-01", "2026-01-01", "2570-01", false},
		{"Gregorian key", "2027-01", "2027-01-01", "2027-01", false},
		{"unpadded month", "2570-1", "2027-01-01", "2570-1", false},
		{"invalid month", "2570-13", "2027-01-01", "2570-13", false},
		{"invalid opening date", "2570-01", "2027-02-30", "2570-01", false},
		{"unrelated year", "2571-01", "2028-01-01", "2571-01", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, valid := normalizePeriodYearMonth(tc.ym, 2569, tc.start)
			if got != tc.want || valid != tc.valid {
				t.Fatalf("got (%q, %v), want (%q, %v)", got, valid, tc.want, tc.valid)
			}
		})
	}
}

func TestSubmissionPeriodUpsertJanuaryCalendarYear(t *testing.T) {
	f := newFixture(t, fixtureOpts{TermStart: "2026-11-01", TermEnd: "2027-03-31"})
	f.clearPeriods()
	in := SubmissionPeriod{
		TermID: f.TermID, YearMonth: "2570-01", Label: "มกราคม 2570",
		StartsOn: "2027-01-01", DueDate: "2027-01-31", RemindDaysBefore: 3,
	}
	p, err := f.Periods.Upsert(f.ctx, f.StaffID, in)
	if err != nil {
		t.Fatalf("create January: %v", err)
	}
	var ym, label, start, due string
	if err := f.Pool.QueryRow(f.ctx, `SELECT year_month, label, starts_on::text, due_date::text
		FROM submission_periods WHERE id=$1`, p.ID).Scan(&ym, &label, &start, &due); err != nil {
		t.Fatal(err)
	}
	if p.YearMonth != "2569-01" || ym != "2569-01" || label != in.Label || start != in.StartsOn || due != in.DueDate {
		t.Fatalf("unexpected saved period: %+v, stored %s %s %s %s", p, ym, label, start, due)
	}
	// The normalized month must still enforce the finance lock for January.
	f.exec(`INSERT INTO submission_period_status (id, submission_period_id, ta_id, teaching_course_id, status)
		VALUES (gen_random_uuid(),$1,$2,$3,'finance_sent')`, p.ID, f.TAID, f.CourseID)
	state, err := resolvePeriodState(f.ctx, f.Pool, f.CourseID, f.TAID, "2027-01-15")
	if err != nil || !state.Found || state.Status != "finance_sent" {
		t.Fatalf("January finance lock lost: %+v, %v", state, err)
	}
	// Editing the same month with its calendar year must not trip the guard
	// that forbids moving an already signed-off period to a different month.
	in.ID = p.ID
	in.RemindDaysBefore = 5
	if _, err := f.Periods.Upsert(f.ctx, f.StaffID, in); err != nil {
		t.Fatalf("edit January: %v", err)
	}
}
