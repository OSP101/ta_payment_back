package service

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
)

// The bulk download has to be BOTH exempt from the quota and recorded as a
// download. Those pulled in opposite directions and the reminder lost: it asked
// the quota counter "has this been downloaded", got 0 after a bulk pull, and kept
// nagging about files the officer had already saved.
//
// These tests hold both halves at once, because fixing either one alone
// re-creates a bug: count bulk against the quota and two clicks lock every TA out
// of re-download; leave it unrecorded and the 7-day-purge reminder cries wolf.

// bulkFixture gives a TA with the three required documents and an officer.
func bulkFixture(t *testing.T) (*DocsService, uuid.UUID, uuid.UUID, []uuid.UUID) {
	t.Helper()
	svc, officer, ta := quotaFixture(t)
	ctx := context.Background()

	var docIDs []uuid.UUID
	for _, kind := range requiredDocKinds {
		id := uuid.New()
		if _, err := svc.pool.Exec(ctx, `
			INSERT INTO ta_documents
			  (id, user_id, kind, filename, mime, size_bytes, storage_key, status, round)
			VALUES ($1,$2,$3,$4,'application/pdf',1,$5,'approved',1)`,
			id, ta, kind, kind+".pdf", "key/"+id.String()); err != nil {
			t.Fatalf("insert doc %s: %v", kind, err)
		}
		docIDs = append(docIDs, id)
	}
	return svc, officer, ta, docIDs
}

func everDownloaded(t *testing.T, svc *DocsService, ta uuid.UUID) bool {
	t.Helper()
	var n int
	if err := svc.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM ta_doc_downloads WHERE user_id = $1`, ta).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// The bug, stated directly: after a bulk download the reminder must go quiet.
func TestBulkDownload_IsRecordedSoTheReminderClears(t *testing.T) {
	svc, officer, ta, docIDs := bulkFixture(t)
	ctx := context.Background()

	if everDownloaded(t, svc, ta) {
		t.Fatal("fixture already has a download recorded")
	}
	if err := svc.RecordBulkDownload(ctx, officer, docIDs); err != nil {
		t.Fatalf("RecordBulkDownload: %v", err)
	}
	if !everDownloaded(t, svc, ta) {
		t.Error("a bulk download left no record — the reminder would keep nagging about files already saved")
	}
}

// Serving the bundle and recording the hand-off must be the same act. Recording
// it in the handler instead left the invariant as a convention, and no test could
// see it being dropped — this one goes through the function that produces the
// bytes, which is the only route the download endpoint has.
func TestBuildAllApprovedBundle_RecordsTheHandoff(t *testing.T) {
	svc, officer, ta, docIDs := bulkFixture(t)
	ctx := context.Background()
	svc.store = newMemStore()
	page := realPDF(t)

	// Give the documents real stored bytes so the bundle can actually assemble;
	// the point of the test is the side effect, but it must only happen on success.
	for _, id := range docIDs {
		key, _, err := svc.store.Save("ta_docs", "x.pdf", bytes.NewReader(page))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.pool.Exec(ctx,
			`UPDATE ta_documents SET storage_key=$1, mime='application/pdf' WHERE id=$2`,
			key, id); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := svc.BuildAllApprovedBundle(ctx, officer, docIDs, 1); err != nil {
		t.Fatalf("BuildAllApprovedBundle: %v", err)
	}
	if !everDownloaded(t, svc, ta) {
		t.Error("the bundle was served without recording the hand-off")
	}
}

// A build that fails must not be remembered as a download, or the reminder goes
// quiet about documents the officer never received.
func TestBuildAllApprovedBundle_FailedBuildIsNotRecorded(t *testing.T) {
	svc, officer, ta, _ := bulkFixture(t)
	ctx := context.Background()

	// No docs at all — buildDocsBundle refuses before touching storage.
	if _, _, err := svc.BuildAllApprovedBundle(ctx, officer, nil, 0); err == nil {
		t.Fatal("an empty bundle must be an error")
	}
	if everDownloaded(t, svc, ta) {
		t.Error("a failed build was recorded as a download")
	}
}

// ...and the allowance must be untouched by it.
func TestBulkDownload_DoesNotSpendTheQuota(t *testing.T) {
	svc, officer, ta, docIDs := bulkFixture(t)
	ctx := context.Background()

	// More bulk pulls than the whole allowance.
	for i := 0; i < maxDocDownloads+2; i++ {
		if err := svc.RecordBulkDownload(ctx, officer, docIDs); err != nil {
			t.Fatalf("bulk download %d: %v", i+1, err)
		}
	}

	used, err := svc.docDownloadsUsed(ctx, ta)
	if err != nil {
		t.Fatal(err)
	}
	if used != 0 {
		t.Errorf("docDownloadsUsed = %d after bulk pulls, want 0 — bulk is exempt (see maxDocDownloads)", used)
	}
	// The real consequence: the individual allowance must still be spendable in
	// full. This is what breaks if bulk rows are counted.
	for i := 1; i <= maxDocDownloads; i++ {
		if err := svc.recordDocDownload(ctx, officer, ta, 1); err != nil {
			t.Fatalf("individual download %d of %d must still be allowed after bulk pulls, got %v",
				i, maxDocDownloads, err)
		}
	}
}

// An individual download must advance BOTH counters — it is a copy leaving the
// system and a hand-off.
func TestIndividualDownload_CountsAndClearsTheReminder(t *testing.T) {
	svc, officer, ta, _ := bulkFixture(t)
	ctx := context.Background()

	if err := svc.recordDocDownload(ctx, officer, ta, 1); err != nil {
		t.Fatalf("recordDocDownload: %v", err)
	}
	used, err := svc.docDownloadsUsed(ctx, ta)
	if err != nil {
		t.Fatal(err)
	}
	if used != 1 {
		t.Errorf("docDownloadsUsed = %d, want 1", used)
	}
	if !everDownloaded(t, svc, ta) {
		t.Error("an individual download must also mark the documents as handed off")
	}
}

// ListReview is what the FE reads, so the two fields have to arrive there
// distinguished — this is the seam the bug actually lived on.
func TestListReview_ReportsEverDownloadedSeparatelyFromQuota(t *testing.T) {
	svc, officer, ta, docIDs := bulkFixture(t)
	ctx := context.Background()

	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO ta_profiles (user_id, prefix, status, completed_at, current_round, verified_at)
		 VALUES ($1, 'นาย', 'approved', NOW(), 1, NOW())`, ta); err != nil {
		t.Fatal(err)
	}

	find := func() PendingProfile {
		t.Helper()
		row, ok := listIDs(t, svc, "approved")[ta]
		if !ok {
			t.Fatal("TA missing from the approved bucket")
		}
		return row
	}

	before := find()
	if before.EverDownloaded {
		t.Error("ever_downloaded is true before any download")
	}

	if err := svc.RecordBulkDownload(ctx, officer, docIDs); err != nil {
		t.Fatal(err)
	}
	after := find()
	if !after.EverDownloaded {
		t.Error("ever_downloaded must be true after a bulk download — this is what the reminder reads")
	}
	if after.DownloadsUsed != 0 {
		t.Errorf("downloads_used = %d after a bulk download, want 0 so the remaining-quota label stays honest",
			after.DownloadsUsed)
	}
}

