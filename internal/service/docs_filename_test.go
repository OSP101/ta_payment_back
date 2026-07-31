package service

import "testing"

// The finance office files every downloaded document by student ID, so the
// name is a contract with people outside this codebase, not an internal
// detail: `รหัสนักศึกษา_ชื่อ_นามสกุล.pdf`. These tests pin it, including the
// two cases where the obvious implementation produces something unusable.

func TestTAFileStem_StudentIDLeadsThenName(t *testing.T) {
	got := taFileStem("653020123-4", "สมชาย", "ใจดี")
	want := "653020123-4_สมชาย_ใจดี"
	if got != want {
		t.Fatalf("taFileStem = %q, want %q", got, want)
	}
}

// A TA can reach a download before their student ID is on record. Joining the
// parts blindly would yield `_สมชาย_ใจดี`, which reads as a broken export and
// sorts to the top of the officer's folder ahead of every real file.
func TestTAFileStem_OmitsMissingStudentID(t *testing.T) {
	got := taFileStem("", "สมชาย", "ใจดี")
	if want := "สมชาย_ใจดี"; got != want {
		t.Fatalf("taFileStem = %q, want %q", got, want)
	}
}

// Names carry spaces in practice, and a stray slash in a hand-entered field
// would otherwise create a subdirectory (or an unopenable download on
// Windows). Thai characters must survive untouched — they are the whole point
// of the name being human-readable.
func TestTAFileStem_SanitizesSeparatorsKeepsThai(t *testing.T) {
	got := taFileStem(" 653020123-4 ", "สม ชาย", "ใจดี/ทดสอบ")
	if want := "653020123-4_สม_ชาย_ใจดี-ทดสอบ"; got != want {
		t.Fatalf("taFileStem = %q, want %q", got, want)
	}
}

// Never return a bare extension: a filename of ".pdf" is hidden on Unix and
// rejected by some browsers' download handlers.
func TestTAFileStem_FallsBackWhenEverythingIsBlank(t *testing.T) {
	if got := taFileStem("", "", ""); got == "" {
		t.Fatal("taFileStem returned empty; a download needs some name")
	}
}
