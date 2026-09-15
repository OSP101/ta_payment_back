package service

import (
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/storage"
)

// NewContainer used to build c.Teaching with c.Notify captured before c.Notify
// was assigned — a struct literal reads the field at that point in the
// function, not when the container is later used, so TeachingService.notify
// was permanently nil in production even though c.Notify itself was fine.
// The nil check at the DeleteMakeup call site swallowed this silently: no
// panic, just a cancellation notice that never reached the TA. This test goes
// through the real NewContainer (unlike f.teaching(), which hand-builds a
// TeachingService and would never have caught a wiring bug in the
// constructor) and checks the actual side effect — a row in `notifications`.
func TestMakeupCancel_NotificationFiresThroughRealContainer(t *testing.T) {
	f, holiday := twoPeriodFixture(t)

	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := NewContainer(f.Pool, store, mail.New(config.Config{}), audit.New(f.Pool), config.Config{}, nil, nil)

	makeupDate := nextMonday(1)
	if err := c.Teaching.AddMakeup(f.ctx, f.LecturerID, f.SectionID, MakeupSchedule{
		OriginalDate: holiday,
		MakeupDate:   makeupDate,
		Kind:         "lab",
		StartTime:    strPtr("13:00"),
		EndTime:      strPtr("15:00"),
	}); err != nil {
		t.Fatalf("AddMakeup: %v", err)
	}

	var makeupID uuid.UUID
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT id FROM makeup_schedules WHERE section_id=$1 AND kind='lab'`,
		f.SectionID).Scan(&makeupID); err != nil {
		t.Fatalf("filed makeup not found: %v", err)
	}

	// A draft hour on the makeup date so DeleteMakeup has a TA to notify.
	f.insertAutoRow(makeupDate, "13:00", "15:00", 2, "lab", "ชดเชย")

	if err := c.Teaching.DeleteMakeup(f.ctx, f.LecturerID, f.SectionID, makeupID); err != nil {
		t.Fatalf("DeleteMakeup: %v", err)
	}

	var count int
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM notifications
		  WHERE user_id = $1 AND title = 'อาจารย์ยกเลิกวันชดเชย'`,
		f.TAID).Scan(&count); err != nil {
		t.Fatalf("querying notifications: %v", err)
	}
	if count == 0 {
		t.Fatal("cancelling the makeup did not notify the TA — TeachingService.notify " +
			"was nil, which happens if NewContainer wires c.Teaching before c.Notify exists")
	}
}
