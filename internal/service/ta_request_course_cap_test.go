package service

import (
	"strings"
	"testing"

	"ta-payment-back/internal/audit"
)

// The per-term course cap is the figure staff set in ตั้งค่า
// (pay_rates.max_courses_per_student). It used to be a hard-coded 3 in the
// auto-decision, the candidate picker and the staff detail view, so changing
// the setting did nothing. These tests pin that every one of them now reads it.

func (rf *requestFixture) setCourseCap(n int) {
	rf.exec(`UPDATE pay_rates SET max_courses_per_student = $1`, n)
}

func capCheck(res *CreateResult) *DecisionCheck {
	for i := range res.Checks {
		if res.Checks[i].Rule == "cap" {
			return &res.Checks[i]
		}
	}
	return nil
}

func TestCourseCap_LoweredSettingRefusesTheSecondCourse(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	rf.addTAClass(5, "13:00", "15:00") // a timetable that clashes with nothing
	rf.secondCourseAssignment(fixtureOpts{})
	rf.setCourseCap(1)

	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Status != "rejected" {
		t.Errorf("status = %q, want rejected — the TA already holds the 1 course the setting allows", res.Status)
	}
	c := capCheck(res)
	if c == nil || c.Passed {
		t.Fatalf("cap check should fail, got %+v", c)
	}
	if !strings.Contains(c.Message, "ครบ 1 วิชา") {
		t.Errorf("message should quote the configured cap, got %q", c.Message)
	}
}

func TestCourseCap_RaisedSettingAllowsAFourthCourse(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	rf.addTAClass(5, "13:00", "15:00")
	for i := 0; i < 3; i++ {
		rf.secondCourseAssignment(fixtureOpts{})
	}
	rf.setCourseCap(4)

	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c := capCheck(res)
	if c == nil || !c.Passed {
		t.Fatalf("3 of 4 courses must pass the cap, got %+v", c)
	}
	if !strings.Contains(c.Message, "ขีดจำกัด 4 วิชา") {
		t.Errorf("message should quote the configured cap, got %q", c.Message)
	}
}

func TestCourseCap_CandidatesAndDetailUseTheSetting(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	rf.addTAClass(5, "13:00", "15:00")
	rf.secondCourseAssignment(fixtureOpts{})
	rf.setCourseCap(1)

	cands, err := rf.Req.Candidates(rf.ctx, rf.CourseID)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	var found bool
	for _, c := range cands {
		if c.ID != rf.TAID {
			continue
		}
		found = true
		if c.CourseCap != 1 || !c.AtQuota {
			t.Errorf("candidate cap = %d at_quota = %v, want 1 / true", c.CourseCap, c.AtQuota)
		}
	}
	if !found {
		t.Fatal("the TA should appear among the candidates")
	}

	// Detail is what staff open on /staff/approvals. Raise the cap so the
	// request is approved, then lower it: the banner must follow the setting.
	rf.setCourseCap(3)
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rf.setCourseCap(1)
	d, err := rf.Req.Detail(rf.ctx, res.ID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if len(d.Assignments) == 0 {
		t.Fatal("detail has no assignments")
	}
	a := d.Assignments[0]
	if a.CourseCap != 1 {
		t.Errorf("detail course_cap = %d, want 1", a.CourseCap)
	}
	var warned bool
	for _, w := range a.Warnings {
		if strings.Contains(w, "ครบ 1 วิชา") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("detail should warn at the configured cap, warnings = %v", a.Warnings)
	}
}

func TestCourseCap_FallsBackWhenSettingIsUnusable(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	rf.setCourseCap(0)
	n, err := maxCoursesPerTerm(rf.ctx, rf.Pool)
	if err != nil {
		t.Fatalf("cap: %v", err)
	}
	if n != defaultMaxCoursesPerTerm {
		t.Errorf("cap = %d, want the default %d when the stored value is 0", n, defaultMaxCoursesPerTerm)
	}
}

func TestCourseCap_SettingRejectsOutOfRange(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	cs := &CourseService{pool: rf.Pool, aud: audit.New(rf.Pool)}
	pr, err := cs.LatestPayRate(rf.ctx)
	if err != nil {
		t.Fatalf("latest rate: %v", err)
	}
	in := *pr
	in.MaxCoursesPerStudent = 11
	if _, err := cs.UpsertPayRate(rf.ctx, rf.LecturerID, in); err == nil || !strings.Contains(err.Error(), "1 ถึง 10") {
		t.Errorf("11 courses should be refused with a Thai range message, got %v", err)
	}
}
