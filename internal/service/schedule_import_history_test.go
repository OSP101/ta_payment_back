package service

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"

	"ta-payment-back/internal/audit"
)

// buildImportXlsxForHistoryTest builds a minimal "Normalized"-sheet .xlsx.
// Each row: {CourseCode, CourseName, Unit, Section, ReservedFor, TotalSeats,
// Day, Time, SessionType, Room} — the exact columns parseNormalizedSheet
// reads (see teaching.go).
func buildImportXlsxForHistoryTest(t *testing.T, rows [][]string) []byte {
	t.Helper()
	f := excelize.NewFile()
	if err := f.SetSheetName("Sheet1", "Normalized"); err != nil {
		t.Fatal(err)
	}
	header := []string{
		"CourseCode", "CourseName", "Unit", "Section", "ReservedFor",
		"TotalSeats", "Day", "Time", "SessionType", "Room",
	}
	if err := f.SetSheetRow("Normalized", "A1", &header); err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		cell := row // copy for SetSheetRow's pointer arg
		if err := f.SetSheetRow("Normalized", "A"+strconv.Itoa(i+2), &cell); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The registrar-import ledger (schedule_imports) has existed since 0001_init
// and CommitImport has always written to it, but only aggregate counts and a
// jsonb `summary` blob — no filename-recognisable history, no code lists, no
// message text, and critically no read path at all (no handler, no route, no
// screen). Found during the 2026-09-14 TOR acceptance review (§3.3 ข.5).
// These tests exercise the fix: the richer columns (migration
// 0115_schedule_imports_detail) and the two read methods.

func TestListImportHistory_LecturerCannotCall(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	if _, err := svc.ListImportHistory(f.ctx, f.LecturerID, nil, 50); err != ErrForbidden {
		t.Fatalf("lecturer: got %v, want ErrForbidden", err)
	}
}

func TestGetImportHistoryDetail_LecturerCannotCall(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	if _, err := svc.GetImportHistoryDetail(f.ctx, f.LecturerID, uuid.New()); err != ErrForbidden {
		t.Fatalf("lecturer: got %v, want ErrForbidden", err)
	}
}

func TestGetImportHistoryDetail_NotFound(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	admin := f.insertUser("admin", "admin")
	if _, err := svc.GetImportHistoryDetail(f.ctx, admin, uuid.New()); err != ErrNotFound {
		t.Fatalf("random id: got %v, want ErrNotFound", err)
	}
}

// A successful commit must leave a history row carrying the filename, the
// hash, every count, and — critically — the actual code lists and messages,
// not just their counts.
func TestScheduleImportHistory_RecordsASuccessfulCommit(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	admin := f.insertUser("admin", "admin")

	body := buildImportXlsxForHistoryTest(t, [][]string{
		{"ZZ900", "วิชาทดสอบประวัติ", "3(3-0-6)", "1", "", "50", "M", "09:00-10:00", "lec", "ROOM1"},
	})
	res, err := svc.CommitImport(f.ctx, admin, f.TermID, "ทดสอบ.xlsx", body, nil, nil)
	if err != nil {
		t.Fatalf("CommitImport: %v", err)
	}
	if len(res.CreatedIDs) != 1 {
		t.Fatalf("setup: created = %d, want 1", len(res.CreatedIDs))
	}

	list, err := svc.ListImportHistory(f.ctx, admin, &f.TermID, 10)
	if err != nil {
		t.Fatalf("ListImportHistory: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("history rows = %d, want 1", len(list))
	}
	row := list[0]
	if row.Filename != "ทดสอบ.xlsx" {
		t.Errorf("Filename = %q", row.Filename)
	}
	if row.FileSHA256 == nil || *row.FileSHA256 == "" {
		t.Error("FileSHA256 is empty — a re-import of the same file can't be recognised")
	}
	if row.CreatedCount != 1 {
		t.Errorf("CreatedCount = %d, want 1", row.CreatedCount)
	}
	if row.ImportedBy == nil || *row.ImportedBy != admin {
		t.Errorf("ImportedBy = %v, want %s", row.ImportedBy, admin)
	}
	if row.FatalError != nil {
		t.Errorf("FatalError = %v, want nil (this commit succeeded)", row.FatalError)
	}

	detail, err := svc.GetImportHistoryDetail(f.ctx, admin, row.ID)
	if err != nil {
		t.Fatalf("GetImportHistoryDetail: %v", err)
	}
	if len(detail.CreatedCodes) != 1 || detail.CreatedCodes[0] != "ZZ900" {
		t.Errorf("CreatedCodes = %v, want [ZZ900]", detail.CreatedCodes)
	}
}

// A file that fails to parse entirely (corrupt / unreadable) must still leave
// a trace — before this fix, such an attempt inserted NOTHING, so a staff
// member who tried and failed had no record of having tried.
func TestScheduleImportHistory_RecordsAFailedCommit(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	admin := f.insertUser("admin", "admin")

	corrupt := append([]byte("PK\x03\x04"), []byte("not a real xlsx")...)
	_, err := svc.CommitImport(f.ctx, admin, f.TermID, "เสีย.xlsx", corrupt, nil, nil)
	if err == nil {
		t.Fatal("expected CommitImport to fail on a corrupt file")
	}

	list, err := svc.ListImportHistory(f.ctx, admin, &f.TermID, 10)
	if err != nil {
		t.Fatalf("ListImportHistory: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("history rows = %d, want 1 (the failed attempt itself)", len(list))
	}
	if list[0].FatalError == nil || *list[0].FatalError == "" {
		t.Error("FatalError is empty — the failure left no explanation in the history")
	}
	if list[0].Filename != "เสีย.xlsx" {
		t.Errorf("Filename = %q", list[0].Filename)
	}
}

// Re-importing the exact same file produces a second history row with the
// SAME hash — the mechanism staff (or this screen) would use to notice
// "I already tried this file".
func TestScheduleImportHistory_SameFileSameHash(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	admin := f.insertUser("admin", "admin")

	body := buildImportXlsxForHistoryTest(t, [][]string{
		{"ZZ901", "วิชาทดสอบแฮช", "3(3-0-6)", "1", "", "50", "M", "09:00-10:00", "lec", "ROOM1"},
	})
	if _, err := svc.CommitImport(f.ctx, admin, f.TermID, "a.xlsx", body, nil, nil); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if _, err := svc.CommitImport(f.ctx, admin, f.TermID, "b.xlsx", body, nil, nil); err != nil {
		t.Fatalf("second commit (same content, different name): %v", err)
	}

	list, err := svc.ListImportHistory(f.ctx, admin, &f.TermID, 10)
	if err != nil {
		t.Fatalf("ListImportHistory: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("history rows = %d, want 2", len(list))
	}
	if list[0].FileSHA256 == nil || list[1].FileSHA256 == nil || *list[0].FileSHA256 != *list[1].FileSHA256 {
		t.Errorf("hashes differ for byte-identical uploads: %v vs %v", list[0].FileSHA256, list[1].FileSHA256)
	}
}
