package service

import "testing"

// timetableFormToPDF is the seam between the JSON the web page renders and the
// PDF the payout zip carries. Both are "the form the lecturer signs", so a field
// that silently fails to cross this boundary produces two documents that claim
// to be the same thing and are not — the paper copy would be the one missing
// information, and it is the one that gets signed.

func TestTimetableFormToPDF_CarriesEveryBlockField(t *testing.T) {
	note := "สอนปฏิบัติ (ชดเชย)"
	in := &TimetableForm{
		TAName:    "นายจิรายุ ร่มลำดวน",
		StudentID: "673380499-3",
		TermLabel: "2569/1",
		YearMonth: "2569-07",
		Blocks: []TimetableBlock{{
			Kind: "lab", CourseCode: "CP351203", SecNo: "2", Track: "special",
			DayOfWeek: 4, StartTime: "13:00", EndTime: "15:00",
			Expected: 4, Logged: 3,
		}},
		Signers: []TimetableSigner{{
			LecturerName: "ผศ.ดร.วรัญญา วรรณศรี",
			Courses:      []string{"CP351203", "CP321002"},
		}},
		OutOfGrid: []TimetableOutOfGrid{{
			WorkDate: "2569-07-25", StartTime: "13:00", EndTime: "15:00",
			Activity: "lab", CourseCode: "CP351203", SecNo: "1",
			Note: &note, Source: "auto",
		}},
	}

	got := timetableFormToPDF(in)

	if got.TAName != in.TAName || got.StudentID != in.StudentID ||
		got.TermLabel != in.TermLabel || got.YearMonth != in.YearMonth {
		t.Errorf("header fields lost: %+v", got)
	}
	if len(got.Blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(got.Blocks))
	}
	b := got.Blocks[0]
	if b.Kind != "lab" || b.CourseCode != "CP351203" || b.SecNo != "2" ||
		b.Track != "special" || b.DayOfWeek != 4 ||
		b.StartTime != "13:00" || b.EndTime != "15:00" {
		t.Errorf("block mapped wrong: %+v", b)
	}
	// The counts are the whole reason a reviewer can check a month without
	// reading every row; dropping them makes the PDF look complete and say less.
	if b.Expected != 4 || b.Logged != 3 {
		t.Errorf("occurrence counts lost: logged=%d expected=%d", b.Logged, b.Expected)
	}

	if len(got.Signers) != 1 || len(got.Signers[0].Courses) != 2 {
		t.Errorf("signer grouping lost: %+v", got.Signers)
	}

	if len(got.OutOfGrid) != 1 {
		t.Fatalf("out-of-grid rows = %d, want 1", len(got.OutOfGrid))
	}
	o := got.OutOfGrid[0]
	if o.Note != note {
		t.Errorf("note = %q, want %q", o.Note, note)
	}
	// Source drives which of the two tables the row lands in. Lose it and a
	// system-generated makeup reads as something the TA typed by hand, which is
	// the opposite of what the reviewer is being asked to check.
	if o.Source != "auto" {
		t.Errorf("source = %q, want auto", o.Source)
	}
	if o.Kind != "ปฏิบัติการ" {
		t.Errorf("activity not translated for paper: %q", o.Kind)
	}
}

// A nil note is the common case and must not panic or print "<nil>".
func TestTimetableFormToPDF_HandlesMissingNote(t *testing.T) {
	got := timetableFormToPDF(&TimetableForm{
		OutOfGrid: []TimetableOutOfGrid{{WorkDate: "2569-07-25", Activity: "review"}},
	})
	if len(got.OutOfGrid) != 1 {
		t.Fatalf("out-of-grid rows = %d, want 1", len(got.OutOfGrid))
	}
	if got.OutOfGrid[0].Note != "" {
		t.Errorf("note = %q, want empty", got.OutOfGrid[0].Note)
	}
}

func TestActivityTH_TranslatesEveryActivity(t *testing.T) {
	for in, want := range map[string]string{
		"lecture": "บรรยาย",
		"lab":     "ปฏิบัติการ",
		"review":  "ตรวจงาน",
		"other":   "อื่นๆ",
	} {
		if got := activityTH(in); got != want {
			t.Errorf("activityTH(%q) = %q, want %q", in, got, want)
		}
	}
	// An unknown activity falls through to itself rather than becoming blank —
	// a code on the paper form is worse than a translation, but far better than
	// an empty cell nobody can question.
	if got := activityTH("weird"); got != "weird" {
		t.Errorf("unknown activity = %q, want it echoed", got)
	}
}
