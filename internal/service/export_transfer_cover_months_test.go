package service

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
)

/* -------------------------------------------------------------------------- */
/* Fiscal-year month slicing (10/08/2026)                                     */
/*                                                                            */
/* งบแผ่นดิน closes 30 กันยายน but ภาคต้น teaches มิ.ย.–ต.ค., so one term is    */
/* claimed on two documents against two different appropriations. The budget   */
/* itself stays ONE pool per course for the whole term — a slice is only a     */
/* view onto part of the already-settled result — which is what makes the      */
/* invariant below both true and worth protecting.                            */
/* -------------------------------------------------------------------------- */

// addPeriod creates the submission_periods row for a Buddhist year_month
// ("2569-09"), which is what TermMonths reads to learn a term's months.
func (f *tcFixture) addPeriod(ym string) uuid.UUID {
	f.t.Helper()
	var id uuid.UUID
	err := f.pool.QueryRow(f.ctx, `SELECT id FROM submission_periods WHERE term_id=$1 AND year_month=$2`,
		f.termID, ym).Scan(&id)
	if err == nil {
		return id
	}
	id = uuid.New()
	f.exec(`INSERT INTO submission_periods (id, term_id, year_month, due_date, label, starts_on, is_closed)
	        VALUES ($1, $2, $3, '2026-12-31'::date, $4, '2026-06-01'::date, FALSE)`,
		id, f.termID, ym, "รอบ "+ym)
	return id
}

