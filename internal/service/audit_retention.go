// audit_retention.go ages audit rows out of the database, five years after the
// fact, and keeps a verifiable copy of everything it removes.
//
// Migration 0107 made audit_logs append-only, which was right for evidence and
// left two things unanswered: the table grew forever, and PDPA erasure could
// not reach it. Both are answered by an age rather than an exception — see
// migration 0108 for why the retention period lives inside the delete trigger
// rather than being enforced only here.
//
// The order is archive, verify, then delete, and never the other way round. A
// purge that deletes first and writes the file afterwards has a window in which
// the rows are gone and the archive does not exist; there is no recovery from
// that window, and it is the one moment the trail is least able to report its
// own failure.
package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// AuditPurgeResult is what one run did, for the scheduler's log and the audit
// row the purge writes about itself.
type AuditPurgeResult struct {
	Archived   int64     `json:"archived"`
	Deleted    int64     `json:"deleted"`
	CoversFrom time.Time `json:"covers_from"`
	CoversTo   time.Time `json:"covers_to"`
	StorageKey string    `json:"storage_key,omitempty"`
	SHA256     string    `json:"sha256,omitempty"`
}

// auditPurgeBatch caps one run.
//
// A first run against a table that has never been purged could otherwise try to
// load years of rows into memory at once. Bounded, the sweep simply takes
// several nights to catch up, which is the right trade for a job nobody is
// waiting on.
const auditPurgeBatch = 50_000

// PurgeExpiredAudit archives and then deletes audit rows past the retention
// period the DATABASE declares.
//
// The period is read from audit_retention() rather than kept as a Go constant:
// two copies of a policy drift, and the copy that matters is the one the delete
// trigger enforces. If this code and the trigger ever disagree, the trigger
// wins and the purge simply deletes nothing — a safe disagreement.
func (s *AuditService) PurgeExpiredAudit(ctx context.Context, aud *audit.Auditor) (*AuditPurgeResult, error) {
	if s.store == nil {
		return nil, fmt.Errorf("audit purge: no document store configured to archive into")
	}
	var cutoff time.Time
	if err := s.pool.QueryRow(ctx, `SELECT NOW() - audit_retention()`).Scan(&cutoff); err != nil {
		return nil, err
	}

	// Read the batch that is about to go. ORDER BY id so the window is
	// contiguous and two runs cannot interleave.
	rows, err := s.pool.Query(ctx, `
		SELECT id, to_jsonb(a) FROM audit_logs a
		WHERE at < $1 ORDER BY id LIMIT $2`, cutoff, auditPurgeBatch)
	if err != nil {
		return nil, err
	}
	var (
		buf     bytes.Buffer
		ids     []int64
		first   time.Time
		last    time.Time
		hasRows bool
	)
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		// JSON Lines: one row per line, appendable, and readable by anything
		// without loading the whole file.
		if _, err := buf.Write(raw); err != nil {
			rows.Close()
			return nil, err
		}
		if err := buf.WriteByte('\n'); err != nil {
			rows.Close()
			return nil, err
		}
		var at struct {
			At time.Time `json:"at"`
		}
		_ = json.Unmarshal(raw, &at)
		if !hasRows || at.At.Before(first) {
			first = at.At
		}
		if !hasRows || at.At.After(last) {
			last = at.At
		}
		hasRows = true
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !hasRows {
		return &AuditPurgeResult{}, nil
	}

	sum := sha256.Sum256(buf.Bytes())
	digest := hex.EncodeToString(sum[:])
	name := fmt.Sprintf("audit-%s-to-%s.jsonl",
		first.UTC().Format("20060102"), last.UTC().Format("20060102"))
	key, size, err := s.store.Save("audit_archives", name, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("audit purge: archive not written, nothing deleted: %w", err)
	}

	// Read the file back and check it against the digest BEFORE deleting
	// anything. "The write returned no error" is not the same as "the bytes are
	// there and readable", and the difference only ever shows up at the moment
	// somebody needs the archive — years later, with the originals gone.
	if err := s.verifyArchive(key, digest); err != nil {
		return nil, fmt.Errorf("audit purge: archive unreadable, nothing deleted: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var archiveID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO audit_archives (covers_from, covers_to, row_count, storage_key, size_bytes, sha256)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		first, last, len(ids), key, size, digest).Scan(&archiveID); err != nil {
		return nil, err
	}
	// Deleted BY ID, not by `at < cutoff`: the archive covers exactly these
	// rows, and a second WHERE clause could remove one the file does not
	// contain. The trigger refuses anything inside retention regardless, so a
	// mistake here fails loudly rather than quietly.
	tag, err := tx.Exec(ctx, `DELETE FROM audit_logs WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != int64(len(ids)) {
		return nil, fmt.Errorf("audit purge: archived %d rows but deleted %d — refusing to commit",
			len(ids), tag.RowsAffected())
	}

	res := &AuditPurgeResult{
		Archived: int64(len(ids)), Deleted: tag.RowsAffected(),
		CoversFrom: first, CoversTo: last, StorageKey: key, SHA256: digest,
	}
	// The purge records itself, in the same transaction and in the table it
	// just deleted from — which is append-only, so this row cannot later be
	// removed to hide that a purge happened. It is the trail's account of its
	// own gap.
	if err := aud.LogTx(ctx, tx, audit.Entry{
		Action: "audit_log.purge", Entity: "audit_archive", EntityID: archiveID.String(),
		Note:  fmt.Sprintf("เก็บ %d รายการลงคลังแล้วลบออกจากตาราง (นโยบายเก็บ 5 ปี)", len(ids)),
		After: res,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return res, nil
}

// verifyArchive reads the stored file back and compares it to the digest taken
// before it was written.
func (s *AuditService) verifyArchive(key, want string) error {
	rc, err := s.store.Open(key)
	if err != nil {
		return err
	}
	defer rc.Close()
	h := sha256.New()
	var back bytes.Buffer
	if _, err := back.ReadFrom(rc); err != nil {
		return err
	}
	h.Write(back.Bytes())
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("checksum %s does not match %s", got, want)
	}
	return nil
}
