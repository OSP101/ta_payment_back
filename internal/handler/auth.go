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
	if err != nil || u == nil {
		return fiber.NewError(fiber.StatusUnauthorized, "invalid credentials")
	}
	if hash == "" || !auth.CheckPassword(hash, in.Password) {
		return fiber.NewError(fiber.StatusUnauthorized, "invalid credentials")
	}
	tok, err := h.Tokens.Issue(u.ID, u.Roles, h.Svc.Cfg.JWTLifetime)
	if err != nil {
		return err
	}
	setAuthCookie(c, tok, h.Svc.Cfg.JWTLifetime)
	h.Aud.Log(c.Context(), audit.Entry{ActorID: &u.ID, Action: "auth.login", Entity: "user", EntityID: u.ID.String(), IP: c.IP(), UserAgent: c.Get("User-Agent")})
	return c.JSON(fiber.Map{"user": u, "token": tok})
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
	NewPassword string `json:"new_password"`
}

func (h *AuthHandler) ChangePassword(c *fiber.Ctx) error {
	var in changePwReq
	if err := c.BodyParser(&in); err != nil || len(in.NewPassword) < 8 {
		return fiber.NewError(fiber.StatusBadRequest, "password must be >= 8 chars")
	}
	if err := h.Svc.Users.UpdatePassword(c.Context(), UserID(c), in.NewPassword); err != nil {
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
