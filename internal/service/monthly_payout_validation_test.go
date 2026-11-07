package service

import (
	"github.com/google/uuid"
	"math"
	"math/rand"
	"sync"
	"testing"
)

// Independent arithmetic oracle: allocation must conserve every satang,
// remain nonnegative, and leave a frozen payment untouched over a full term.
func TestMonthlyMoneyConservationAcrossChangingWeights(t *testing.T) {
	rng := rand.New(rand.NewSource(78111))
	for sample := 0; sample < 2000; sample++ {
		totalCents := int64(rng.Intn(10000001) + 1)
		all := []TermMonth{{YearMonth: "2026-11"}, {YearMonth: "2026-12"}, {YearMonth: "2027-01"}, {YearMonth: "2027-02"}, {YearMonth: "2027-03"}}
		paid := int64(0)
		frozen := &frozenGradLump{months: map[string]float64{}}
		for i, m := range all {
			actual, planned := map[string]float64{}, map[string]float64{}
			for _, p := range all {
				planned[p.YearMonth] = float64(rng.Intn(100) + 1)
				if rng.Intn(3) > 0 {
					actual[p.YearMonth] = float64(rng.Intn(100) + 1)
				}
			}
			allocation := placeLump(float64(totalCents-paid)/100, withoutFrozen(monthlyLumpWeights(all, actual, planned), frozen))
			monthCents := int64(math.Round(allocation[m.YearMonth] * 100))
			if monthCents < 0 || monthCents > totalCents-paid {
				t.Fatalf("sample %d invalid amount %d", sample, monthCents)
			}
			paid += monthCents
			frozen.months[m.YearMonth] = float64(monthCents) / 100
			if i == len(all)-1 && paid != totalCents {
				t.Fatalf("sample %d paid %d cents, basis %d", sample, paid, totalCents)
			}
		}
	}
}

