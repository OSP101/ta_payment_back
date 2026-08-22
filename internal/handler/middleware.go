package handler

import (
	"crypto/subtle"
	"errors"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/auth"
	"ta-payment-back/internal/rbac"
	"ta-payment-back/internal/service"
)

type ctxKey string

const (
	CtxUserID             ctxKey = "user_id"
	CtxRoles              ctxKey = "roles"
	CtxSessionID          ctxKey = "session_id"
	CtxSelectedEnrollment ctxKey = "selected_enrollment_id"
)

// Authenticated attaches user id, roles and session id from JWT (cookie or
// Authorization header). It does not itself check whether the session is
// still valid — that needs a DB round trip and is AccountGuard's job, so a
// route with Authenticated but no AccountGuard (there are none today, but
// nothing stops one being added) would only get identity, not liveness.
func Authenticated(tokens *auth.TokenService) fiber.Handler {
	return func(c *fiber.Ctx) error {
		raw := extractToken(c)
		if raw == "" {
			return fiber.NewError(fiber.StatusUnauthorized, "missing token")
		}
		claims, err := tokens.Parse(raw)
		if err != nil {
			return fiber.NewError(fiber.StatusUnauthorized, "invalid token")
		}
		c.Locals(string(CtxUserID), claims.UserID)
		c.Locals(string(CtxRoles), claims.Roles)
		// A token issued before migration 0085 (or otherwise missing its jti)
		// parses to uuid.Nil here rather than failing the request — AccountGuard
		// then finds no matching session row and rejects it as session_revoked,
		// the same clean "please log in again" as any other revoked session.
		sid, _ := claims.SessionID()
		c.Locals(string(CtxSessionID), sid)
		return c.Next()
	}
}

func RequireRole(roles ...string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		got, _ := c.Locals(string(CtxRoles)).([]string)
		if !rbac.Has(got, roles...) {
			return fiber.NewError(fiber.StatusForbidden, "forbidden")
		}
		return c.Next()
	}
}

// RequireExecutiveView guards the read-only budget-analytics endpoints. The
// synthetic executive role travels in the JWT (see Login), but the flag is
// ticked by staff while the lecturer may already hold a 12-hour token — so a
// claims miss falls back to the live users.is_executive read, and the grant
// works immediately instead of after the next login. Revocation still waits
// for token expiry only when the claims carry the role; the fallback path
// re-reads every request.
func RequireExecutiveView(pool *pgxpool.Pool) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleExecutive) {
			return c.Next()
		}
		var ok bool
		_ = pool.QueryRow(c.Context(),
			`SELECT is_executive FROM users WHERE id = $1 AND deleted_at IS NULL`,
			UserID(c)).Scan(&ok)
		if ok {
			return c.Next()
		}
		return fiber.NewError(fiber.StatusForbidden, "forbidden")
	}
}

// AccountGuard runs after Authenticated on every protected route. It re-reads
// the user's live state from the DB so that (a) a deactivated or deleted
// account loses access immediately instead of when its 12h token expires, and
// (b) a user with a pending forced password change cannot use feature
// endpoints until they change it. The /me and /me/password endpoints stay
// reachable so the change flow itself can complete.
//
// It also enforces single-device login and the 15-minute idle timeout (see
// migration 0085 and SessionService): one SELECT joins users to this
// request's session row (by the JWT's jti — see Authenticated) so both
// checks cost one round trip on the common path. A session that is revoked
// (superseded by another login, or by a password change / deactivation /
// role edit — see handlers) or has gone idle past SessionService.IdleTimeout
// fails the request with a distinct error code so the frontend can show the
// right message instead of a generic "log in again":
//
//	session_superseded — signed in elsewhere; this device lost the race
//	session_idle        — no activity for 15 minutes
//	session_revoked      — anything else (logout, password change, …)
//
// A request that IS allowed through still needs a write sometimes: mutating
// requests (and POST /auth/heartbeat, which is a POST) touch
// last_activity_at so the session's idle clock resets. GET requests do not —
// a tab left open on a read-only page, or background polling, must not by
// itself keep a dead session alive.
// mfaMandatoryFor decides whether the mandatory-2FA-enrolment tier applies to
// a request carrying roles (from JWT claims) and isExecutive (read live from
// the DB — see AccountGuard's own comment on why this one field can't be
// trusted from the token). Pulled out of AccountGuard as its own function so
// the tier logic — the part product policy actually changes, e.g. "should
// lecturer be mandatory too" — has one answer that's unit-testable without
// standing up a full service.Container and an HTTP round trip.
func mfaMandatoryFor(roles []string, isExecutive bool) bool {
	return rbac.Has(roles, rbac.RoleAdmin, rbac.RoleStaff) || isExecutive
}

