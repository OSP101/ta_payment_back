package service

import (
	"testing"

	"ta-payment-back/internal/config"
)

func TestTAAccountBatchLegacyImportAndHistoryBackfill(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := NewTAAccountBatch(f.Pool, config.Config{})
	input := accountBatchInput()
	records, err := svc.Import(f.ctx, f.StaffID, "legacy-before", input, true, func([]TAAccountRecord) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	id := records[0].UserID
	var beforeHash string
	var beforeRequests, beforeNotices, beforeUsers int
	if err = f.Pool.QueryRow(f.ctx, `SELECT password_hash FROM users WHERE id=$1`, id).Scan(&beforeHash); err != nil {
		t.Fatal(err)
	}
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM ta_requests`).Scan(&beforeRequests)
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM notifications`).Scan(&beforeNotices)
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM users`).Scan(&beforeUsers)
	input[0].Seniority = "returning"
	input[0].HistorySource = "synthetic legacy history"
	preview, err := svc.MarkReturning(f.ctx, f.StaffID, "legacy", input, false)
	if err != nil || preview[0].Status != "ready_returning" {
		t.Fatalf("preview: %v", err)
	}
	var override *string
	f.Pool.QueryRow(f.ctx, `SELECT ta_seniority_override FROM users WHERE id=$1`, id).Scan(&override)
	if override != nil {
		t.Fatal("preview mutated account")
	}
	got, err := svc.MarkReturning(f.ctx, f.StaffID, "legacy", input, true)
	if err != nil || got[0].Status != "marked_returning" {
		t.Fatalf("mark: %v", err)
	}
	again, err := svc.MarkReturning(f.ctx, f.StaffID, "legacy", input, true)
	if err != nil || again[0].Status != "already_returning" {
		t.Fatalf("rerun: %v", err)
	}
	var auditCount, requests, notices, users int
	var hash string
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM audit_logs WHERE action='user.ta_history.import' AND entity_id=$1`, id.String()).Scan(&auditCount)
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM ta_requests`).Scan(&requests)
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM notifications`).Scan(&notices)
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM users`).Scan(&users)
	f.Pool.QueryRow(f.ctx, `SELECT password_hash FROM users WHERE id=$1`, id).Scan(&hash)
	if hash != beforeHash || requests != beforeRequests || notices != beforeNotices || users != beforeUsers || auditCount != 1 {
		t.Fatal("backfill changed credentials, unrelated data or duplicated audit")
	}
	conflict := append([]TAAccountInput(nil), input...)
	conflict[0].Email = "another@example.invalid"
	bad, err := svc.MarkReturning(f.ctx, f.StaffID, "legacy", conflict, true)
	if err != nil || bad[0].Status != "conflict" {
		t.Fatalf("identity mismatch: %v", err)
	}
	f.exec(`UPDATE users SET ta_seniority_override='new' WHERE id=$1`, id)
	bad, err = svc.MarkReturning(f.ctx, f.StaffID, "legacy", input, true)
	if err != nil || bad[0].Status != "conflict" {
		t.Fatalf("explicit override: %v", err)
	}
	// A newly imported legacy account receives the flag in the original transaction.
	input[0].StudentID = "693380998-1"
	input[0].Email = "new-legacy@example.invalid"
	created, err := svc.Import(f.ctx, f.StaffID, "legacy", input, true, func([]TAAccountRecord) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	f.Pool.QueryRow(f.ctx, `SELECT ta_seniority_override FROM users WHERE id=$1`, created[0].UserID).Scan(&override)
	if override == nil || *override != "returning" {
		t.Fatal("legacy import did not set returning")
	}
	missing := append([]TAAccountInput(nil), input...)
	missing[0].StudentID = "693380997-1"
	missing[0].Email = "missing@example.invalid"
	skipped, err := svc.MarkReturning(f.ctx, f.StaffID, "legacy", missing, true)
	if err != nil || skipped[0].Status != "not_found" {
		t.Fatalf("missing account: %v", err)
	}
}