// RecordBulkDownload resolves the TA set from the documents it was handed, so a
// pull covering several people must mark every one of them.
func TestBulkDownload_MarksEveryTAInTheBundle(t *testing.T) {
	svc, officer, ta1, docs1 := bulkFixture(t)
	ctx := context.Background()

	// A second TA in the SAME database as the first — bulkFixture would hand back
	// its own pool, and two pools cannot appear in one bundle.
	ta2 := uuid.New()
	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active)
		 VALUES ($1, $2, 'สอง', 'ทดสอบ', TRUE)`,
		ta2, "bulk2-"+ta2.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	all := append([]uuid.UUID{}, docs1...)
	for _, kind := range requiredDocKinds {
		id := uuid.New()
		if _, err := svc.pool.Exec(ctx, `
			INSERT INTO ta_documents
			  (id, user_id, kind, filename, mime, size_bytes, storage_key, status, round)
			VALUES ($1,$2,$3,$4,'application/pdf',1,$5,'approved',1)`,
			id, ta2, kind, kind+".pdf", "key/"+id.String()); err != nil {
			t.Fatal(err)
		}
		all = append(all, id)
	}

	if err := svc.RecordBulkDownload(ctx, officer, all); err != nil {
		t.Fatal(err)
	}
	for _, ta := range []uuid.UUID{ta1, ta2} {
		if !everDownloaded(t, svc, ta) {
			t.Errorf("%s was in the bundle but not recorded", ta)
		}
	}
	// One row per TA, not one per document: three files for one person is one
	// hand-off, and inflating it would make the access trail unreadable.
	var rows int
	if err := svc.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ta_doc_downloads WHERE user_id = $1`, ta1).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("recorded %d rows for one TA in one bulk pull, want 1", rows)
	}
}
