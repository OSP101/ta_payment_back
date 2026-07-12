package service

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/auth"
)

// requiredDocKinds is the closed set the review flow expects: without all
// three there is nothing meaningful to approve.
var requiredDocKinds = []string{"national_id", "bank_book", "creditor_form"}

// zipTokenTTL keeps the one-shot approve→download handshake tight so a
// captured token can't be replayed hours later.
const zipTokenTTL = 60 * time.Second

// RejectItem is a single (doc, reason) entry the officer submits in a batch
// rejection. Reason must be non-empty after trim.
type RejectItem struct {
	DocID  uuid.UUID `json:"doc_id"`
	Reason string    `json:"reason"`
}

// ApproveAllResult carries what the FE needs to trigger the auto-download:
// the doc ids we approved (audit / debug) and a short-lived opaque token the
// download.zip endpoint verifies.
type ApproveAllResult struct {
	ApprovedDocIDs []uuid.UUID `json:"approved_docs"`
	ZipToken       string      `json:"zip_token"`
}

// ApproveAll approves all three required docs + the profile in one tx and
// mints a one-shot ZIP-download token. Contract:
//   - the tx locks the three current docs FOR UPDATE, so a concurrent TA
//     re-upload can't race in between.
//   - if any of the three required kinds is missing (superseded_at IS NULL
//     count < 3), the caller sees a UserError explaining why.
//   - expires_at is set to NOW()+7d so the retention job scrubs the file
//     later without the officer having to do anything.
func (s *DocsService) ApproveAll(ctx context.Context, actor, userID uuid.UUID) (*ApproveAllResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Confirm the profile exists + is in a state we can approve. Fetch the
	// round so the immutable submission-history row updates in the same tx.
	var profStatus string
	var round int
	err = tx.QueryRow(ctx,
		`SELECT status::text, COALESCE(current_round, 1) FROM ta_profiles WHERE user_id = $1`,
		userID,
	).Scan(&profStatus, &round)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, Invalid("ไม่พบโปรไฟล์ของ TA คนนี้")
	}
	if err != nil {
		return nil, err
	}
	if profStatus == "approved" {
		return nil, Conflict("โปรไฟล์นี้ถูกอนุมัติไปแล้ว")
	}

	// Lock the three current docs. Ordering by kind keeps the lock order
	// deterministic across concurrent officers.
	rows, err := tx.Query(ctx, `
		SELECT id, kind FROM ta_documents
		WHERE user_id = $1
		  AND kind = ANY($2)
		  AND superseded_at IS NULL
		ORDER BY kind
		FOR UPDATE`, userID, requiredDocKinds)
	if err != nil {
		return nil, err
	}
	type docRow struct {
		id   uuid.UUID
		kind string
	}
	var docs []docRow
	for rows.Next() {
		var d docRow
		if err := rows.Scan(&d.id, &d.kind); err != nil {
			rows.Close()
			return nil, err
		}
		docs = append(docs, d)
	}
	rows.Close()
	if len(docs) < len(requiredDocKinds) {
		return nil, Invalid("เอกสารบังคับยังไม่ครบ (บัตรประชาชน/สมุดบัญชี/แบบฟอร์มเจ้าหนี้)")
	}

	ids := make([]uuid.UUID, 0, len(docs))
	for _, d := range docs {
		ids = append(ids, d.id)
	}

	// Approve every locked doc. Clearing expires_at first would matter only
	// if a doc had been re-approved after a purge — that path never happens
	// in the current flow, but setting NOW()+7d unconditionally is safe.
	if _, err := tx.Exec(ctx, `
		UPDATE ta_documents
		SET status='approved',
		    reject_reason=NULL,
		    reviewed_at=NOW(),
		    reviewed_by=$1,
		    expires_at=NOW() + INTERVAL '7 days'
		WHERE id = ANY($2)`,
		actor, ids); err != nil {
		return nil, err
	}

	// Flip the profile + freeze the submission snapshot for this round.
	if _, err := tx.Exec(ctx, `
		UPDATE ta_profiles
		SET status='approved', reject_reason=NULL, verified_at=NOW(), verified_by=$1
		WHERE user_id=$2`,
		actor, userID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ta_profile_submissions
		SET status='approved', reject_reason=NULL, reviewed_at=NOW(), reviewed_by=$1
		WHERE user_id=$2 AND round=$3`,
		actor, userID, round); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "ta_profile.approve_all", Entity: "ta_profile",
		EntityID: userID.String(), After: map[string]any{"docs": ids, "round": round}})

	token, err := s.mintZipToken(actor, userID, ids)
	if err != nil {
		// Approve already committed; a mint failure is a soft error the FE
		// can recover from via the re-download endpoint.
		log.Printf("mint zip token failed for user %s: %v", userID, err)
		return &ApproveAllResult{ApprovedDocIDs: ids}, nil
	}
	return &ApproveAllResult{ApprovedDocIDs: ids, ZipToken: token}, nil
}

// RejectBatch rejects a per-doc list of items in one tx and flips the
// profile into needs_fix. Reasons must be non-empty; the officer picked
// which files to reject via a UI popover.
//
// Docs are NOT superseded here — supersede happens when the TA re-uploads
// (see Upload). Keeping the rejected row current makes the reject_reason
// visible in the TA's onboarding view without extra joins.
func (s *DocsService) RejectBatch(ctx context.Context, actor, userID uuid.UUID, items []RejectItem) error {
	if len(items) == 0 {
		return Invalid("ต้องเลือกอย่างน้อยหนึ่งเอกสาร")
	}
	for i := range items {
		items[i].Reason = strings.TrimSpace(items[i].Reason)
		if items[i].Reason == "" {
			return Invalid("กรุณาระบุเหตุผลการปฏิเสธของทุกไฟล์ที่เลือก")
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var round int
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(current_round, 1) FROM ta_profiles WHERE user_id = $1`, userID,
	).Scan(&round)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invalid("ไม่พบโปรไฟล์ของ TA คนนี้")
	}
	if err != nil {
		return err
	}

	// Validate every submitted doc_id: must belong to this user, be a
	// required kind, be current (not superseded), and not yet approved.
	// A single tx-lock per doc prevents the officer racing themselves.
	batchID := uuid.New()
	reasons := make([]string, 0, len(items))
	for _, it := range items {
		var (
			ownerID    uuid.UUID
			kind, st   string
			superseded bool
		)
		err := tx.QueryRow(ctx, `
			SELECT user_id, kind, status::text, superseded_at IS NOT NULL
			FROM ta_documents WHERE id = $1 FOR UPDATE`, it.DocID,
		).Scan(&ownerID, &kind, &st, &superseded)
		if errors.Is(err, pgx.ErrNoRows) {
			return Invalid("ไม่พบเอกสารที่เลือก")
		}
		if err != nil {
			return err
		}
		if ownerID != userID {
			return Invalid("เอกสารที่เลือกไม่ใช่ของ TA คนนี้")
		}
		if !DocKinds[kind] {
			return Invalid("ชนิดเอกสารไม่ถูกต้อง")
		}
		if superseded {
			return Conflict("เอกสารบางรายการถูกอัปโหลดใหม่ กรุณารีเฟรช")
		}
		if st == "approved" {
			return Conflict("เอกสารบางรายการถูกอนุมัติไปแล้ว")
		}
		if _, err := tx.Exec(ctx, `
			UPDATE ta_documents
			SET status='rejected',
			    reject_reason=$1,
			    reviewed_at=NOW(),
			    reviewed_by=$2,
			    reject_batch_id=$3
			WHERE id=$4`,
			it.Reason, actor, batchID, it.DocID); err != nil {
			return err
		}
		reasons = append(reasons, kindLabel(kind)+": "+it.Reason)
	}

	// Compose a truncated summary on the profile so the TA lands on their
	// onboarding page and sees why they were bounced back.
	summary := strings.Join(reasons, "; ")
	if len(summary) > 500 {
		summary = summary[:500]
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ta_profiles
		SET status='needs_fix', reject_reason=$1, verified_at=NOW(), verified_by=$2
		WHERE user_id=$3`, summary, actor, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ta_profile_submissions
		SET status='needs_fix', reject_reason=$1, reviewed_at=NOW(), reviewed_by=$2
		WHERE user_id=$3 AND round=$4`, summary, actor, userID, round); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "ta_docs.reject_batch", Entity: "ta_profile",
		EntityID: userID.String(), After: map[string]any{"batch_id": batchID, "items": items, "round": round}})
	return nil
}