func AccountGuard(svc *service.Container) fiber.Handler {
	return func(c *fiber.Ctx) error {
		p := c.Path()
		uid := UserID(c)
		sid := SessionID(c)

		var isActive, mustChange, isExecutive bool
		var totpEnabledAt *time.Time
		var sessRevokedAt *time.Time
		var sessRevokeReason *string
		var sessLastActivity *time.Time
		var selectedEnrollmentID *uuid.UUID
		err := svc.Pool.QueryRow(c.Context(),
			`SELECT u.is_active, u.must_change_password, u.is_executive, u.totp_enabled_at,
			        s.revoked_at, s.revoke_reason, s.last_activity_at, s.selected_enrollment_id
			 FROM users u
			 LEFT JOIN sessions s ON s.id = $2
			 WHERE u.id = $1 AND u.deleted_at IS NULL`,
			uid, sid).Scan(&isActive, &mustChange, &isExecutive, &totpEnabledAt,
			&sessRevokedAt, &sessRevokeReason, &sessLastActivity, &selectedEnrollmentID)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "บัญชีนี้ไม่สามารถใช้งานได้"})
		}
		if err != nil {
			return err
		}
		if !isActive {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "บัญชีนี้ถูกปิดการใช้งาน"})
		}

		switch {
		case sessLastActivity == nil:
			// No matching session row at all — deleted, or a pre-migration/
			// jti-less token (see Authenticated). Same outcome either way.
			return sessionRejected(c, "session_revoked")
		case sessRevokedAt != nil && sessRevokeReason != nil && *sessRevokeReason == "superseded":
			return sessionRejected(c, "session_superseded")
		case sessRevokedAt != nil:
			return sessionRejected(c, "session_revoked")
		case time.Since(*sessLastActivity) > service.IdleTimeout:
			_ = svc.Sessions.MarkIdle(c.Context(), sid)
			return sessionRejected(c, "session_idle")
		}

		if mustChange &&
			!strings.HasSuffix(p, "/me") &&
			!strings.HasSuffix(p, "/me/password") &&
			!strings.HasSuffix(p, "/auth/heartbeat") {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "password_change_required"})
		}

		// Mandatory 2FA enrolment for admin/staff/executive accounts. Checked
		// AFTER mustChange, not before: a brand-new admin can have BOTH
		// pending (must_change_password from CreateUserInput, no TOTP yet
		// enrolled), and resolving password-change first means the two forced
		// flows never have to be reconciled in one screen — see
		// docs/SECURITY.md for why this ordering is load-bearing, not
		// incidental.
		//
		// mustEnroll reads roles from the JWT (Roles(c)), NOT a fresh query —
		// see Authenticated, which puts them there — because a role EDIT
		// already revokes every session for that user (roles_changed, see
		// UserHandler.Update), so a stale claim here is not possible: by the
		// time a promoted admin/staff account is making requests again, it is
		// on a freshly issued token. isExecutive is the one exception and IS
		// read live above: RequireExecutiveView already established that
		// is_executive can change without a session revocation (unlike
		// user_roles), so trusting a token claim for it would let a freshly
		// flagged executive skip enrolment for up to a full JWTLifetime.
		//
		// svc.Cfg.IsDemoSlot, NOT svc.Cfg.DemoMode — see that field's own doc
		// comment. DemoMode is true on the real production Config too
		// whenever the sandbox is merely switched on; branching on it here
		// would silently turn mandatory 2FA off for every real admin/staff
		// account the moment DEMO_MODE=true.
		//
		// MFAMandatoryEnforced is the separate operational kill switch (see
		// its own doc comment) — MFA_MANDATORY_ENFORCED=false lets a
		// mandatory-tier account without TOTP through without touching
		// anything for an account that already has 2FA on: the second
		// factor is still checked at login (LoginTwoFactor) regardless of
		// this flag, which only controls whether NOT having one yet blocks
		// every other endpoint.
		mustEnroll := svc.Cfg.MFAMandatoryEnforced && !svc.Cfg.IsDemoSlot &&
			totpEnabledAt == nil && mfaMandatoryFor(Roles(c), isExecutive)
		if mustEnroll &&
			!strings.HasSuffix(p, "/me") &&
			!strings.Contains(p, "/me/2fa/") &&
			!strings.HasSuffix(p, "/auth/heartbeat") {
			// strings.Contains, not HasPrefix: demo mounts these same routes
			// under /api/demo/w/<n>/me/2fa/... (see MountAll) — a prefix
			// test would silently fail to exempt them there. Moot while
			// IsDemoSlot already skips this branch entirely for every demo
			// slot, but keeping the match style consistent with the
			// mustChange allowlist above means this stays correct if that
			// ever changes.
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "mfa_setup_required"})
		}

		c.Locals(string(CtxSelectedEnrollment), selectedEnrollmentID)

		if c.Method() != fiber.MethodGet && c.Method() != fiber.MethodHead {
			if err := svc.Sessions.Touch(c.Context(), sid); err != nil {
				return err
			}
		}
		return c.Next()
	}
}

