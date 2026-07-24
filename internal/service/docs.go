package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/pdfgen"
	"ta-payment-back/internal/storage"
)

type DocsService struct {
	pool  *pgxpool.Pool
	aud   *audit.Auditor
	store storage.Store

	// zipTokens holds one-shot download tokens minted after approve-all so
	// the client can pull the ZIP without re-triggering the approve tx.
	zipTokens sync.Map // token(string) -> *zipTokenEntry
}

// zipTokenEntry pins a download token to a specific actor + user + doc set
// so a leaked token can't be repurposed. TTL is short (60s) and single-use.
type zipTokenEntry struct {
	Actor   uuid.UUID
	UserID  uuid.UUID
	DocIDs  []uuid.UUID
	Expires time.Time
}

// DocKinds is the closed set of supporting-document kinds a TA can submit.
// Kept as a package var so handlers/tests share the same source of truth.
var DocKinds = map[string]bool{
	"national_id":   true,
	"bank_book":     true,
	"creditor_form": true,
}

// maxDocBytes caps individual TA document uploads. Storage layer also caps via
// Fiber BodyLimit, but we double-check here so the message is meaningful. The
// UI advertises the same cap on the file picker so the reject path is rare.
const maxDocBytes int64 = 2 * 1024 * 1024

// acceptedDocMIMEs are the file types the review workflow can render inline.
// Enforced server-side so scripted / non-browser clients get the same limit.
var acceptedDocMIMEs = map[string]bool{
	"application/pdf": true,
	"image/jpeg":      true,
	"image/jpg":       true,
	"image/png":       true,
}

// sniffAllowedDoc reports whether head (the leading bytes of an upload) begins
// with a PDF, JPEG, or PNG magic-byte signature. We verify the real bytes
// rather than trusting the client-supplied Content-Type / filename extension,
// both of which the client fully controls.
func sniffAllowedDoc(head []byte) bool {
	switch {
	case bytes.HasPrefix(head, []byte("%PDF")):
		return true
	case bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}): // JPEG
		return true
	case bytes.HasPrefix(head, []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}): // PNG
		return true
	}
	return false
}

// filenameExt returns the lowercase extension after the last "." (no dot).
func filenameExt(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 || i == len(name)-1 {
		return ""
	}
	return strings.ToLower(name[i+1:])
}

type TAProfile struct {
	StudentID       string  `json:"student_id"`
	Prefix          string  `json:"prefix"`
	Phone           string  `json:"phone"`
	NationalID      string  `json:"national_id"`
	BankName        string  `json:"bank_name"`
	BankBranch      string  `json:"bank_branch"`
	BranchCode      string  `json:"branch_code"`
	AccountNo       string  `json:"account_no"`
	AccountName     string  `json:"account_name"`
	SignatureSVG    string  `json:"signature_svg"`
	SignaturePNGB64 string  `json:"signature_png_b64"`
	Status          string  `json:"status"`
	CurrentRound    int     `json:"current_round"`
	RejectReason    *string `json:"reject_reason,omitempty"`
}

// AllowedPrefixes are the exact strings the PDF overlay knows how to circle.
// Any value outside this set is rejected on submit so the generator never has
// to guess coordinates for an unknown prefix.
var AllowedPrefixes = map[string]bool{
	"นาย":    true,
	"นาง":    true,
	"นางสาว": true,
}

