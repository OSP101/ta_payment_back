package service

import (
	"strings"
	"testing"
)

// The daily pay cap and the payout engine must agree on what a day costs.
//
// They did not. A sitting taught to sec 1 (ภาคปกติ) and sec 2 (โครงการพิเศษ) at
// the same hour is recorded against both sections — the generator writes both,
// and export.go rule B2 then pays the shared hours ONCE at the regular rate.
// enforceDailyBahtCap summed both rows, so it charged the cap for money nobody
// is paid.
//
// Found on live data: สุพพิธาน's Tuesday 21 July came to 360฿ against a 300฿ cap
// (2h + 2h billed twice at 40฿ and 50฿) where the payout engine settles it at
// 160฿. Every row of that day was frozen — the block was hit while editing a
// NOTE, with the times unchanged.

// cotaughtDayFixture gives the TA two co-scheduled sections of one course and
// logs the same two hours against both, the way the generator does.
func cotaughtDayFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t, fixtureOpts{
		// 40฿/h regular, 50฿/h special, 300฿ daily cap — the real 2569 rates.
		Rates: rateOverrides{
			UndergradRegular: 40, UndergradSpecial: 50, DailyPayCapBaht: 300,
		},
	})
	sibling := f.cotaughtSiblingAssignment("special")
	d := day(10)

	f.mustUpsert(f.entry(d, "15:00", "17:00", 2)) // sec 1 ภาคปกติ → 80฿
	second := f.entry(d, "15:00", "17:00", 2)     // sec 2 ภาคพิเศษ → 100฿ on paper
	second.AssignmentID = sibling
	f.mustUpsert(second)
	return f, d
}

// 2h regular (80฿) + the same 2h special (100฿) is ONE sitting. Counting it as
// 180฿ means a second sitting of the same length is refused at 360฿ — a limit
// the TA never actually reaches.
func TestDailyBahtCap_CountsACoTaughtSittingOnce(t *testing.T) {
	f, d := cotaughtDayFixture(t)

	// A second sitting, also co-taught. Real total = 80 + 80 = 160฿, far under
	// the 300฿ cap; the double-counting version reaches 360฿ and refuses.
	third := f.entry(d, "17:00", "19:00", 2)
	if _, err := f.upsert(third); err != nil {
		t.Fatalf("a second co-taught sitting must fit under the daily cap — "+
			"the payout engine settles this day at 160฿, not 360฿: %v", err)
	}
}

// ...and editing a row that is already there must not be refused either. This is
// the exact shape that was reported: only the note changed.
func TestDailyBahtCap_EditingANoteOnACoTaughtRowIsAllowed(t *testing.T) {
	f, d := cotaughtDayFixture(t)

	var id string
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT wl.id::text FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		WHERE a.id = $1 AND wl.work_date = $2::date`, f.AssignmentID, d).Scan(&id); err != nil {
		t.Fatal(err)
	}

	edited := f.entry(d, "15:00", "17:00", 2) // identical times
	edited.ID = mustUUID(t, id)
	note := "เช็คชื่อ แทน"
	edited.Note = &note

	if _, err := f.Svc.StaffUpsert(f.ctx, f.StaffID, true, edited); err != nil {
		t.Fatalf("changing only the note must not be refused by the pay cap: %v", err)
	}
}

// The cap must still bite when a day genuinely costs too much. Without this the
// fix could have been "stop checking".
func TestDailyBahtCap_StillRefusesAGenuinelyExpensiveDay(t *testing.T) {
	f := newFixture(t, fixtureOpts{
		Rates: rateOverrides{
			UndergradRegular: 40, UndergradSpecial: 50, DailyPayCapBaht: 300,
			// The hour ceiling is opened right up so ONLY the baht rule can
			// refuse. Left at its default 7h it fired first and the test passed
			// without ever reaching the code it was written for.
			UGRegularDailyCap: 24,
		},
	})
	d := day(10)
	// One section, no co-teaching: 7h × 40฿ = 280฿.
	f.mustUpsert(f.entry(d, "08:00", "15:00", 7))

	// Another hour takes it to 320฿.
	_, err := f.upsert(f.entry(d, "15:00", "16:00", 1))
	if err == nil {
		t.Fatal("a day that really does exceed 300฿ must still be refused")
	}
	if !strings.Contains(err.Error(), "300") {
		t.Errorf("the refusal should name the cap, got: %v", err)
	}
}

// WHICH rate the shared sitting is counted at, not just that it is counted once.
//
// Rule B2 pays the overlap at the REGULAR rate ("เบิกภาคปกติก่อน"). If the cap
// picked the special rate instead it would over-charge by the difference and
// refuse days the payout engine happily settles — the same class of mismatch,
// just smaller. The numbers below sit either side of the cap so only the right
// choice passes: 4h shared + 3h alone is 280฿ at 40฿/h and 320฿ if the shared
// hours are priced at 50฿/h.
func TestDailyBahtCap_PricesASharedSittingAtTheRegularRate(t *testing.T) {
	f := newFixture(t, fixtureOpts{
		Rates: rateOverrides{
			UndergradRegular: 40, UndergradSpecial: 50, DailyPayCapBaht: 300,
			UGRegularDailyCap: 24,
		},
	})
	sibling := f.cotaughtSiblingAssignment("special")
	d := day(10)

	f.mustUpsert(f.entry(d, "15:00", "19:00", 4)) // sec 1 ภาคปกติ
	shared := f.entry(d, "15:00", "19:00", 4)     // sec 2 ภาคพิเศษ, same sitting
	shared.AssignmentID = sibling
	f.mustUpsert(shared)

	// 4h shared (160฿ at the regular rate) + 3h alone (120฿) = 280฿ ≤ 300฿.
	if _, err := f.upsert(f.entry(d, "08:00", "11:00", 3)); err != nil {
		t.Fatalf("the shared sitting must be priced at the regular rate, as export "+
			"rule B2 pays it — at the special rate this day reads 320฿ and is "+
			"refused: %v", err)
	}
}
