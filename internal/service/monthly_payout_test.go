package service

import (
	"math"
	"strings"
	"testing"
)

func TestMonthlyLumpReservesFutureMonthsAndPaysWithoutLogs(t *testing.T) {
	all := []TermMonth{{YearMonth: "2026-06"}, {YearMonth: "2026-07"}, {YearMonth: "2026-08"}}
	weights := monthlyLumpWeights(all, map[string]float64{"2026-06": 10}, map[string]float64{"2026-06": 8, "2026-07": 10, "2026-08": 20})
	split := placeLump(4000, weights)
	if split["2026-06"] != 1000 || split["2026-07"] != 1000 || split["2026-08"] != 2000 {
		t.Fatalf("split=%v", split)
	}
	noLogs := placeLump(4000, monthlyLumpWeights(all, nil, nil))
	sum := 0.0
	for _, m := range all {
		if noLogs[m.YearMonth] <= 0 {
			t.Fatalf("no-log month unpaid: %v", noLogs)
		}
		sum += noLogs[m.YearMonth]
	}
	if math.Abs(sum-4000) > 0.001 {
		t.Fatalf("sum=%v", sum)
	}
	frozen := &frozenGradLump{months: map[string]float64{"2026-06": 1000}, total: 1000, basis: 4000}
	later := placeLump(4000-frozen.total, withoutFrozen(monthlyLumpWeights(all, map[string]float64{"2026-07": 5}, map[string]float64{"2026-08": 10}), frozen))
	if later["2026-07"]+later["2026-08"] != 3000 {
		t.Fatal(later)
	}
}

func TestMonthlyProgressSeparatesMonthsAndLegacy(t *testing.T) {
	f := newTCFixture(t)
	for _, ym := range []string{"2569-06", "2569-07", "2569-10"} {
		f.addPeriod(ym)
	}
	course, sec, _ := f.insertCourse(tcCourseOpts{Code: "CPMONTH", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("รายเดือน", "undergrad")
	f.assignTAOn(ta, course, sec, "undergrad", []string{"2026-06-08", "2026-07-06"})
	svc := progressSvcTC(f)
	overview, err := svc.GetOverview(f.ctx, f.termID)
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.Rounds) != 3 {
		t.Fatalf("rounds=%+v", overview.Rounds)
	}
	if overview.Rounds[0].Round != 202606 || overview.Rounds[1].Round != 202607 {
		t.Fatal(overview.Rounds)
	}
	if overview.Rounds[0].AllExported || overview.Rounds[1].AllExported {
		t.Fatal("unissued month is ready")
	}
	// A July tick cannot appear on June's document.
	f.exec(`INSERT INTO signature_checklist(term_id,teaching_course_id,role,signer_id,fiscal_round,signed_at) VALUES($1,$2,'ta',$3,202607,now())`, f.termID, course, ta)
	items, err := svc.ListChecklist(f.ctx, f.termID, 202606)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.SignedAt != nil {
			t.Fatal("July signature leaked into June")
		}
	}
	f.exec(`INSERT INTO payout_legacy_months VALUES($1,'2026-06')`, f.termID)
	if _, err = svc.GetRound(f.ctx, f.termID, 202606); err == nil {
		t.Fatal("legacy month reinterpreted as monthly")
	}
	if _, exists, err := svc.roundMonths(f.ctx, f.termID, 202601); err != nil || exists {
		t.Fatalf("foreign month exists=%v err=%v", exists, err)
	}
}

