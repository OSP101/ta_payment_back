package handler

import (
	"errors"
	"log"
	"strings"

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
	CtxUserID ctxKey = "user_id"
	CtxRoles  ctxKey = "roles"
)

// Authenticated attaches user id and roles from JWT (cookie or Authorization header).
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
// the user's live state from the DB so that (a) a deactivated or deleted account
// loses access immediately instead of when its 12h token expires, and (b) a user
// with a pending forced password change cannot use feature endpoints until they
// change it. The /me and /me/password endpoints stay reachable so the change
// flow itself can complete.
func AccountGuard(pool *pgxpool.Pool) fiber.Handler {
	return func(c *fiber.Ctx) error {
		p := c.Path()
		var isActive, mustChange bool
		err := pool.QueryRow(c.Context(),
			`SELECT is_active, must_change_password FROM users WHERE id=$1 AND deleted_at IS NULL`,
			UserID(c)).Scan(&isActive, &mustChange)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "บัญชีนี้ไม่สามารถใช้งานได้"})
		}
		if err != nil {
			return err
		}
		if !isActive {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "บัญชีนี้ถูกปิดการใช้งาน"})
		}
		if mustChange &&
			!strings.HasSuffix(p, "/me") &&
			!strings.HasSuffix(p, "/me/password") {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "password_change_required"})
		}
		return c.Next()
	}
}

func UserID(c *fiber.Ctx) uuid.UUID {
	v, _ := c.Locals(string(CtxUserID)).(uuid.UUID)
	return v
}

func Roles(c *fiber.Ctx) []string {
	v, _ := c.Locals(string(CtxRoles)).([]string)
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