func (s *DocsService) UpsertProfile(ctx context.Context, userID uuid.UUID, in TAProfile) error {
	// PDPA: national ID must be 13 digits, numeric.
	nid := stripNonDigits(in.NationalID)
	if len(nid) != 13 {
		return errors.New("national_id must be 13 digits")
	}
	in.NationalID = nid

	sid := strings.TrimSpace(in.StudentID)
	if !studentIDPattern(sid) {
		return errors.New("student_id must be in the form XXXXXXXXX-X (11 chars incl. dash)")
	}
	in.StudentID = sid
	if !AllowedPrefixes[in.Prefix] {
		return errors.New("prefix must be one of นาย, นาง, or นางสาว")
	}
	// Phone goes onto the creditor form, so require a dialable TH number:
	// 9 digits (landline) or 10 (mobile).
	phone := stripNonDigits(in.Phone)
	if len(phone) != 9 && len(phone) != 10 {
		return errors.New("phone must be 9-10 digits")
	}
	in.Phone = phone
	if err := validateBank(in); err != nil {
		return err
	}
	if strings.TrimSpace(in.SignatureSVG) == "" {
		return errors.New("signature is required")
	}
	// Signature payloads are user-controlled and otherwise unbounded; cap both
	// the SVG path data and the rasterized PNG so a client can't wedge the row.
	if len(in.SignatureSVG) > 300000 || len(in.SignaturePNGB64) > 300000 {
		return Invalid("ลายเซ็นมีขนาดใหญ่เกินไป")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`UPDATE users SET student_id = $2, phone = $3, updated_at = NOW() WHERE id = $1`,
		userID, sid, phone); err != nil {
		return err
	}

	// Determine the effective round: if the current profile is approved we
	// refuse to overwrite (should not happen — FE hides the form). Otherwise
	// if it exists and is 'rejected' or 'needs_fix' we bump the round.
	var prevStatus string
	var prevRound int
	err = tx.QueryRow(ctx,
		`SELECT status::text, current_round FROM ta_profiles WHERE user_id = $1`, userID,
	).Scan(&prevStatus, &prevRound)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		prevStatus = ""
		prevRound = 0
	case err != nil:
		return err
	}
	if prevStatus == "approved" {
		return errors.New("profile already approved; contact staff to reopen")
	}
	round := prevRound
	if round == 0 {
		round = 1
	} else if prevStatus == "rejected" || prevStatus == "needs_fix" {
		round = prevRound + 1
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO ta_profiles (user_id, prefix, national_id, bank_name, bank_branch, branch_code, account_no, account_name, signature_svg, signature_png_b64, status, completed_at, current_round, reject_reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'submitted', NOW(), $11, NULL)
		ON CONFLICT (user_id) DO UPDATE SET
		  prefix=EXCLUDED.prefix,
		  national_id=EXCLUDED.national_id, bank_name=EXCLUDED.bank_name, bank_branch=EXCLUDED.bank_branch,
		  branch_code=EXCLUDED.branch_code, account_no=EXCLUDED.account_no, account_name=EXCLUDED.account_name,
		  signature_svg=EXCLUDED.signature_svg, signature_png_b64=EXCLUDED.signature_png_b64,
		  status = 'submitted',
		  completed_at = NOW(),
		  current_round = EXCLUDED.current_round,
		  reject_reason = NULL`,
		userID, in.Prefix, in.NationalID, in.BankName, in.BankBranch, in.BranchCode, in.AccountNo, in.AccountName, in.SignatureSVG, in.SignaturePNGB64, round); err != nil {
		return err
	}

	// Insert (or upsert) the immutable snapshot for this round. The row is
	// unique by (user_id, round) so re-saving the same round overwrites the
	// draft snapshot rather than creating dupes.
	if _, err := tx.Exec(ctx, `
		INSERT INTO ta_profile_submissions
		  (user_id, round, prefix, national_id, bank_name, bank_branch, branch_code, account_no, account_name, signature_svg, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'submitted')
		ON CONFLICT (user_id, round) DO UPDATE SET
		  prefix=EXCLUDED.prefix,
		  national_id=EXCLUDED.national_id, bank_name=EXCLUDED.bank_name, bank_branch=EXCLUDED.bank_branch,
		  branch_code=EXCLUDED.branch_code, account_no=EXCLUDED.account_no, account_name=EXCLUDED.account_name,
		  signature_svg=EXCLUDED.signature_svg, status='submitted', submitted_at=NOW(),
		  reviewed_at=NULL, reviewed_by=NULL, reject_reason=NULL`,
		userID, round, in.Prefix, in.NationalID, in.BankName, in.BankBranch, in.BranchCode, in.AccountNo, in.AccountName, in.SignatureSVG,
	); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &userID, Action: "ta_profile.submit", Entity: "ta_profile",
		EntityID: userID.String(), After: map[string]any{"round": round}})
	return nil
}

func (s *DocsService) GetProfile(ctx context.Context, userID uuid.UUID) (*TAProfile, error) {
	p := &TAProfile{}
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(u.student_id,''), COALESCE(p.prefix,''), COALESCE(u.phone,''),
		        COALESCE(p.national_id,''), COALESCE(p.bank_name,''), COALESCE(p.bank_branch,''),
		        COALESCE(p.branch_code,''), COALESCE(p.account_no,''), COALESCE(p.account_name,''),
		        COALESCE(p.signature_svg,''), COALESCE(p.signature_png_b64,''),
		        p.status::text, p.reject_reason, COALESCE(p.current_round, 1)
		 FROM ta_profiles p JOIN users u ON u.id = p.user_id
		 WHERE p.user_id = $1`, userID).Scan(
		&p.StudentID, &p.Prefix, &p.Phone,
		&p.NationalID, &p.BankName, &p.BankBranch, &p.BranchCode, &p.AccountNo, &p.AccountName,
		&p.SignatureSVG, &p.SignaturePNGB64, &p.Status, &p.RejectReason, &p.CurrentRound)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Upload stores a supporting document and marks any previous non-superseded
