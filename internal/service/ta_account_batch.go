package service

import (
	"context"
	"errors"
	"fmt"
	netmail "net/mail"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/auth"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
)

// TAAccountBatch creates identities only. It never files a TA request or sends mail.
type TAAccountBatch struct {
	pool  *pgxpool.Pool
	aud   *audit.Auditor
	users *UserService
}

func NewTAAccountBatch(pool *pgxpool.Pool, cfg config.Config) *TAAccountBatch {
	aud := audit.New(pool)
	return &TAAccountBatch{pool: pool, aud: aud, users: &UserService{pool: pool, aud: aud, notify: &NotifyService{pool: pool, mailer: mail.New(cfg), baseURL: config.PrimaryBaseURL(cfg.AppBaseURL)}}}
}

type TAAccountInput struct {
	StudentID     string `json:"student_id"`
	Email         string `json:"email"`
	Title         string `json:"title"`
	FirstName     string `json:"first_name"`
	LastName      string `json:"last_name"`
	StudyLevel    string `json:"study_level"`
	Seniority     string `json:"ta_seniority,omitempty"`
	HistorySource string `json:"history_source,omitempty"`
}

// These records must only be persisted in an encrypted manifest, never logs.
type TAAccountRecord struct {
	TAAccountInput
	UserID   uuid.UUID `json:"user_id"`
	Status   string    `json:"status"`
	Password string    `json:"password,omitempty"`
	Detail   string    `json:"detail,omitempty"`
}

