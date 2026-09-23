package handler

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

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
	return h.finishLogin(c, u, false)
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
	return h.finishLogin(c, u, c.Cookies(ssoPendingCookie) == "1")
}

// finishLogin mints the session, token and cookie for an already-fully-
// authenticated user (password alone when 2FA is off, or password+code when
// it's on) and returns the same {"user": ...} body either path used to
// return directly.
func (h *AuthHandler) finishLogin(c *fiber.Ctx, u *service.User, viaSSO bool) error {
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
	setSSOCookie(c, viaSSO, h.Svc.Cfg.JWTLifetime, h.Svc.Cfg.CookieSecure)
	// The token lives only in the HttpOnly cookie — it is deliberately NOT
	// returned in the body so it can't be stashed in localStorage where XSS
	// could read it.
	return c.JSON(fiber.Map{"user": u})
}

// SSOURL tells the login page whether to show the KKU button and where it
// goes. See service.SSOService for the whole flow; the browser is sent to
// KKU directly, nothing on our side happens until the callback.
func (h *AuthHandler) SSOURL(c *fiber.Ctx) error {
	// Config-derived, not user-specific, but still not safe for a shared
	// cache to serve stale: SSOEnabled can change between one request and
	// the next (env update + restart), and a cached "enabled: false" would
	// lock a browser out of SSO until the cache expired.
	c.Set("Cache-Control", "no-store")
	if !h.Svc.SSO.Enabled() {
		return c.JSON(fiber.Map{"enabled": false})
	}
	// logout_url is for the login page's "use another KKU account": leaving
	// the confirm card for /login keeps the KKU session alive, and the next
	// KKU click signs the same person straight back in. Going through KKU's
	// logout (which returns to our registered logout callback, /login) is the
	// only way to actually switch account on a shared machine.
	return c.JSON(fiber.Map{"enabled": true, "url": h.Svc.SSO.LoginURL(), "logout_url": h.Svc.SSO.LogoutURL()})
}

type ssoExchangeReq struct {
	Code string `json:"code" validate:"required"`
}

// ssoRejectedMsg covers a stale, reused or mis-issued code alike — the
// remedy is the same (start again), and which one it was is KKU's business.
const ssoRejectedMsg = "ลิงก์เข้าสู่ระบบหมดอายุหรือถูกใช้ไปแล้ว กรุณากดเข้าสู่ระบบด้วย KKU อีกครั้ง"

// SSOExchange is step 1 of the SSO login (POST /auth/sso/exchange): the
// callback page hands over the code KKU appended to its URL, and gets back
// a confirm ticket plus whose account it resolved to — no session yet. The
// unknown-account case is a 403 that names the KKU email, because "which
// address do I ask staff to register" is the one thing that person needs.
func (h *AuthHandler) SSOExchange(c *fiber.Ctx) error {
	if !h.Svc.SSO.Enabled() {
		return fiber.NewError(fiber.StatusNotFound, "SSO is not enabled")
	}
	var in ssoExchangeReq
	if err := Bind(c, &in); err != nil {
		return err
	}
	p, err := h.Svc.SSO.Exchange(c.Context(), in.Code, c.IP(), c.Get("User-Agent"))
	if err != nil {
		var noAcct *service.SSONoAccountError
		switch {
		case errors.Is(err, service.ErrSSORejected):
			return fiber.NewError(fiber.StatusUnauthorized, ssoRejectedMsg)
		case errors.As(err, &noAcct):
			// The address goes in the message itself: the frontend's
			// ApiError only carries `error`, and the address is the one
			// thing this person needs to show staff.
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"error":     "บัญชี " + noAcct.Email + " ยังไม่ได้ลงทะเบียนในระบบ กรุณาติดต่อเจ้าหน้าที่วิทยาลัยเพื่อเปิดใช้งานด้วยอีเมลนี้",
				"code":      "sso_no_account",
				"kku_email": noAcct.Email,
			})
		}
		return err
	}
	c.Set("Cache-Control", "no-store")
	return c.JSON(p)
}

type ssoConfirmReq struct {
	Ticket string `json:"ticket" validate:"required"`
}

