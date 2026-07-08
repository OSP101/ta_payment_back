package handler

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/auth"
	"ta-payment-back/internal/rbac"
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

func ErrorHandler(c *fiber.Ctx, err error) error {
	var fe *fiber.Error
	if errors.As(err, &fe) {
		return c.Status(fe.Code).JSON(fiber.Map{"error": fe.Message})
	}
	return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
}
