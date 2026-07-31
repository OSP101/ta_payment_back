package service

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/testutil"
)

// A TA's class schedule decides which of their work-log hours clash with a
// class and are therefore unpayable. Once staff export the payout documents,
// those decisions are printed and on their way to finance — so the schedule has
// to freeze with them. Editing it afterwards would leave the system disagreeing
// with a document that has already left the building, silently.
//
// Past terms stay readable; only writing is refused.
func TestReplaceClasses_LockedAfterExport(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	svc := &WorkloadService{pool: pool}

	term := insertTerm(t, pool, 2569, 1, true)
	ta := insertDashUser(t, pool, "ta")
	course := insertDashCourse(t, pool, term, "LOCK", 3, 0, 60)
	period := insertDashPeriod(t, pool, term, "2569-06")

	block := []ClassBlock{{
		TermID: term, CourseCode: "SC101", CourseName: "วิชาทดสอบ",
		DayOfWeek: 1, StartTime: "09:00", EndTime: "11:00",
	}}

	// Before any export the schedule is editable.
	if reason, err := svc.ScheduleLockedReason(ctx, ta, term); err != nil {
		t.Fatalf("ScheduleLockedReason: %v", err)
	} else if reason != "" {
		t.Fatalf("locked before any export: %q", reason)
	}
	if err := svc.ReplaceClasses(ctx, ta, term, block); err != nil {
		t.Fatalf("ReplaceClasses before export must succeed: %v", err)
	}

	// A month that staff have merely reviewed is not exported yet — still open.
	insertDashPeriodStatus(t, pool, period, ta, course, "staff_reviewed")
	if err := svc.ReplaceClasses(ctx, ta, term, block); err != nil {
		t.Fatalf("staff_reviewed must not lock the schedule: %v", err)
	}

	// Exported: writes are refused from here on.
	if _, err := pool.Exec(ctx,
		`UPDATE submission_period_status SET status = 'exported'
		 WHERE submission_period_id = $1 AND ta_id = $2`, period, ta); err != nil {
		t.Fatalf("mark exported: %v", err)
	}
	reason, err := svc.ScheduleLockedReason(ctx, ta, term)
	if err != nil {
		t.Fatalf("ScheduleLockedReason: %v", err)
	}
	if reason == "" {
		t.Fatal("schedule still editable after export — TA could change the clash " +
			"rules behind a document already sent to finance")
	}
	err = svc.ReplaceClasses(ctx, ta, term, block)
	if err == nil {
		t.Fatal("ReplaceClasses succeeded after export, want refusal")
	}
	if !strings.Contains(err.Error(), "ส่งออกเอกสาร") {
		t.Errorf("error = %q, want it to say why (ส่งออกเอกสารแล้ว)", err.Error())
	}

	// Reading the frozen term still works — the whole point of "ดูย้อนหลังได้".
	got, err := svc.ListClasses(ctx, ta, term)
	if err != nil {
		t.Fatalf("ListClasses on a locked term must still work: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("ListClasses returned %d blocks, want the 1 saved before the lock", len(got))
	}
}

// The lock is per TA and per term: one TA's exported month must not freeze a
// colleague, nor the same TA's next semester.
func TestScheduleLock_ScopedToTAAndTerm(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	svc := &WorkloadService{pool: pool}

	locked := insertTerm(t, pool, 2569, 1, true)
	other := insertTerm(t, pool, 2569, 2, false)
	exportedTA := insertDashUser(t, pool, "ta")
	otherTA := insertDashUser(t, pool, "ta")
	course := insertDashCourse(t, pool, locked, "SCOPE", 3, 0, 60)
	period := insertDashPeriod(t, pool, locked, "2569-06")
	insertDashPeriodStatus(t, pool, period, exportedTA, course, "exported")

	cases := []struct {
		name string
		ta   uuid.UUID
		term uuid.UUID
		want bool
	}{
		{"exported TA, exported term", exportedTA, locked, true},
		{"other TA, same term", otherTA, locked, false},
		{"exported TA, other term", exportedTA, other, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason, err := svc.ScheduleLockedReason(ctx, c.ta, c.term)
			if err != nil {
				t.Fatalf("ScheduleLockedReason: %v", err)
			}
			if (reason != "") != c.want {
				t.Errorf("locked = %v, want %v (reason %q)", reason != "", c.want, reason)
			}
		})
	}
}