// SSOConfirm is step 2 (POST /auth/sso/confirm): the confirm click. From
// here on it is a password login that has just passed the password check —
// including the TOTP branch. A KKU assertion says who this is; it says
// nothing about possession of the account's second factor, so the branch is
// copied from Login rather than skipped.
func (h *AuthHandler) SSOConfirm(c *fiber.Ctx) error {
	if !h.Svc.SSO.Enabled() {
		return fiber.NewError(fiber.StatusNotFound, "SSO is not enabled")
	}
	var in ssoConfirmReq
	if err := Bind(c, &in); err != nil {
		return err
	}
	u, err := h.Svc.SSO.Redeem(c.Context(), in.Ticket, c.IP(), c.Get("User-Agent"))
	if err != nil {
		if errors.Is(err, service.ErrSSOTicketInvalid) {
			return fiber.NewError(fiber.StatusUnauthorized, ssoRejectedMsg)
		}
		return err
	}
	if u.TOTPEnabled {
		challenge, err := h.Svc.MFA.IssueChallenge(c.Context(), u.ID)
		if err != nil {
			return err
		}
		// Carry "this login came through KKU" across the 2FA round trip so
		// the eventual session is marked for Logout — see ssoPendingCookie.
		c.Cookie(&fiber.Cookie{
			Name: ssoPendingCookie, Value: "1", HTTPOnly: true, SameSite: "Lax",
			Secure: h.Svc.Cfg.CookieSecure, Path: "/", Expires: time.Now().Add(service.MFAChallengeTTL),
		})
		return c.JSON(fiber.Map{"mfa_required": true, "challenge": challenge})
	}
	return h.finishLogin(c, u, true)
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
	// A session that came in through KKU should go out through KKU too —
	// see ssonext.Client.LogoutURL for the shared-machine reason. The
	// frontend navigates there instead of to /login; KKU then sends the
	// browser back to our registered logout callback.
	viaSSO := c.Cookies(ssoSessionCookie) == "1"
	setSSOCookie(c, false, 0, h.Svc.Cfg.CookieSecure)
	if viaSSO && h.Svc.SSO.Enabled() {
		return c.JSON(fiber.Map{"ok": true, "sso_logout_url": h.Svc.SSO.LogoutURL()})
	}
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

// meResponse embeds service.User (its fields flatten to the JSON top level,
// same as if they were declared directly) plus the one piece of state that
// belongs to the SESSION, not the account: which ta_enrollments period this
// login is currently viewing (migration 0096). Read straight from
// SelectedEnrollmentID(c) — already fetched by AccountGuard's own combined
// query this request, no extra DB round trip.
type meResponse struct {
	*service.User
	SelectedEnrollmentID *uuid.UUID `json:"selected_enrollment_id,omitempty"`
}

func (h *AuthHandler) Me(c *fiber.Ctx) error {
	u, err := h.Svc.Users.Get(c.Context(), UserID(c))
	if err != nil {
		return err
	}
	return c.JSON(meResponse{User: u, SelectedEnrollmentID: SelectedEnrollmentID(c)})
}

// DataExport answers the PDPA "what do you have on me" request — see
// UserService.ExportMyData. Requesting your own export is itself audit-worthy,
// the same reasoning DocsService.RevealCitizenID already applies to reading
// PII back out.
func (h *AuthHandler) DataExport(c *fiber.Ctx) error {
	uid := UserID(c)
	out, err := h.Svc.Users.ExportMyData(c.Context(), uid)
	if err != nil {
		return err
	}
	if err := h.Svc.Auditor.Log(c.Context(), audit.Entry{
		ActorID: &uid, Action: "user.data_export", Entity: "user", EntityID: uid.String(),
		IP: c.IP(), UserAgent: c.Get("User-Agent"),
	}); err != nil {
		return err
	}
	return c.JSON(out)
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
	uid := UserID(c)
	// All the business rules (current-password requirement for a voluntary
	// change, reuse rejection, and the fuller ValidatePassword rule) live in
	// UserService.ChangePassword so they're covered by service-level tests
	// without needing an HTTP harness — see internal/service/user.go.
	if err := h.Svc.Users.ChangePassword(c.Context(), uid, in.CurrentPassword, in.NewPassword); err != nil {
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

// ssoSessionCookie marks a session as having been opened through KKU SSO so
// Logout knows to end the KKU-side session too. ssoPendingCookie is the
// same idea for the gap between SSOConfirm returning mfa_required and
// LoginTwoFactor finishing — the mfa_challenges row has no column for it.
// Neither is secret (a yes/no about the login method) and neither is
// trusted for anything security-relevant: the worst a forged one does is
// send the browser to KKU's logout page.
const (
	ssoSessionCookie = "sso_session"
	ssoPendingCookie = "sso_pending"
)

func setSSOCookie(c *fiber.Ctx, on bool, ttl time.Duration, secure bool) {
	val, exp := "", time.Now().Add(-time.Hour)
	if on {
		val, exp = "1", time.Now().Add(ttl)
	}
	c.Cookie(&fiber.Cookie{
		Name: ssoSessionCookie, Value: val, HTTPOnly: true, SameSite: "Lax", Secure: secure, Path: "/", Expires: exp,
	})
	// The pending marker is done either way once a session exists.
	c.Cookie(&fiber.Cookie{
		Name: ssoPendingCookie, Value: "", HTTPOnly: true, SameSite: "Lax", Secure: secure, Path: "/", Expires: time.Now().Add(-time.Hour),
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
