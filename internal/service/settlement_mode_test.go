package service

import (
	"strings"
	"testing"
)

// Who may move a course between the two budget-cutting rules, and when.
//
// The switch decides which months of the term reach a TA's bank account, so the
// interesting cases are not the happy path: they are the lecturer of a DIFFERENT
// course reaching in, and the course whose money has already left the building.

// lockCourseMonth marks one of the course's submission periods as sent to
// finance — the state that freezes the settlement rule for everybody.
func lockCourseMonth(f *fixture) string {
	return courseMonthAt(f, "finance_sent")
}

// courseMonthAt puts one of the course's months at the given review status.
func courseMonthAt(f *fixture, status string) string {
	id := f.addSubmissionPeriod("06", "2026-07-31", status, false)
	var label string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT label FROM submission_periods WHERE id = $1`, id).Scan(&label); err != nil {
		f.t.Fatal(err)
	}
	return label
}

func courseMode(f *fixture) string {
	var m string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT settlement_mode FROM teaching_courses WHERE id = $1`, f.CourseID).Scan(&m); err != nil {
		f.t.Fatal(err)
	}
	return m
}

// Every course that existed before this feature must behave exactly as it did.
func TestSettlementMode_DefaultsToTheOriginalRule(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	if got := courseMode(f); got != string(SettleChronological) {
		t.Errorf("a new course settles as %q, want %q — changing the default would "+
			"silently re-cut every course in the system", got, SettleChronological)
	}
}

func TestSettlementMode_LecturerOfTheCourseCanTurnItOnAndOff(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := exportSvcWithNotify(f)

	if err := svc.SetSettlementMode(f.ctx, f.LecturerID, f.CourseID, SettleSpread, false); err != nil {
		t.Fatalf("the course's own lecturer must be able to turn it on: %v", err)
	}
	if got := courseMode(f); got != string(SettleSpread) {
		t.Fatalf("mode is %q after turning it on", got)
	}
	// Both directions: a lecturer who changes their mind must not need a support
	// request to get back.
	if err := svc.SetSettlementMode(f.ctx, f.LecturerID, f.CourseID, SettleChronological, false); err != nil {
		t.Fatalf("the same lecturer must be able to turn it off again: %v", err)
	}
	if got := courseMode(f); got != string(SettleChronological) {
		t.Errorf("mode is %q after turning it off", got)
	}
}

// A lecturer teaches their own courses. Nothing stops them holding an account
// and knowing another course's id.
func TestSettlementMode_AnotherCoursesLecturerIsRefused(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := exportSvcWithNotify(f)
	outsider := f.insertUser("lecturer", "outsider")

	err := svc.SetSettlementMode(f.ctx, outsider, f.CourseID, SettleSpread, false)
	if err == nil {
		t.Fatal("a lecturer who does not teach this course changed how its TAs are paid")
	}
	if got := courseMode(f); got != string(SettleChronological) {
		t.Errorf("the refusal still wrote: mode is %q", got)
	}
}

func TestSettlementMode_StaffMayChangeItForALecturerWhoAsks(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := exportSvcWithNotify(f)

	if err := svc.SetSettlementMode(f.ctx, f.StaffID, f.CourseID, SettleSpread, true); err != nil {
		t.Fatalf("staff must be able to act on a lecturer's request: %v", err)
	}
	if got := courseMode(f); got != string(SettleSpread) {
		t.Errorf("mode is %q", got)
	}
}

// THE ONE THAT PROTECTS THE MONEY. Once a month is with finance, re-cutting the
// term would produce figures that do not reconcile with the signed document.
func TestSettlementMode_RefusedOnceAnyMonthHasGoneToFinance(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := exportSvcWithNotify(f)
	label := lockCourseMonth(f)

	err := svc.SetSettlementMode(f.ctx, f.LecturerID, f.CourseID, SettleSpread, false)
	if err == nil {
		t.Fatal("the rule was changed after a month had already been paid out")
	}
	if !strings.Contains(err.Error(), label) {
		t.Errorf("the refusal should name the month that blocks it (%s), got: %v", label, err)
	}
	if got := courseMode(f); got != string(SettleChronological) {
		t.Errorf("the refusal still wrote: mode is %q", got)
	}
}

