// mfa.go: the self-service 2FA enrolment/management endpoints (POST
// /me/2fa/*) plus the admin-only reset (POST /users/:id/2fa/reset). Login's
// own two-step flow lives in auth.go — this file is what a user reaches
// AFTER authenticating (either already enrolled and managing it from
// /account, or blocked at mfa_setup_required and working through
// /setup-2fa).
package handler

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image/png"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/pquerna/otp"

	"ta-payment-back/internal/service"
)

type MFAHandler struct {
	Svc *service.Container
}

// setupResp is what the QR-scan screen renders. Secret is included so the
// page can offer "can't scan? type this code instead" — it's the same
// secret embedded in OTPAuthURL/the QR image, not additional exposure.
type setupResp struct {
	Secret      string `json:"secret"`
	OTPAuthURL  string `json:"otpauth_url"`
	QRPNGBase64 string `json:"qr_png_base64"`
}

// Setup generates a new pending TOTP secret and renders it as a scannable QR
// code. Safe to call more than once (refits a stray double-submit) — see
// MFAService.GenerateEnrollment's own doc comment on why it only refuses when
// 2FA is ALREADY fully enabled, not on a second call while still pending.
func (h *MFAHandler) Setup(c *fiber.Ctx) error {
	uid := UserID(c)
	me, err := h.Svc.Users.Get(c.Context(), uid)
	if err != nil {
		return err
	}
	enr, err := h.Svc.MFA.GenerateEnrollment(c.Context(), uid, me.Email)
	if err != nil {
		if errors.Is(err, service.ErrMFAAlreadyEnabled) {
			return fiber.NewError(fiber.StatusConflict, "เปิดใช้งาน 2FA อยู่แล้ว หากต้องการเปลี่ยนอุปกรณ์ ให้ปิดใช้งาน 2FA เดิมก่อน")
		}
		return err
	}
	key, err := otp.NewKeyFromURL(enr.OTPAuthURL)
	if err != nil {
		return err
	}
	img, err := key.Image(256, 256)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	return c.JSON(setupResp{
		Secret:      enr.Secret,
		OTPAuthURL:  enr.OTPAuthURL,
		QRPNGBase64: "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()),
	})
}

type enable2FAReq struct {
	Code string `json:"code" validate:"required"`
}

// Enable confirms the pending secret from Setup and returns 10 recovery
// codes — the only time they are ever shown in plaintext.
func (h *MFAHandler) Enable(c *fiber.Ctx) error {
	var in enable2FAReq
	if err := Bind(c, &in); err != nil {
		return err
	}
	codes, err := h.Svc.MFA.Enable(c.Context(), UserID(c), in.Code)
	if err != nil {
		return mfaEnrollError(err)
	}
	return c.JSON(fiber.Map{"recovery_codes": codes})
}

type disable2FAReq struct {
	Password string `json:"password" validate:"required"`
	Code     string `json:"code" validate:"required"`
}

// Disable turns 2FA off. Requires BOTH the account password (re-auth, same
// gate every other sensitive account action uses) and a valid second-factor
// code — proving both factors to remove either, not just one.
func (h *MFAHandler) Disable(c *fiber.Ctx) error {
	var in disable2FAReq
	if err := Bind(c, &in); err != nil {
		return err
	}
	uid := UserID(c)
	if err := service.VerifyUserPassword(c.Context(), h.Svc.Pool, uid, in.Password); err != nil {
		return err
	}
	if err := h.Svc.MFA.Disable(c.Context(), uid, in.Code); err != nil {
		return mfaEnrollError(err)
	}
	return c.JSON(fiber.Map{"ok": true})
}

type regenerateCodesReq struct {
	Password string `json:"password" validate:"required"`
}

// RegenerateRecoveryCodes replaces every existing recovery code. Password
// only, no TOTP code required — this is the "I still have my authenticator
// but I'm down to my last code" path, distinct from Disable/AdminReset which
// remove 2FA entirely.
func (h *MFAHandler) RegenerateRecoveryCodes(c *fiber.Ctx) error {
	var in regenerateCodesReq
	if err := Bind(c, &in); err != nil {
		return err
	}
	uid := UserID(c)
	if err := service.VerifyUserPassword(c.Context(), h.Svc.Pool, uid, in.Password); err != nil {
		return err
	}
	codes, err := h.Svc.MFA.RegenerateRecoveryCodes(c.Context(), uid)
	if err != nil {
		return mfaEnrollError(err)
	}
	return c.JSON(fiber.Map{"recovery_codes": codes})
}

type adminReset2FAReq struct {
	Password string `json:"password" validate:"required"`
}

// AdminReset clears a target user's 2FA entirely — the break-glass path for
// "I lost my phone and my recovery codes". Admin-only (see router.go), and
// requires the ACTING admin's own password before it will touch anyone
// else's account — see MFAService.AdminReset's own doc comment for why this
// cannot be opened up to staff the way password reset already is.
func (h *MFAHandler) AdminReset(c *fiber.Ctx) error {
	targetID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	var in adminReset2FAReq
	if err := Bind(c, &in); err != nil {
		return err
	}
	actorID := UserID(c)
	if err := service.VerifyUserPassword(c.Context(), h.Svc.Pool, actorID, in.Password); err != nil {
		return err
	}
	if err := h.Svc.MFA.AdminReset(c.Context(), actorID, targetID); err != nil {
		return err
	}
	// The target's TOTP secret is gone; whatever session(s) they hold no
	// longer correspond to an account 2FA-related screens can trust — force
	// re-login, same posture as a password reset.
	if err := h.Svc.Sessions.RevokeAllForUser(c.Context(), targetID, "mfa_reset"); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// mfaEnrollError translates the MFAService sentinels into Thai user-facing
// errors. Kept out of the central ErrorHandler switch (middleware.go) since
// these are specific to this one handler file, not general service errors.
func mfaEnrollError(err error) error {
	switch {
	case errors.Is(err, service.ErrMFAAlreadyEnabled):
		return fiber.NewError(fiber.StatusConflict, "เปิดใช้งาน 2FA อยู่แล้ว")
	case errors.Is(err, service.ErrMFANotPending):
		return fiber.NewError(fiber.StatusBadRequest, "ยังไม่ได้เริ่มตั้งค่า 2FA กรุณาเริ่มใหม่")
	case errors.Is(err, service.ErrMFANotEnabled):
		return fiber.NewError(fiber.StatusBadRequest, "ยังไม่ได้เปิดใช้งาน 2FA")
	case errors.Is(err, service.ErrMFAInvalidCode):
		return fiber.NewError(fiber.StatusUnauthorized, "รหัสยืนยันไม่ถูกต้อง")
	default:
		return err
	}
}