// kindLabel returns the Thai display label for a doc kind. Kept close to the
// review flow so a UI rename doesn't drift out of sync silently.
func kindLabel(kind string) string {
	switch kind {
	case "national_id":
		return "บัตรประชาชน"
	case "bank_book":
		return "สมุดบัญชี"
	case "creditor_form":
		return "แบบฟอร์มเจ้าหนี้"
	}
	return kind
}

// mintZipToken records a one-shot download token bound to (actor, userID,
// docIDs). The token expires after zipTokenTTL and is deleted on consume.
func (s *DocsService) mintZipToken(actor, userID uuid.UUID, docIDs []uuid.UUID) (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	s.zipTokens.Store(token, &zipTokenEntry{
		Actor:   actor,
		UserID:  userID,
		DocIDs:  append([]uuid.UUID(nil), docIDs...),
		Expires: time.Now().Add(zipTokenTTL),
	})
	return token, nil
}

// MintZipToken lets the officer re-download the ZIP later from the approved
// bucket. Fresh token every call; docs are the current non-superseded rows.
// Returns 410-style error if the physical files have all been purged.
//
// Re-download requires the officer's password because the zip contains PII
// (national ID, bank account). The initial post-approve download is a
// separate path (token minted inline in ApproveAll) and doesn't ask again,
// since the officer just proved presence by clicking approve.
func (s *DocsService) MintZipToken(ctx context.Context, actor, userID uuid.UUID, password string) (string, error) {
	password = strings.TrimSpace(password)
	if password == "" {
		return "", Invalid("กรุณากรอกรหัสผ่านเพื่อยืนยันการดาวน์โหลด")
	}
	// Verify the officer's password against the users row. Timing is
	// dominated by bcrypt so we don't need an extra DummyCompare here —
	// a missing row (deactivated mid-session) returns pgx.ErrNoRows.
	var hash string
	err := s.pool.QueryRow(ctx,
		`SELECT password_hash FROM users WHERE id = $1 AND is_active AND deleted_at IS NULL`,
		actor,
	).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", &UserError{Status: 401, Msg: "บัญชีนี้ไม่สามารถใช้งานได้"}
	}
	if err != nil {
		return "", err
	}
	if !auth.CheckPassword(hash, password) {
		return "", &UserError{Status: 401, Msg: "รหัสผ่านไม่ถูกต้อง"}
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id FROM ta_documents
		WHERE user_id = $1
		  AND kind = ANY($2)
		  AND superseded_at IS NULL
		  AND status = 'approved'
		  AND file_deleted_at IS NULL`, userID, requiredDocKinds)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return "", &UserError{Status: 410, Msg: "ไฟล์ถูกลบตามนโยบายเก็บรักษา 7 วัน"}
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "ta_docs.redownload_verify", Entity: "ta_profile",
		EntityID: userID.String(), After: map[string]any{"doc_count": len(ids)}})
	return s.mintZipToken(actor, userID, ids)
}

// ConsumeZipToken validates + consumes a token. Returns the doc set the
// caller is authorized to download. Zero DocIDs means the token is invalid,
// expired, or already used.
func (s *DocsService) ConsumeZipToken(token string, actor, userID uuid.UUID) ([]uuid.UUID, error) {
	v, ok := s.zipTokens.Load(token)
	if !ok {
		return nil, &UserError{Status: 401, Msg: "โทเคนดาวน์โหลดไม่ถูกต้องหรือถูกใช้ไปแล้ว"}
	}
	entry := v.(*zipTokenEntry)
	// Single-use: delete regardless of validation outcome.
	s.zipTokens.Delete(token)
	if time.Now().After(entry.Expires) {
		return nil, &UserError{Status: 401, Msg: "โทเคนดาวน์โหลดหมดอายุ กรุณาลองใหม่"}
	}
	if entry.Actor != actor || entry.UserID != userID {
		return nil, &UserError{Status: 403, Msg: "โทเคนไม่ตรงกับผู้ใช้"}
	}
	return entry.DocIDs, nil
}

// BuildDocsZip loads each doc's storage payload and packs the originals
// (unwatermarked — this is the audit copy). Filenames inside the zip carry
// the kind prefix so a later re-download stays intelligible.
func (s *DocsService) BuildDocsZip(ctx context.Context, docIDs []uuid.UUID) ([]byte, string, error) {
	if len(docIDs) == 0 {
		return nil, "", errors.New("no docs to zip")
	}
	// Fetch metadata + owner for a suggested filename.
	rows, err := s.pool.Query(ctx, `
		SELECT d.id, d.kind, d.filename, d.storage_key, d.file_deleted_at,
		       u.first_name, u.last_name
		FROM ta_documents d
		JOIN users u ON u.id = d.user_id
		WHERE d.id = ANY($1)
		ORDER BY d.kind`, docIDs)
	if err != nil {
		return nil, "", err
	}
	type entry struct {
		id                  uuid.UUID
		kind, filename, key string
		deletedAt           *time.Time
		first, last         string
	}
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.kind, &e.filename, &e.key, &e.deletedAt, &e.first, &e.last); err != nil {
			rows.Close()
			return nil, "", err
		}
		entries = append(entries, e)
	}
	rows.Close()
	if len(entries) == 0 {
		return nil, "", &UserError{Status: 404, Msg: "ไม่พบเอกสารที่จะดาวน์โหลด"}
	}

	// If every requested doc is already purged we can't build a meaningful
	// ZIP. Partial purge (some intact, some gone) also fails — an audit
	// copy must be complete or it isn't an audit copy.
	for _, e := range entries {
		if e.deletedAt != nil {
			return nil, "", &UserError{Status: 410, Msg: "ไฟล์ถูกลบตามนโยบายเก็บรักษา 7 วัน"}
		}
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		rc, err := s.store.Open(e.key)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, "", &UserError{Status: 410, Msg: "ไฟล์ถูกลบตามนโยบายเก็บรักษา 7 วัน"}
			}
			return nil, "", err
		}
		ext := filepath.Ext(e.filename)
		w, err := zw.Create(e.kind + ext)
		if err != nil {
			rc.Close()
			return nil, "", err
		}
		if _, err := io.Copy(w, rc); err != nil {
			rc.Close()
			return nil, "", err
		}
		rc.Close()
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}

	safe := func(s string) string {
		s = strings.ReplaceAll(s, " ", "_")
		s = strings.ReplaceAll(s, "/", "_")
		return s
	}
	name := fmt.Sprintf("%s_%s_%s.zip",
		safe(entries[0].last), safe(entries[0].first), time.Now().Format("20060102"))
	return buf.Bytes(), name, nil
}

// LookupEmail returns the given user's email — used to compose the preview
// watermark. Falls back to a short hash of the UUID if the row somehow has
// no email, so the watermark never renders blank.
func (s *DocsService) LookupEmail(ctx context.Context, userID uuid.UUID) (string, error) {
	var email string
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(email, '') FROM users WHERE id = $1`, userID).Scan(&email)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(email) == "" {
		s := userID.String()
		if len(s) > 8 {
			s = s[:8]
		}
		email = "user-" + s
	}
	return email, nil
}

