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
	pdfcpuAPI "github.com/pdfcpu/pdfcpu/pkg/api"
	pdfcpuModel "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/timeutil"
)

// requiredDocKinds is the closed set the review flow expects: without all
// three there is nothing meaningful to approve.
var requiredDocKinds = []string{"national_id", "bank_book", "creditor_form"}

// zipTokenTTL keeps the one-shot approve→download handshake tight so a
// captured token can't be replayed hours later.
const zipTokenTTL = 60 * time.Second

// RejectItem is a single (doc, reason) entry the officer submits in a batch
// rejection. Reason must be non-empty after trim (RejectBatch below rejects
// an empty one unconditionally, unlike Review/ReviewProfile's reason which is
// only required when rejecting — every RejectBatch item IS a rejection).
type RejectItem struct {
	DocID  uuid.UUID `json:"doc_id" validate:"required"`
	Reason string    `json:"reason" validate:"required,max=500"`
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
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "ta_profile.approve_all", Entity: "ta_profile",
		EntityID: userID.String(), After: map[string]any{"docs": ids, "round": round}}); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

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
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "ta_docs.reject_batch", Entity: "ta_profile",
		EntityID: userID.String(), After: map[string]any{"batch_id": batchID, "items": items, "round": round}}); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
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

// MintAllApprovedZipToken gates the bulk download behind the officer's password
// and resolves the documents of the TAs the caller names.
//
// userIDs is REQUIRED and comes from the review screen: the people the officer
// approved in the session they are looking at. It used to be every approved TA in
// the database, which quietly swept in hand-offs from weeks ago — people who were
// not even on the screen — and produced a file the officer could not account for.
// "ที่อนุมัติ ณ ตอนนั้น" means this session's approvals, not all history.
//
// The list is not trusted, only used to narrow: each id still has to be an
// approved profile with approved, current, undeleted documents. That makes a
// tampered list unable to reach anything the officer could not already download
// one TA at a time, and ids that no longer qualify simply drop out.
//
// Deliberately NOT subject to the per-TA download quota (maxDocDownloads). A
// bulk pull would otherwise spend one of everyone's two allowances at once, and
// two clicks would lock every TA out of individual re-download for good. The
// audit entry is what keeps it accountable instead: who pulled it, when, and
// exactly which TAs were inside.
func (s *DocsService) MintAllApprovedZipToken(ctx context.Context, actor uuid.UUID, password string, userIDs []uuid.UUID) (string, int, error) {
	password = strings.TrimSpace(password)
	if password == "" {
		return "", 0, Invalid("กรุณากรอกรหัสผ่านเพื่อยืนยันการดาวน์โหลด")
	}
	// No implicit "everyone": an empty list must not fall back to the whole
	// database, which is exactly the behaviour being removed.
	if len(userIDs) == 0 {
		return "", 0, Invalid("ยังไม่มีใครในรายชื่อนี้ที่อนุมัติครบทั้ง 3 ไฟล์")
	}
	if err := s.verifyOfficerPassword(ctx, actor, password); err != nil {
		return "", 0, err
	}

	// Approved required documents, still on disk, belonging to the named TAs.
	// Ordered by student so the merged PDF reads like a stack of per-person
	// folders rather than an interleaved pile.
	rows, err := s.pool.Query(ctx, `
		SELECT d.id, d.user_id
		  FROM ta_documents d
		  JOIN ta_profiles p ON p.user_id = d.user_id AND p.status = 'approved'
		  JOIN users u ON u.id = d.user_id
		 WHERE d.kind = ANY($1)
		   AND d.user_id = ANY($2)
		   AND d.superseded_at IS NULL
		   AND d.status = 'approved'
		   AND d.file_deleted_at IS NULL
		 ORDER BY COALESCE(u.student_id, ''), u.first_name, d.kind`, requiredDocKinds, userIDs)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	var (
		ids    []uuid.UUID
		people = map[uuid.UUID]bool{}
	)
	for rows.Next() {
		var id, uid uuid.UUID
		if err := rows.Scan(&id, &uid); err != nil {
			return "", 0, err
		}
		ids = append(ids, id)
		people[uid] = true
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	if len(ids) == 0 {
		return "", 0, &UserError{Status: 404, Msg: "ยังไม่มีเอกสารที่อนุมัติแล้วให้ดาวน์โหลด"}
	}

	included := make([]string, 0, len(people))
	for uid := range people {
		included = append(included, uid.String())
	}
	if err := s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "ta_docs.download_all", Entity: "ta_profile",
		After: map[string]any{
			"ta_count":  len(people),
			"doc_count": len(ids),
			"ta_ids":    included,
			// Recorded alongside the resolved set so a later reader can see when
			// the two differ — a requested TA whose files were already purged, for
			// instance, is silently absent from the file and this is the only place
			// that says so.
			"requested_count": len(userIDs),
		},
	}); err != nil {
		return "", 0, err
	}

	// uuid.Nil for UserID marks the token as bulk: ConsumeZipToken's per-user
	// binding does not apply, so DownloadAllZip must be the only route that
	// accepts it (see the handler).
	token, err := s.mintZipToken(actor, uuid.Nil, ids)
	if err != nil {
		return "", 0, err
	}
	return token, len(people), nil
}

