package service

import "testing"

// These cases pin the server to TaPlanner.tsx's guideFor/ceilingFor over
// sittingGroups. If one fails after a planner change, change both.
func TestRecommendFromSections(t *testing.T) {
	r := PlanRatios{StudentsPerTA: 25, MinStudentsPerTA: 15, SuggestedTACap: 3}
	mon := func(start, end string) []recSlot { return []recSlot{{day: 1, start: start, end: end}} }
	tue := func(start, end string) []recSlot { return []recSlot{{day: 2, start: start, end: end}} }

	cases := []struct {
		name  string
		secs  []recSection
		total int
		want  TARecommendation
	}{
		{
			name: "one big section is capped at 3 but the ceiling is not",
			secs: []recSection{{students: 90, slots: mon("09:00", "12:00")}},
			want: TARecommendation{Students: 90, Sittings: 1, Recommended: 3, Ceiling: 6},
		},
		{
			name: "two sections at different times are two sittings",
			secs: []recSection{
				{students: 30, slots: mon("09:00", "12:00")},
				{students: 30, slots: tue("09:00", "12:00")},
			},
			want: TARecommendation{Students: 60, Sittings: 2, Recommended: 4, Ceiling: 4},
		},
		{
			name: "overlapping sections sit together",
			secs: []recSection{
				{students: 40, slots: mon("09:00", "12:00")},
				{students: 12, slots: mon("10:00", "11:00")},
			},
			want: TARecommendation{Students: 52, Sittings: 1, Recommended: 3, Ceiling: 4},
		},
		{
			name: "touching end and start do not overlap",
			secs: []recSection{
				{students: 10, slots: mon("09:00", "12:00")},
				{students: 10, slots: mon("12:00", "15:00")},
			},
			want: TARecommendation{Students: 20, Sittings: 2, Recommended: 2, Ceiling: 2},
		},
		{
			name: "every sitting gets at least one, even with no students",
			secs: []recSection{{students: 0, slots: mon("09:00", "12:00")}},
			want: TARecommendation{Students: 0, Sittings: 1, Recommended: 1, Ceiling: 1},
		},
		{
			name:  "blank sections fall back to the course count as one sitting",
			secs:  []recSection{{students: 0}, {students: 0}},
			total: 74,
			want:  TARecommendation{Students: 74, Sittings: 1, Recommended: 3, Ceiling: 5},
		},
		{
			name: "no sections at all",
			want: TARecommendation{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := recommendFromSections(c.secs, c.total, r)
			if got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestStaffingStatus(t *testing.T) {
	base := CourseStaffing{Students: 90, Recommended: 3, Ceiling: 6}
	for _, c := range []struct {
		requested int
		students  int
		want      string
	}{
		{0, 90, StaffingNoRequest},
		{2, 0, StaffingNoStudents},
		{2, 90, StaffingUnder},
		{3, 90, StaffingMatch},
		{5, 90, StaffingAboveGuide},
		{6, 90, StaffingAboveGuide},
		{7, 90, StaffingOverCeiling},
	} {
		row := base
		row.Requested, row.Students = c.requested, c.students
		if got := staffingStatus(row); got != c.want {
			t.Errorf("requested %d students %d: got %s want %s", c.requested, c.students, got, c.want)
		}
	}
}

func TestGregorianYM(t *testing.T) {
	for in, want := range map[string]string{"2569-06": "2026-06", "2026-06": "2026-06", "bad": "bad"} {
		if got := gregorianYM(in); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}
