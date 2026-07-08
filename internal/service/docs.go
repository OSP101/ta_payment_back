package service

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/docxgen"
	"ta-payment-back/internal/storage"
)

type DocsService struct {
	pool  *pgxpool.Pool
	aud   *audit.Auditor
	store storage.Store
}

type TAProfile struct {
	StudentID       string  `json:"student_id"`
	NationalID      string  `json:"national_id"`
	BankName        string  `json:"bank_name"`
	BankBranch      string  `json:"bank_branch"`
	BranchCode      string  `json:"branch_code"`
	AccountNo       string  `json:"account_no"`
	AccountName     string  `json:"account_name"`
	SignatureSVG    string  `json:"signature_svg"`
	SignaturePNGB64 string  `json:"signature_png_b64"`
	Status          string  `json:"status"`
	RejectReason    *string `json:"reject_reason,omitempty"`
}

func (s *DocsService) UpsertProfile(ctx context.Context, userID uuid.UUID, in TAProfile) error {
	// PDPA: national ID must be 13 chars, numeric
	if len(strings.ReplaceAll(in.NationalID, "-", "")) != 13 {
		return errors.New("national_id must be 13 digits")
	}
	// student_id is required for a TA — used on the creditor form and to
	// match records with the university payroll roster.
	sid := strings.TrimSpace(in.StudentID)
	if sid == "" {
		return errors.New("student_id is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`UPDATE users SET student_id = $2, updated_at = NOW() WHERE id = $1`,
		userID, sid); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO ta_profiles (user_id, national_id, bank_name, bank_branch, branch_code, account_no, account_name, signature_svg, signature_png_b64, status, completed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'submitted', NOW())
		ON CONFLICT (user_id) DO UPDATE SET
		  national_id=EXCLUDED.national_id, bank_name=EXCLUDED.bank_name, bank_branch=EXCLUDED.bank_branch,
		  branch_code=EXCLUDED.branch_code, account_no=EXCLUDED.account_no, account_name=EXCLUDED.account_name,
		  signature_svg=EXCLUDED.signature_svg, signature_png_b64=EXCLUDED.signature_png_b64,
		  status = CASE WHEN ta_profiles.status = 'approved' THEN ta_profiles.status ELSE 'submitted' END,
		  completed_at = NOW()`,
		userID, in.NationalID, in.BankName, in.BankBranch, in.BranchCode, in.AccountNo, in.AccountName, in.SignatureSVG, in.SignaturePNGB64); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &userID, Action: "ta_profile.submit", Entity: "ta_profile", EntityID: userID.String()})
	return nil
}

func (s *DocsService) GetProfile(ctx context.Context, userID uuid.UUID) (*TAProfile, error) {
	p := &TAProfile{}
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(u.student_id,''),
		        COALESCE(p.national_id,''), COALESCE(p.bank_name,''), COALESCE(p.bank_branch,''),
		        COALESCE(p.branch_code,''), COALESCE(p.account_no,''), COALESCE(p.account_name,''),
		        COALESCE(p.signature_svg,''), COALESCE(p.signature_png_b64,''), p.status::text, p.reject_reason
		 FROM ta_profiles p JOIN users u ON u.id = p.user_id
		 WHERE p.user_id = $1`, userID).Scan(
		&p.StudentID,
		&p.NationalID, &p.BankName, &p.BankBranch, &p.BranchCode, &p.AccountNo, &p.AccountName,
		&p.SignatureSVG, &p.SignaturePNGB64, &p.Status, &p.RejectReason)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Upload a supporting document (national_id, bank_book, creditor_form).
func (s *DocsService) Upload(ctx context.Context, userID uuid.UUID, kind, filename, mime string, size int64, r io.Reader) (uuid.UUID, error) {
	if kind != "national_id" && kind != "bank_book" && kind != "creditor_form" {
		return uuid.Nil, errors.New("invalid document kind")
	}
	key, savedSize, err := s.store.Save("ta_docs", filename, r)
	if err != nil {
		return uuid.Nil, err
	}
	if size == 0 {
		size = savedSize
	}
	id := uuid.New()
	_, err = s.pool.Exec(ctx, `
		INSERT INTO ta_documents (id, user_id, kind, filename, mime, size_bytes, storage_key, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'submitted')`,
		id, userID, kind, filename, mime, size, key)
	if err != nil {
		return uuid.Nil, err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &userID, Action: "ta_doc.upload", Entity: "ta_document", EntityID: id.String(), After: map[string]any{"kind": kind, "filename": filename}})
	return id, nil
}

