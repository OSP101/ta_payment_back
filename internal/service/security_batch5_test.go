package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/timeutil"
)

// Batch 5, finding 20: a course whose term has no dates must not accept
// worklog entries for any date at all.

func clearCourseDates(f *fixture) {
	f.exec(`UPDATE academic_terms SET starts_on = NULL, ends_on = NULL WHERE id = $1`, f.TermID)
	f.exec(`UPDATE teaching_courses SET starts_on = NULL, ends_on = NULL WHERE id = $1`, f.CourseID)
}

func TestUpsert_RefusesWhenCourseAndTermHaveNoDates(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	clearCourseDates(f)

	// The audit's reproduction: a date far outside any real term.
	_, err := f.upsert(f.entry("2099-12-31", "09:00", "10:00", 1))
	if !errors.Is(err, errCourseDatesUnset) {
		t.Fatalf("an entry on a course with no dates must be refused, got %v", err)
	}
	if n := f.countLogs(); n != 0 {
		t.Fatalf("nothing may be persisted, found %d rows", n)
	}
}

func TestUpsert_StillBoundByTermDatesWhenCourseHasNone(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	// Course falls back to the term's range (the shared CourseStartSQL rule).
	f.exec(`UPDATE teaching_courses SET starts_on = NULL, ends_on = NULL WHERE id = $1`, f.CourseID)
	if _, err := f.upsert(f.entry("2099-12-31", "09:00", "10:00", 1)); userErrStatus(err) != 400 {
		t.Fatalf("a date outside the term must still be refused, got %v", err)
	}
	f.mustUpsert(f.entry(day(10), "09:00", "10:00", 1))
}

// The planner is used while a term is being set up, so it degrades instead of
// failing — and must not walk an open-ended calendar day by day.
func TestPlanFacts_NoDatesDegradesQuickly(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	clearCourseDates(f)
	svc := &ExportService{pool: f.Pool, aud: audit.New(f.Pool)}

	start := time.Now()
	pf, err := svc.PlanFacts(f.ctx, f.CourseID)
	if err != nil {
		t.Fatalf("planner must still answer without dates: %v", err)
	}
	if pf.StartsOn != "" || pf.EndsOn != "" {
		t.Fatalf("dates must read as unset, got %q..%q", pf.StartsOn, pf.EndsOn)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("planner took %v — it is walking an unbounded calendar", d)
	}
}

// Batch 5, finding 10: a pay rate saved with a future date waits for it.

func payRateInput(from string, ugRegular float64) PayRate {
	return PayRate{EffectiveFrom: from, UndergradRegular: ugRegular, UndergradSpecial: 50,
		GraduateRegular: 50, GraduateSpecialLumpsum: 4000}
}

func dateOffset(days int) string {
	return time.Now().In(bangkokForTest()).AddDate(0, 0, days).Format("2006-01-02")
}

func bangkokForTest() *time.Location {
	loc, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		return time.FixedZone("ICT", 7*3600)
	}
	return loc
}

