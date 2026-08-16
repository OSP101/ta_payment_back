package handler

import (
	"errors"
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
	Email    string `json:"email" validate:"required,email"`
	Password string `json:"password" validate:"required"`
}

func (h *AuthHandler) Login(c *fiber.Ctx) error {
	var in loginReq
	if err := Bind(c, &in); err != nil {
		return err
	}
	// Authenticate owns the whole login rule — enumeration-safe timing,
	// per-account lockout, and the audit trail for every outcome. See its doc
	// comment in user.go.
	u, err := h.Svc.Users.Authenticate(c.Context(), in.Email, in.Password, c.IP(), c.Get("User-Agent"))
	if err != nil {
		return err
	}
	if u.TOTPEnabled {
		// Password verified, second factor still outstanding: no session,
		// no cookie, and deliberately NO user object in the response — the
		// full profile (email, names, roles) must not be handed out on a
		// password-only success. See POST /auth/login/2fa (LoginTwoFactor)
		// for step 2.
		challenge, err := h.Svc.MFA.IssueChallenge(c.Context(), u.ID)
		if err != nil {
			return err
		}
		return c.JSON(fiber.Map{"mfa_required": true, "challenge": challenge})
	}
	return h.finishLogin(c, u)
}

type login2FAReq struct {
	Challenge string `json:"challenge" validate:"required"`
	Code      string `json:"code" validate:"required"`
}

// mfaInvalidMsg is shown for every rejection reason POST /auth/login/2fa can
// hit — wrong code, expired challenge, already-consumed challenge, or
// attempts exhausted. Deliberately identical across all of them: see
// service.ErrMFAChallengeInvalid's own doc comment for why the difference
// between those cases must not be observable to the caller.
const mfaInvalidMsg = "รหัสยืนยันไม่ถูกต้องหรือหมดอายุ กรุณาเข้าสู่ระบบใหม่อีกครั้ง"

// LoginTwoFactor is step 2 of login: redeem the challenge IssueChallenge
// handed back from step 1 with a TOTP or recovery code. On success this does
// exactly what a password-only Login already does — CreateAndSupersede,
// Issue, setAuthCookie — via the same finishLogin helper.
func (h *AuthHandler) LoginTwoFactor(c *fiber.Ctx) error {
	var in login2FAReq
	if err := Bind(c, &in); err != nil {
		return err
	}
	userID, err := h.Svc.MFA.RedeemChallenge(c.Context(), in.Challenge, in.Code, c.IP(), c.Get("User-Agent"))
	if err != nil {
		if errors.Is(err, service.ErrMFAChallengeInvalid) {
			return fiber.NewError(fiber.StatusUnauthorized, mfaInvalidMsg)
		}
		return err
	}
	u, err := h.Svc.Users.Get(c.Context(), userID)
	if err != nil {
		return err
	}
	return h.finishLogin(c, u)
}