// row of the same kind for that user as superseded. Round increments each
// time the TA replaces a rejected doc so history shows submission attempts.
func (s *DocsService) Upload(ctx context.Context, userID uuid.UUID, kind, filename, mime string, size int64, r io.Reader) (uuid.UUID, error) {
	if !DocKinds[kind] {
		return uuid.Nil, errors.New("invalid document kind")
	}
	if size > maxDocBytes {
		return uuid.Nil, errors.New("file too large (max 2 MB)")
	}
	// Reject unsupported file types up front — same rule the UI enforces.
	// Trust MIME when the client sets a known one, else fall back to the
	// filename extension so browsers that omit type still work.
	if mime != "" && !acceptedDocMIMEs[mime] {
		return uuid.Nil, errors.New("unsupported file type: only PDF, JPG, or PNG are accepted")
	}
	if mime == "" {
		switch filenameExt(filename) {
		case "pdf", "jpg", "jpeg", "png":
			// ok
		default:
			return uuid.Nil, errors.New("unsupported file type: only PDF, JPG, or PNG are accepted")
		}
	}

	// Content sniffing: the MIME/extension checks above are advisory only since
	// the client controls both. Read the leading bytes and confirm the file is
	// genuinely a PDF/JPEG/PNG, then reattach the consumed prefix so the full
	// file still reaches storage.
	head := make([]byte, 512)
	n, err := io.ReadFull(r, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return uuid.Nil, err
	}
	head = head[:n]
	if !sniffAllowedDoc(head) {
		return uuid.Nil, Invalid("ชนิดไฟล์ไม่ถูกต้อง รองรับเฉพาะ PDF, JPG, PNG")
	}
	r = io.MultiReader(bytes.NewReader(head), r)

	// Save first so we can abort cleanly on DB failure.
	key, savedSize, err := s.store.Save("ta_docs", filename, r)
	if err != nil {
		return uuid.Nil, err
	}
	if size == 0 {
		size = savedSize
	}
	if size > maxDocBytes {
		_ = s.store.Delete(key)
		return uuid.Nil, errors.New("file too large (max 2 MB)")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		_ = s.store.Delete(key)
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)

	// Find current round (if any) for this (user, kind).
	var prevID uuid.UUID
	var prevRound int
	err = tx.QueryRow(ctx, `
		SELECT id, round FROM ta_documents
		WHERE user_id = $1 AND kind = $2 AND superseded_at IS NULL`,
		userID, kind,
	).Scan(&prevID, &prevRound)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		prevRound = 0
	case err != nil:
		_ = s.store.Delete(key)
		return uuid.Nil, err
	}

	newRound := 1
	if prevRound > 0 {
		newRound = prevRound + 1
	}

	// Three-step supersede so we satisfy both constraints in the schema:
	//   1) Free the partial unique index (user_id, kind) WHERE superseded_at IS NULL
	//      by marking the old row's superseded_at first (superseded_by still NULL,
	//      so the FK check is trivially satisfied).
	//   2) INSERT the new row.
	//   3) Point superseded_by at the new row — now the FK target exists.
	// Doing this in a single UPDATE + INSERT pair fails with FK 23503 because
	// the new id doesn't exist yet when the UPDATE runs.
	id := uuid.New()
	if prevRound > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE ta_documents SET superseded_at = NOW() WHERE id = $1`,
			prevID); err != nil {
			_ = s.store.Delete(key)
			return uuid.Nil, err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO ta_documents (id, user_id, kind, filename, mime, size_bytes, storage_key, status, round)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'submitted',$8)`,
		id, userID, kind, filename, mime, size, key, newRound); err != nil {
		_ = s.store.Delete(key)
		return uuid.Nil, err
	}
	if prevRound > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE ta_documents SET superseded_by = $2 WHERE id = $1`,
			prevID, id); err != nil {
			_ = s.store.Delete(key)
			return uuid.Nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		_ = s.store.Delete(key)
		return uuid.Nil, err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &userID, Action: "ta_doc.upload", Entity: "ta_document",
		EntityID: id.String(), After: map[string]any{"kind": kind, "filename": filename, "round": newRound}})
	return id, nil
}

type Document struct {
	ID            uuid.UUID  `json:"id"`
	Kind          string     `json:"kind"`
	Filename      string     `json:"filename"`
	MIME          string     `json:"mime"`
	Size          int64      `json:"size_bytes"`
	Status        string     `json:"status"`
	Round         int        `json:"round"`
	UploadedAt    *time.Time `json:"uploaded_at,omitempty"`
	ReviewedAt    *time.Time `json:"reviewed_at,omitempty"`
	Superseded    bool       `json:"superseded"`
	RejectReason  *string    `json:"reject_reason,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	FileDeletedAt *time.Time `json:"file_deleted_at,omitempty"`
	StorageKey    string     `json:"-"`
}

