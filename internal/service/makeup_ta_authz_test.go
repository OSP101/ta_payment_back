package service

import (
	"testing"

	"github.com/google/uuid"
)

// Filing the compensation day (วันชดเชย) used to be the lecturer's alone. It is
// now open to the course's TAs as well — see assertMakeupManager. The rule that
// has to survive is the object-level half: "TA" is not a licence to touch any
// course, only one the TA actually holds an approved assignment in.

func TestMakeup_TAOnTheCourseMayFileAndDelete(t *testing.T) {
	f, holiday := twoPeriodFixture(t)

	if err := f.teaching().AddMakeup(f.ctx, f.TAID, f.SectionID, MakeupSchedule{
		OriginalDate: holiday,
		MakeupDate:   nextMonday(1),
		Kind:         "lab",
		StartTime:    strPtr("13:00"),
		EndTime:      strPtr("15:00"),
	}); err != nil {
		t.Fatalf("a TA assigned to this course must be able to file a makeup, got %v", err)
	}

	var makeupID uuid.UUID
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT id FROM makeup_schedules WHERE section_id=$1 AND kind='lab'`,
		f.SectionID).Scan(&makeupID); err != nil {
		t.Fatalf("the makeup the TA filed is not in the table: %v", err)
	}

	// Delete is the other half of "edit": the form replaces a makeup by deleting
	// then re-inserting, so a TA who could only add would be unable to correct a
	// date they had just entered.
	if err := f.teaching().DeleteMakeup(f.ctx, f.TAID, f.SectionID, makeupID); err != nil {
		t.Fatalf("a TA on the course must be able to delete a makeup, got %v", err)
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
