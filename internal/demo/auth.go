package demo

import (
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"ta-payment-back/internal/auth"
	"ta-payment-back/internal/handler"
	"ta-payment-back/internal/service"
)

// CookieName, BasePathCookieName, and CookiePath are exported so main.go's
// clear-demo-cookies-on-real-login middleware can reference them directly
// instead of duplicating the literal cookie names — a copy that could
// silently drift out of sync with these if either ever changed.
//
// CookieName is deliberately NOT "access_token". Login (see AuthHandler.Login
// in internal/handler/auth.go) sets that name with Path "/" — root-scoped,
// on purpose, so it covers every real route. If a demo login reused it, the
// browser would silently overwrite whichever cookie was set more recently:
// an officer testing the sandbox in the same tab they do real work in would
// find themselves logged out of production, or the demo session clobbered by
// their own next real login, with no error to explain either. A different
// name sidesteps the collision outright rather than relying on Path scoping
// to prevent it.
const CookieName = "demo_access_token"

// BasePathCookieName carries which slot's routes this login belongs to
// (e.g. "/api/demo/w/3") to the ONE other piece of this app that cannot get
// it any other way: ta_payment_front/app/lib/session.ts's server-side
// getMe()/requireRole(), which run on the Next.js server for every page
// under a role layout (app/staff/layout.tsx and friends all call
// requireRole before rendering ANYTHING). The browser-side api.ts instead
// gets base_path directly from POST /demo/enter's JSON body — this cookie
// exists only for the server-rendering path that call can't reach.
const BasePathCookieName = "demo_base_path"

// CookiePath is root ("/"), not apiRoot, DELIBERATELY — unlike
// apiRoot/CookieName's own reasoning: session.ts's server-side checks run
// against arbitrary page paths ("/staff", "/lecturer/courses/x", ...), and a
// cookie is only ever attached by the browser to a request whose path it is
// a PREFIX of. Scoping to apiRoot ("/api/demo") would mean neither cookie
// ever reaches a page-route request at all, breaking every role layout's
// server-side guard. Root scoping is safe here — unlike reusing the NAME
// "access_token" at Path "/" would be — because both demo cookies use their
// own distinct names, so nothing about them can be confused with, or
// overwrite, the real "access_token" cookie at any path.
const CookiePath = "/"

func setDemoCookies(c *fiber.Ctx, token, basePath string, ttl time.Duration, secure bool) {
	c.Cookie(&fiber.Cookie{
		Name:     CookieName,
		Value:    token,
		HTTPOnly: true,
		SameSite: "Lax",
		Secure:   secure,
		Path:     CookiePath,
		Expires:  time.Now().Add(ttl),
	})
	c.Cookie(&fiber.Cookie{
		Name:     BasePathCookieName,
		Value:    basePath,
		HTTPOnly: true,
		SameSite: "Lax",
		Secure:   secure,
		Path:     CookiePath,
		Expires:  time.Now().Add(ttl),
	})
}

func clearDemoCookies(c *fiber.Ctx, secure bool) {
	for _, name := range []string{CookieName, BasePathCookieName} {
		c.Cookie(&fiber.Cookie{
			Name:     name,
			Value:    "",
			HTTPOnly: true,
			SameSite: "Lax",
			Secure:   secure,
			Path:     CookiePath,
			Expires:  time.Now().Add(-time.Hour),
		})
	}
}

