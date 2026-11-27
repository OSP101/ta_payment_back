package service

import "testing"

// Exercise SQL writers as well as review options: filtering just the screen
// must not allow an excluded upstream status through a direct request.
func TestTDBMEligibility_AutoAndManual(t *testing.T) {
	for _, status := range []string{"A", "D", "P", "C", "X", "", "?"} {
		for _, mode := range []string{"automatic", "manual"} {
			t.Run(status+"/"+mode, func(t *testing.T) {
				f := newFixture(t, fixtureOpts{})
				s := f.reviewService()
				f.reviewHoliday(firstMonday())
				f.reviewEntry(970001, day(27), "09:00", "12:00")
				f.reviewEntry(970002, day(27), "13:00", "16:00")
				f.exec(`UPDATE tdbm_extra_teachings SET opt_status=$1`, status)
				eligible := status == "A" || status == "D"
				if mode == "automatic" {
					n, err := s.AutoFillMakeupSchedules(f.ctx, f.AcademicYear, 1)
					want := 0
					if eligible {
						want = 2
					}
					if err != nil || n != want || f.makeupCount() != want {
						t.Fatalf("automatic: count=%d err=%v want=%d", n, err, want)
					}
				} else {
					rows, err := s.ListExtraTeachings(f.ctx, f.AcademicYear, 1)
					if err != nil || len(rows) != 2 {
						t.Fatalf("staging rows=%d err=%v", len(rows), err)
					}
					for _, row := range rows {
						if (len(row.MatchOptions) > 0) != eligible {
							t.Fatalf("unexpected review eligibility: %+v", row)
						}
					}
					n, err := s.MatchMakeups(f.ctx, f.StaffID, f.reviewInput(TDBMMatchItem{
						ExtraClassID: 970001, OriginalDate: firstMonday(), Kind: "lecture",
					}))
					if eligible {
						if err != nil || n != 1 || f.makeupCount() != 1 {
							t.Fatalf("manual: count=%d err=%v", n, err)
						}
					} else if err == nil || n != 0 || f.makeupCount() != 0 {
						t.Fatalf("excluded status accepted: count=%d err=%v", n, err)
					}
				}
			})
		}
	}
}

func TestTDBMEligibility_RecheckBeforeAutomaticWrite(t *testing.T) {
	for _, status := range []string{"P", "C", "X", "missing"} {
		t.Run(status, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{})
			s := f.reviewService()
			f.reviewEntry(970001, day(27), "09:00", "12:00")
			candidates, err := s.loadTDBMCandidates(f.ctx, f.AcademicYear, 1)
			if err != nil || len(candidates) != 1 {
				t.Fatalf("candidates=%d err=%v", len(candidates), err)
			}
			if status == "missing" {
				f.exec(`DELETE FROM tdbm_extra_teachings WHERE extra_class_id=970001`)
			} else {
				f.exec(`UPDATE tdbm_extra_teachings SET opt_status=$1`, status)
			}
			n, err := s.applyOneMakeup(f.ctx, f.SectionID, unresolvedPeriod{
				SectionID: f.SectionID, HolidayDate: firstMonday(), Kind: "lecture",
			}, candidates[0])
			if err != nil || n != 0 || f.makeupCount() != 0 {
				t.Fatalf("stale candidate applied: count=%d err=%v", n, err)
			}
		})
	}
}

func TestTDBMEligibility_ExcludedAliasesStayUnapplied(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	s := f.reviewService()
	f.reviewHoliday(firstMonday())
	f.reviewEntry(970001, day(27), "09:00", "12:00")
	f.reviewEntry(970002, day(27), "13:00", "16:00")
	for i, status := range []string{"P", "C", "X", "?"} {
		f.reviewEntry(970010+i, day(27), "09:00", "12:00")
		f.exec(`UPDATE tdbm_extra_teachings SET opt_status=$1 WHERE extra_class_id=$2`, status, 970010+i)
	}
	// Run twice to exercise both initial alias linking and the later sweep.
	for i := 0; i < 2; i++ {
		if _, err := s.AutoFillMakeupSchedules(f.ctx, f.AcademicYear, 1); err != nil {
			t.Fatal(err)
		}
	}
	var linked int
	if err := f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM tdbm_extra_teachings
	 WHERE opt_status NOT IN ('A','D') AND applied_makeup_id IS NOT NULL`).Scan(&linked); err != nil || linked != 0 || f.makeupCount() != 2 {
		t.Fatalf("excluded aliases linked=%d err=%v", linked, err)
	}
}