// ListForUser returns only current (non-superseded) documents for a user.
// This is what the TA sees on their onboarding page.
func (s *DocsService) ListForUser(ctx context.Context, userID uuid.UUID) ([]Document, error) {
	return s.queryDocs(ctx,
		`SELECT id, kind, filename, mime, size_bytes, status::text, COALESCE(round,1),
		        uploaded_at, reviewed_at, superseded_at IS NOT NULL, reject_reason,
		        expires_at, file_deleted_at, storage_key
		 FROM ta_documents
		 WHERE user_id = $1 AND superseded_at IS NULL
		 ORDER BY uploaded_at DESC`, userID)
}

// ListAllForUser returns every doc the user has ever uploaded, current and
// superseded, newest first. Used by staff review to inspect history.
func (s *DocsService) ListAllForUser(ctx context.Context, userID uuid.UUID) ([]Document, error) {
	return s.queryDocs(ctx,
		`SELECT id, kind, filename, mime, size_bytes, status::text, COALESCE(round,1),
		        uploaded_at, reviewed_at, superseded_at IS NOT NULL, reject_reason,
		        expires_at, file_deleted_at, storage_key
		 FROM ta_documents
		 WHERE user_id = $1
		 ORDER BY uploaded_at DESC`, userID)
}

func (s *DocsService) queryDocs(ctx context.Context, sql string, args ...any) ([]Document, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Document{}
	for rows.Next() {
		var d Document
		var uploadedAt, reviewedAt, expiresAt, fileDeletedAt *time.Time
		if err := rows.Scan(&d.ID, &d.Kind, &d.Filename, &d.MIME, &d.Size, &d.Status, &d.Round,
			&uploadedAt, &reviewedAt, &d.Superseded, &d.RejectReason,
			&expiresAt, &fileDeletedAt, &d.StorageKey); err != nil {
			return nil, err
		}
		d.UploadedAt = uploadedAt
		d.ReviewedAt = reviewedAt
		d.ExpiresAt = expiresAt
		d.FileDeletedAt = fileDeletedAt
		out = append(out, d)
	}
	return out, nil
}