// realSessionCookieName/Path mirror internal/handler/auth.go's own
// setAuthCookie exactly ("access_token", Path "/") — duplicated as literals
// rather than imported, since they are unexported there. Cleared on demo
// login so a browser that was ALSO signed into production in this same
// profile doesn't keep BOTH cookies alive at once: session.ts's
// resolveSession() (ta_payment_front/app/lib/session.ts) has to pick one
// when a server-rendered page reads them both, and a stale winner there is
// exactly the "logs in fine, then every page 404s" bug this fixes — see the
// symmetric clearDemoCookies call added to real login/logout in main.go.
func clearRealSessionCookie(c *fiber.Ctx, secure bool) {
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

func extractDemoToken(c *fiber.Ctx) string {
	if h := c.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return c.Cookies(CookieName)
}

// DemoAuthenticated is handler.Authenticated's demo counterpart: same shape
// (parse the token, stash user id / roles / session id on c.Locals under
// handler's own exported keys), different source cookie. Using handler's
// keys — CtxUserID etc. are exported specifically so identity flows through
// them — means every existing handler in internal/handler that calls
// handler.UserID(c)/handler.Roles(c)/handler.SessionID(c) works completely
// unmodified when reached through a demo slot's route tree; only THIS
// function and AccountGuard sit between the request and the token, and
// AccountGuard needs no changes at all (it already takes the Container as a
// parameter — see MountAll passing slot.Container per slot).
func DemoAuthenticated(tokens *auth.TokenService) fiber.Handler {
	return func(c *fiber.Ctx) error {
		raw := extractDemoToken(c)
		if raw == "" {
			return fiber.NewError(fiber.StatusUnauthorized, "missing token")
		}
		claims, err := tokens.Parse(raw)
		if err != nil {
			return fiber.NewError(fiber.StatusUnauthorized, "invalid token")
		}
		c.Locals(string(handler.CtxUserID), claims.UserID)
		c.Locals(string(handler.CtxRoles), claims.Roles)
		sid, _ := claims.SessionID()
		c.Locals(string(handler.CtxSessionID), sid)
		return c.Next()
	}
}

// LoginHandler is AuthHandler's demo counterpart (internal/handler/auth.go),
// kept as its own small type — rather than reusing AuthHandler directly —
// for exactly one reason: AuthHandler.Login calls the unexported
// setAuthCookie, which is not swappable from outside package handler. Every
// other line mirrors AuthHandler.Login/Logout call for call.
//
// DELIBERATELY NOT mirrored: AuthHandler.Login's mandatory-2FA branch (the
// TOTPEnabled check that returns mfa_required instead of a session). This
// is a full COPY, not a delegation, so that omission does not happen
// automatically — it happens because seeded demo accounts (staff@demo.local
// and friends) never enrol in TOTP, and the demo login password is public
// knowledge by design (see docs/PLAN-demo-sandbox.md), so a second factor on
// top of it would protect nothing while blocking every external tester.
// AccountGuard's own IsDemoSlot check is the belt-and-suspenders version of
// this same call — see its doc comment — but if a future edit ever adds 2FA
// logic HERE too, on a slot Container whose IsDemoSlot is already true, it
// would simply never trigger. If that ever needs to change, change
// IsDemoSlot's scope, not this function.
type LoginHandler struct {
	Svc       *service.Container
	Tokens    *auth.TokenService
	BasePath  string
	Manager   *Manager
	SlotIndex int
}

type loginReq struct {
	Email    string `json:"email" validate:"required,email"`
	Password string `json:"password" validate:"required"`
}

func (h *LoginHandler) Login(c *fiber.Ctx) error {
	var in loginReq
	if err := handler.Bind(c, &in); err != nil {
		return err
	}
	// The real access-control check, not the account-picker filtering
	// /api/demo/enter already did — that only hides cards the tester
	// shouldn't click, which stops nobody who opens devtools and POSTs
	// straight here. Resolved fresh per request (never cached) against
	// whoever CURRENTLY owns this slot, so a tier change or revocation in
	// demo_authorized_testers takes effect on this very login attempt.
	tier, err := h.Manager.TierForSlotIndex(c.Context(), h.SlotIndex)
	if err != nil {
		if errors.Is(err, ErrNotAuthorized) {
			return fiber.NewError(fiber.StatusForbidden, "สิทธิ์ทดสอบของคุณถูกยกเลิกแล้ว กรุณาติดต่อเจ้าหน้าที่")
		}
		return err
	}
	if !TierAllowsAccountEmail(tier, in.Email) {
		return fiber.NewError(fiber.StatusForbidden, "บัญชีนี้อยู่นอกเหนือระดับสิทธิ์ทดสอบของคุณ")
	}
	u, err := h.Svc.Users.Authenticate(c.Context(), in.Email, in.Password, c.IP(), c.Get("User-Agent"))
	if err != nil {
		return err
	}
	tokenRoles := u.Roles
	if u.IsExecutive {
		tokenRoles = append(append([]string{}, u.Roles...), "executive")
	}
	sessionID, err := h.Svc.Sessions.CreateAndSupersede(c.Context(), u.ID, h.Svc.Cfg.JWTLifetime, c.IP(), c.Get("User-Agent"))
	if err != nil {
		return err
	}
	tok, err := h.Tokens.Issue(u.ID, tokenRoles, sessionID, h.Svc.Cfg.JWTLifetime)
	if err != nil {
		return err
	}
	setDemoCookies(c, tok, h.BasePath, h.Svc.Cfg.JWTLifetime, h.Svc.Cfg.CookieSecure)
	// Entering demo mode supersedes any real production session this browser
	// also happens to hold — see clearRealSessionCookie's doc comment.
	clearRealSessionCookie(c, h.Svc.Cfg.CookieSecure)
	// Best-effort: a successful login is the strongest "still in use" signal
	// Claim's idle-reclaim fallback has (see Manager.TouchSlotActivity) —
	// never worth failing an otherwise-successful login over.
	_ = h.Manager.TouchSlotActivity(c.Context(), h.SlotIndex)
	return c.JSON(fiber.Map{"user": u})
}

func (h *LoginHandler) Logout(c *fiber.Ctx) error {
	if raw := extractDemoToken(c); raw != "" {
		if claims, err := h.Tokens.Parse(raw); err == nil {
			if sid, err := claims.SessionID(); err == nil {
				_ = h.Svc.Sessions.Revoke(c.Context(), sid, "logout")
			}
		}
	}
	clearDemoCookies(c, h.Svc.Cfg.CookieSecure)
	return c.JSON(fiber.Map{"ok": true})
}
