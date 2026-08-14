package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/antivirus"
	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/pdfgen"
	"ta-payment-back/internal/pii"
	"ta-payment-back/internal/storage"
)

type DocsService struct {
	pool  *pgxpool.Pool
	aud   *audit.Auditor
	store storage.Store
	// av scans uploads before they are stored. Never nil — antivirus.Disabled
	// stands in when no clamd address is configured, so the upload path can ask
	// Enabled() instead of nil-checking.
	av antivirus.Scanner
	// pii encrypts the one field migration 0047 now deliberately makes an
	// exception for — the citizen ID (see citizen_id.go). May be nil in tests
	// that never call a citizen-ID path; every such path guards against that
	// explicitly rather than risk a silent no-op.
	pii *pii.Cipher

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

// maxDocBytes caps individual TA document uploads. Fiber's BodyLimit is a
// coarser backstop (32 MB for the whole request); this is the per-file rule and
// the one that produces a message a TA can act on. The UI advertises the same
// cap on the file picker so the reject path is rare.
//
// Raised from 2 MB: a phone-scanned ID copy routinely exceeds 2 MB, and TAs were
// being told to shrink the file with no practical way to do it on a phone.
//
// NOTE: config.MaxUploadMB (env MAX_UPLOAD_MB) does NOT control this — it is
// declared and never read anywhere. Setting that variable changes nothing; this
// constant is the limit.
const maxDocBytes int64 = 10 * 1024 * 1024

// maxDocMBLabel is the number the error messages quote. Derived from the
// constant rather than written out: the limit was previously spelled out by hand
// in every refusal, so moving it meant finding each one.
var maxDocMBLabel = strconv.FormatInt(maxDocBytes/(1024*1024), 10)

// errDocTooLarge is the single refusal for an oversized upload.
func errDocTooLarge() error {
	return Invalid("ไฟล์ใหญ่เกิน " + maxDocMBLabel + " MB กรุณาลดขนาดไฟล์แล้วอัปโหลดใหม่")
}

// acceptedDocMIMEs is the file type NEW uploads may use. PDF only, per the
// 24/07/2026 meeting.
//
// Images used to be accepted, and plenty of existing rows are JPEG/PNG — those
// stay readable and reviewable forever. The restriction applies at the door,
// for two reasons the meeting cared about:
//   - the officer download has to become a single merged PDF, which cannot be
//     assembled reliably from a pile of phone photos;
//   - a photo of a document is what a misfiled upload usually looks like, and
//     requiring PDF removes the commonest way to get it wrong.
var acceptedDocMIMEs = map[string]bool{
	"application/pdf": true,
}

// sniffAllowedDoc reports whether head (the leading bytes of an upload) begins
// with a PDF magic-byte signature. We verify the real bytes rather than
// trusting the client-supplied Content-Type / filename extension, both of
// which the client fully controls.
func sniffAllowedDoc(head []byte) bool {
	return bytes.HasPrefix(head, []byte("%PDF"))
}

// taFileStem builds the filename (without extension) for anything downloaded
// about one TA: `รหัสนักศึกษา_ชื่อ_นามสกุล`, e.g. `653020123-4_สมชาย_ใจดี`.
//
// Student ID leads because that is the key the finance office files and
// searches by; the name follows so a human can still recognise the file at a
// glance. Every per-person download shares this helper so the officer's folder
// sorts cleanly instead of mixing `creditor_form_ชื่อ` with `นามสกุล_ชื่อ_วันที่`.
//
// The ID is omitted rather than left blank when the user has none on record —
// a leading underscore reads like a broken export. Names keep their Thai
// characters; only separators that would confuse a filesystem are replaced.
func taFileStem(studentID, firstName, lastName string) string {
	parts := make([]string, 0, 3)
	for _, p := range []string{studentID, firstName, lastName} {
		if p = sanitizeFilenamePart(p); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "ta_document"
	}
	return strings.Join(parts, "_")
}

// sanitizeFilenamePart strips whitespace and the characters Windows, macOS and
// ZIP entry names variously reject, so a name can never escape its directory
// or produce an unopenable download.
func sanitizeFilenamePart(s string) string {
	s = strings.TrimSpace(s)
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return '_'
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '-'
		}
		return r
	}, s)
}

