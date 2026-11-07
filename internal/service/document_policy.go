package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/audit"
)

// DocumentPolicy applies retention on completion and limits individual bundle pulls.
// Existing expiry timestamps and download history are preserved when settings change.
type DocumentPolicy struct {
	RetentionDays int `json:"retention_days"`
	DownloadLimit int `json:"download_limit"`
}

func readDocumentPolicy(ctx context.Context, q querier) (DocumentPolicy, error) {
	var p DocumentPolicy
	err := q.QueryRow(ctx, `SELECT retention_days, download_limit FROM document_policy WHERE id`).Scan(&p.RetentionDays, &p.DownloadLimit)
	return p, err
}

func (s *DocsService) DocumentPolicy(ctx context.Context) (DocumentPolicy, error) {
	return readDocumentPolicy(ctx, s.pool)
}

func (s *DocsService) UpdateDocumentPolicy(ctx context.Context, actor uuid.UUID, p DocumentPolicy) (DocumentPolicy, error) {
	if p.RetentionDays < 1 || p.RetentionDays > 3650 || p.DownloadLimit < 1 || p.DownloadLimit > 1000 {
		return p, Invalid("อายุไฟล์ต้องเป็นจำนวนเต็ม 1–3650 วัน และจำนวนดาวน์โหลดต้องเป็นจำนวนเต็ม 1–1000 ครั้ง")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return p, err
	}
	defer tx.Rollback(ctx)
	var before DocumentPolicy
	if err := tx.QueryRow(ctx, `SELECT retention_days, download_limit FROM document_policy WHERE id FOR UPDATE`).Scan(&before.RetentionDays, &before.DownloadLimit); err != nil {
		return p, err
	}
	if _, err := tx.Exec(ctx, `UPDATE document_policy SET retention_days=$1, download_limit=$2, updated_at=NOW(), updated_by=$3 WHERE id`, p.RetentionDays, p.DownloadLimit, actor); err != nil {
		return p, err
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "document_policy.update", Entity: "document_policy", EntityID: "policy", Before: before, After: p}); err != nil {
		return p, err
	}
	return p, tx.Commit(ctx)
}

// Called exactly when the complete required set is approved, in the same tx.
func startDocumentRetention(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	p, err := readDocumentPolicy(ctx, tx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE ta_documents SET expires_at=NOW() + $2 * INTERVAL '1 day'
        WHERE user_id=$1 AND superseded_at IS NULL AND status='approved'
        AND kind = ANY(ta_required_doc_kinds($1)) AND file_deleted_at IS NULL`, userID, p.RetentionDays)
	return err
}

// A rejected set no longer has a full approval; pause its current file clocks.
func pauseDocumentRetention(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE ta_documents SET expires_at=NULL
        WHERE user_id=$1 AND superseded_at IS NULL
        AND kind = ANY(ta_required_doc_kinds($1)) AND file_deleted_at IS NULL`, userID)
	return err
}
