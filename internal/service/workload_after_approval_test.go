package service

import (
	"strings"
	"testing"

	"ta-payment-back/internal/audit"
)

// 01/10/2026 — a TA editing their own timetable after a request was approved.
//
// Before: adding a class on the wrong day silently trimmed approved sessions,
// and removing that mistaken class was refused (every class was frozen once
// anything was approved), so the TA was stuck until staff stepped in.

var keepFixtureClass = ClassBlock{CourseCode: "ZZ000", Kind: "lecture", DayOfWeek: 0, StartTime: "07:00", EndTime: "08:00"}

// The fixture's approved section meets Monday 09:00–12:00 (lecture). A new own
// class over it must be confirmed, with the lost session named.
func TestSaveOwnClasses_AsksBeforeTrimmingApprovedSessions(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := &WorkloadService{pool: f.Pool, aud: audit.New(f.Pool)}
	blocks := []ClassBlock{keepFixtureClass,
		{CourseCode: "NEW101", Kind: "lecture", DayOfWeek: 1, StartTime: "10:00", EndTime: "11:00"}}

	err := svc.SaveOwnClasses(f.ctx, f.TAID, f.TermID, blocks, false)
	if userErrStatus(err) != ClassSaveNeedsConfirm {
		t.Fatalf("a save that trims approved sessions must ask first (428), got %v", err)
	}
	if !strings.Contains(err.Error(), "Sec 01") || !strings.Contains(err.Error(), "09:00–12:00") {
		t.Errorf("the preview must name the session that would be lost, got: %v", err)
	}
	var n int
	_ = f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM ta_class_schedules WHERE user_id=$1 AND course_code='NEW101'`, f.TAID).Scan(&n)
	if n != 0 {
		t.Fatal("an unconfirmed save must not write anything")
	}
	if err := svc.SaveOwnClasses(f.ctx, f.TAID, f.TermID, blocks, true); err != nil {
		t.Fatalf("a confirmed save must go through: %v", err)
	}
	// A save that costs nothing needs no confirmation.
	if err := svc.SaveOwnClasses(f.ctx, f.TAID, f.TermID, append(blocks,
		ClassBlock{CourseCode: "NEW202", Kind: "lecture", DayOfWeek: 3, StartTime: "13:00", EndTime: "15:00"}), false); err != nil {
		t.Fatalf("a harmless save must not ask: %v", err)
	}
}

func TestSaveOwnClasses_ClassAddedAfterApprovalCanBeRemoved(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := &WorkloadService{pool: f.Pool, aud: audit.New(f.Pool)}
	mistaken := ClassBlock{CourseCode: "OOPS1", Kind: "lecture", DayOfWeek: 2, StartTime: "13:00", EndTime: "15:00"}
	if err := svc.SaveOwnClasses(f.ctx, f.TAID, f.TermID, []ClassBlock{keepFixtureClass, mistaken}, true); err != nil {
		t.Fatal(err)
	}
	// An unrelated autosave in between must not turn the mistake into a
	// protected class (the table is replaced wholesale on every save).
	if err := svc.SaveOwnClasses(f.ctx, f.TAID, f.TermID, []ClassBlock{keepFixtureClass, mistaken}, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.SaveOwnClasses(f.ctx, f.TAID, f.TermID, []ClassBlock{keepFixtureClass}, false); err != nil {
		t.Fatalf("a class added after approval must be removable by the TA: %v", err)
	}
	// The class that existed at approval time stays protected, even after a
	// save that re-sent it under a fresh client id.
	if err := svc.SaveOwnClasses(f.ctx, f.TAID, f.TermID, []ClassBlock{{
		ID: "b-123", CourseCode: "ZZ000", Kind: "lecture", DayOfWeek: 0, StartTime: "07:00", EndTime: "08:00"}}, false); err != nil {
		t.Fatal(err)
	}
	if err := svc.SaveOwnClasses(f.ctx, f.TAID, f.TermID, nil, false); err == nil {
		t.Fatal("a class that existed when the request was approved must still be protected")
	}
}

func TestReplaceClasses_RefusesOverlappingOwnClasses(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := &WorkloadService{pool: f.Pool, aud: audit.New(f.Pool)}
	err := svc.SaveOwnClasses(f.ctx, f.TAID, f.TermID, []ClassBlock{keepFixtureClass,
		{CourseCode: "AA111", Kind: "lecture", DayOfWeek: 5, StartTime: "15:00", EndTime: "16:00"},
		{CourseCode: "BB222", Kind: "lecture", DayOfWeek: 5, StartTime: "15:30", EndTime: "17:00"},
	}, true)
	if err == nil || !strings.Contains(err.Error(), "ซ้อน") {
		t.Fatalf("two different classes at the same time must be refused, got %v", err)
	}
	// Sections of one course meeting together stay allowed.
	if err := svc.SaveOwnClasses(f.ctx, f.TAID, f.TermID, []ClassBlock{keepFixtureClass,
		{CourseCode: "AA111", Kind: "lecture", SecNo: "1", DayOfWeek: 5, StartTime: "15:00", EndTime: "16:00"},
		{CourseCode: "AA111", Kind: "lab", SecNo: "1", DayOfWeek: 5, StartTime: "15:30", EndTime: "17:00"},
	}, true); err != nil {
		t.Fatalf("same-course blocks may overlap: %v", err)
	}
}
