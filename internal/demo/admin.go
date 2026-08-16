// admin.go registers the PRODUCTION-side endpoints staff use to manage
// demo_authorized_testers (see that migration's doc comment and tiers.go) —
// deliberately separate from Mount, which registers everything under
// apiRoot ("/api/demo") for people who have already gotten in. Who is
// ALLOWED in is a decision real, authenticated staff make from the real
// app (/staff/settings), so these routes live on the production router
// group instead, gated by the same RequireRole(admin, staff) every other
// staff-only endpoint uses.
//
// Kept inside package demo — rather than in internal/handler where the
// route tree otherwise lives — because everything demo-related stays
// together in one importable, independently-deployable package: it runs
// against its own separate database (config.DemoDatabaseURL) and mounts
// only when `cfg.DemoMode` is on, so an environment that doesn't want the
// sandbox at all (DemoMode=false) never registers these routes, never opens
// a connection to the demo database, and never needs it to exist.
package demo

import (
	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/auth"
	"ta-payment-back/internal/handler"
	"ta-payment-back/internal/rbac"
	"ta-payment-back/internal/service"
)

// MountAdmin wires GET/POST/DELETE /api/v1/demo-testers onto app, authed and
// role-gated exactly like any other staff-only production endpoint. Must run
// before app.Listen, same as Mount — see that function's doc comment.
//
// svc/tokens are PRODUCTION's — Authenticated/AccountGuard must check the
// real, authenticated staff session, and the one users-table lookup below
// (whose email is adding this tester) only exists in production. demoPool
// is the SEPARATE demo database (see config.DemoDatabaseURL) — where
// demo_authorized_testers actually lives; every call into authorized.go
// below uses demoPool, never svc.Pool, or it would query a table that
// simply isn't in production's database anymore. mgr lets removing a
// tester also release whatever demo slot they're holding, so revoking
// access gives the slot back to the pool instead of leaving it held by
// someone who can no longer log into it anyway.
func MountAdmin(app *fiber.App, svc *service.Container, tokens *auth.TokenService, demoPool *pgxpool.Pool, mgr *Manager) {
	adminOrStaff := handler.RequireRole(rbac.RoleAdmin, rbac.RoleStaff)
	testers := app.Group("/api/v1/demo-testers", handler.Authenticated(tokens), handler.AccountGuard(svc), adminOrStaff)

	testers.Get("", func(c *fiber.Ctx) error {
		items, err := ListAuthorizedTesters(c.Context(), demoPool)
		if err != nil {
			return err
		}
		return c.JSON(fiber.Map{"items": items})
	})

	testers.Post("", func(c *fiber.Ctx) error {
		var in struct {
			Email string `json:"email" validate:"required,email"`
			Tier  string `json:"tier" validate:"required"`
			Note  string `json:"note"`
		}
		if err := handler.Bind(c, &in); err != nil {
			return err
		}
		tier, ok := ParseTier(in.Tier)
		if !ok {
			return fiber.NewError(fiber.StatusBadRequest, "ระดับสิทธิ์ไม่ถูกต้อง")
		}
		// added_by is stored as an email (not the caller's user id) so the
		// settings list reads like "เพิ่มโดย ..." without a second lookup —
		// looked up here (from PRODUCTION's users table, where the caller's
		// account actually lives) rather than trusting a client-supplied name.
		var addedBy string
		if err := svc.Pool.QueryRow(c.Context(),
			`SELECT email FROM users WHERE id = $1`, handler.UserID(c),
		).Scan(&addedBy); err != nil {
			return err
		}
		if err := AddAuthorizedTester(c.Context(), demoPool, in.Email, tier, in.Note, addedBy); err != nil {
			return err
		}
		return c.JSON(fiber.Map{"ok": true})
	})

	testers.Delete("/:email", func(c *fiber.Ctx) error {
		email := c.Params("email")
		if err := RemoveAuthorizedTester(c.Context(), demoPool, email); err != nil {
			return err
		}
		released, err := mgr.Release(c.Context(), email)
		if err != nil {
			return err
		}
		return c.JSON(fiber.Map{"ok": true, "released_slot": released})
	})
}