// OpenStored fetches the file bytes plus the owner user_id so callers can
// enforce access control.
func (s *DocsService) OpenStored(ctx context.Context, id uuid.UUID) (rc io.ReadCloser, filename, mime string, ownerID uuid.UUID, err error) {
	var key string
	err = s.pool.QueryRow(ctx,
		`SELECT storage_key, filename, mime, user_id FROM ta_documents WHERE id = $1`, id,
	).Scan(&key, &filename, &mime, &ownerID)
	if err != nil {
		return nil, "", "", uuid.Nil, err
	}
	rc, err = s.store.Open(key)
	return rc, filename, mime, ownerID, err
}

func (s *DocsService) Review(ctx context.Context, actor, docID uuid.UUID, approve bool, reason string) error {
	reason = strings.TrimSpace(reason)
	if !approve && reason == "" {
		return errors.New("reject reason required")
	}
	status := "approved"
	if !approve {
		status = "rejected"
	}
	// Guard: cannot review a superseded doc — that would silently rewrite
	// history and break the audit trail.
	var superseded bool
	if err := s.pool.QueryRow(ctx,
		`SELECT superseded_at IS NOT NULL FROM ta_documents WHERE id = $1`, docID,
	).Scan(&superseded); err != nil {
		return err
	}
	if superseded {
		return errors.New("cannot review a superseded document")
	}

	if !approve {
		_, err := s.pool.Exec(ctx,
			`UPDATE ta_documents SET status=$1::doc_status, reject_reason=$2, reviewed_at=NOW(), reviewed_by=$3 WHERE id=$4`,
			status, reason, actor, docID)
		if err == nil {
			s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "ta_doc.review", Entity: "ta_document",
				EntityID: docID.String(), After: map[string]any{"status": status, "reason": reason}})
		}
		return err
	}
	// Approve — clear any stale reject_reason.
	_, err := s.pool.Exec(ctx,
		`UPDATE ta_documents SET status='approved', reject_reason=NULL, reviewed_at=NOW(), reviewed_by=$1 WHERE id=$2`,
		actor, docID)
	if err == nil {
		s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "ta_doc.review", Entity: "ta_document",
			EntityID: docID.String(), After: map[string]any{"status": status}})
	}
	return err
}

func (s *DocsService) ReviewProfile(ctx context.Context, actor, userID uuid.UUID, approve bool, reason string) error {
	reason = strings.TrimSpace(reason)
	if !approve && reason == "" {
		return errors.New("reject reason required")
	}
	status := "approved"
	if !approve {
		status = "rejected"
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
		return errors.New("profile not found")
	}
	if err != nil {
		return err
	}

	// RULE C6: staff may not approve a profile until every mandatory supporting
	// document is present AND has cleared review. Require a current (not
	// superseded) row of each kind sitting at status='approved'.
	if approve {
		var approvedRequired int
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(DISTINCT kind) FROM ta_documents
			WHERE user_id = $1
			  AND kind IN ('national_id','bank_book','creditor_form')
			  AND superseded_at IS NULL
			  AND status = 'approved'`, userID,
		).Scan(&approvedRequired); err != nil {
			return err
		}
		if approvedRequired < 3 {
			return Invalid("ไม่สามารถอนุมัติได้: เอกสารบังคับยังไม่ครบหรือยังไม่ผ่านการตรวจ (บัตรประชาชน/สมุดบัญชี/แบบฟอร์มเจ้าหนี้)")
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE ta_profiles SET status=$1::doc_status, reject_reason=$2, verified_at=NOW(), verified_by=$3
		 WHERE user_id=$4`,
		status, nilStrPtr(reason), actor, userID); err != nil {
		return err
	}
	// Freeze the outcome on the submission row too so history stays honest.
	if _, err := tx.Exec(ctx,
		`UPDATE ta_profile_submissions
		 SET status=$1::doc_status, reject_reason=$2, reviewed_at=NOW(), reviewed_by=$3
		 WHERE user_id=$4 AND round=$5`,
		status, nilStrPtr(reason), actor, userID, round); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "ta_profile.review", Entity: "ta_profile",
		EntityID: userID.String(), After: map[string]any{"status": status, "reason": reason, "round": round}})
	return nil
}