// assignTAOn logs one approved 1-hour คาบ per explicit date, so a test can put
// work either side of the 30 กันยายน boundary — the fixture's own day() helper
// can only reach the current calendar month.
func (f *tcFixture) assignTAOn(taID, courseID, sectionID uuid.UUID, level string, dates []string) uuid.UUID {
	f.t.Helper()
	reqID := uuid.New()
	f.exec(`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at, decided_at)
	        VALUES ($1,$2,$3,'both'::reimburse_scope,'approved'::ta_request_status, NOW(), NOW())`,
		reqID, courseID, f.lectID)
	assignID := uuid.New()
	f.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
	        VALUES ($1,$2,$3,$4,$5::study_level)`, assignID, reqID, sectionID, taID, level)
	for i, d := range dates {
		start := fmt.Sprintf("%02d:00", 8+i%10)
		end := fmt.Sprintf("%02d:00", 9+i%10)
		f.exec(`INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status)
		        VALUES (gen_random_uuid(), $1, $2::date, $3, $4, 1, 'lecture', 'approved')`,
			assignID, d, start, end)
	}
	return assignID
}

// financeSendPeriod marks one specific month finance_sent, so a test can have
// กันยายน ready while ตุลาคม is still being taught.
func (f *tcFixture) financeSendPeriod(courseID uuid.UUID, ym string) {
	f.t.Helper()
	spID := f.addPeriod(ym)
	rows, err := f.pool.Query(f.ctx, `
		SELECT DISTINCT a.ta_id
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		JOIN sections sec ON sec.id = a.section_id
		WHERE sec.teaching_course_id = $1`, courseID)
	if err != nil {
		f.t.Fatalf("query tas: %v", err)
	}
	var taIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			f.t.Fatal(err)
		}
		taIDs = append(taIDs, id)
	}
	rows.Close()
	for _, taID := range taIDs {
		f.exec(`INSERT INTO submission_period_status (id, submission_period_id, ta_id, teaching_course_id, status)
		        VALUES (gen_random_uuid(), $1, $2, $3, 'finance_sent')
		        ON CONFLICT (submission_period_id, ta_id, teaching_course_id)
		        DO UPDATE SET status = 'finance_sent'`, spID, taID, courseID)
	}
}

func sheetsTotal(sheets []transferCoverSheet) float64 {
	var t float64
	for _, sh := range sheets {
		t += sh.TotalBaht
	}
	return round2(t)
}

// THE invariant. Slicing a term into fiscal documents must be a partition of
// the money, never a recomputation: every คาบ lands in exactly one slice, so
// the slices sum to the undivided total. If this ever fails, either somebody
// is being paid twice across two documents or a month has silently fallen
// between them — the two failures this whole feature exists to prevent.
func TestTransferCoverMonthSlices_SumToTheWholeTerm(t *testing.T) {
	f := newTCFixture(t)
	for _, ym := range []string{"2569-06", "2569-07", "2569-08", "2569-09", "2569-10"} {
		f.addPeriod(ym)
	}
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP301", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("สมหญิง ตัดงบ", "undergrad")
	// Work either side of 30 กันยายน: three คาบ in the closing budget year,
	// two in the new one.
	f.assignTAOn(ta, courseID, regSec, "undergrad",
		[]string{"2026-08-10", "2026-09-14", "2026-09-21", "2026-10-05", "2026-10-12"})

	whole, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID, nil)
	if err != nil {
		t.Fatalf("whole term: %v", err)
	}
	before, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID,
		[]string{"2026-06", "2026-07", "2026-08", "2026-09"})
	if err != nil {
		t.Fatalf("มิ.ย.–ก.ย.: %v", err)
	}
	after, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID, []string{"2026-10"})
	if err != nil {
		t.Fatalf("ต.ค.: %v", err)
	}

	total := sheetsTotal(whole)
	if total <= 0 {
		t.Fatalf("fixture bug: whole-term total is %.2f, expected real money", total)
	}
	sum := round2(sheetsTotal(before) + sheetsTotal(after))
	if sum != total {
		t.Errorf("slices sum to %.2f but the whole term is %.2f — money is being duplicated or dropped (มิ.ย.–ก.ย.=%.2f, ต.ค.=%.2f)",
			sum, total, sheetsTotal(before), sheetsTotal(after))
	}
	// And each slice really is its own months: 3 คาบ × 50 vs 2 คาบ × 50.
	if got := sheetsTotal(before); got != 150 {
		t.Errorf("มิ.ย.–ก.ย. = %.2f, want 150 (three 1-hour คาบ at 50)", got)
	}
	if got := sheetsTotal(after); got != 100 {
		t.Errorf("ต.ค. = %.2f, want 100 (two 1-hour คาบ at 50)", got)
	}
}

// The gate must judge only the months being issued. Otherwise the มิ.ย.–ก.ย.
// document — which has to be filed BEFORE 30 กันยายน — cannot be produced until
// October has been taught, reviewed and sent to finance, i.e. only after the
// budget year it belongs to has already closed.
func TestTermExportBlockers_ScopedToSelectedMonths(t *testing.T) {
	f := newTCFixture(t)
	for _, ym := range []string{"2569-09", "2569-10"} {
		f.addPeriod(ym)
	}
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP302", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("สมปอง ยังไม่ส่ง", "undergrad")
	f.assignTAOn(ta, courseID, regSec, "undergrad", []string{"2026-09-14", "2026-10-05"})
	// กันยายน is settled with finance; ตุลาคม is still in progress.
	f.financeSendPeriod(courseID, "2569-09")

	scoped, err := f.svc.TermExportBlockers(f.ctx, f.termID, []string{"2026-09"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 0 {
		t.Errorf("กันยายน is finance_sent, so its document must be issuable; got blockers: %+v", scoped)
	}

	wholeTerm, err := f.svc.TermExportBlockers(f.ctx, f.termID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(wholeTerm) == 0 {
		t.Error("ตุลาคม has not reached finance_sent, so the undivided document must still be blocked")
	}
}

// The graduate-special lump is a flat TERM figure with no คาบ behind it, so a
// month filter cannot select it. Staff chose to apportion it by the share of
// months a document covers (10/08/2026) — which keeps the slice-sum invariant
// and stops a TA's ตุลาคม document reading 0.00 for work they did do.
func TestTransferCoverMonthSlices_GradLumpProRated(t *testing.T) {
	f := newTCFixture(t)
	for _, ym := range []string{"2569-06", "2569-07", "2569-08", "2569-09", "2569-10"} {
		f.addPeriod(ym)
	}
	courseID, _, specSec := f.insertCourse(tcCourseOpts{Code: "CP303", Curriculum: "CY", LectureHrs: 100})
	grad := f.newTA("บัณฑิต เหมาจ่าย", "master")
	f.assignTAOn(grad, courseID, specSec, "master", []string{"2026-09-14"})

	whole, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID, nil)
	if err != nil {
		t.Fatal(err)
	}
	lump := sheetsTotal(whole)
	// graduate_special_lumpsum IS the whole-term-per-course figure (2026
	// meeting correction) — flat 1000, under the 12000 cap, NOT × term_months.
	// The hours themselves price at 0 for grad-special.
	if lump != 1000 {
		t.Fatalf("fixture bug: whole-term grad lump = %.2f, want 1000", lump)
	}

	// This course has no regular-track class schedule, so the apportionment
	// falls back to an even per-calendar-month share: five months in the
	// term, so a four-month slice carries 4/5 of the lump.
	before, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID,
		[]string{"2026-06", "2026-07", "2026-08", "2026-09"})
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID, []string{"2026-10"})
	if err != nil {
		t.Fatal(err)
	}
	if got := sheetsTotal(before); got != 800 {
		t.Errorf("มิ.ย.–ก.ย. lump = %.2f, want 800 (4/5 of 1000)", got)
	}
	if got := sheetsTotal(after); got != 200 {
		t.Errorf("ต.ค. lump = %.2f, want 200 (1/5 of 1000)", got)
	}
	if sum := round2(sheetsTotal(before) + sheetsTotal(after)); sum != lump {
		t.Errorf("pro-rated lump sums to %.2f, want the undivided %.2f", sum, lump)
	}
}

// Same invariant as above, but this time the course DOES have a regular-track
// class schedule, so the apportionment must follow the real weighted share
// (2026 meeting correction) instead of falling back to an even per-month
// split — confirming the fallback and the real path never silently merge.
func TestTransferCoverMonthSlices_GradLumpFollowsRealScheduleWhenAvailable(t *testing.T) {
	f := newTCFixture(t)
	for _, ym := range []string{"2569-06", "2569-07", "2569-08", "2569-09", "2569-10"} {
		f.addPeriod(ym)
	}
	courseID, regSec, specSec := f.insertCourse(tcCourseOpts{Code: "CP305", Curriculum: "CY", LectureHrs: 100})
	// A weekly Monday lecture across the whole term. The exact per-month
	// Monday count isn't hand-verified here (that's grad_special_schedule_test.go's
	// job) — this test only needs the resulting split to NOT match the
	// uniform-fallback figures (800/200), proving the real weighting is used
	// once a schedule exists rather than silently falling back.
	f.exec(`INSERT INTO section_schedules (id, section_id, kind, day_of_week, start_time, end_time)
	        VALUES (gen_random_uuid(), $1, 'lecture', 1, '09:00', '11:00')`, regSec)
	grad := f.newTA("บัณฑิต ตารางจริง", "master")
	f.assignTAOn(grad, courseID, specSec, "master", []string{"2026-09-14"})

	before, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID,
		[]string{"2026-06", "2026-07", "2026-08", "2026-09"})
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := f.svc.buildTransferCoverSheets(f.ctx, f.termID, []string{"2026-10"})
	if err != nil {
		t.Fatal(err)
	}
	// The uniform fallback would give exactly 800/200 (4/5 vs 1/5 of 1000);
	// the real schedule-weighted share must differ from that, proving it is
	// not silently falling back to the uniform split now that a schedule exists.
	if got := sheetsTotal(before); got == 800 {
		t.Errorf("มิ.ย.–ก.ย. lump = %.2f, matches the uniform fallback exactly — schedule weighting is not being used", got)
	}
	if sum := round2(sheetsTotal(before) + sheetsTotal(after)); sum != 1000 {
		t.Errorf("weighted lump sums to %.2f, want the undivided 1000", sum)
	}
}

// Coverage is what stops staff double-issuing ตุลาคม or forgetting it. It must
// report the term's months, which are already on paper, and where the budget
// year cuts — read from the ledger, not from anyone's memory.
func TestTransferCoverCoverage_TracksIssuedMonths(t *testing.T) {
	f := newTCFixture(t)
	for _, ym := range []string{"2569-06", "2569-07", "2569-08", "2569-09", "2569-10"} {
		f.addPeriod(ym)
	}
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP304", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("สมศรี ออกแล้ว", "undergrad")
	f.assignTAOn(ta, courseID, regSec, "undergrad", []string{"2026-09-14", "2026-10-05"})
	f.storeCitizenID(ta, "1234567890121")
	f.financeSendPeriod(courseID, "2569-09")
	f.financeSendPeriod(courseID, "2569-10")

	cov, err := f.svc.TransferCoverCoverage(f.ctx, f.termID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cov.Months) != 5 {
		t.Fatalf("coverage months = %d, want the term's 5", len(cov.Months))
	}
	if !cov.Split.Crosses || len(cov.Split.After) != 1 || cov.Split.After[0] != "2026-10" {
		t.Errorf("มิ.ย.–ต.ค. must be flagged as crossing 30 ก.ย. with ต.ค. on the far side; got %+v", cov.Split)
	}
	for _, m := range cov.Months {
		if m.Issued {
			t.Errorf("%s reported as issued before anything was generated", m.YearMonth)
		}
	}

	// Issue มิ.ย.–ก.ย. only.
	if _, _, err := f.svc.BuildTransferCoverWorkbook(f.ctx, f.actor(), f.termID,
		[]string{"2026-06", "2026-07", "2026-08", "2026-09"}); err != nil {
		t.Fatalf("issue มิ.ย.–ก.ย.: %v", err)
	}
	cov, err = f.svc.TransferCoverCoverage(f.ctx, f.termID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range cov.Months {
		want := m.YearMonth != "2026-10"
		if m.Issued != want {
			t.Errorf("%s issued = %v, want %v after issuing only มิ.ย.–ก.ย.", m.YearMonth, m.Issued, want)
		}
	}
}