func TestMonthlyExportSelectionAndDeadline(t *testing.T) {
	f := newTCFixture(t)
	f.addPeriod("2569-06")
	f.addPeriod("2569-07")
	if err := f.svc.ValidatePayoutSelection(f.ctx, f.termID, []string{"2026-06", "2026-07"}); err == nil {
		t.Fatal("new multi-month export accepted")
	}
	f.exec(`INSERT INTO payout_legacy_months VALUES($1,'2026-06'),($1,'2026-07')`, f.termID)
	if err := f.svc.ValidatePayoutSelection(f.ctx, f.termID, []string{"2026-06", "2026-07"}); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE submission_periods SET due_date=CURRENT_DATE-1,is_closed=false WHERE term_id=$1`, f.termID)
	closed, err := f.svc.MonthlyPayoutClosed(f.ctx, f.termID, "2026-06")
	if err != nil || !closed {
		t.Fatalf("yesterday's deadline must be closed: %v %v", closed, err)
	}
}

func TestMonthlyGraduateSpecialPayableWithoutSubmission(t *testing.T) {
	f := newTCFixture(t)
	f.addPeriod("2569-06")
	f.addPeriod("2569-07")
	course, _, special := f.insertCourse(tcCourseOpts{Code: "CPGRADMONTH", Curriculum: "CY", LectureHrs: 100, Level: "graduate"})
	ta := f.newTA("เหมาจ่าย", "master")
	f.assignTAOn(ta, course, special, "master", nil)
	ids, err := f.svc.PayoutMonthCourses(f.ctx, f.termID, "2026-06")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range ids {
		found = found || id == course
	}
	if !found {
		t.Fatal("lump holder without logs excluded")
	}
	m, err := f.svc.gradLumpByMonth(f.ctx, course, ta, 1000, true)
	if err != nil {
		t.Fatal(err)
	}
	if m["2026-06"] <= 0 || m["2026-07"] <= 0 || sumMonths(m, nil) != 1000 {
		t.Fatal(m)
	}
}

func TestMonthlyFreezeRollsBackEveryCourse(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	payoutReady(f)
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='approved' WHERE assignment_id=$1`, f.AssignmentID)
	f.addSubmissionPeriod(day(10)[5:7], day(28), "staff_reviewed", false)
	f.addAppointmentOrder()
	exp := exportSvcFor(f)
	fp, err := f.Periods.CourseWorklogFingerprint(f.ctx, f.CourseID, nil)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if cells, e := f.Periods.MarkCourseExportedInTx(f.ctx, tx, f.StaffID, f.CourseID, nil, fp, nil, "staff"); e != nil || len(cells) == 0 {
		t.Fatalf("first course should lock before simulated failure: %v %v", cells, e)
	}
	// No matter how far a month pack gets, a later failure must discard its
	// course archive rows and graduate ledger in the same transaction.
	batchSvc := &ExportBatchService{pool: f.Pool, aud: exp.aud}
	if _, err = batchSvc.RecordTx(f.ctx, tx, f.StaffID, ExportBatch{TeachingCourseID: f.CourseID, FileName: "month.zip", FilePath: "test", Months: []string{"2026-06"}}); err != nil {
		t.Fatal(err)
	}
	_, err = f.Periods.MarkCourseExportedInTx(f.ctx, tx, f.StaffID, f.CourseID, nil, fp+"changed", nil, "staff")
	if err == nil || !strings.Contains(err.Error(), "เปลี่ยน") {
		t.Fatalf("expected stale-build failure: %v", err)
	}
	tx.Rollback(f.ctx)
	var count int
	if err = f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM export_batches WHERE teaching_course_id=$1`, f.CourseID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("partial month committed")
	}
	var status string
	if err = f.Pool.QueryRow(f.ctx, `SELECT status FROM submission_period_status WHERE teaching_course_id=$1`, f.CourseID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "staff_reviewed" {
		t.Fatalf("first course lock escaped rollback: %s", status)
	}

}

func TestProgressMonthlyScopeValidation(t *testing.T) {
	for _, n := range []int{1, 2, 202606, 202701} {
		if !ProgressScopeValid(n) {
			t.Fatal(n)
		}
	}
	for _, n := range []int{0, 3, 202600, 202613, -1} {
		if ProgressScopeValid(n) {
			t.Fatal(n)
		}
	}
}

func TestMonthlyStaffCorrectionAfterDeadlineStillLocksExports(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	w := f.entry(day(10), "09:00", "11:00", 2)
	w.ID = f.mustUpsert(w)
	f.closePeriodFor(t, day(10)[5:7])
	note := "แก้ไขรายการที่ส่งทันเวลา"
	w.Note = &note
	if _, err := f.Svc.StaffUpsert(f.ctx, f.StaffID, true, w, &EditStepUp{Reason: "ตรวจสอบและแก้ไขตามหลักฐาน", verified: true}); err != nil {
		t.Fatal(err)
	}
	rows, err := f.Svc.StaffListByCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 || rows[0].Locked || !rows[0].PeriodClosed {
		t.Fatalf("staff edit state=%+v", rows)
	}
	f.exec(`INSERT INTO submission_period_status(id,submission_period_id,ta_id,teaching_course_id,status,exported_at) SELECT gen_random_uuid(),id,$2,$3,'exported',now() FROM submission_periods WHERE term_id=$1 AND right(year_month,2)=$4`, f.TermID, f.TAID, f.CourseID, day(10)[5:7])
	if _, err = f.Svc.StaffUpsert(f.ctx, f.StaffID, true, w, &EditStepUp{verified: true}); err == nil {
		t.Fatal("exported month was editable")
	}
}