// finishLogin mints the session, token and cookie for an already-fully-
// authenticated user (password alone when 2FA is off, or password+code when
// it's on) and returns the same {"user": ...} body either path used to
// return directly.
func (h *AuthHandler) finishLogin(c *fiber.Ctx, u *service.User) error {
	// The synthetic executive role rides in the TOKEN only, not in u.Roles:
	// RequireRole reads roles from JWT claims, so leaving it out here made the
	// analytics endpoints 403 for a flagged lecturer. u.Roles itself stays the
	// user_roles list — the users screen renders those as role chips and shows
	// the flag separately.
	tokenRoles := u.Roles
	if u.IsExecutive {
		tokenRoles = append(append([]string{}, u.Roles...), rbac.RoleExecutive)
	}
	// Single-device login: this also revokes every other session this user
	// currently holds (reason "superseded"), so whatever device was signed in
	// before is rejected on its very next request — see SessionService and
	// AccountGuard.
	sessionID, err := h.Svc.Sessions.CreateAndSupersede(c.Context(), u.ID, h.Svc.Cfg.JWTLifetime, c.IP(), c.Get("User-Agent"))
	if err != nil {
		return err
	}
	tok, err := h.Tokens.Issue(u.ID, tokenRoles, sessionID, h.Svc.Cfg.JWTLifetime)
	if err != nil {
		return err
	}
	setAuthCookie(c, tok, h.Svc.Cfg.JWTLifetime, h.Svc.Cfg.CookieSecure)
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
//
// When this is implemented: it MUST branch on TOTPEnabled and issue an MFA
// challenge exactly like Login does, not go straight to CreateAndSupersede +
// Issue + setAuthCookie. SSO is a second, independent path to a session, and
// nothing about a KKU SSO assertion proves possession of the account's
// second factor — copy Login's TOTPEnabled branch, don't skip it because
// "SSO already authenticated them".
func (h *AuthHandler) SSOCallback(c *fiber.Ctx) error {
	return fiber.NewError(fiber.StatusNotImplemented, "SSO callback not yet configured — supply SSO_* env vars and update handler once KKU IT provides endpoints/credentials")
}

// Logout sits outside the authed group (see router.go) so a client with an
// already-expired or invalid token can still clear its cookie — logging out
// must never itself require being logged in. That means UserID/SessionID
// (which read Authenticated's Locals) are not available here; the token is
// parsed directly, best-effort, so the session row gets revoked when there is
// one to revoke.
func (h *AuthHandler) Logout(c *fiber.Ctx) error {
	if raw := extractToken(c); raw != "" {
		if claims, err := h.Tokens.Parse(raw); err == nil {
			if sid, err := claims.SessionID(); err == nil {
				_ = h.Svc.Sessions.Revoke(c.Context(), sid, "logout")
			}
		}
	}
	clearAuthCookie(c, h.Svc.Cfg.CookieSecure)
	return c.JSON(fiber.Map{"ok": true})
}

// Heartbeat is a deliberate no-op: AccountGuard already touches this
// request's session (it is a POST) before the request ever reaches a
// handler, so all this endpoint needs to do is exist as something the
// frontend's idle-activity tracker can POST to. See SessionActivityGuard on
// the frontend, which calls this at most once a minute while the user is
// actually doing something — not on a fixed timer — so an idle tab cannot
// keep its own session alive by polling.
func (h *AuthHandler) Heartbeat(c *fiber.Ctx) error {
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
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	// Checked up front for a fast, specific error before the current-password
	// round trip below — UpdatePassword enforces the same rule again itself
	// (see service.ValidatePassword), so this exists for response ordering
	// only, not as the only place the rule is enforced.
	if err := service.ValidatePassword(in.NewPassword); err != nil {
		return err
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
	// A changed password invalidates whatever session(s) were issued under the
	// old one — including this very request's, which is deliberate: the
	// response below still reaches the client (AccountGuard already let this
	// request through before the change happened), but the next request has
	// to sign in again with the new password. Under single-device login this
	// revokes exactly the session that just changed the password, since it is
	// the only one that could exist.
	if err := h.Svc.Sessions.RevokeAllForUser(c.Context(), uid, "password_change"); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// secure comes from config.CookieSecure (AppBaseURL's scheme), not
// c.Protocol() — see that field's doc comment for why request-time protocol
// detection isn't reliable here.
func setAuthCookie(c *fiber.Ctx, token string, ttl time.Duration, secure bool) {
	c.Cookie(&fiber.Cookie{
		Name:     "access_token",
		Value:    token,
		HTTPOnly: true,
		SameSite: "Lax",
		Secure:   secure,
		Path:     "/",
		Expires:  time.Now().Add(ttl),
	})
}

func clearAuthCookie(c *fiber.Ctx, secure bool) {
	c.Cookie(&fiber.Cookie{
		Name:     "access_token",
		Value:    "",
		HTTPOnly: true,
		SameSite: "Lax",
		Secure:   secure,
		Path:     "/",
		Expires:  time.Now().Add(-time.Hour),
	})
}