// CountTAsInDocs reports how many distinct TAs a doc set covers — used to name
// the bulk file. Returns 0 on error rather than failing the download: a wrong
// number in a filename is not worth denying the officer their documents over.
func (s *DocsService) CountTAsInDocs(ctx context.Context, docIDs []uuid.UUID) int {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(DISTINCT user_id) FROM ta_documents WHERE id = ANY($1)`, docIDs).Scan(&n); err != nil {
		return 0
	}
	return n
}

// verifyOfficerPassword re-authenticates the officer before a download that
// carries PII. Shared by the single-TA and the bulk paths so both are gated
// identically — the bulk file is every approved TA's national ID and bank
// account in one document, so it cannot be the laxer of the two.
//
// Timing is dominated by bcrypt, so no extra dummy compare is needed; an account
// deactivated mid-session simply has no row to match.
func (s *DocsService) verifyOfficerPassword(ctx context.Context, actor uuid.UUID, password string) error {
	return VerifyUserPassword(ctx, s.pool, actor, password)
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
	if err := s.verifyOfficerPassword(ctx, actor, password); err != nil {
		return "", err
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
	// Check the quota here as well as at the download itself. This is the copy
	// that produces a useful message: the officer is standing at the password
	// prompt and can read it. The one at download time is the enforcement that
	// cannot be raced; hitting that one is already the unusual path.
	used, err := s.docDownloadsUsed(ctx, userID)
	if err != nil {
		return "", err
	}
	if used >= maxDocDownloads {
		return "", quotaExceeded(used)
	}
	if err := s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "ta_docs.redownload_verify", Entity: "ta_profile",
		EntityID: userID.String(), After: map[string]any{"doc_count": len(ids), "downloads_used": used}}); err != nil {
		return "", err
	}
	return s.mintZipToken(actor, userID, ids)
}

// maxDocDownloads caps how many times one TA's approved bundle may be
// downloaded. The bundle holds a national ID, a bank account number and a
// signature; the password gate proves who is pulling it, this limits how many
// copies exist at all.
//
// Counted per TA for good, NOT per submission round. Approval means all three
// documents passed, and an approved TA never resubmits — so a round can only
// change if someone rejects a document on purpose. Counting per round would make
// that the loophole: reject one file, have it resubmitted and re-approved, and
// two more copies are available. There is no way to hand a spent allowance back.
//
// `round` is still recorded on each row (which document set was pulled), it just
// does not divide the allowance. See migrations 0051 and 0053.
const maxDocDownloads = 2

// docDownloadsUsed reports how many downloads of this TA's documents have been
// taken, across every round.
//
// Bulk (all-approved) pulls are recorded in the same table but excluded here:
// they are exempt from the allowance by design (see MintAllApprovedZipToken), and
// counting them would let two bulk clicks lock every TA out of individual
// re-download for good.
func (s *DocsService) docDownloadsUsed(ctx context.Context, userID uuid.UUID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ta_doc_downloads
		  WHERE user_id = $1 AND counts_toward_quota`, userID).Scan(&n)
	return n, err
}

// currentDocRound resolves the round the TA's current (non-superseded) approved
// documents belong to, which is what the quota is counted against. Falls back to
// the profile's round, then to 1, so a set predating rounds still gets a bucket.
func (s *DocsService) currentDocRound(ctx context.Context, userID uuid.UUID) (int, error) {
	var round *int
	if err := s.pool.QueryRow(ctx, `
		SELECT MAX(round) FROM ta_documents
		 WHERE user_id = $1 AND superseded_at IS NULL AND kind = ANY($2)`,
		userID, requiredDocKinds).Scan(&round); err != nil {
		return 0, err
	}
	if round != nil {
		return *round, nil
	}
	var profileRound *int
	if err := s.pool.QueryRow(ctx,
		`SELECT current_round FROM ta_profiles WHERE user_id = $1`, userID).Scan(&profileRound); err != nil &&
		!errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	if profileRound != nil {
		return *profileRound, nil
	}
	return 1, nil
}

// quotaExceeded is the shared refusal, so the message the officer sees is the
// same whether they hit the limit at the password prompt or at the download.
func quotaExceeded(used int) error {
	return &UserError{
		Status: 403,
		Msg: fmt.Sprintf(
			"ดาวน์โหลดครบ %d ครั้งแล้ว (จำกัด %d ครั้งต่อ TA หนึ่งคน) ไม่สามารถดาวน์โหลดเพิ่มได้อีก โปรดใช้ไฟล์ที่ดาวน์โหลดไปแล้ว",
			used, maxDocDownloads),
	}
}