// Profiles for staff to review. `bucket` selects a tab-style filter:
//   "pending"  (default) → submitted + needs_fix, oldest completion first
//   "approved"           → approved, most recently verified first
//   "rejected"           → rejected, most recently verified first
type PendingProfile struct {
	UserID       uuid.UUID `json:"user_id"`
	FullName     string    `json:"full_name"`
	Email        string    `json:"email"`
	Status       string    `json:"status"`
	Round        int       `json:"round"`
	SubmittedAt  *string   `json:"submitted_at,omitempty"`
	VerifiedAt   *string   `json:"verified_at,omitempty"`
	// EarliestExpiresAt is the soonest expires_at among the TA's current
	// approved required docs. Only meaningful for the approved bucket —
	// null in pending/rejected. The FE uses it to show "จะลบใน N วัน".
	EarliestExpiresAt *string `json:"earliest_expires_at,omitempty"`
	// AllFilesDeleted is true when the retention job has purged every one
	// of the current approved docs. FE hides the re-download button and
	// shows an "ถูกลบแล้ว" hint in that case.
	AllFilesDeleted bool `json:"all_files_deleted,omitempty"`
}

func (s *DocsService) ListReview(ctx context.Context, bucket string) ([]PendingProfile, error) {
	var where, order string
	switch bucket {
	case "approved":
		where = "p.status = 'approved'"
		order = "p.verified_at DESC NULLS LAST"
	case "rejected":
		where = "p.status = 'rejected'"
		order = "p.verified_at DESC NULLS LAST"
	default: // pending
		where = "p.status IN ('submitted','needs_fix')"
		order = "p.completed_at NULLS LAST"
	}
	// LEFT JOIN a subquery on ta_documents so we can surface the retention
	// clock next to each approved row without an extra roundtrip. The
	// subquery aggregates min(expires_at) + a boolean for "everything purged"
	// across just the three required kinds on their current (non-superseded)
	// rows.
	rows, err := s.pool.Query(ctx, `
		SELECT p.user_id, u.first_name || ' ' || u.last_name, u.email, p.status::text,
		       COALESCE(p.current_round, 1), p.completed_at, p.verified_at,
		       d.earliest_expires_at, COALESCE(d.all_deleted, FALSE)
		FROM ta_profiles p
		JOIN users u ON u.id = p.user_id
		LEFT JOIN LATERAL (
			SELECT MIN(expires_at) AS earliest_expires_at,
			       BOOL_AND(file_deleted_at IS NOT NULL) AS all_deleted
			FROM ta_documents
			WHERE user_id = p.user_id
			  AND kind IN ('national_id','bank_book','creditor_form')
			  AND superseded_at IS NULL
			  AND status = 'approved'
		) d ON TRUE
		WHERE `+where+` ORDER BY `+order)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingProfile{}
	for rows.Next() {
		var p PendingProfile
		var completedAt, verifiedAt, earliestExp *time.Time
		if err := rows.Scan(&p.UserID, &p.FullName, &p.Email, &p.Status, &p.Round,
			&completedAt, &verifiedAt, &earliestExp, &p.AllFilesDeleted); err != nil {
			return nil, err
		}
		if completedAt != nil {
			s := completedAt.Format(time.RFC3339)
			p.SubmittedAt = &s
		}
		if verifiedAt != nil {
			s := verifiedAt.Format(time.RFC3339)
			p.VerifiedAt = &s
		}
		if earliestExp != nil {
			s := earliestExp.Format(time.RFC3339)
			p.EarliestExpiresAt = &s
		}
		out = append(out, p)
	}
	return out, nil
}

