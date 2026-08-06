package handler

import (
	"time"

	"github.com/gofiber/fiber/v2"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/auth"
	"ta-payment-back/internal/rbac"
	"ta-payment-back/internal/service"
)

type AuthHandler struct {
	Svc    *service.Container
	Tokens *auth.TokenService
	RBAC   *rbac.RBAC
	Aud    *audit.Auditor
}

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *AuthHandler) Login(c *fiber.Ctx) error {
	var in loginReq
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	u, hash, err := h.Svc.Users.FindByEmail(c.Context(), in.Email)
	if err != nil || u == nil || hash == "" {
		// Spend the same time a real bcrypt check would, so a missing account is
		// indistinguishable from a wrong password by timing.
		auth.DummyCompare(in.Password)
		return fiber.NewError(fiber.StatusUnauthorized, "อีเมลหรือรหัสผ่านไม่ถูกต้อง")
	}
	if !auth.CheckPassword(hash, in.Password) {
		return fiber.NewError(fiber.StatusUnauthorized, "อีเมลหรือรหัสผ่านไม่ถูกต้อง")
	}
	// The synthetic executive role rides in the TOKEN only, not in u.Roles:
	// RequireRole reads roles from JWT claims, so leaving it out here made the
	// analytics endpoints 403 for a flagged lecturer. u.Roles itself stays the
	// user_roles list — the users screen renders those as role chips and shows
	// the flag separately.
	tokenRoles := u.Roles
	if u.IsExecutive {
		tokenRoles = append(append([]string{}, u.Roles...), rbac.RoleExecutive)
	}
	tok, err := h.Tokens.Issue(u.ID, tokenRoles, h.Svc.Cfg.JWTLifetime)
	if err != nil {
		return err
	}
	setAuthCookie(c, tok, h.Svc.Cfg.JWTLifetime)
	h.Aud.Log(c.Context(), audit.Entry{ActorID: &u.ID, Action: "auth.login", Entity: "user", EntityID: u.ID.String(), IP: c.IP(), UserAgent: c.Get("User-Agent")})
	// The token lives only in the HttpOnly cookie — it is deliberately NOT
	// returned in the body so it can't be stashed in localStorage where XSS
	// could read it.
	return c.JSON(fiber.Map{"user": u})
}

// SSOURL returns the SSO redirect URL for the frontend to send the user to.
// Stub: real implementation depends on KKU IT integration protocol.
func (h *AuthHandler) SSOURL(c *fiber.Ctx) error {
	if !h.Svc.Cfg.SSOEnabled {
		return c.JSON(fiber.Map{"enabled": false})
	}
	return c.JSON(fiber.Map{
		"enabled": true,
		"url":     h.Svc.Cfg.SSOAuthURL + "?client_id=" + h.Svc.Cfg.SSOClientID + "&redirect_uri=" + h.Svc.Cfg.SSORedirect + "&response_type=code&scope=openid+email+profile",
	})
}

// SSOCallback exchanges an authorization code for a session.
// Stub: real implementation depends on KKU IT integration protocol.
func (h *AuthHandler) SSOCallback(c *fiber.Ctx) error {
	return fiber.NewError(fiber.StatusNotImplemented, "SSO callback not yet configured — supply SSO_* env vars and update handler once KKU IT provides endpoints/credentials")
}

func (h *AuthHandler) Logout(c *fiber.Ctx) error {
	clearAuthCookie(c)
	return c.JSON(fiber.Map{"ok": true})
}

func (h *AuthHandler) Me(c *fiber.Ctx) error {
	u, err := h.Svc.Users.Get(c.Context(), UserID(c))
	if err != nil {
		return err
	}
	return c.JSON(u)
}

type changePwReq struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (h *AuthHandler) ChangePassword(c *fiber.Ctx) error {
	var in changePwReq
	if err := c.BodyParser(&in); err != nil || len(in.NewPassword) < 8 {
		return fiber.NewError(fiber.StatusBadRequest, "password must be >= 8 chars")
	}
	uid := UserID(c)
	u, err := h.Svc.Users.Get(c.Context(), uid)
	if err != nil {
		return err
	}
	// A forced first-login change (must_change_password) has no current password
	// to confirm — the user just authenticated with the temp password. For a
	// voluntary change we require the current password so a hijacked live session
	// on an unattended machine cannot silently take over the account.
	if !u.MustChangePassword {
		if in.CurrentPassword == "" {
			return fiber.NewError(fiber.StatusBadRequest, "กรุณากรอกรหัสผ่านปัจจุบัน")
		}
		// Re-fetch the stored hash the same way Login does, then bcrypt-compare.
		_, hash, err := h.Svc.Users.FindByEmail(c.Context(), u.Email)
		if err != nil || hash == "" || !auth.CheckPassword(hash, in.CurrentPassword) {
			return fiber.NewError(fiber.StatusUnauthorized, "รหัสผ่านปัจจุบันไม่ถูกต้อง")
		}
	}
	if err := h.Svc.Users.UpdatePassword(c.Context(), uid, in.NewPassword); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

func setAuthCookie(c *fiber.Ctx, token string, ttl time.Duration) {
	c.Cookie(&fiber.Cookie{
		Name:     "access_token",
		Value:    token,
		HTTPOnly: true,
		SameSite: "Lax",
		Secure:   c.Protocol() == "https",
		Path:     "/",
		Expires:  time.Now().Add(ttl),
	})
}

func clearAuthCookie(c *fiber.Ctx) {
	c.Cookie(&fiber.Cookie{
		Name:     "access_token",
		Value:    "",
		HTTPOnly: true,
		SameSite: "Lax",
		Path:     "/",
		Expires:  time.Now().Add(-time.Hour),
	})
}
