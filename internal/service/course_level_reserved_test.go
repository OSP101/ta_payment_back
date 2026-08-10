package service

import "testing"

// courseLevelFromReserved reads only the FIRST clause's label — the same
// "first wins" rule curriculumFromReserved already applies to programme
// tokens. A whole-string substring scan (the pre-Phase-4 implementation)
// answered graduate for "ตรี : SC-IT ปี 3, โท : CP-DSAI ปี 1" just because "โท"
// appears somewhere in the string, even though the section's own primary
// label is "ตรี" — this is the exact case the plan's Phase 4 mutation names.
func TestCourseLevelFromReserved(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain undergrad", "ตรี : SC-IT ปี 3 ขึ้นไป", "undergrad"},
		{"undergrad special program, no colon", "ตรี โครงการพิเศษ", "undergrad"},
		{"plain graduate master, no colon", "โท ปี 1", "graduate"},
		{"plain graduate phd", "เอก ปี 1", "graduate"},
		{"บัณฑิตศึกษา label", "บัณฑิตศึกษา : CP-DSAI ปี 1", "graduate"},
		{
			// The plan's own mutation case: a compound ReservedFor whose
			// first clause is undergrad and whose SECOND clause happens to
			// contain a graduate keyword. The whole-string scan answered
			// graduate here; only reading the first clause's label answers
			// undergrad, which is correct — this section's primary group is
			// "ตรี : SC-IT ปี 3".
			"undergrad first, graduate second clause", "ตรี : SC-IT ปี 3, โท : CP-DSAI ปี 1", "undergrad",
		},
		{
			"graduate first, undergrad second clause",
			"โท : CP-DSAI ปี 1, ตรี : SC-IT ปี 3", "graduate",
		},
		{"empty string defaults undergrad", "", "undergrad"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := courseLevelFromReserved(c.in); got != c.want {
				t.Errorf("courseLevelFromReserved(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
