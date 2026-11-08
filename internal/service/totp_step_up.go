package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// VerifyStepUp requires a fresh authenticator code for the current action.
// Recovery codes are reserved for login/recovery and never authorize an action.
// The account gate is shared across all sensitive actions, including the legacy
// admin unlock endpoint. No password fallback is permitted.
func (s *MFAService) VerifyStepUp(ctx context.Context, actor uuid.UUID, code string) error {
	if s == nil || s.totp == nil {
		return &UserError{Status: 503, Msg: "ระบบยืนยัน TOTP ยังไม่พร้อมใช้งาน"}
	}
	if err := pwGateCheck(actor); err != nil {
		return err
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return Invalid("กรุณากรอกรหัส TOTP 6 หลักจากแอป Authenticator")
	}
	var secret []byte
	var enabledAt *time.Time
	err := s.pool.QueryRow(ctx, `SELECT totp_secret_enc, totp_enabled_at FROM users
 WHERE id = $1 AND is_active AND deleted_at IS NULL`, actor).Scan(&secret, &enabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return &UserError{Status: 401, Msg: "บัญชีนี้ไม่สามารถใช้งานได้"}
	}
	if err != nil {
		return err
	}
	if enabledAt == nil || len(secret) == 0 {
		return Forbidden("กรุณาเปิดใช้งาน 2FA ที่หน้าบัญชีของฉันก่อนทำรายการนี้")
	}
	if !isNumericTOTP(code) {
		pwGateFail(actor)
		return &UserError{Status: 401, Msg: "กรุณาใช้รหัส TOTP 6 หลักจากแอป Authenticator"}
	}
	ok, err := s.verifyLiveCode(ctx, actor, secret, code)
	if err != nil {
		return err
	}
	if !ok {
		pwGateFail(actor)
		return &UserError{Status: 401, Msg: "รหัส TOTP ไม่ถูกต้อง หมดอายุ หรือใช้ไปแล้ว กรุณารอรหัสใหม่จากแอป Authenticator"}
	}
	pwGateSucceed(actor)
	return nil
}
