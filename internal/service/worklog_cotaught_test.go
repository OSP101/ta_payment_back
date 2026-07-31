package service

import (
	"strings"
	"testing"
)

// 31/07/2026 — the co-taught exemption to the clock-overlap rule.
//
// At CP KKU one lab often serves sec 1 (ภาคปกติ) and sec 2 (โครงการพิเศษ) in the
// same room at the same hour. The TA works it once, but it has to be recorded
// against BOTH sections, because the two tracks draw on separate budgets. The
// college's signed form does this, and so did the rest of this system:
//
//   - Generate never called enforceNoOverlap, so it had been writing those
//     overlapping rows all along (12 sit approved in the live database);
//   - export.go rule B2 then counts the shared hours once at the regular rate
//     and strips the duplicate off the special side, so nothing is double-paid.
//
// enforceNoOverlap was the lone dissenter: it refused any hand-written or
// hand-corrected row that overlapped, which meant a TA could not fix a row the
// generator had produced for them.
//
// These tests pin the exemption AND its boundary — it must not become a general
// licence to bill one hour twice.

// The case the exemption exists for.
func TestUpsert_CotaughtSectionsMayShareTheSameHours(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	sibling := f.cotaughtSiblingAssignment("special")
	d := day(10)

	f.mustUpsert(f.entry(d, "09:00", "11:00", 2))

	second := f.entry(d, "09:00", "11:00", 2)
	second.AssignmentID = sibling
	if _, err := f.upsert(second); err != nil {
		t.Fatalf("one sitting taught to two co-scheduled sections must be recordable "+
			"against both — the generator already writes exactly this: %v", err)
	}
}

// The boundary. Two DIFFERENT courses are never one sitting, whatever the clock
// says: the TA cannot be in both rooms, and both sides would bill in full.
func TestUpsert_DifferentCoursesStillCannotShareHours(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	other := f.secondCourseAssignment(fixtureOpts{})
	d := day(10)

	f.mustUpsert(f.entry(d, "09:00", "11:00", 2))

	clash := f.entry(d, "10:00", "12:00", 2)
	clash.AssignmentID = other
	if _, err := f.upsert(clash); err == nil {
		t.Fatal("the co-taught exemption must not reach across courses — " +
			"those are two separate commitments billed in full on both sides")
	}
}

// The other boundary, and the easier one to get wrong: within ONE assignment the
// TA still cannot log the same hour twice. The exemption is about a second
// SECTION, not a second row.
func TestUpsert_CotaughtExemptionDoesNotAllowSelfOverlap(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.cotaughtSiblingAssignment("special") // group marker present on both
	d := day(10)

	f.mustUpsert(f.entry(d, "09:00", "11:00", 2))

	if _, err := f.upsert(f.entry(d, "10:00", "12:00", 2)); err == nil {
		t.Fatal("two rows on the SAME assignment still overlap — being co-taught " +
			"with another section says nothing about billing your own hour twice")
	} else if !strings.Contains(err.Error(), "ซ้อน") {
		t.Errorf("the refusal should still name the overlap, got: %v", err)
	}
}

// Two sections on the SAME request that are NOT taught together. This is the
// only shape that exercises the NOT NULL guard: same request, so that clause
// passes, and both groups NULL. Without the guard `NULL = NULL` yields NULL,
// `NOT (… AND NULL)` is NULL, the row is filtered out, and every ungrouped
// sibling silently becomes exempt.
func TestUpsert_UngroupedSiblingsOnOneRequestKeepTheOldOverlapRule(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	sibling := f.siblingAssignment("special", nil) // no co-taught marker
	d := day(10)

	f.mustUpsert(f.entry(d, "13:00", "15:00", 2))

	clash := f.entry(d, "13:00", "15:00", 2)
	clash.AssignmentID = sibling
	if _, err := f.upsert(clash); err == nil {
		t.Fatal("two ungrouped sections of one request must not share hours — " +
			"NULL cotaught_group means 'not co-taught', not 'exempt'")
	}
}

// Group numbers are per-request counters, so two different courses can both call
// a group "1". Matching on the number alone would make those two unrelated
// courses exempt from each other.
func TestUpsert_SameGroupNumberInAnotherCourseIsNotCoTaught(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	other := f.secondCourseAssignment(fixtureOpts{})
	f.groupSecondCourse(other, 1) // same number, different request
	d := day(10)

	f.mustUpsert(f.entry(d, "13:00", "15:00", 2))

	clash := f.entry(d, "13:00", "15:00", 2)
	clash.AssignmentID = other
	if _, err := f.upsert(clash); err == nil {
		t.Fatal("cotaught_group is numbered per request — the same number in another " +
			"course is a coincidence, not one sitting")
	}
}