// recordDocDownload claims one download from the TA's allowance and returns the
// refusal if none is left.
//
// The count and the insert share a transaction guarded by an advisory lock, so
// two officers clicking at the same moment cannot both read "1 used" and both
// write — a plain check-then-insert under READ COMMITTED allows exactly that,
// and the whole point of a limit is that it cannot be raced past. The lock is
// keyed on the TA, so it never blocks work on anyone else.
func (s *DocsService) recordDocDownload(ctx context.Context, actor, userID uuid.UUID, round int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1))`, userID.String()+"/doc-download"); err != nil {
		return err
	}
	var used int
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM ta_doc_downloads
		  WHERE user_id = $1 AND counts_toward_quota`, userID).Scan(&used); err != nil {
		return err
	}
	if used >= maxDocDownloads {
		return quotaExceeded(used)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ta_doc_downloads (user_id, round, actor_id, counts_toward_quota)
		 VALUES ($1,$2,$3,TRUE)`,
		userID, round, actor); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RecordBulkDownload logs one row per TA covered by an all-approved download,
// resolving the TA set from the documents that were actually served.
//
// No quota check and no advisory lock: these rows do not spend the allowance, so
// there is no counter to race on. What they DO establish is that this TA's
// documents have been handed off — which is what the "อย่าลืมดาวน์โหลด" reminder
// asks, and what it previously could not see, because only quota-consuming pulls
// were written here.
//
// One statement rather than a loop: a bulk pull can cover every approved TA, and
// a partial write would leave some of them still marked as never downloaded.
func (s *DocsService) RecordBulkDownload(ctx context.Context, actor uuid.UUID, docIDs []uuid.UUID) error {
	if len(docIDs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO ta_doc_downloads (user_id, round, actor_id, counts_toward_quota)
		SELECT d.user_id, COALESCE(MAX(d.round), 1), $2, FALSE
		  FROM ta_documents d
		 WHERE d.id = ANY($1)
		 GROUP BY d.user_id`, docIDs, actor)
	return err
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

// ClaimZipDownload spends one of the TA's allowed downloads. Called by the
// download handler once the bytes are ready, so a build that fails costs the
// officer nothing, and returns the quota refusal when none is left.
func (s *DocsService) ClaimZipDownload(ctx context.Context, actor, userID uuid.UUID) error {
	round, err := s.currentDocRound(ctx, userID)
	if err != nil {
		return err
	}
	if err := s.recordDocDownload(ctx, actor, userID, round); err != nil {
		return err
	}
	if err := s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "ta_docs.download", Entity: "ta_profile",
		EntityID: userID.String(), After: map[string]any{"round": round}}); err != nil {
		return err
	}
	return nil
}

// BuildDocsZip loads each doc's storage payload and packs the originals
// (unwatermarked — this is the audit copy). Filenames inside the zip carry
// the kind prefix so a later re-download stays intelligible.
// mergePDFs concatenates PDFs into one document.
//
// pdfcpu PANICS on some malformed input rather than returning an error (its
// own fault.Catch re-panics), and this runs on a request the officer triggered
// by clicking approve. A panic here would 500 the approval they just completed,
// so the recover converts it into the ZIP fallback instead.
func mergePDFs(readers []io.ReadSeeker) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("pdfcpu panicked while merging: %v", r)
		}
	}()
	conf := pdfcpuModel.NewDefaultConfiguration()
	// Officer uploads are ordinary scanner/phone output; strict validation
	// rejects PDFs that every reader opens without complaint.
	conf.ValidationMode = pdfcpuModel.ValidationRelaxed

	var buf bytes.Buffer
	if err := pdfcpuAPI.MergeRaw(readers, &buf, false, conf); err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("%PDF")) {
		return nil, errors.New("merge produced something that is not a PDF")
	}
	return buf.Bytes(), nil
}

func (s *DocsService) BuildDocsZip(ctx context.Context, docIDs []uuid.UUID) ([]byte, string, error) {
	return s.buildDocsBundle(ctx, docIDs, "")
}