func TestMonthlyConcurrentLumpExportsCannotOverdrawWholeTerm(t *testing.T) {
	f := newTCFixture(t)
	course, _, _ := f.insertCourse(tcCourseOpts{Code: "CPRACE", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("race", "master")
	actor := f.actor()
	snap := func(ym string) *GradLumpSnapshot {
		return &GradLumpSnapshot{CourseID: course, Lump: 1000, byTA: map[uuid.UUID]map[string]float64{ta: {ym: 900}}, basis: map[uuid.UUID]float64{ta: 1000}}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, ym := range []string{"2026-06", "2026-07"} {
		wg.Add(1)
		go func(month string) {
			defer wg.Done()
			<-start
			results <- f.svc.FreezeGradLumpSnapshot(f.ctx, actor, snap(month), []string{month})
		}(ym)
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if userErrStatus(err) != 409 {
			t.Fatal(err)
		}
	}
	var paid float64
	if err := f.pool.QueryRow(f.ctx, `SELECT COALESCE(sum(baht),0)::float8 FROM grad_lump_ledger WHERE teaching_course_id=$1`, course).Scan(&paid); err != nil {
		t.Fatal(err)
	}
	if success != 1 || paid > 1000 {
		t.Fatalf("stale concurrent snapshots accepted: success=%d paid=%.2f basis=1000", success, paid)
	}
}

func TestMonthlyLumpRejectsAStaleRateBasis(t *testing.T) {
	f := newTCFixture(t)
	course, _, _ := f.insertCourse(tcCourseOpts{Code: "CPBASIS", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("basis", "master")
	actor := f.actor()
	snap := func(ym string, basis, amount float64) *GradLumpSnapshot {
		return &GradLumpSnapshot{CourseID: course, Lump: basis, byTA: map[uuid.UUID]map[string]float64{ta: {ym: amount}}, basis: map[uuid.UUID]float64{ta: basis}}
	}
	if err := f.svc.FreezeGradLumpSnapshot(f.ctx, actor, snap("2026-06", 1000, 500), []string{"2026-06"}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.FreezeGradLumpSnapshot(f.ctx, actor, snap("2026-07", 1200, 100), []string{"2026-07"}); userErrStatus(err) != 409 {
		t.Fatalf("changed term basis must be rejected: %v", err)
	}
}

func TestMonthlyClaimEligibilityPreservesApprovedHistoricalHours(t *testing.T) {
	for _, kind := range []string{"dropped", "rejected"} {
		t.Run(kind, func(t *testing.T) {
			f := newTCFixture(t)
			course, sec, _ := f.insertCourse(tcCourseOpts{Code: "CPINACTIVE", Curriculum: "CY", LectureHrs: 100})
			ta := f.newTA("inactive", "undergrad")
			assignment := f.assignTAOn(ta, course, sec, "undergrad", []string{"2026-06-01"})
			if kind == "dropped" {
				f.exec(`UPDATE ta_request_assignments SET state='dropped' WHERE id=$1`, assignment)
			} else {
				f.exec(`UPDATE ta_requests SET status='rejected' WHERE id=(SELECT request_id FROM ta_request_assignments WHERE id=$1)`, assignment)
			}
			ids, err := f.svc.PayoutMonthCourses(f.ctx, f.termID, "2026-06")
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if kind == "dropped" {
				want = 1
			} // ending an assignment does not forfeit earned, approved hourly work
			if len(ids) != want {
				t.Fatalf("%s: payable courses=%v want count%d", kind, ids, want)
			}
		})
	}
}

func TestMonthlyLumpFiguresAgreeAcrossClaimCoverAndBudgetSummary(t *testing.T) {
	f := newTCFixture(t)
	for _, ym := range []string{"2569-06", "2569-07", "2569-08"} {
		f.addPeriod(ym)
	}
	course, regular, special := f.insertCourse(tcCourseOpts{Code: "CP999001", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("ตรวจยอดรายเดือน", "master")
	actor := f.actor()
	assignment := f.assignTAOn(ta, course, special, "master", nil)
	f.exec(`INSERT INTO section_schedules(id,section_id,kind,day_of_week,start_time,end_time) VALUES(gen_random_uuid(),$1,'lecture',1,'09:00','11:00')`, regular)
	f.exec(`INSERT INTO work_logs(id,assignment_id,work_date,start_time,end_time,hours,activity,status) VALUES(gen_random_uuid(),$1,'2026-06-01','09:00','11:00',2,'lecture','approved'),(gen_random_uuid(),$1,'2026-07-06','09:00','13:00',4,'lecture','approved')`, assignment)
	// June actual 2h, July actual 4h, August's five Mondays planned 10h.
	// Independent expected split of 1,000 baht: 125 + 250 + 625.
	split, err := f.svc.gradLumpByMonth(f.ctx, course, ta, 1000, true)
	if err != nil {
		t.Fatal(err)
	}
	for ym, want := range map[string]float64{"2026-06": 125, "2026-07": 250, "2026-08": 625} {
		if split[ym] != want {
			t.Fatalf("%s got %.2f want %.2f", ym, split[ym], want)
		}
	}
	if err = f.svc.FreezeGradLumps(f.ctx, actor, course, []string{"2026-06"}); err != nil {
		t.Fatal(err)
	}
	// A new rate and later approval must not change the issued June figure.
	f.exec(`UPDATE pay_rates SET graduate_special_lumpsum=2000`)
	f.exec(`UPDATE work_logs SET hours=8,end_time='17:00' WHERE assignment_id=$1 AND work_date='2026-07-06'`, assignment)
	split, err = f.svc.gradLumpByMonth(f.ctx, course, ta, 2000, true)
	if err != nil {
		t.Fatal(err)
	}
	if split["2026-06"] != 125 || sumMonths(split, nil) != 1000 {
		t.Fatal(split)
	}
	cover, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID, []string{"2026-06"}, "graduate")
	if err != nil {
		t.Fatal(err)
	}
	if got := sheetsTotal(cover); got != 125 {
		t.Fatalf("cover=%.2f want125", got)
	}
	summary, _, _, _, err := f.svc.CourseSummaryPreview(f.ctx, f.termID, []string{"2026-06"})
	if err != nil {
		t.Fatal(err)
	}
	row := findCourseSummaryRow(t, summary, "CP999001")
	if got := findMonthAmount(t, row, "2026-06").Special; got != 125 {
		t.Fatalf("budget summary=%.2f want125", got)
	}
}

func TestMonthlyNoLogLumpStillRequiresAppointmentAndDocuments(t *testing.T) {
	f := newTCFixture(t)
	f.addPeriod("2569-06")
	course, _, special := f.insertCourse(tcCourseOpts{Code: "CPORDER", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("appointment", "master")
	f.assignTAOn(ta, course, special, "master", nil)
	blockers, err := f.svc.CourseExportBlockers(f.ctx, course, []string{"2026-06"})
	if err != nil {
		t.Fatal(err)
	}
	foundOrder, foundProfile := false, false
	for _, b := range blockers {
		foundOrder = foundOrder || b.Kind == "not_appointed"
		foundProfile = foundProfile || b.Kind == "profile"
	}
	if !foundOrder || !foundProfile {
		t.Fatalf("no-log lump bypassed prerequisites: %+v", blockers)
	}
}
