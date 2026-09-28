package service

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The request form greys out the in-class duty of a kind whose every session
// collides with the TA's own class. The server must reach the same verdict, or
// a call that skips the form declares hours the TA can never work.
//
// At submission the hours are zeroed (applyClashOutcome), not refused, so the
// result is the same whichever order the timetable arrived in. Staff
// corrections, made after the verdict, are refused (checkInClassDuties).

func (rf *requestFixture) declaredInClass(reqID uuid.UUID) (attendance, lab float64) {
	rf.t.Helper()
	if err := rf.Pool.QueryRow(rf.ctx, `
		SELECT wf.attendance_hrs, wf.lab_hrs
		FROM ta_workload_forms wf
		JOIN ta_request_assignments a ON a.id = wf.assignment_id
		WHERE a.request_id = $1`, reqID).Scan(&attendance, &lab); err != nil {
		rf.t.Fatalf("workload lookup: %v", err)
	}
	return attendance, lab
}

// The fixture's lab (13:00–16:00) sits on the TA's class; the lecture is free.
// สอนปฏิบัติการ is zeroed, เช็คชื่อ survives — in both filing orders.
func TestSubmitZeroesInClassDutyOfFullyBlockedKind_EitherOrder(t *testing.T) {
	check := func(label string, rf *requestFixture, reqID uuid.UUID) {
		t.Helper()
		att, lab := rf.declaredInClass(reqID)
		if lab != 0 {
			t.Errorf("%s: lab_hrs = %.1f, want 0 — every lab clashes", label, lab)
		}
		if att != 2 {
			t.Errorf("%s: attendance_hrs = %.1f, want 2 — the lecture is free", label, att)
		}
	}

	before := newRequestFixture(t, fixtureOpts{})
	before.addTAClass(1, "13:00", "16:00")
	res, err := before.Req.Create(before.ctx, before.LecturerID, before.createInput())
	if err != nil {
		t.Fatalf("create (timetable first) must not refuse: %v", err)
	}
	check("timetable first", before, res.ID)
	// The lecturer is told at submit time, not left to find the lab hours gone.
	var told bool
	for _, c := range res.Checks {
		if c.Rule == "clash_trimmed" && strings.Contains(c.Message, "ตัดชั่วโมงสอนปฏิบัติการ") {
			told = true
		}
	}
	if !told {
		t.Errorf("no notice that the lab hours were removed; checks = %+v", res.Checks)
	}

	after := newRequestFixture(t, fixtureOpts{})
	res2, err := after.Req.Create(after.ctx, after.LecturerID, after.createInput())
	if err != nil {
		t.Fatalf("create (submit first): %v", err)
	}
	after.addTAClass(1, "13:00", "16:00")
	if err := after.Req.ReevaluateForTA(after.ctx, after.TAID, after.TermID); err != nil {
		t.Fatalf("reevaluate: %v", err)
	}
	check("submit first", after, res2.ID)
}

// The case that prompted it: the TA's class covers the lab but not the lecture.
// The lecture side stays fully requestable; สอนปฏิบัติการ is refused.
func TestInClassDutyRefusedOnlyOnTheFullyBlockedKind(t *testing.T) {
	f := newCapFixture(t)
	sec := f.addSection("2",
		[3]string{"lecture", "09:00", "12:00"},
		[3]string{"lab", "13:00", "16:00"})
	f.ownClassAt(1, "13:00", "16:00") // covers the lab only

	check := func(w WorkloadInput) error {
		return f.svc.checkInClassDuties(f.ctx, f.pool, f.ta, sec, w, "ผู้ช่วย", "2")
	}
	if err := check(ugWorkload(1, 2, 1, 0, 0)); err != nil {
		t.Fatalf("lecture duties must stay declarable when only the lab clashes: %v", err)
	}
	if err := check(ugWorkload(0, 0, 0, 0, 2)); err != nil {
		t.Fatalf("อื่น ๆ (ปฏิบัติการ) is off-slot and must stay declarable: %v", err)
	}
	if err := check(ugWorkload(0, 0, 0, 2, 0)); err == nil {
		t.Fatal("สอนปฏิบัติการ on a fully-clashing lab must be refused")
	}
}

func TestValidateInClassDuties(t *testing.T) {
	lecBlocked := kindClash{"lecture": {Total: 2, Clashing: 2}, "lab": {Total: 1, Clashing: 0}}
	partial := kindClash{"lecture": {Total: 2, Clashing: 1}, "lab": {Total: 2, Clashing: 1}}

	cases := []struct {
		name    string
		w       WorkloadInput
		k       kindClash
		wantErr bool
	}{
		{"เช็คชื่อ on blocked lecture", ugWorkload(0, 1, 0, 0, 0), lecBlocked, true},
		{"grading on blocked lecture", ugWorkload(2, 0, 1, 0, 0), lecBlocked, false},
		{"lab teaching on free lab", ugWorkload(0, 0, 0, 2, 0), lecBlocked, false},
		{"partial clash costs nothing", ugWorkload(0, 1, 0, 1, 0), partial, false},
		{"no timetable, nothing clashes", ugWorkload(0, 1, 0, 1, 0), kindClash{}, false},
	}
	for _, c := range cases {
		err := validateInClassDuties(c.w, c.k, "ผู้ช่วย", "1")
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v, wantErr=%v", c.name, err, c.wantErr)
		}
	}
}