// sessionRejected responds with the machine-readable code in the `error`
// field — same convention as RequireApprovedTAProfile's ta_profile_not_approved
// and AccountGuard's own password_change_required below: the frontend's
// CODE_MESSAGES table (app/lib/api.ts) turns the code into the Thai message the
// user sees, rather than the backend hardcoding English or Thai text here.
func sessionRejected(c *fiber.Ctx, code string) error {
	return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": code})
}

func UserID(c *fiber.Ctx) uuid.UUID {
	v, _ := c.Locals(string(CtxUserID)).(uuid.UUID)
	return v
}

func Roles(c *fiber.Ctx) []string {
	v, _ := c.Locals(string(CtxRoles)).([]string)
	return v
}

// SessionID returns the sessions.id (JWT jti) this request authenticated
// with — uuid.Nil for a token issued before migration 0085 or otherwise
// missing one (see Authenticated).
func SessionID(c *fiber.Ctx) uuid.UUID {
	v, _ := c.Locals(string(CtxSessionID)).(uuid.UUID)
	return v
}

// SelectedEnrollmentID returns the ta_enrollments period (migration 0094)
// this TA session is currently "viewing" — nil when unset (no selection made,
// or the TA has zero/one period and was never asked). Set by AccountGuard
// from sessions.selected_enrollment_id on every request, so a change made via
// EnrollmentService.SetSessionScope takes effect on the very next request
// with no re-login. Display-scoping only — see that column's own doc
// comment for why this never gates what a NEW assignment attaches to.
func SelectedEnrollmentID(c *fiber.Ctx) *uuid.UUID {
	v, _ := c.Locals(string(CtxSelectedEnrollment)).(*uuid.UUID)
	return v
}

func extractToken(c *fiber.Ctx) string {
	if h := c.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return c.Cookies("access_token")
}

// RequireApprovedTAProfile blocks TA-scoped features until the TA's profile
// (and required documents) have been approved by staff. Non-TA roles pass
// through untouched — the gate only applies when TA is the acting role.
//
// A 403 with error code "ta_profile_not_approved" is returned so the FE can
// distinguish it from a plain permission failure and redirect to the profile
// page instead of showing a generic error.
func RequireApprovedTAProfile(pool *pgxpool.Pool) fiber.Handler {
	return func(c *fiber.Ctx) error {
		roles := Roles(c)
		// Only apply the gate when the user acts as TA. Staff/admin/lecturer
		// may hit shared endpoints without a ta_profile row.
		if !rbac.Has(roles, rbac.RoleTA) {
			return c.Next()
		}
		if rbac.Has(roles, rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer) {
			return c.Next()
		}
		uid := UserID(c)
		var status string
		err := pool.QueryRow(c.Context(),
			`SELECT status::text FROM ta_profiles WHERE user_id = $1`, uid).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"error":  "ta_profile_not_approved",
				"status": "pending",
			})
		}
		if err != nil {
			return err
		}
		if status != "approved" {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"error":  "ta_profile_not_approved",
				"status": status,
			})
		}
		return c.Next()
	}
}