type Document struct {
	ID         uuid.UUID `json:"id"`
	Kind       string    `json:"kind"`
	Filename   string    `json:"filename"`
	MIME       string    `json:"mime"`
	Size       int64     `json:"size_bytes"`
	Status     string    `json:"status"`
	RejectReason *string `json:"reject_reason,omitempty"`
	StorageKey string    `json:"-"`
}

func (s *DocsService) ListForUser(ctx context.Context, userID uuid.UUID) ([]Document, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, kind, filename, mime, size_bytes, status::text, reject_reason, storage_key
		 FROM ta_documents WHERE user_id = $1 ORDER BY uploaded_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Document
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.ID, &d.Kind, &d.Filename, &d.MIME, &d.Size, &d.Status, &d.RejectReason, &d.StorageKey); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (s *DocsService) OpenStored(ctx context.Context, id uuid.UUID) (io.ReadCloser, string, string, error) {
	var key, filename, mime string
	err := s.pool.QueryRow(ctx,
		`SELECT storage_key, filename, mime FROM ta_documents WHERE id = $1`, id).Scan(&key, &filename, &mime)
	if err != nil {
		return nil, "", "", err
	}
	rc, err := s.store.Open(key)
	return rc, filename, mime, err
}

func (s *DocsService) Review(ctx context.Context, actor, docID uuid.UUID, approve bool, reason string) error {
	if !approve && reason == "" {
		return errors.New("reject reason required")
	}
	status := "approved"
	if !approve {
		status = "rejected"
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE ta_documents SET status=$1::doc_status, reject_reason=$2, reviewed_at=NOW(), reviewed_by=$3 WHERE id=$4`,
		status, nilStrPtr(reason), actor, docID)
	if err == nil {
		s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "ta_doc.review", Entity: "ta_document",
			EntityID: docID.String(), After: map[string]any{"status": status, "reason": reason}})
	}
	return err
}

func (s *DocsService) ReviewProfile(ctx context.Context, actor, userID uuid.UUID, approve bool, reason string) error {
	if !approve && reason == "" {
		return errors.New("reject reason required")
	}
	status := "approved"
	if !approve {
		status = "rejected"
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE ta_profiles SET status=$1::doc_status, reject_reason=$2, verified_at=NOW(), verified_by=$3 WHERE user_id=$4`,
		status, nilStrPtr(reason), actor, userID)
	if err == nil {
		s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "ta_profile.review", Entity: "ta_profile",
			EntityID: userID.String(), After: map[string]any{"status": status, "reason": reason}})
	}
	return err
}

// Profiles for staff to review. `bucket` selects a tab-style filter:
//   "pending"  (default) → submitted + needs_fix, oldest completion first
//   "approved"           → approved, most recently verified first
//   "rejected"           → rejected, most recently verified first
type PendingProfile struct {
	UserID     uuid.UUID `json:"user_id"`
	FullName   string    `json:"full_name"`
	Email      string    `json:"email"`
	Status     string    `json:"status"`
	SubmittedAt *string  `json:"submitted_at,omitempty"`
	VerifiedAt  *string  `json:"verified_at,omitempty"`
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
	rows, err := s.pool.Query(ctx, `
		SELECT p.user_id, u.first_name || ' ' || u.last_name, u.email, p.status::text,
		       p.completed_at, p.verified_at
		FROM ta_profiles p JOIN users u ON u.id = p.user_id
		WHERE `+where+` ORDER BY `+order)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingProfile
	for rows.Next() {
		var p PendingProfile
		var completedAt, verifiedAt *time.Time
		if err := rows.Scan(&p.UserID, &p.FullName, &p.Email, &p.Status, &completedAt, &verifiedAt); err != nil {
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
		out = append(out, p)
	}
	return out, nil
}

// BuildCreditorForm generates the filled DOCX for a TA. Reads user + profile,
// resolves signature (PNG base64), and calls docxgen with the shipped template.
// Returns (bytes, filename).
func (s *DocsService) BuildCreditorForm(ctx context.Context, userID uuid.UUID, templatePath string) ([]byte, string, error) {
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
	body, err := docxgen.Fill(templatePath, docxgen.Data{
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
	})
	if err != nil {
		return nil, "", err
	}
	safe := strings.ReplaceAll(fn+"_"+ln, " ", "_")
	return body, "creditor_form_" + safe + ".docx", nil
}

func nilStrPtr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