// ProfileSubmission is one immutable snapshot of a profile submission round.
type ProfileSubmission struct {
	ID           uuid.UUID `json:"id"`
	Round        int       `json:"round"`
	Prefix       string    `json:"prefix"`
	NationalID   string    `json:"national_id"`
	BankName     string    `json:"bank_name"`
	BankBranch   string    `json:"bank_branch"`
	BranchCode   string    `json:"branch_code"`
	AccountNo    string    `json:"account_no"`
	AccountName  string    `json:"account_name"`
	Status       string    `json:"status"`
	SubmittedAt  time.Time `json:"submitted_at"`
	ReviewedAt   *time.Time `json:"reviewed_at,omitempty"`
	RejectReason *string   `json:"reject_reason,omitempty"`
}

// History bundles all past profile submissions and every uploaded document
// (current + superseded) for a user. Consumed by the staff review UI to
// display full submission history even after final approval.
type History struct {
	Profile     *TAProfile          `json:"profile"`
	Submissions []ProfileSubmission `json:"submissions"`
	Documents   []Document          `json:"documents"`
}

func (s *DocsService) GetHistory(ctx context.Context, userID uuid.UUID) (*History, error) {
	h := &History{}
	prof, err := s.GetProfile(ctx, userID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	h.Profile = prof

	rows, err := s.pool.Query(ctx, `
		SELECT id, round, COALESCE(prefix,''),
		       COALESCE(national_id,''), COALESCE(bank_name,''),
		       COALESCE(bank_branch,''), COALESCE(branch_code,''),
		       COALESCE(account_no,''), COALESCE(account_name,''),
		       status::text, submitted_at, reviewed_at, reject_reason
		FROM ta_profile_submissions
		WHERE user_id = $1
		ORDER BY round DESC`, userID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sub ProfileSubmission
		if err := rows.Scan(&sub.ID, &sub.Round, &sub.Prefix,
			&sub.NationalID, &sub.BankName, &sub.BankBranch,
			&sub.BranchCode, &sub.AccountNo, &sub.AccountName, &sub.Status,
			&sub.SubmittedAt, &sub.ReviewedAt, &sub.RejectReason); err != nil {
			rows.Close()
			return nil, err
		}
		h.Submissions = append(h.Submissions, sub)
	}
	rows.Close()

	docs, err := s.ListAllForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	h.Documents = docs
	return h, nil
}

// BuildCreditorFormPDF generates the filled creditor-form PDF for a TA by
// overlaying data from ta_profiles + users onto a blank PDF template.
//
// Returns (pdfBytes, filenameSuggestion). Callers stream this either as a
// preview response or hand it to storage.Save when the TA confirms.
//
// grid=true draws a debug coordinate grid on top — used during calibration to
// tune the hard-coded field positions in internal/pdfgen. Never expose this
// flag on a public route.
func (s *DocsService) BuildCreditorFormPDF(ctx context.Context, userID uuid.UUID, templatePath, fontDir string, grid bool) ([]byte, string, error) {
	var fn, ln, phone, email string
	if err := s.pool.QueryRow(ctx,
		`SELECT first_name, last_name, COALESCE(phone,''), email FROM users WHERE id=$1`, userID,
	).Scan(&fn, &ln, &phone, &email); err != nil {
		return nil, "", err
	}
	p, err := s.GetProfile(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	var sig []byte
	if p.SignaturePNGB64 != "" {
		// Accept both raw base64 and data URLs like "data:image/png;base64,..."
		b64 := p.SignaturePNGB64
		if i := strings.Index(b64, ","); i >= 0 && strings.Contains(b64[:i], "base64") {
			b64 = b64[i+1:]
		}
		if b, err := base64.StdEncoding.DecodeString(b64); err == nil {
			sig = b
		}
	}
	if _, err := os.Stat(templatePath); err != nil {
		return nil, "", errors.New("creditor form template not found at " + templatePath)
	}
	body, err := pdfgen.FillCreditor(pdfgen.CreditorInput{
		TemplatePath: templatePath,
		FontDir:      fontDir,
		Debug:        grid,
		Data: pdfgen.CreditorData{
			Prefix:       p.Prefix,
			FullName:     fn + " " + ln,
			NationalID:   p.NationalID,
			Phone:        phone,
			Email:        email,
			AccountName:  p.AccountName,
			BankName:     p.BankName,
			BranchCode:   p.BranchCode,
			Branch:       p.BankBranch,
			AccountNo:    p.AccountNo,
			SignaturePNG: sig,
		},
	})
	if err != nil {
		return nil, "", err
	}
	safe := strings.ReplaceAll(fn+"_"+ln, " ", "_")
	return body, "creditor_form_" + safe + ".pdf", nil
}

// AttachGeneratedCreditorForm generates the creditor-form PDF, stores it, and
// upserts a ta_documents row of kind=creditor_form (superseding any previous
// creditor_form doc for that user in the same way as a manual upload). Called
// when the TA clicks "ยืนยัน" on the preview.
func (s *DocsService) AttachGeneratedCreditorForm(ctx context.Context, userID uuid.UUID, templatePath, fontDir string) (uuid.UUID, error) {
	body, filename, err := s.BuildCreditorFormPDF(ctx, userID, templatePath, fontDir, false)
	if err != nil {
		return uuid.Nil, err
	}
	docID, err := s.Upload(ctx, userID, "creditor_form", filename, "application/pdf", int64(len(body)), bytes.NewReader(body))
	if err != nil {
		return uuid.Nil, err
	}
	// PDPA: the national ID is only needed to render the creditor-form PDF.
	// Now that the PDF exists, scrub the number from the database (the live
	// profile + the immutable submission snapshots) so it is never retained at
	// rest. Regenerating the form later requires the TA to re-enter it.
	_, _ = s.pool.Exec(ctx, `UPDATE ta_profiles SET national_id = NULL WHERE user_id = $1`, userID)
	_, _ = s.pool.Exec(ctx, `UPDATE ta_profile_submissions SET national_id = NULL WHERE user_id = $1`, userID)
	return docID, nil
}

func validateBank(p TAProfile) error {
	name := strings.TrimSpace(p.BankName)
	if name == "" {
		return errors.New("bank_name is required")
	}
	bank := lookupBank(name)
	if bank == nil {
		return errors.New("bank_name is not in the accepted list")
	}
	if strings.TrimSpace(p.AccountName) == "" {
		return errors.New("account_name is required")
	}
	acct := stripNonDigits(p.AccountNo)
	if !acceptsAccountLen(bank, len(acct)) {
		return errors.New("account_no length does not match the selected bank")
	}
	return nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// stripNonDigits removes anything that isn't 0-9. Used before length-checking
// bank account numbers and national IDs so users can freely include dashes
// or spaces while typing.
func stripNonDigits(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// studentIDPattern accepts KKU student IDs of the form XXXXXXXXX-X: nine
// digits, a single dash, then a check digit (11 chars total).
func studentIDPattern(s string) bool {
	if len(s) != 11 || s[9] != '-' {
		return false
	}
	for i, c := range s {
		if i == 9 {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func nilStrPtr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