func (s *TAAccountBatch) CheckActor(ctx context.Context, actor uuid.UUID) error {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users u JOIN user_roles r ON r.user_id=u.id WHERE u.id=$1 AND u.is_active AND u.deleted_at IS NULL AND r.role IN ('admin','staff'))`, actor).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("actor must be an active admin or staff account")
	}
	return nil
}
func validateTAAccountInputs(in []TAAccountInput) error {
	if len(in) == 0 {
		return errors.New("empty account input")
	}
	ids, emails := map[string]bool{}, map[string]bool{}
	for i, r := range in {
		if r.Seniority != "" && (r.Seniority != "returning" || strings.TrimSpace(r.HistorySource) == "" || len([]rune(r.HistorySource)) > 200) {
			return fmt.Errorf("invalid legacy TA history at row %d", i+1)
		}
		a, err := netmail.ParseAddress(r.Email)
		if !studentIDPattern(r.StudentID) || err != nil || a.Address != r.Email || strings.TrimSpace(r.FirstName) == "" || strings.TrimSpace(r.LastName) == "" || len([]rune(r.FirstName)) > 100 || len([]rune(r.LastName)) > 100 || !validAccountLevel(r.StudyLevel) {
			return fmt.Errorf("invalid account input at row %d", i+1)
		}
		e := strings.ToLower(r.Email)
		if ids[r.StudentID] || emails[e] {
			return fmt.Errorf("duplicate student ID or email at row %d", i+1)
		}
		ids[r.StudentID] = true
		emails[e] = true
	}
	return nil
}
func validAccountLevel(level string) bool {
	return level == "undergrad" || level == "master" || level == "phd"
}

// Import writes the encrypted checkpoint before committing the transaction. If a
// checkpoint fails no account is created. Existing accounts are never modified.
func (s *TAAccountBatch) Import(ctx context.Context, actor uuid.UUID, batch string, in []TAAccountInput, apply bool, checkpoint func([]TAAccountRecord) error) ([]TAAccountRecord, error) {
	if err := validateTAAccountInputs(in); err != nil {
		return nil, err
	}
	if err := s.CheckActor(ctx, actor); err != nil {
		return nil, err
	}
	if apply && checkpoint == nil {
		return nil, errors.New("encrypted checkpoint is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if apply {
		if _, err = tx.Exec(ctx, `LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return nil, err
		}
	}
	result := make([]TAAccountRecord, 0, len(in))
	for _, r := range in {
		r.Email = strings.ToLower(r.Email)
		rec := TAAccountRecord{TAAccountInput: r, Status: "ready"}
		existing, err := tx.Query(ctx, `SELECT u.id,u.email,COALESCE(u.student_id,''),u.is_active,EXISTS(SELECT 1 FROM user_roles ur WHERE ur.user_id=u.id AND ur.role='ta') FROM users u WHERE u.deleted_at IS NULL AND (lower(u.email)=lower($1) OR regexp_replace(COALESCE(u.student_id,''),'[^0-9]','','g')=replace($2,'-',''))`, r.Email, r.StudentID)
		if err != nil {
			return nil, err
		}
		count := 0
		var id uuid.UUID
		var email, sid string
		var active, isTA bool
		for existing.Next() {
			count++
			if err = existing.Scan(&id, &email, &sid, &active, &isTA); err != nil {
				existing.Close()
				return nil, err
			}
		}
		err = existing.Err()
		existing.Close()
		if err != nil {
			return nil, err
		}
		if count > 0 {
			rec.Status = "conflict"
			rec.Detail = "existing identity needs review"
			switch {
			case count > 1:
				rec.Detail = "multiple existing accounts match the student ID/email"
			case !strings.EqualFold(email, r.Email):
				rec.Detail = "student ID exists with another email"
			case sid == "":
				rec.Detail = "email exists but existing account has no student ID"
			case strings.ReplaceAll(sid, "-", "") != strings.ReplaceAll(r.StudentID, "-", ""):
				rec.Detail = "email exists with another student ID"
			case !active:
				rec.Detail = "matching account is inactive"
			case !isTA:
				rec.Detail = "matching account does not have TA role"
			}
			if count == 1 && strings.EqualFold(email, r.Email) && strings.ReplaceAll(sid, "-", "") == strings.ReplaceAll(r.StudentID, "-", "") && active && isTA {
				rec.Status = "existing"
				rec.UserID = id
				rec.Detail = "unchanged"
			}
			result = append(result, rec)
			continue
		}
		if !apply {
			result = append(result, rec)
			continue
		}
		rec.UserID = uuid.New()
		rec.Password = generateTempPassword(18)
		hash, err := auth.HashPassword(rec.Password)
		if err != nil {
			return nil, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO users(id,email,title,first_name,last_name,study_level,student_id,password_hash,must_change_password,is_active,profile_completed,ta_seniority_override) VALUES($1,$2,NULLIF($3,''),$4,$5,$6::study_level,$7,$8,TRUE,TRUE,FALSE,NULLIF($9,''))`, rec.UserID, r.Email, r.Title, r.FirstName, r.LastName, r.StudyLevel, r.StudentID, hash, r.Seniority)
		if err != nil {
			return nil, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO user_roles(user_id,role) VALUES($1,'ta')`, rec.UserID); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO ta_enrollments(user_id,student_id,study_level,created_by,note) VALUES($1,$2,$3::study_level,$4,$5)`, rec.UserID, r.StudentID, r.StudyLevel, actor, "account import "+batch); err != nil {
			return nil, err
		}
		if err = s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "user.create", Entity: "user", EntityID: rec.UserID.String(), After: map[string]any{"email": r.Email, "student_id": r.StudentID, "roles": []string{"ta"}, "batch": batch, "ta_seniority": r.Seniority, "history_source": r.HistorySource}, Note: "account-only import; no mail"}); err != nil {
			return nil, err
		}
		rec.Status = "pending"
		result = append(result, rec)
	}
	if !apply {
		return result, nil
	}
	if err = checkpoint(result); err != nil {
		return nil, fmt.Errorf("manifest checkpoint: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit uncertain: keep manifest and reconcile accounts before retrying: %w", err)
	}
	return result, nil
}

func (s *TAAccountBatch) ValidateCredential(ctx context.Context, r TAAccountRecord) error {
	var email, sid string
	var hash *string
	var active, mustChange, isTA bool
	err := s.pool.QueryRow(ctx, `SELECT email,COALESCE(student_id,''),password_hash,is_active,must_change_password,EXISTS(SELECT 1 FROM user_roles WHERE user_id=users.id AND role='ta') FROM users WHERE id=$1 AND deleted_at IS NULL`, r.UserID).Scan(&email, &sid, &hash, &active, &mustChange, &isTA)
	if err != nil {
		return errors.New("account no longer available")
	}
	if !active || !isTA || !mustChange || hash == nil || !strings.EqualFold(email, r.Email) || sid != r.StudentID || !auth.CheckPassword(*hash, r.Password) {
		return errors.New("account identity or temporary password changed; delivery skipped")
	}
	return nil
}
func (s *TAAccountBatch) Send(ctx context.Context, actor uuid.UUID, r TAAccountRecord) error {
	if err := s.CheckActor(ctx, actor); err != nil {
		return err
	}
	if err := s.ValidateCredential(ctx, r); err != nil {
		return err
	}
	return s.users.SendCredentials(ctx, actor, r.UserID, r.Password)
}

// ResolveActor uses an explicitly selected account, or the only active admin.
func (s *TAAccountBatch) ResolveActor(ctx context.Context, email string) (uuid.UUID, error) {
	var ids []uuid.UUID
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT u.id FROM users u JOIN user_roles r ON r.user_id=u.id WHERE u.is_active AND u.deleted_at IS NULL AND (($1<>'' AND lower(u.email)=lower($1) AND r.role IN ('admin','staff')) OR ($1='' AND r.role='admin'))`, email)
	if err != nil {
		return uuid.Nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			return uuid.Nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		return uuid.Nil, err
	}
	if len(ids) != 1 {
		return uuid.Nil, errors.New("specify -actor-email for one active admin/staff account")
	}
	return ids[0], nil
}