// ErrorHandler is the single place where errors become HTTP responses. It maps
// service sentinels and UserError values to their proper status codes, converts
// database errors into safe generic messages (never leaking SQL/constraint
// text), and treats any other plain business error as a 400 with its Thai
// message. Genuine internal failures are logged server-side and returned as a
// generic 500.
func ErrorHandler(c *fiber.Ctx, err error) error {
	// Explicit fiber errors (auth, bad body, etc.).
	var fe *fiber.Error
	if errors.As(err, &fe) {
		return c.Status(fe.Code).JSON(fiber.Map{"error": fe.Message})
	}

	// User-facing business errors with an explicit status.
	var ue *service.UserError
	if errors.As(err, &ue) {
		return c.Status(ue.Status).JSON(fiber.Map{"error": ue.Msg})
	}

	// Service sentinels.
	switch {
	case errors.Is(err, service.ErrNotFound):
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "ไม่พบข้อมูลที่ต้องการ"})
	case errors.Is(err, service.ErrForbidden):
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "คุณไม่มีสิทธิ์ดำเนินการนี้"})
	case errors.Is(err, service.ErrConflict):
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "ข้อมูลขัดแย้งกับสถานะปัจจุบัน"})
	case errors.Is(err, service.ErrInvalidInput):
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "ข้อมูลไม่ถูกต้อง"})
	case errors.Is(err, pgx.ErrNoRows):
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "ไม่พบข้อมูลที่ต้องการ"})
	}

	// Database errors — log the real detail, return a safe generic message.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		log.Printf("db error [%s] on %s %s: %s", pgErr.Code, c.Method(), c.Path(), pgErr.Message)
		switch pgErr.Code {
		case "23505": // unique_violation
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "ข้อมูลนี้มีอยู่แล้วในระบบ"})
		case "23503": // foreign_key_violation
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "ไม่สามารถดำเนินการได้เพราะมีข้อมูลอ้างอิงอยู่"})
		case "23514", "22001", "22003", "22007", "22P02": // check/length/numeric/datetime/text-repr
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "ข้อมูลที่กรอกไม่ถูกต้องตามรูปแบบที่กำหนด"})
		default:
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "ระบบขัดข้อง กรุณาลองใหม่ภายหลัง"})
		}
	}

	// Any remaining plain error is treated as a user-facing business message
	// (services raise these via errors.New/fmt.Errorf with Thai text). These are
	// never database errors — those are handled above — so it is safe to show.
	return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
}

// OriginCheck is defense-in-depth against cross-site request forgery,
// layered UNDER the cookie's own SameSite=Lax (see setAuthCookie) rather
// than replacing it — SameSite=Lax already blocks the classic cross-site
// <form> POST, since a Lax cookie is only sent on top-level GET navigations.
// This exists for the cases that guarantee does not cover: a future
// subdomain relationship that changes what counts as "the same site", or the
// documented cross-origin upload path (UPLOAD_ORIGIN in
// ta_payment_front/app/lib/api.ts) being pointed somewhere it shouldn't.
//
// allowedOrigins is CORS_ORIGINS (comma-separated) — the same list already
// governing what fetch() from a browser may read a response from, reused
// here as the answer to "who is allowed to submit a mutating request" too.
//
// Only mutating methods are checked; GET/HEAD/OPTIONS carry no state-changing
// risk. And only when the browser actually sent an Origin or Referer header:
// on the NORMAL path here — browser → Next.js → this process — Next.js's own
// server-side fetch proxying that rewrite typically carries neither header at
// all, since it is not a browser-originated cross-origin request. Rejecting
// on absence would break that path; SameSite=Lax is the primary defense for
// exactly the traffic this cannot see into.
func OriginCheck(allowedOrigins string) fiber.Handler {
	allowed := map[string]bool{}
	for _, o := range strings.Split(allowedOrigins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			allowed[o] = true
		}
	}
	return func(c *fiber.Ctx) error {
		switch c.Method() {
		case fiber.MethodGet, fiber.MethodHead, fiber.MethodOptions:
			return c.Next()
		}
		origin := c.Get(fiber.HeaderOrigin)
		if origin == "" {
			// Some clients send Referer but not Origin; take its origin as a
			// fallback rather than leaving this unchecked.
			if ref := c.Get(fiber.HeaderReferer); ref != "" {
				if u, err := url.Parse(ref); err == nil && u.Scheme != "" && u.Host != "" {
					origin = u.Scheme + "://" + u.Host
				}
			}
		}
		if origin == "" {
			return c.Next()
		}
		if !allowed[origin] {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "คำขอถูกปฏิเสธ (แหล่งที่มาไม่ได้รับอนุญาต)"})
		}
		return c.Next()
	}
}

// VerifyTDBMWebhookSecret gates POST /tdbm-webhook — the one route in this
// app called by another SYSTEM rather than a signed-in user, so it has no JWT
// to check. TDBM's webhook design (see docs/TDBM-API-requirements.md) carries
// no signature of its own, so the shared secret below is the only thing
// stopping anyone who finds the URL from making us re-pull TDBM on demand.
//
// Same "closed by default" posture as BOT_API_CLIENT_ID: an unset secret
// refuses every call (503, "not configured") rather than accepting an
// unauthenticated trigger because nobody got around to setting one.
// subtle.ConstantTimeCompare avoids leaking the secret's length/prefix via
// response-time differences on a wrong guess.
func VerifyTDBMWebhookSecret(secret string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if secret == "" {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "tdbm webhook not configured"})
		}
		got := c.Get("X-TDBM-Webhook-Secret")
		if len(got) != len(secret) || subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid webhook secret"})
		}
		return c.Next()
	}
}
