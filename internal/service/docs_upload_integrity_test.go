package service

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestUpload_ApprovedDocumentCannotBeReplacedBeforeProfileApproval(t *testing.T) {
	for _, kind := range []string{"national_id", "bank_book", "creditor_form"} {
		t.Run(kind, func(t *testing.T) {
			svc, store, uid := avFixture(t, nil)
			id, err := uploadPDF(svc, uid, kind)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.pool.Exec(context.Background(), `UPDATE ta_documents SET status='approved' WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			if _, err := uploadPDF(svc, uid, kind); userErrStatus(err) != 409 {
				t.Fatalf("want 409, got %v", err)
			}
			if store.saved != 1 {
				t.Fatalf("approved file replaced: %d saves", store.saved)
			}
		})
	}
}

func TestUpload_RenamedRetryKeepsSameDocumentAndRound(t *testing.T) {
	svc, store, uid := avFixture(t, nil)
	id, err := uploadPDF(svc, uid, "national_id")
	if err != nil {
		t.Fatal(err)
	}
	body := pdfBytes()
	retry, err := svc.Upload(context.Background(), uid, "national_id", "renamed.pdf", "application/pdf", int64(len(body)), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if retry != id || store.saved != 1 {
		t.Fatalf("duplicate created: id=%s retry=%s saves=%d", id, retry, store.saved)
	}
	var count, round int
	if err := svc.pool.QueryRow(context.Background(), `SELECT COUNT(*), MAX(round) FROM ta_documents WHERE user_id=$1`, uid).Scan(&count, &round); err != nil {
		t.Fatal(err)
	}
	if count != 1 || round != 1 {
		t.Fatalf("count=%d round=%d", count, round)
	}
}

func TestUpload_ChangedFileSupersedesPendingAndStaleReviewIsRefused(t *testing.T) {
	svc, store, uid := avFixture(t, nil)
	id, err := uploadPDF(svc, uid, "national_id")
	if err != nil {
		t.Fatal(err)
	}
	body := append(pdfBytes(), []byte("\n% corrected scan\n")...)
	replacement, err := svc.Upload(context.Background(), uid, "national_id", "corrected.pdf", "application/pdf", int64(len(body)), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if replacement == id || store.saved != 2 {
		t.Fatal("changed file did not create a revision")
	}
	for _, approve := range []bool{true, false} {
		if err := svc.Review(context.Background(), uid, id, approve, "stale review"); userErrStatus(err) != 409 {
			t.Fatalf("stale review approve=%v: %v", approve, err)
		}
	}
	var supersededBy uuid.UUID
	if err := svc.pool.QueryRow(context.Background(), `SELECT superseded_by FROM ta_documents WHERE id=$1 AND superseded_at IS NOT NULL`, id).Scan(&supersededBy); err != nil {
		t.Fatal(err)
	}
	if supersededBy != replacement {
		t.Fatal("revision link is incorrect")
	}
}

type uploadGateScanner struct{ started, release chan struct{} }

func (g *uploadGateScanner) Enabled() bool { return true }
func (g *uploadGateScanner) Scan(ctx context.Context, _ io.Reader) error {
	close(g.started)
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestUpload_ApprovalDuringScanIsRecheckedBeforeStorage(t *testing.T) {
	svc, store, uid := avFixture(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := svc.pool.Exec(ctx, `INSERT INTO ta_profiles (user_id,status) VALUES ($1,'submitted')`, uid); err != nil {
		t.Fatal(err)
	}
	g := &uploadGateScanner{started: make(chan struct{}), release: make(chan struct{})}
	svc.av = g
	done := make(chan error, 1)
	go func() {
		body := pdfBytes()
		_, err := svc.Upload(ctx, uid, "national_id", "id.pdf", "application/pdf", int64(len(body)), bytes.NewReader(body))
		done <- err
	}()
	select {
	case <-g.started:
	case <-ctx.Done():
		t.Fatal("scan did not start")
	}
	_, updateErr := svc.pool.Exec(ctx, `UPDATE ta_profiles SET status='approved' WHERE user_id=$1`, uid)
	close(g.release)
	if updateErr != nil {
		t.Fatal(updateErr)
	}
	select {
	case err := <-done:
		if userErrStatus(err) != 409 {
			t.Fatalf("want conflict after concurrent approval, got %v", err)
		}
	case <-ctx.Done():
		t.Fatal("upload did not finish")
	}
	if store.saved != 0 {
		t.Fatalf("stored %d files after approval", store.saved)
	}
}

func TestUpload_ConcurrentIdenticalRetriesCreateOneRevision(t *testing.T) {
	svc, store, uid := avFixture(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type result struct {
		id  uuid.UUID
		err error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			body := pdfBytes()
			id, err := svc.Upload(ctx, uid, "bank_book", "bank.pdf", "application/pdf", int64(len(body)), bytes.NewReader(body))
			results <- result{id, err}
		}()
	}
	close(start)
	var first uuid.UUID
	for range 2 {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatal(result.err)
			}
			if first == uuid.Nil {
				first = result.id
			} else if first != result.id {
				t.Fatal("concurrent retry created a different document")
			}
		case <-ctx.Done():
			t.Fatal("concurrent upload did not finish")
		}
	}
	if store.saved != 1 {
		t.Fatalf("want one stored file, got %d", store.saved)
	}
}