// filenameExt returns the lowercase extension after the last "." (no dot).
func filenameExt(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 || i == len(name)-1 {
		return ""
	}
	return strings.ToLower(name[i+1:])
}

// TAProfile is bound from the client on three routes (UpsertProfile,
// CreditorFormPDF preview, ConfirmCreditorForm) and all three funnel through
// validateProfileInput below before anything is written or rendered — that
// function normalises (strips dashes/spaces) THEN checks exact length/format,
// so the validate tags here deliberately stay looser than the service check:
// they exist to reject an obviously-missing field before the DB/PDF work,
// not to re-derive the normalized format rule (which would risk rejecting a
// validly-formatted-but-not-yet-stripped value, e.g. a dashed national ID,
// before the service gets a chance to normalise it). This is doubly true for
// NationalID: see internal/service/citizen_id.go and internal/pii — it is
// PII that gets encrypted downstream, so the tag here is intentionally just
// "present", not a checksum/format re-implementation.
type TAProfile struct {
	// StudentID/Phone are TrimSpace'd/digit-stripped then pattern-checked by
	// validateProfileInput; required here only guards "missing entirely".
	StudentID string `json:"student_id" validate:"required"`
	Prefix    string `json:"prefix" validate:"required,oneof=นาย นาง นางสาว"`
	Phone     string `json:"phone" validate:"required"`
	// Sensitive inputs. These arrive in the request, are rendered onto the
	// creditor-form PDF, and are never written to any table (migration 0047).
	// GetProfile always returns them empty.
	NationalID string `json:"national_id" validate:"required"`
	BankName   string `json:"bank_name" validate:"required,max=200"`
	BankBranch string `json:"bank_branch" validate:"omitempty,max=200"`
	BranchCode string `json:"branch_code" validate:"omitempty,max=50"`
	// AccountNo is digit-stripped then length-checked against the selected
	// bank by validateBank; required here only guards "missing entirely".
	AccountNo       string  `json:"account_no" validate:"required"`
	AccountName     string  `json:"account_name" validate:"required,max=200"`
	SignatureSVG    string  `json:"signature_svg" validate:"required,max=300000"`
	SignaturePNGB64 string  `json:"signature_png_b64" validate:"omitempty,max=300000"`
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

// validateProfileInput checks and normalises a submitted form in place. It is
// shared by the preview, the confirm, and the profile submit so all three agree
// on what a complete form is — none of them can rely on a stored copy to fall
// back on any more.
func validateProfileInput(in *TAProfile) error {
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
	if err := validateBank(*in); err != nil {
		return err
	}
	if strings.TrimSpace(in.SignatureSVG) == "" {
		return errors.New("signature is required")
	}
	// Signature payloads are user-controlled and otherwise unbounded; cap both
	// the SVG path data and the rasterized PNG so a client can't wedge a request.
	if len(in.SignatureSVG) > 300000 || len(in.SignaturePNGB64) > 300000 {
		return Invalid("ลายเซ็นมีขนาดใหญ่เกินไป")
	}
	return nil
}

// UpsertProfile records that the TA submitted a complete form. It writes
// workflow state only — status, round, timestamps — plus the student id and
// phone, which live on `users` and are not part of the PDPA-sensitive set.
//
// Nothing sensitive is persisted: the national ID, bank details and signature
// travel to BuildCreditorFormPDF in the same request and end up in the PDF.
func (s *DocsService) UpsertProfile(ctx context.Context, userID uuid.UUID, in TAProfile) error {
	if err := validateProfileInput(&in); err != nil {
		return err
	}
	sid, phone := in.StudentID, in.Phone

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
		INSERT INTO ta_profiles (user_id, prefix, status, completed_at, current_round, reject_reason)
		VALUES ($1,$2,'submitted', NOW(), $3, NULL)
		ON CONFLICT (user_id) DO UPDATE SET
		  prefix = EXCLUDED.prefix,
		  status = 'submitted',
		  completed_at = NOW(),
		  current_round = EXCLUDED.current_round,
		  reject_reason = NULL`,
		userID, in.Prefix, round); err != nil {
		return err
	}

	// Encrypted, unlike everything else validateProfileInput checked — see
	// migration 0076 and internal/pii for why the citizen ID alone is kept.
	if err := s.storeCitizenID(ctx, tx, userID, in.NationalID); err != nil {
		return err
	}

	// Insert (or upsert) the immutable snapshot for this round. The row is
	// unique by (user_id, round) so re-saving the same round overwrites the
	// draft snapshot rather than creating dupes.
	// Per-round audit trail: when it was sent, what staff decided, why it was
	// sent back. The values themselves are deliberately not snapshotted.
	if _, err := tx.Exec(ctx, `
		INSERT INTO ta_profile_submissions (user_id, round, prefix, status)
		VALUES ($1,$2,$3,'submitted')
		ON CONFLICT (user_id, round) DO UPDATE SET
		  prefix=EXCLUDED.prefix, status='submitted', submitted_at=NOW(),
		  reviewed_at=NULL, reviewed_by=NULL, reject_reason=NULL`,
		userID, round, in.Prefix,
	); err != nil {
		return err
	}

	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &userID, Action: "ta_profile.submit", Entity: "ta_profile",
		EntityID: userID.String(), After: map[string]any{"round": round}}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

// GetProfile returns the TA's payment profile, pre-filled from what the system
// already knows about them.
//
// The query starts FROM users with a LEFT JOIN, not from ta_profiles: a TA who
// has never opened this form has no ta_profiles row at all, and the previous
// inner join made that case an error, which the handler turned into a null
// body. The result was a first-time TA retyping their student id, phone and
// name — data staff had already entered — which is exactly what the
// 24/07/2026 meeting asked to stop.
//
// Only fields the system can know are seeded. Bank details and the national ID
// are left blank because guessing them would be worse than an empty box.
func (s *DocsService) GetProfile(ctx context.Context, userID uuid.UUID) (*TAProfile, error) {
	p := &TAProfile{}
	var userTitle, firstName, lastName string
	err := s.pool.QueryRow(ctx,
		// Only non-sensitive prefill. Bank details and the signature are not
		// stored anywhere (migration 0047), so the form comes back blank for
		// those and the TA re-enters them whenever a new creditor form has to
		// be produced. The national ID IS stored now (migration 0076), but
		// encrypted and deliberately NOT selected here — see citizen_id.go's
		// RevealCitizenID for the one function allowed to read it back.
		`SELECT COALESCE(u.student_id,''), COALESCE(p.prefix,''), COALESCE(u.phone,''),
		        COALESCE(p.status::text, 'pending'), p.reject_reason, COALESCE(p.current_round, 1),
		        COALESCE(u.title,''), COALESCE(u.first_name,''), COALESCE(u.last_name,'')
		 FROM users u
		 LEFT JOIN ta_profiles p ON p.user_id = u.id
		 WHERE u.id = $1`, userID).Scan(
		&p.StudentID, &p.Prefix, &p.Phone,
		&p.Status, &p.RejectReason, &p.CurrentRound,
		&userTitle, &firstName, &lastName)
	if err != nil {
		return nil, err
	}

	// users.title carries academic titles too ("ผศ. ดร."), which the creditor
	// form's prefix circles cannot render — seed only when it is one of the
	// three the form accepts.
	if p.Prefix == "" && AllowedPrefixes[userTitle] {
		p.Prefix = userTitle
	}
	// The bank account name is the person's own name in almost every case.
	// Seeding it saves the most-retyped field; it stays editable because the
	// passbook is the authority when they disagree.
	if p.AccountName == "" {
		p.AccountName = strings.TrimSpace(strings.Join(
			[]string{p.Prefix, firstName, lastName}, " "))
		p.AccountName = strings.Join(strings.Fields(p.AccountName), " ")
	}
	return p, nil
}

// scanUpload runs the virus scan and decides what a failure means.
//
// FAIL-CLOSED, on purpose. There are three outcomes and they are NOT two:
//
//   - clean            → allow
//   - signature found  → reject, and record it (the uploader is told nothing
//     about the signature; naming it only helps an attacker
//     iterate)
//   - scan unavailable → REJECT. This is the decision worth being explicit
//     about: treating "the scanner is down" as "the file is fine" turns the
//     control off exactly when it is least observable, and an attacker who can
//     reach clamd can then also disable it. An upload outage is visible and
//     recoverable; a silently unscanned file is neither.
//
// When no scanner is configured at all (antivirus.Disabled), uploads proceed —
// otherwise no deployment could ever run without clamd. That gap is logged at
// startup rather than here, so it is stated once and loudly instead of once per
// upload.
func (s *DocsService) scanUpload(ctx context.Context, userID uuid.UUID, kind, filename string, body []byte) error {
	if s.av == nil || !s.av.Enabled() {
		return nil
	}
	err := s.av.Scan(ctx, bytes.NewReader(body))
	if err == nil {
		return nil
	}

	var infected *antivirus.ErrInfected
	if errors.As(err, &infected) {
		if err := s.aud.Log(ctx, audit.Entry{
			ActorID: &userID, Action: "ta_doc.upload_infected", Entity: "ta_document",
			After: map[string]any{
				"kind": kind, "filename": filename,
				"signature": infected.Signature, "size_bytes": len(body),
			},
		}); err != nil {
			return err
		}
		return &UserError{
			Status: 422,
			Msg:    "ไฟล์นี้ตรวจพบความเสี่ยงด้านความปลอดภัย จึงไม่รับอัปโหลด กรุณาสแกนไวรัสในเครื่องแล้วสร้างไฟล์ PDF ใหม่",
		}
	}

	// Scan could not complete. Log the reason for whoever has to fix clamd, and
	// refuse — see the fail-closed note above.
	log.Printf("antivirus: scan failed for %s upload by %s: %v", kind, userID, err)
	if err := s.aud.Log(ctx, audit.Entry{
		ActorID: &userID, Action: "ta_doc.upload_scan_failed", Entity: "ta_document",
		After: map[string]any{"kind": kind, "filename": filename, "error": err.Error()},
	}); err != nil {
		return err
	}
	return &UserError{
		Status: 503,
		Msg:    "ระบบตรวจไวรัสไม่พร้อมใช้งานชั่วคราว จึงยังไม่รับอัปโหลด กรุณาลองใหม่อีกครั้ง หากยังไม่ได้โปรดแจ้งผู้ดูแลระบบ",
	}
}

// Upload stores a supporting document and marks any previous non-superseded
// row of the same kind for that user as superseded. Round increments each
// time the TA replaces a rejected doc so history shows submission attempts.
func (s *DocsService) Upload(ctx context.Context, userID uuid.UUID, kind, filename, mime string, size int64, r io.Reader) (uuid.UUID, error) {
	if !DocKinds[kind] {
		return uuid.Nil, errors.New("invalid document kind")
	}
	if size > maxDocBytes {
		return uuid.Nil, errDocTooLarge()
	}
	// Reject unsupported file types up front — same rule the UI enforces.
	// Trust MIME when the client sets a known one, else fall back to the
	// filename extension so browsers that omit type still work.
	if mime != "" && !acceptedDocMIMEs[mime] {
		return uuid.Nil, Invalid("รองรับเฉพาะไฟล์ PDF เท่านั้น หากถ่ายรูปมา กรุณาแปลงเป็น PDF ก่อนอัปโหลด")
	}
	if mime == "" && filenameExt(filename) != "pdf" {
		return uuid.Nil, Invalid("รองรับเฉพาะไฟล์ PDF เท่านั้น หากถ่ายรูปมา กรุณาแปลงเป็น PDF ก่อนอัปโหลด")
	}

	// Read the whole file into memory before anything touches storage.
	//
	// The previous version streamed straight to disk and then checked the size
	// again afterwards, deleting the file if it turned out too big — the client
	// supplies Content-Length, so that second check was load-bearing. Buffering
	// removes the write-then-delete dance, and it is what makes scanning
	// possible at all: the scanner and the store both need the same bytes, and a
	// stream can only be read once.
	//
	// Bounded by construction: LimitReader stops one byte past the cap, so a
	// client lying about its length cannot make this allocate without limit.
	buf, err := io.ReadAll(io.LimitReader(r, maxDocBytes+1))
	if err != nil {
		return uuid.Nil, err
	}
	if int64(len(buf)) > maxDocBytes {
		return uuid.Nil, errDocTooLarge()
	}
	if len(buf) == 0 {
		return uuid.Nil, Invalid("ไฟล์ว่าง กรุณาเลือกไฟล์ใหม่")
	}

	// Content sniffing: the MIME/extension checks above are advisory only since
	// the client controls both. Confirm the file is genuinely a PDF from its
	// leading bytes.
	if !sniffAllowedDoc(buf) {
		return uuid.Nil, Invalid("ไฟล์นี้ไม่ใช่ PDF จริง (ตรวจจากเนื้อไฟล์ ไม่ใช่นามสกุล) กรุณาแปลงเป็น PDF ก่อนอัปโหลด")
	}

	// Virus scan BEFORE storage, so an infected file never lands on disk at all
	// — quarantining afterwards would mean the malicious bytes were written,
	// indexed, and briefly downloadable.
	if err := s.scanUpload(ctx, userID, kind, filename, buf); err != nil {
		return uuid.Nil, err
	}

	key, savedSize, err := s.store.Save("ta_docs", filename, bytes.NewReader(buf))
	if err != nil {
		return uuid.Nil, err
	}
	size = savedSize

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
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &userID, Action: "ta_doc.upload", Entity: "ta_document",
		EntityID: id.String(), After: map[string]any{"kind": kind, "filename": filename, "round": newRound}}); err != nil {
		// The blob is already in object storage but the row will roll back, so
		// it has to go too — same cleanup every other failure below the upload
		// performs. Without it a failed audit leaks an unreferenced file.
		_ = s.store.Delete(key)
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		_ = s.store.Delete(key)
		return uuid.Nil, err
	}
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
		// A transaction for a single UPDATE, so the rejection and the record of
		// it land together — the approve branch below already works that way,
		// and a document whose rejection is missing from the trail is exactly
		// the case a TA would dispute.
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx,
			`UPDATE ta_documents SET status=$1::doc_status, reject_reason=$2, reviewed_at=NOW(), reviewed_by=$3 WHERE id=$4`,
			status, reason, actor, docID); err != nil {
			return err
		}
		if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "ta_doc.review", Entity: "ta_document",
			EntityID: docID.String(), After: map[string]any{"status": status, "reason": reason}}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	// Approve. Three things happen together, so they share a transaction: the
	// document flips, its 7-day retention clock starts, and — if this was the
	// last of the three required documents — the profile itself is approved.
	//
	// That last part used to be a separate click ("อนุมัติและดาวน์โหลดเอกสาร
	// ทั้งหมด"). Approving all three documents and then not being approved was a
	// state with no meaning: the officer had already made every judgement the
	// approval represents. Now the third approval finalises the profile, and the
	// download is its own action.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var userID uuid.UUID
	if err := tx.QueryRow(ctx, `
		UPDATE ta_documents
		   SET status='approved', reject_reason=NULL, reviewed_at=NOW(), reviewed_by=$1,
		       expires_at=NOW() + INTERVAL '7 days'
		 WHERE id=$2
		 RETURNING user_id`,
		actor, docID).Scan(&userID); err != nil {
		return err
	}

	profileApproved, round, err := s.finalizeProfileIfComplete(ctx, tx, actor, userID)
	if err != nil {
		return err
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "ta_doc.review", Entity: "ta_document",
		EntityID: docID.String(), After: map[string]any{"status": status}}); err != nil {
		return err
	}
	// The auto-approval happened inside finalizeProfileIfComplete, on this same
	// transaction — so its record belongs here too, not after the commit.
	if profileApproved {
		if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "ta_profile.auto_approved", Entity: "ta_profile",
			EntityID: userID.String(),
			After:    map[string]any{"round": round, "trigger_doc": docID.String()}}); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

// finalizeProfileIfComplete approves the TA's profile when all three required
// documents are approved, and reports whether it did.
//
// Runs inside the caller's transaction with the profile row locked, so two
// officers approving the last two documents at the same moment cannot both
// decide they were the one who completed the set and write the approval twice.
//
// Returns (false, round, nil) — not an error — when the set is incomplete or the
// profile is already approved. Both are ordinary outcomes of approving a
// document, not failures.
func (s *DocsService) finalizeProfileIfComplete(
	ctx context.Context, tx pgx.Tx, actor, userID uuid.UUID,
) (bool, int, error) {
	var status string
	var round int
	err := tx.QueryRow(ctx,
		`SELECT status::text, COALESCE(current_round, 1) FROM ta_profiles
		  WHERE user_id = $1 FOR UPDATE`, userID).Scan(&status, &round)
	if errors.Is(err, pgx.ErrNoRows) {
		// A document with no profile row: nothing to finalise, and not this
		// function's problem to complain about.
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if status == "approved" {
		return false, round, nil
	}

	var approved int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM ta_documents
		 WHERE user_id = $1 AND kind = ANY($2)
		   AND superseded_at IS NULL AND status = 'approved'`,
		userID, requiredDocKinds).Scan(&approved); err != nil {
		return false, 0, err
	}
	if approved < len(requiredDocKinds) {
		return false, round, nil
	}

	if _, err := tx.Exec(ctx, `
		UPDATE ta_profiles
		   SET status='approved', reject_reason=NULL, verified_at=NOW(), verified_by=$1
		 WHERE user_id=$2`, actor, userID); err != nil {
		return false, 0, err
	}
	// Freeze the submission snapshot for this round, exactly as the old
	// approve-all click did — the history row is what staff read back later.
	if _, err := tx.Exec(ctx, `
		UPDATE ta_profile_submissions
		   SET status='approved', reject_reason=NULL, reviewed_at=NOW(), reviewed_by=$1
		 WHERE user_id=$2 AND round=$3`, actor, userID, round); err != nil {
		return false, 0, err
	}
	return true, round, nil
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

	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "ta_profile.review", Entity: "ta_profile",
		EntityID: userID.String(), After: map[string]any{"status": status, "reason": reason, "round": round}}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

