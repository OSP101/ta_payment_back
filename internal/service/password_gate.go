package service

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/auth"
)

// VerifyUserPassword re-authenticates the person already holding a session,
// immediately before an act that a stolen session must not be able to perform.
//
// Two callers today: releasing a document bundle full of national IDs, and
// rewriting a TA's approved hours. Both are cases where "you are logged in" is
// not enough — the first hands over PII, the second moves money and overrides a
// lecturer's approval.
//
// Extracted from DocsService on 31/07/2026 when the worklog editor needed the
// same gate. A second copy of a password check is exactly the kind of thing that
// drifts: one side gets the is_active test and the other quietly does not.
//
// Timing is dominated by bcrypt, so no dummy compare is needed; an account
// deactivated mid-session simply has no row to match.
func VerifyUserPassword(ctx context.Context, pool *pgxpool.Pool, actor uuid.UUID, password string) error {
	if password == "" {
		return &UserError{Status: 401, Msg: "ต้องกรอกรหัสผ่านเพื่อยืนยันตัวตน"}
	}
	var hash string
	err := pool.QueryRow(ctx,
		`SELECT password_hash FROM users WHERE id = $1 AND is_active AND deleted_at IS NULL`,
		actor,
	).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return &UserError{Status: 401, Msg: "บัญชีนี้ไม่สามารถใช้งานได้"}
	}
	if err != nil {
		return err
	}
	if !auth.CheckPassword(hash, password) {
		return &UserError{Status: 401, Msg: "รหัสผ่านไม่ถูกต้อง"}
	}
	return nil
}
