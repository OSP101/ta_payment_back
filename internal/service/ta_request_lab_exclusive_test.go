package service

import (
	"strings"
	"testing"
)

// ปฏิบัติการ is either-or. A TA either runs the lab session (lab_hrs) or
// supports it from outside — prep, materials, marking lab sheets (lab_other_hrs).
// Declaring both bills the same lab twice and asks one person to be in two
// places for one slot.
//
// Before this, "อื่น ๆ" existed only on the lecture side, so a lecturer who
// wanted out-of-slot lab support had no field for it at all.

func TestLabTeachingAndLabOtherAreMutuallyExclusive(t *testing.T) {
	hrs := sectionWeekly{Lecture: 3, Lab: 3}

	err := validateUndergradSectionCaps(ugWorkload(0, 0, 0, 2, 1), "ผู้ช่วย ทดสอบ", "1", hrs)
	if err == nil {
		t.Fatal("declaring both lab teaching and lab other-work must be refused")
	}
	if !strings.Contains(err.Error(), "เลือกได้อย่างใดอย่างหนึ่ง") {
		t.Fatalf("the message should say pick one, got %q", err)
	}
}

func TestEitherLabFieldAloneIsAccepted(t *testing.T) {
	hrs := sectionWeekly{Lecture: 3, Lab: 3}

	if err := validateUndergradSectionCaps(ugWorkload(0, 0, 0, 3, 0), "ผู้ช่วย", "1", hrs); err != nil {
		t.Fatalf("lab teaching alone must be accepted: %v", err)
	}
	if err := validateUndergradSectionCaps(ugWorkload(0, 0, 0, 0, 3), "ผู้ช่วย", "1", hrs); err != nil {
		t.Fatalf("lab other-work alone must be accepted: %v", err)
	}
}

// Both draw on the same ceiling: the section's real lab hours.
func TestLabOtherObeysTheLabCeiling(t *testing.T) {
	hrs := sectionWeekly{Lecture: 3, Lab: 2}

	err := validateUndergradSectionCaps(ugWorkload(0, 0, 0, 0, 3), "ผู้ช่วย", "1", hrs)
	if err == nil {
		t.Fatal("3h of lab other-work must not fit a 2h lab")
	}
	if !strings.Contains(err.Error(), "อื่น ๆ (ปฏิบัติการ)") {
		t.Fatalf("the message should name the field, got %q", err)
	}
}

// The declared total drives the "did the lecturer declare anything?" gate and
// the daily-feasibility maths. Leaving lab other-work out of it would make a
// request that declares ONLY that work look empty.
func TestLabOtherCountsInTheWeeklyTotal(t *testing.T) {
	w := ugWorkload(0, 0, 0, 0, 2.5)

	if got := weeklyWorkloadTotal(w, "undergrad"); got != 2.5 {
		t.Fatalf("lab other-work must count toward the weekly total, got %.2f", got)
	}
	// And it composes with the rest rather than replacing it.
	full := ugWorkload(1, 1, 1, 0, 2)
	if got := weeklyWorkloadTotal(full, "undergrad"); got != 5 {
		t.Fatalf("want 5.0 total, got %.2f", got)
	}
}

// The lecture-side total is bounded at 3× the class hours (raised from 2× on
// 2026-10-03). The lecture side has exactly three fields, each already capped
// at 1× by the per-field rule, so 3× is the largest total they can reach: the
// total cap is now a backstop that only bites if a fourth field is added.
func TestSectionTotalIsCappedAtThreeTimesTheClassHours(t *testing.T) {
	hrs := sectionWeekly{Lecture: 2, Lab: 2}

	// The shape everyone actually files: 2 + 2 on a 2-hour lecture.
	if err := validateUndergradSectionCaps(ugWorkload(2, 2, 0, 0, 0), "ผู้ช่วย", "1", hrs); err != nil {
		t.Fatalf("the arrangement in live use must stay legal: %v", err)
	}
	// All three fields at the per-field ceiling is exactly 3× and inclusive.
	if err := validateUndergradSectionCaps(ugWorkload(2, 2, 2, 0, 0), "ผู้ช่วย", "1", hrs); err != nil {
		t.Fatalf("exactly 3× the class hours must be allowed: %v", err)
	}
	// The per-field ceiling still refuses a single field above the class hours.
	if err := validateUndergradSectionCaps(ugWorkload(3, 0, 0, 0, 0), "ผู้ช่วย", "1", hrs); err == nil {
		t.Fatal("one field above the class hours must still be refused")
	}
}

// The lab side is bounded the same way. It cannot exceed 1× today because the
// two lab fields are mutually exclusive, but the rule is stated for both sides
// so that relaxing the exclusivity later cannot silently uncap the total.
func TestSectionTotalAppliesToTheLabSideToo(t *testing.T) {
	hrs := sectionWeekly{Lecture: 2, Lab: 2}

	if err := validateUndergradSectionCaps(ugWorkload(0, 0, 0, 2, 0), "ผู้ช่วย", "1", hrs); err != nil {
		t.Fatalf("2h of lab teaching on a 2h lab must be allowed: %v", err)
	}
}

// A group with no lecture sessions is governed by the per-field rule, which
// gives the clearer message. The total cap must not pre-empt it with a
// confusing "เกินเพดานรวม 0.00".
func TestAbsentKindKeepsThePerFieldMessage(t *testing.T) {
	hrs := sectionWeekly{Lecture: 0, Lab: 2}

	err := validateUndergradSectionCaps(ugWorkload(1, 0, 0, 0, 0), "ผู้ช่วย", "1", hrs)
	if err == nil {
		t.Fatal("a group with no lectures must refuse lecture-side hours")
	}
	if !strings.Contains(err.Error(), "ไม่มีคาบประเภทนี้ในตารางสอน") {
		t.Fatalf("the per-field message should win, got %q", err)
	}
}