// Profiles for staff to review. `bucket` selects a tab-style filter:
//
//	"pending"    (default) → submitted + needs_fix AND all 3 docs in, oldest first
//	"incomplete"           → submitted + needs_fix but still missing a document
//	"approved"             → approved, most recently verified first
//	"rejected"             → rejected, most recently verified first
type PendingProfile struct {
	UserID      uuid.UUID `json:"user_id"`
	FullName    string    `json:"full_name"`
	Email       string    `json:"email"`
	Status      string    `json:"status"`
	Round       int       `json:"round"`
	SubmittedAt *string   `json:"submitted_at,omitempty"`
	VerifiedAt  *string   `json:"verified_at,omitempty"`
	// EarliestExpiresAt is the soonest expires_at among the TA's current
	// approved required docs. Only meaningful for the approved bucket —
	// null in pending/rejected. The FE uses it to show "จะลบใน N วัน".
	EarliestExpiresAt *string `json:"earliest_expires_at,omitempty"`
	// AllFilesDeleted is true when the retention job has purged every one
	// of the current approved docs. FE hides the re-download button and
	// shows an "ถูกลบแล้ว" hint in that case.
	AllFilesDeleted bool `json:"all_files_deleted,omitempty"`
	// DownloadsUsed / DownloadsLimit are the download quota, counted per TA for
	// good (see maxDocDownloads). Surfaced so the button can say how many pulls
	// are left instead of letting the officer discover the limit by hitting it.
	DownloadsUsed  int `json:"downloads_used"`
	DownloadsLimit int `json:"downloads_limit"`
	// EverDownloaded is whether these documents have left the system AT ALL,
	// including via the quota-exempt bulk download. Distinct from DownloadsUsed on
	// purpose: the "อย่าลืมดาวน์โหลด" reminder asks this question, and asking the
	// quota counter instead made it nag about files already saved in bulk.
	EverDownloaded bool `json:"ever_downloaded"`
	// DocsIn is how many of the three required kinds currently have a document.
	// Only interesting in the incomplete bucket, where it is the whole point:
	// "2/3" tells the officer whether to nudge or wait.
	DocsIn int `json:"docs_in"`
}