func TestPayRate_FutureVersionWaitsForItsDate(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	cs := &CourseService{pool: f.Pool, aud: audit.New(f.Pool)}
	before, err := cs.LatestPayRate(f.ctx)
	if err != nil {
		t.Fatal(err)
	}

	future, err := cs.UpsertPayRate(f.ctx, f.StaffID, payRateInput(dateOffset(365), 999))
	if err != nil {
		t.Fatalf("staging next year's rate must be allowed: %v", err)
	}
	now, err := cs.LatestPayRate(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if now.ID != before.ID || now.UndergradRegular == 999 {
		t.Fatalf("a future-dated rate is already in force: got %+v", now)
	}
	sched, err := cs.ScheduledPayRates(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sched) != 1 || sched[0].ID != future.ID {
		t.Fatalf("the future version must be listed as scheduled, got %+v", sched)
	}

	// Every other reader goes through the same definition — spot-check the
	// daily money cap, which is read with its own query.
	var dailyCap float64
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT daily_pay_cap_baht FROM `+payRatesInForce).Scan(&dailyCap); err != nil {
		t.Fatal(err)
	}
	if dailyCap != before.DailyPayCapBaht {
		t.Fatalf("daily cap came from the future row: %v", dailyCap)
	}

	// Once its date arrives it takes over.
	f.exec(`UPDATE pay_rates SET effective_from = CURRENT_DATE WHERE id = $1`, future.ID)
	now, err = cs.LatestPayRate(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if now.ID != future.ID {
		t.Fatalf("the version dated today must be in force, got %s", now.EffectiveFrom)
	}
}

func TestPayRate_RefusesDatesThatWouldNotDoWhatTheDialogSays(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	cs := &CourseService{pool: f.Pool, aud: audit.New(f.Pool)}

	// Fixture's in-force rate starts 2020-01-01: an earlier version would be
	// saved and never used.
	if _, err := cs.UpsertPayRate(f.ctx, f.StaffID, payRateInput("2019-06-01", 45)); userErrStatus(err) != 400 {
		t.Fatalf("a version older than the one in force must be refused, got %v", err)
	}
	if _, err := cs.UpsertPayRate(f.ctx, f.StaffID, payRateInput("not-a-date", 45)); userErrStatus(err) != 400 {
		t.Fatalf("a malformed date must be a 400, got %v", err)
	}

	// With nothing in force, a first version dated in the future would leave
	// the system with no rate at all.
	f.exec(`DELETE FROM pay_rates`)
	if _, err := cs.UpsertPayRate(f.ctx, f.StaffID, payRateInput(dateOffset(30), 45)); userErrStatus(err) != 400 {
		t.Fatalf("a first-ever version dated in the future must be refused, got %v", err)
	}
	if _, err := cs.UpsertPayRate(f.ctx, f.StaffID, payRateInput(dateOffset(0), 45)); err != nil {
		t.Fatalf("a first version dated today must be accepted: %v", err)
	}
}

func TestPayRate_OnlyScheduledVersionsCanBeWithdrawn(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	cs := &CourseService{pool: f.Pool, aud: audit.New(f.Pool)}
	inForce, err := cs.LatestPayRate(f.ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := cs.DeleteScheduledPayRate(f.ctx, f.StaffID, inForce.ID); userErrStatus(err) != 409 {
		t.Fatalf("a version already in force must never be deleted, got %v", err)
	}
	if err := cs.DeleteScheduledPayRate(f.ctx, f.StaffID, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id must be not-found, got %v", err)
	}

	future, err := cs.UpsertPayRate(f.ctx, f.StaffID, payRateInput(dateOffset(90), 999))
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.DeleteScheduledPayRate(f.ctx, f.StaffID, future.ID); err != nil {
		t.Fatalf("a mistyped future version must be withdrawable: %v", err)
	}
	var n int
	if err := f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM pay_rates WHERE id = $1`, future.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("the scheduled version is still there")
	}
	var before *string
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT before::text FROM audit_logs
		 WHERE action = 'pay_rate.delete_scheduled' AND entity_id = $1`, future.ID.String()).Scan(&before); err != nil {
		t.Fatalf("withdrawal must be audited: %v", err)
	}
	if before == nil {
		t.Fatal("the audit entry must keep the withdrawn version's contents")
	}
}

// Batch 5, finding 15 (export half): a class restored AFTER approval is caught
// before the hours become a claim document.
func TestExportBlockers_FlagApprovedHoursThatNowClashWithOwnClass(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.addAppointmentOrder()
	periodID := f.addSubmissionPeriod(currentMonthMM(), openDueDate(), "", false)
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, "", false); err != nil {
		t.Fatal(err)
	}
	exp := &ExportService{pool: f.Pool, aud: audit.New(f.Pool)}
	hasClash := func() bool {
		t.Helper()
		bl, err := exp.CourseExportBlockers(f.ctx, f.CourseID, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range bl {
			if b.Kind == "class_clash" {
				return true
			}
		}
		return false
	}
	if hasClash() {
		t.Fatal("no clash before the timetable changes")
	}

	// The TA puts a class back over the approved hours.
	d, err := timeutil.ParseDate(day(10))
	if err != nil {
		t.Fatal(err)
	}
	ws := &WorkloadService{pool: f.Pool, aud: audit.New(f.Pool)}
	if err := ws.ReplaceClasses(f.ctx, f.TAID, f.TermID, []ClassBlock{
		{CourseCode: "ZZ000", Kind: "lecture", DayOfWeek: 0, StartTime: "07:00", EndTime: "08:00"},
		{CourseCode: "CLASH1", Kind: "lecture", DayOfWeek: int(d.Weekday()), StartTime: "10:00", EndTime: "12:00"},
	}); err != nil {
		t.Fatal(err)
	}
	if !hasClash() {
		t.Fatal("approved hours overlapping the TA's current class must block the export")
	}

	// Once the month is exported it is frozen and no longer re-judged.
	f.exec(`DELETE FROM submission_period_status WHERE submission_period_id = $1`, periodID)
	f.exec(`INSERT INTO submission_period_status (id, submission_period_id, ta_id, teaching_course_id, status)
	        VALUES (gen_random_uuid(), $1, $2, $3, 'exported')`, periodID, f.TAID, f.CourseID)
	if hasClash() {
		t.Fatal("an exported month must not be re-judged")
	}
}

// Batch 5, finding 2: an exported month's grad-special lump share stays what
// the claim document said, whatever later months do.

// gradLumpFixture sets up a master's special-track holder with approved hours
// in this month (10h), and returns the lump the rates give and both month keys.
func gradLumpFixture(t *testing.T) (*fixture, *ExportService, float64, string, string) {
	t.Helper()
	f := newFixture(t, fixtureOpts{Level: "master", Track: "special"})
	id := f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	// The later month's row exists from the start as a draft two months on —
	// invisible to the split until addLaterHours approves it.
	later := f.mustUpsert(f.entry(day(11), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status = 'approved', hours = 10 WHERE id = $1`, id)
	f.exec(`UPDATE work_logs SET hours = 30, work_date = work_date + INTERVAL '2 months' WHERE id = $1`, later)
	exp := &ExportService{pool: f.Pool, aud: audit.New(f.Pool)}
	pr, err := (&CourseService{pool: f.Pool}).LatestPayRate(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	lump := pr.GraduateSpecialLumpsum
	if pr.GradSpecialTermCap > 0 && lump > pr.GradSpecialTermCap {
		lump = pr.GradSpecialTermCap
	}
	m1 := monthStart().Format("2006-01")
	m3 := monthStart().AddDate(0, 2, 0).Format("2006-01")
	return f, exp, lump, m1, m3
}