// Staff are not an escape hatch here. The lock is about documents that have left
// the building, and no role makes those documents change.
func TestSettlementMode_FinanceLockBindsStaffToo(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := exportSvcWithNotify(f)
	lockCourseMonth(f)

	if err := svc.SetSettlementMode(f.ctx, f.StaffID, f.CourseID, SettleSpread, true); err == nil {
		t.Error("staff walked past the finance lock")
	}
}

func TestSettlementMode_RejectsAnUnknownRule(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := exportSvcWithNotify(f)

	if err := svc.SetSettlementMode(f.ctx, f.LecturerID, f.CourseID, SettlementMode("prorata"), true); err == nil {
		t.Error("an unrecognised rule was accepted and stored")
	}
}

// The TAs cannot see the switch, but it moves which months they are paid for.
func TestSettlementMode_TellsTheTAsTheirPayMoved(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := exportSvcWithNotify(f)

	if err := svc.SetSettlementMode(f.ctx, f.LecturerID, f.CourseID, SettleSpread, false); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM notifications
		  WHERE user_id = $1 AND channel = 'in_app' AND title LIKE 'วิธีแบ่งงบ%'`,
		f.TAID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the TA got %d notifications about it, want 1", n)
	}
}

// Setting the mode it is already on is not a change, so it must not spend a
// notification on the TAs or an audit line on the lecturer.
func TestSettlementMode_SettingTheSameRuleIsSilent(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := exportSvcWithNotify(f)

	if err := svc.SetSettlementMode(f.ctx, f.LecturerID, f.CourseID, SettleChronological, false); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM notifications WHERE channel = 'in_app' AND title LIKE 'วิธีแบ่งงบ%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d notifications sent for a change that did not happen", n)
	}
}

// The screen has to show the lecturer what the other rule would do BEFORE they
// commit to it, and tell them when the choice is no longer available.
func TestSettlementView_CarriesTheAlternativeAndTheLock(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := exportSvcWithNotify(f)

	view, err := svc.SettlementForViewer(f.ctx, f.LecturerID, f.CourseID, false)
	if err != nil {
		t.Fatal(err)
	}
	if view.Mode != SettleChronological {
		t.Errorf("view reports mode %q", view.Mode)
	}
	if view.Alternative == nil {
		t.Error("no alternative forecast — the lecturer would have to flip the " +
			"switch to find out what it does")
	}
	if !view.CanChangeMode {
		t.Error("the switch is reported locked on a course with nothing at finance")
	}

	label := lockCourseMonth(f)
	view, err = svc.SettlementForViewer(f.ctx, f.LecturerID, f.CourseID, false)
	if err != nil {
		t.Fatal(err)
	}
	if view.CanChangeMode {
		t.Error("the switch is still offered after a month went to finance — the " +
			"button would fail on press")
	}
	if len(view.LockedMonths) == 0 || view.LockedMonths[0] != label {
		t.Errorf("locked months %v, want the screen to be able to say why (%s)",
			view.LockedMonths, label)
	}
}

// Staff sign a month off, and from that moment the figures are theirs to answer
// for. A lecturer moving the split underneath a checked document would leave the
// officer defending numbers they never saw.
func TestSettlementMode_LecturerLosesTheSwitchOnceStaffHaveChecked(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := exportSvcWithNotify(f)
	label := courseMonthAt(f, "staff_reviewed")

	err := svc.SetSettlementMode(f.ctx, f.LecturerID, f.CourseID, SettleSpread, false)
	if err == nil {
		t.Fatal("the lecturer changed the split after staff had checked the month")
	}
	if !strings.Contains(err.Error(), label) {
		t.Errorf("the refusal should name the checked month (%s), got: %v", label, err)
	}
	if got := courseMode(f); got != string(SettleChronological) {
		t.Errorf("the refusal still wrote: mode is %q", got)
	}
}

// Staff keep it, and are the ones the lecturer is told to ask.
func TestSettlementMode_StaffKeepTheSwitchAfterReview(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := exportSvcWithNotify(f)
	courseMonthAt(f, "staff_reviewed")

	if err := svc.SetSettlementMode(f.ctx, f.StaffID, f.CourseID, SettleSpread, true); err != nil {
		t.Fatalf("staff must still be able to change it: %v", err)
	}
	if got := courseMode(f); got != string(SettleSpread) {
		t.Errorf("mode is %q", got)
	}
}

// THE ONE THAT PROTECTS THE DOCUMENT. A claim form issued under the old split no
// longer says what the system says, so changing the rule must force it to be
// downloaded again rather than leave a stale file on its way to finance.
func TestSettlementMode_ChangingAfterExportForcesAFreshDownload(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := exportSvcWithNotify(f)
	label := courseMonthAt(f, "exported")
	f.exec(`UPDATE teaching_courses SET exported_at = NOW() WHERE id = $1`, f.CourseID)

	if err := svc.SetSettlementMode(f.ctx, f.StaffID, f.CourseID, SettleSpread, true); err != nil {
		t.Fatalf("staff must be able to change it before the money is sent: %v", err)
	}

	var status string
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT st.status FROM submission_period_status st
		JOIN submission_periods sp ON sp.id = st.submission_period_id
		WHERE st.teaching_course_id = $1 AND sp.label = $2`,
		f.CourseID, label).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "staff_reviewed" {
		t.Errorf("month %s is still %q — the claim document was built on the old "+
			"split and must be re-issued, not left locked and stale", label, status)
	}
	var exportedAt *string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT exported_at::text FROM teaching_courses WHERE id = $1`, f.CourseID).Scan(&exportedAt); err != nil {
		t.Fatal(err)
	}
	if exportedAt != nil {
		t.Error("the course still reads as exported, so the export screen would " +
			"never offer the download again")
	}
}

// Nothing to re-issue, nothing disturbed: a course that was never exported keeps
// its months exactly where they were.
func TestSettlementMode_ChangingWithoutAnExportLeavesMonthsAlone(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := exportSvcWithNotify(f)
	label := courseMonthAt(f, "staff_reviewed")

	if err := svc.SetSettlementMode(f.ctx, f.StaffID, f.CourseID, SettleSpread, true); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT st.status FROM submission_period_status st
		JOIN submission_periods sp ON sp.id = st.submission_period_id
		WHERE st.teaching_course_id = $1 AND sp.label = $2`,
		f.CourseID, label).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "staff_reviewed" {
		t.Errorf("status moved to %q for no reason", status)
	}
}

// The view answers for whoever is asking: the same course is open to staff and
// closed to the lecturer once a month is checked.
func TestSettlementView_LockIsAnsweredPerViewer(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := exportSvcWithNotify(f)
	courseMonthAt(f, "staff_reviewed")

	asLecturer, err := svc.SettlementForViewer(f.ctx, f.LecturerID, f.CourseID, false)
	if err != nil {
		t.Fatal(err)
	}
	if asLecturer.CanChangeMode {
		t.Error("the lecturer is offered a switch that would fail on press")
	}
	if asLecturer.LockReason != "staff_reviewed" {
		t.Errorf("lock reason %q, want staff_reviewed so the screen can say who to ask",
			asLecturer.LockReason)
	}

	asStaff, err := svc.SettlementForViewer(f.ctx, f.StaffID, f.CourseID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !asStaff.CanChangeMode {
		t.Error("staff lost a switch they are supposed to keep")
	}
}