// PreviewDoc is the metadata the preview handler needs to decide whether to
// serve, watermark, or return 410. Kept small so callers don't have to load
// the whole Document struct just to check flags.
type PreviewDoc struct {
	OwnerID       uuid.UUID
	Kind          string
	Filename      string
	MIME          string
	StorageKey    string
	Superseded    bool
	Round         int
	FileDeletedAt *time.Time
}

// LoadPreviewMeta returns the info the preview endpoint needs before it
// opens the file. Separated from OpenStored so the 410 check happens before
// paying the storage read cost.
func (s *DocsService) LoadPreviewMeta(ctx context.Context, docID uuid.UUID) (*PreviewDoc, error) {
	var m PreviewDoc
	err := s.pool.QueryRow(ctx, `
		SELECT user_id, kind, filename, mime, storage_key,
		       superseded_at IS NOT NULL, COALESCE(round,1), file_deleted_at
		FROM ta_documents WHERE id = $1`, docID,
	).Scan(&m.OwnerID, &m.Kind, &m.Filename, &m.MIME, &m.StorageKey,
		&m.Superseded, &m.Round, &m.FileDeletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// OpenByKey reads the raw stored bytes for a given storage key. Used by the
// preview endpoint so LoadPreviewMeta's flags can gate access before the
// storage read.
func (s *DocsService) OpenByKey(key string) (io.ReadCloser, error) {
	return s.store.Open(key)
}

// IsFileDeleted returns true when the retention job has already scrubbed the
// physical file for the given doc id. Used by the Download handler to emit
// 410 before opening a nonexistent path.
func (s *DocsService) IsFileDeleted(ctx context.Context, docID uuid.UUID) (bool, error) {
	var deletedAt *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT file_deleted_at FROM ta_documents WHERE id = $1`, docID,
	).Scan(&deletedAt)
	if err != nil {
		return false, err
	}
	return deletedAt != nil, nil
}

// RunRetention loops until ctx is cancelled, sweeping expired-but-not-yet
// -purged docs every retentionSweepInterval. Wired in main.go alongside
// the announcement scheduler.
const retentionSweepInterval = 15 * time.Minute
const retentionBatchLimit = 200

func (s *DocsService) RunRetention(ctx context.Context) {
	t := time.NewTicker(retentionSweepInterval)
	defer t.Stop()
	// Kick off an immediate sweep on boot so a restarted process doesn't
	// wait 15 min before catching up on stale work.
	s.sweepExpired(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepExpired(ctx)
		}
	}
}

// sweepExpired deletes the on-disk blob for docs whose expires_at has
// passed. The DB row is preserved (audit) and its file_deleted_at gets set.
// os.ErrNotExist is treated as success so retries after a partial failure
// are idempotent.
func (s *DocsService) sweepExpired(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		rows, err := s.pool.Query(ctx, `
			SELECT id, storage_key FROM ta_documents
			WHERE expires_at IS NOT NULL
			  AND expires_at <= NOW()
			  AND file_deleted_at IS NULL
			ORDER BY expires_at
			LIMIT $1`, retentionBatchLimit)
		if err != nil {
			log.Printf("retention: query failed: %v", err)
			return
		}
		type toPurge struct {
			id  uuid.UUID
			key string
		}
		var batch []toPurge
		for rows.Next() {
			var p toPurge
			if err := rows.Scan(&p.id, &p.key); err != nil {
				rows.Close()
				log.Printf("retention: scan failed: %v", err)
				return
			}
			batch = append(batch, p)
		}
		rows.Close()
		if len(batch) == 0 {
			return
		}
		for _, p := range batch {
			if err := s.store.Delete(p.key); err != nil && !os.IsNotExist(err) {
				// Log and skip; the next sweep will retry.
				log.Printf("retention: delete %s failed: %v", p.key, err)
				continue
			}
			if _, err := s.pool.Exec(ctx,
				`UPDATE ta_documents SET file_deleted_at = NOW()
				 WHERE id = $1 AND file_deleted_at IS NULL`, p.id); err != nil {
				log.Printf("retention: mark deleted %s failed: %v", p.id, err)
				continue
			}
			s.aud.Log(ctx, audit.Entry{Action: "ta_doc.expire", Entity: "ta_document",
				EntityID: p.id.String(), Before: map[string]any{"storage_key": p.key}})
		}
		if len(batch) < retentionBatchLimit {
			return
		}
	}
}