// addLaterHours approves 30 more special-track hours two months later.
func addLaterHours(t *testing.T, f *fixture) {
	t.Helper()
	f.exec(`UPDATE work_logs SET status = 'approved' WHERE assignment_id = $1 AND status = 'draft'`, f.AssignmentID)
}

func sumLump(m map[string]float64) float64 {
	var t float64
	for _, v := range m {
		t += v
	}
	return t
}

func TestGradLump_ExportedMonthDoesNotDriftWhenLaterMonthsGainHours(t *testing.T) {
	f, exp, lump, m1, m3 := gradLumpFixture(t)

	first, err := exp.gradLumpByMonth(f.ctx, f.CourseID, f.TAID, lump, true)
	if err != nil {
		t.Fatal(err)
	}
	if first[m1] != lump {
		t.Fatalf("with hours only in %s the whole lump lands there: %v", m1, first)
	}
	if err := exp.FreezeGradLumps(f.ctx, f.StaffID, f.CourseID, []string{m1}); err != nil {
		t.Fatal(err)
	}

	addLaterHours(t, f)

	after, err := exp.gradLumpByMonth(f.ctx, f.CourseID, f.TAID, lump, true)
	if err != nil {
		t.Fatal(err)
	}
	// The audit's reproduction: this used to become lump×10/40.
	if after[m1] != first[m1] {
		t.Fatalf("exported month %s moved from %.2f to %.2f", m1, first[m1], after[m1])
	}
	if got := sumLump(after); got != lump {
		t.Fatalf("slices must still sum to the lump: %.2f vs %.2f (%v)", got, lump, after)
	}
	_ = m3

	// Re-exporting the same month freezes nothing new.
	if err := exp.FreezeGradLumps(f.ctx, f.StaffID, f.CourseID, []string{m1}); err != nil {
		t.Fatal(err)
	}
	again, err := exp.gradLumpByMonth(f.ctx, f.CourseID, f.TAID, lump, true)
	if err != nil {
		t.Fatal(err)
	}
	if again[m1] != first[m1] {
		t.Fatalf("a re-export must print the first figure, got %.2f", again[m1])
	}
}

