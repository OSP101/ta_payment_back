package service

import (
	"os"
	"strings"
	"testing"

	"ta-payment-back/internal/audit"
)

// Pinned against the real registrar file used in the 2026-09-14 TOR
// acceptance review (§3.3 ข.4): importing it produced 127/127 courses
// created, and 65 rows the old code lumped into one "error 65 รายการ" bucket
// next to a successful 127/127 import — reading as broken when it mostly
// wasn't. The split resolves them into what they actually are:
//   - 57 are the expected shape for a course with no timetable at all
//     (โครงงาน/สหกิจศึกษา/วิทยานิพนธ์) — not a problem, now a warning.
//   - 8 are genuine data problems the split surfaces rather than hides: rows
//     whose Day cell reads 'S' or is blank while a real class time IS given
//     (e.g. CP020003, SC310005, CP002001, SC362102) — worth an officer's
//     attention, and previously indistinguishable from the harmless 57.
//
// This is the reference case for the split; a future change to the parser
// must not blur these two counts back together.
func TestCommitImport_RealFileSeparatesWarningsFromErrors(t *testing.T) {
	body, err := os.ReadFile("../../../docs/รายวิชาที่เปิดสอน-1-2569.xlsx")
	if err != nil {
		t.Skip("real registrar file not present:", err)
	}
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	admin := f.insertUser("admin", "admin")

	res, err := svc.CommitImport(f.ctx, admin, f.TermID, "รายวิชาที่เปิดสอน-1-2569.xlsx", body, nil, nil)
	if err != nil {
		t.Fatalf("CommitImport: %v", err)
	}

	if len(res.CreatedIDs) != 127 {
		t.Errorf("created = %d, want 127", len(res.CreatedIDs))
	}
	if res.ErrorCount != 8 {
		t.Errorf("ErrorCount = %d, want 8 (genuine unreadable-day rows) — got: %v", res.ErrorCount, res.Errors)
	}
	if res.WarningCount != 57 {
		t.Errorf("WarningCount = %d, want 57 (no-schedule courses) — got: %v", res.WarningCount, res.Warnings)
	}
	if res.WarningCount+res.ErrorCount != 65 {
		t.Errorf("WarningCount+ErrorCount = %d, want 65 (matches the pre-split total, nothing lost)",
			res.WarningCount+res.ErrorCount)
	}
	if len(res.Warnings) != res.WarningCount {
		t.Errorf("len(Warnings)=%d does not match WarningCount=%d", len(res.Warnings), res.WarningCount)
	}
	// Every reported warning must be the "no schedule" shape, never a real
	// parse failure sneaking into the wrong bucket.
	for _, w := range res.Warnings {
		if !strings.Contains(w, "ไม่มีตารางเรียน") {
			t.Errorf("warning does not read as the expected 'no schedule' shape: %q", w)
		}
	}
	// Conversely, every reported error must be a real problem — never the
	// no-schedule shape sneaking into the error bucket.
	for _, e := range res.Errors {
		if strings.Contains(e, "ไม่มีตารางเรียน") {
			t.Errorf("a 'no schedule' row leaked into Errors instead of Warnings: %q", e)
		}
	}
}