// ProfileDocsInSQL counts the required documents a TA currently has, as a scalar
// subquery correlated on the given user-id column (`p.user_id`, `u.id`, …). Every
// place that decides whether a TA is "ส่งครบ" must use THIS, or the review page
// and the dashboard card that links to it will disagree about who is waiting.
//
// COUNT(*) = 3 is sound as an all-three-present test only because of the partial
// unique index ta_documents_current_uidx on (user_id, kind) WHERE superseded_at
// IS NULL — without it, three uploads of the same kind would count as complete.
//
// Deliberately counts documents regardless of their review status: a rejected
// document is still a document that was submitted, and its owner belongs in the
// review queue (the officer is who rejected it). Missing is what excludes.
func ProfileDocsInSQL(userCol string) string {
	return `(
		SELECT COUNT(*) FROM ta_documents d
		WHERE d.user_id = ` + userCol + `
		  AND d.kind IN ('national_id','bank_book','creditor_form')
		  AND d.superseded_at IS NULL
	)`
}

func (s *DocsService) ListReview(ctx context.Context, bucket string) ([]PendingProfile, error) {
	// Every bucket except "incomplete" is about a profile that exists, so the
	// base relation is users LEFT JOIN profiles and their p.status predicate
	// excludes the profile-less rows on its own. "incomplete" is the reason for
	// the LEFT: a TA who never opened the form has no ta_profiles row at all, and
	// they are exactly the people staff most need to chase.
	docsIn := ProfileDocsInSQL("u.id")
	var where, order string
	switch bucket {
	case "approved":
		where = "p.status = 'approved'"
		order = "p.verified_at DESC NULLS LAST"
	case "rejected":
		where = "p.status = 'rejected'"
		order = "p.verified_at DESC NULLS LAST"
	case "incomplete":
		// Not a queue — a chase list. Driven off the TA ROLE rather than off the
		// profile table, so "has not started" and "started but stalled" appear
		// together: staff asked to see how many TAs owe documents at all, and a
		// TA with no profile row is invisible to any profile-driven query.
		//
		// Approved is excluded belt-and-braces: approval requires all three
		// documents, so docs_in is already 3 for those rows.
		where = `u.is_active
			  AND EXISTS (SELECT 1 FROM user_roles ur WHERE ur.user_id = u.id AND ur.role = 'ta')
			  AND COALESCE(p.status::text, '') <> 'approved'
			  AND ` + docsIn + ` < 3`
		// Furthest behind first: 0/3 is a different conversation from 2/3, and
		// the people who have not started are the ones at risk of being missed.
		order = docsIn + ` ASC, u.first_name, u.last_name`
	default: // pending
		// Saving the step-1 profile form is what sets status='submitted', which
		// happens before a single file is uploaded. Listing on status alone put
		// TAs with 0 of 3 documents in the review queue, so an officer opened
		// them only to find nothing to review.
		where = "p.status IN ('submitted','needs_fix') AND " + docsIn + " = 3"
		order = "p.completed_at NULLS LAST"
	}
	// LEFT JOIN a subquery on ta_documents so we can surface the retention
	// clock next to each approved row without an extra roundtrip. The
	// subquery aggregates min(expires_at) + a boolean for "everything purged"
	// across just the three required kinds on their current (non-superseded)
	// rows.
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.first_name || ' ' || u.last_name, u.email,
		       -- No profile row yet: a distinct status rather than an empty
		       -- string, so the FE can say "ยังไม่เริ่ม" instead of showing blank.
		       COALESCE(p.status::text, 'not_started'),
		       COALESCE(p.current_round, 1), p.completed_at, p.verified_at,
		       d.earliest_expires_at, COALESCE(d.all_deleted, FALSE),
		       COALESCE(dl.used, 0), COALESCE(dl.ever, FALSE), `+docsIn+`
		FROM users u
		LEFT JOIN ta_profiles p ON p.user_id = u.id
		LEFT JOIN LATERAL (
			SELECT MIN(expires_at) AS earliest_expires_at,
			       BOOL_AND(file_deleted_at IS NOT NULL) AS all_deleted
			FROM ta_documents
			WHERE user_id = u.id
			  AND kind IN ('national_id','bank_book','creditor_form')
			  AND superseded_at IS NULL
			  AND status = 'approved'
		) d ON TRUE
		-- Two different questions off one table, which is why both are here:
		--   used  must count exactly what the quota check counts (see
		--         docDownloadsUsed) — quota-consuming rows only, every round;
		--   ever  must count every row, bulk included, because it answers "have
		--         these files been handed off" for the reminder.
		LEFT JOIN LATERAL (
			SELECT COUNT(*) FILTER (WHERE counts_toward_quota) AS used,
			       COUNT(*) > 0 AS ever
			FROM ta_doc_downloads
			WHERE user_id = u.id
		) dl ON TRUE
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
			&completedAt, &verifiedAt, &earliestExp, &p.AllFilesDeleted,
			&p.DownloadsUsed, &p.EverDownloaded, &p.DocsIn); err != nil {
			return nil, err
		}
		p.DownloadsLimit = maxDocDownloads
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
	ID     uuid.UUID `json:"id"`
	Round  int       `json:"round"`
	Prefix string    `json:"prefix"`
	// No value columns: a submission round records that it happened and what
	// staff decided, not what was in it (PDPA, migration 0047).
	Status       string     `json:"status"`
	SubmittedAt  time.Time  `json:"submitted_at"`
	ReviewedAt   *time.Time `json:"reviewed_at,omitempty"`
	RejectReason *string    `json:"reject_reason,omitempty"`
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
		       status::text, submitted_at, reviewed_at, reject_reason
		FROM ta_profile_submissions
		WHERE user_id = $1
		ORDER BY round DESC`, userID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sub ProfileSubmission
		if err := rows.Scan(&sub.ID, &sub.Round, &sub.Prefix, &sub.Status,
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

// BuildCreditorFormPDF renders the filled creditor form from the payload the
// TA just submitted — NOT from the database.
//
// PDPA (policy 2026-07-29): the national ID, bank details and signature are
// never stored. They arrive in the request, get drawn onto the PDF, and leave
// with the response. The PDF is the only artefact that keeps them, and it is
// deleted by the retention sweep like any other document. Reading them back
// from a table is not merely discouraged here — there is no table to read.
//
// Returns (pdfBytes, filenameSuggestion). Callers stream this either as a
// preview response or hand it to storage.Save when the TA confirms.
//
// grid=true draws a debug coordinate grid on top — used during calibration to
// tune the hard-coded field positions in internal/pdfgen. Never expose this
// flag on a public route.
func (s *DocsService) BuildCreditorFormPDF(ctx context.Context, userID uuid.UUID, in TAProfile, templatePath, fontDir string, grid bool) ([]byte, string, error) {
	if err := validateProfileInput(&in); err != nil {
		return nil, "", err
	}
	var fn, ln, phone, email, studentID string
	if err := s.pool.QueryRow(ctx,
		`SELECT first_name, last_name, COALESCE(phone,''), email, COALESCE(student_id,'')
		   FROM users WHERE id=$1`, userID,
	).Scan(&fn, &ln, &phone, &email, &studentID); err != nil {
		return nil, "", err
	}
	p := in
	if p.Phone != "" {
		phone = p.Phone
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
	// The TA types their student ID into the form on this same screen, so prefer
	// the value they just submitted over the users row, which may not be
	// updated yet on a first submission.
	if in.StudentID != "" {
		studentID = in.StudentID
	}
	return body, taFileStem(studentID, fn, ln) + ".pdf", nil
}

// AttachGeneratedCreditorForm generates the creditor-form PDF, stores it, and
// upserts a ta_documents row of kind=creditor_form (superseding any previous
// creditor_form doc for that user in the same way as a manual upload). Called
// when the TA clicks "ยืนยัน" on the preview.
func (s *DocsService) AttachGeneratedCreditorForm(ctx context.Context, userID uuid.UUID, in TAProfile, templatePath, fontDir string) (uuid.UUID, error) {
	body, filename, err := s.BuildCreditorFormPDF(ctx, userID, in, templatePath, fontDir, false)
	if err != nil {
		return uuid.Nil, err
	}
	// No scrub step any more: nothing sensitive was written in the first place
	// (migration 0047). The PDF holds the data and the retention sweep deletes
	// it on the same clock as every other uploaded document.
	return s.Upload(ctx, userID, "creditor_form", filename, "application/pdf", int64(len(body)), bytes.NewReader(body))
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