// BuildAllApprovedBundle merges the bulk set. Same packing as BuildDocsZip; only
// the filename differs, because a file holding many people cannot be named after
// whichever of them happened to sort first.
// The actor is taken so the hand-off can be recorded HERE rather than in the
// handler. The recording is what stops the "อย่าลืมดาวน์โหลด" reminder nagging
// about files already saved, and leaving it to the caller made it a convention
// somebody has to remember — the kind of wiring that gets dropped in a refactor
// and produces a bug with no failing test. Building the bytes and recording that
// they were handed over now happen in one place, and only after the build
// succeeds: a bundle that failed to assemble is not a download.
func (s *DocsService) BuildAllApprovedBundle(ctx context.Context, actor uuid.UUID, docIDs []uuid.UUID, taCount int) ([]byte, string, error) {
	stem := fmt.Sprintf("เอกสารเบิกจ่าย_อนุมัติแล้ว_%dคน_%s",
		taCount, timeutil.Now().Format("20060102"))
	body, name, err := s.buildDocsBundle(ctx, docIDs, stem)
	if err != nil {
		return nil, "", err
	}
	if err := s.RecordBulkDownload(ctx, actor, docIDs); err != nil {
		return nil, "", err
	}
	return body, name, nil
}

// buildDocsBundle is the shared implementation. stemOverride names the output
// when the set is not about one person; empty means "derive from the owner".
func (s *DocsService) buildDocsBundle(ctx context.Context, docIDs []uuid.UUID, stemOverride string) ([]byte, string, error) {
	if len(docIDs) == 0 {
		return nil, "", errors.New("no docs to zip")
	}
	// Fetch metadata + owner for a suggested filename.
	rows, err := s.pool.Query(ctx, `
		SELECT d.id, d.kind, d.filename, d.storage_key, d.file_deleted_at,
		       u.first_name, u.last_name, COALESCE(u.student_id,'')
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
		studentID           string
	}
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.kind, &e.filename, &e.key, &e.deletedAt,
			&e.first, &e.last, &e.studentID); err != nil {
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

	// Read every document into memory. They are capped at 2 MB each and there
	// are three of them, so this is bounded; pdfcpu's merge needs io.ReadSeeker
	// anyway, which a store stream is not.
	type loaded struct {
		entry entry
		data  []byte
	}
	files := make([]loaded, 0, len(entries))
	for _, e := range entries {
		rc, err := s.store.Open(e.key)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, "", &UserError{Status: 410, Msg: "ไฟล์ถูกลบตามนโยบายเก็บรักษา 7 วัน"}
			}
			return nil, "", err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, "", err
		}
		files = append(files, loaded{entry: e, data: data})
	}

	// รหัสนักศึกษา_ชื่อ_นามสกุล — same convention as every other per-person
	// download (taFileStem). The download date used to be appended; it was
	// dropped because the officers name these by the student they belong to, and
	// a date made two pulls of the same person look like two different people.
	stem := stemOverride
	if stem == "" {
		stem = taFileStem(entries[0].studentID, entries[0].first, entries[0].last)
	}

	// One merged PDF is what the officers asked for: three separate files meant
	// three prints and three chances to mislay one. Uploads are PDF-only now
	// (see acceptedDocMIMEs), so in practice every set merges.
	//
	// Rows predating that rule may still be JPEG/PNG, and merging those would
	// need rasterising into pages — out of scope here. Rather than silently
	// dropping them, fall back to the ZIP for that set: a complete archive in
	// the older shape beats an incomplete book.
	allPDF := true
	for _, f := range files {
		// Checked on bytes, not on the stored MIME column, which records only
		// what the client claimed at upload time.
		if !bytes.HasPrefix(f.data, []byte("%PDF")) {
			allPDF = false
			break
		}
	}

	if allPDF {
		readers := make([]io.ReadSeeker, 0, len(files))
		for _, f := range files {
			readers = append(readers, bytes.NewReader(f.data))
		}
		if merged, err := mergePDFs(readers); err == nil {
			return merged, stem + ".pdf", nil
		} else {
			// A malformed member must not block the download entirely — fall
			// through to the ZIP, which copies bytes without parsing them.
			log.Printf("docs merge: falling back to zip for %s: %v", stem, err)
		}
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		w, err := zw.Create(f.entry.kind + filepath.Ext(f.entry.filename))
		if err != nil {
			return nil, "", err
		}
		if _, err := w.Write(f.data); err != nil {
			return nil, "", err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), stem + ".zip", nil
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
			// The blob is already gone from storage, so the row must be marked and
			// the expiry recorded together — a mark without a record leaves a
			// document that looks retained and has no file behind it.
			if err := writeAudited(ctx, s.pool, s.aud,
				audit.Entry{Action: "ta_doc.expire", Entity: "ta_document",
					EntityID: p.id.String(), Before: map[string]any{"storage_key": p.key}},
				func(tx pgx.Tx) error {
					_, err := tx.Exec(ctx,
						`UPDATE ta_documents SET file_deleted_at = NOW()
						 WHERE id = $1 AND file_deleted_at IS NULL`, p.id)
					return err
				}); err != nil {
				// This is a background sweep with no caller to return to; the next
				// pass retries the same document.
				log.Printf("retention: mark deleted %s failed: %v", p.id, err)
				continue
			}
		}
		if len(batch) < retentionBatchLimit {
			return
		}
	}
}
