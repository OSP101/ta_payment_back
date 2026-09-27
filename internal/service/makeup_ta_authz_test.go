package service

import (
	"testing"

	"github.com/google/uuid"
)

// Filing the compensation day (วันชดเชย) is the course's call. TAs could file
// and delete makeups for a while; the faculty decided on 27/09/2026 that makeup
// dates come from TDBM and the course only.

func TestMakeup_TAOnTheCourseRefused(t *testing.T) {
	f, holiday := twoPeriodFixture(t)

	err := f.teaching().AddMakeup(f.ctx, f.TAID, f.SectionID, MakeupSchedule{
		OriginalDate: holiday,
		MakeupDate:   nextMonday(1),
		Kind:         "lab",
		StartTime:    strPtr("13:00"),
		EndTime:      strPtr("15:00"),
	})
	if err != ErrForbidden {
		t.Fatalf("a TA on the course filed a makeup (even for a holiday): got %v, want ErrForbidden", err)
	}

	// Filed by the lecturer, the makeup is still out of the TA's reach.
	if err := f.teaching().AddMakeup(f.ctx, f.LecturerID, f.SectionID, MakeupSchedule{
		OriginalDate: holiday, MakeupDate: nextMonday(1), Kind: "lab",
		StartTime: strPtr("13:00"), EndTime: strPtr("15:00"),
	}); err != nil {
		t.Fatal(err)
	}
	var makeupID uuid.UUID
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT id FROM makeup_schedules WHERE section_id=$1 AND kind='lab'`,
		f.SectionID).Scan(&makeupID); err != nil {
		t.Fatal(err)
	}
	if err := f.teaching().DeleteMakeup(f.ctx, f.TAID, f.SectionID, makeupID); err != ErrForbidden {
		t.Fatalf("a TA deleted the lecturer's makeup: got %v, want ErrForbidden", err)
	}
	if err := f.teaching().WaiveMakeup(f.ctx, f.TAID, f.SectionID, WaiveMakeupRequest{OriginalDate: holiday, Kind: "lab"}); err != ErrForbidden {
		t.Fatalf("a TA waived a makeup: got %v, want ErrForbidden", err)
	}
}

// The TA's reach stops at the course boundary — an approved TA elsewhere in the
// system is a stranger here, exactly as a lecturer who does not teach the course
// is.
func TestMakeup_ForeignTARefused(t *testing.T) {
	f, holiday := twoPeriodFixture(t)
	stranger := f.insertUser("ta", "stranger")

	err := f.teaching().AddMakeup(f.ctx, stranger, f.SectionID, MakeupSchedule{
		OriginalDate: holiday,
		MakeupDate:   nextMonday(1),
		Kind:         "lab",
		StartTime:    strPtr("13:00"),
		EndTime:      strPtr("15:00"),
	})
	if err != ErrForbidden {
		t.Fatalf("want bare ErrForbidden for a TA with no assignment in this course, got %v", err)
	}
}