func TestGradLump_OpenMonthsShareOnlyWhatIsLeft(t *testing.T) {
	f, exp, lump, m1, m3 := gradLumpFixture(t)
	addLaterHours(t, f)
	// Both months have hours now: 10h and 30h → a quarter and three quarters.
	before, err := exp.gradLumpByMonth(f.ctx, f.CourseID, f.TAID, lump, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := exp.FreezeGradLumps(f.ctx, f.StaffID, f.CourseID, []string{m1}); err != nil {
		t.Fatal(err)
	}
	// More hours in the open month change nothing for the frozen one, and the
	// open month gets exactly the rest.
	f.exec(`UPDATE work_logs SET hours = hours + 50 WHERE to_char(work_date,'YYYY-MM') = $1`, m3)
	after, err := exp.gradLumpByMonth(f.ctx, f.CourseID, f.TAID, lump, true)
	if err != nil {
		t.Fatal(err)
	}
	if after[m1] != before[m1] {
		t.Fatalf("frozen month moved: %.2f → %.2f", before[m1], after[m1])
	}
	if after[m3] != lump-before[m1] {
		t.Fatalf("open month must get the rest of the lump: %.2f, want %.2f", after[m3], lump-before[m1])
	}
}

// Decision 23/09/2026: the lump is locked with the first freeze, so a rate
// change mid-term cannot leave frozen months totalling more than the lump.
func TestGradLump_RateChangeMidTermKeepsTheFrozenBasis(t *testing.T) {
	f, exp, lump, m1, _ := gradLumpFixture(t)
	addLaterHours(t, f)
	if err := exp.FreezeGradLumps(f.ctx, f.StaffID, f.CourseID, []string{m1}); err != nil {
		t.Fatal(err)
	}
	frozenM1, err := exp.gradLumpByMonth(f.ctx, f.CourseID, f.TAID, lump, true)
	if err != nil {
		t.Fatal(err)
	}

	smaller := lump / 4
	after, err := exp.gradLumpByMonth(f.ctx, f.CourseID, f.TAID, smaller, true)
	if err != nil {
		t.Fatal(err)
	}
	if after[m1] != frozenM1[m1] {
		t.Fatalf("frozen month moved under a rate change: %.2f → %.2f", frozenM1[m1], after[m1])
	}
	for ym, v := range after {
		if v < 0 {
			t.Fatalf("month %s went negative: %.2f", ym, v)
		}
	}
	if got := sumLump(after); got != lump {
		t.Fatalf("the term lump is the frozen basis %.2f, got %.2f", lump, got)
	}
}

// The audit before-image of a new pay rate is the rate IN FORCE, never a
// version saved ahead of time that has not started yet.
func TestPayRate_AuditBeforeIsTheRateInForce(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	cs := &CourseService{pool: f.Pool, aud: audit.New(f.Pool)}
	inForce, err := cs.LatestPayRate(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	scheduled, err := cs.UpsertPayRate(f.ctx, f.StaffID, payRateInput(dateOffset(90), 999))
	if err != nil {
		t.Fatal(err)
	}
	today, err := cs.UpsertPayRate(f.ctx, f.StaffID, payRateInput(dateOffset(0), 45))
	if err != nil {
		t.Fatal(err)
	}
	readAudit := func(id uuid.UUID) (beforeID, note string) {
		t.Helper()
		if err := f.Pool.QueryRow(f.ctx, `
			SELECT COALESCE(before->>'id',''), COALESCE(note,'') FROM audit_logs
			WHERE action = 'pay_rate.create' AND entity_id = $1`, id.String()).Scan(&beforeID, &note); err != nil {
			t.Fatal(err)
		}
		return
	}
	if b, note := readAudit(today.ID); b != inForce.ID.String() {
		t.Fatalf("before = %q, want the rate in force %q (not scheduled %q)", b, inForce.ID, scheduled.ID)
	} else if strings.Contains(note, "ตั้งล่วงหน้า") {
		t.Errorf("a version starting today is not scheduled, note = %q", note)
	}
	if _, note := readAudit(scheduled.ID); !strings.Contains(note, "ตั้งล่วงหน้า") {
		t.Errorf("a future version must be noted as scheduled, note = %q", note)
	}
}
