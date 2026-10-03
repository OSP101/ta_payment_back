package handler

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/auth"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/rbac"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/storage"
	"ta-payment-back/internal/testutil"
)

// Staff may reset 2FA for TA and lecturer accounts (help-desk case, 02/10/2026)
// but never for a privileged account — the same target rule as password reset,
// so the two resets cannot chain into taking over an admin.
func TestAdminReset2FA_StaffLimitedToNonPrivilegedTargets(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.NewContainer(pool, store, mail.New(config.Config{}), audit.New(pool), config.Config{}, nil, nil)

	const pw = "officer-test-password"
	hash, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	mkUser := func(role string) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, email, first_name, last_name, password_hash, totp_enabled_at)
			 VALUES ($1, $2, 'ท', 'ท', $3, NOW())`, id, id.String()+"@test.local", hash); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2::role_code)`, id, role); err != nil {
			t.Fatal(err)
		}
		return id
	}
	staffActor := mkUser(rbac.RoleStaff)
	adminActor := mkUser(rbac.RoleAdmin)
	otherAdmin := mkUser(rbac.RoleAdmin)
	otherStaff := mkUser(rbac.RoleStaff)
	ta := mkUser(rbac.RoleTA)
	lecturer := mkUser(rbac.RoleLecturer)

	h := &MFAHandler{Svc: svc}
	post := func(actor uuid.UUID, role string, target uuid.UUID) int {
		t.Helper()
		app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
		app.Post("/users/:id/2fa/reset", func(c *fiber.Ctx) error {
			c.Locals(string(CtxUserID), actor)
			c.Locals(string(CtxRoles), []string{role})
			return c.Next()
		}, h.AdminReset)
		req := httptest.NewRequest("POST", "/users/"+target.String()+"/2fa/reset", strings.NewReader(`{"password":"`+pw+`"}`))
		req.Header.Set("Content-Type", "application/json")
		res, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	for _, tc := range []struct {
		name   string
		actor  uuid.UUID
		role   string
		target uuid.UUID
		want   int
	}{
		{"staff → admin", staffActor, rbac.RoleStaff, otherAdmin, fiber.StatusForbidden},
		{"staff → staff", staffActor, rbac.RoleStaff, otherStaff, fiber.StatusForbidden},
		{"staff → TA", staffActor, rbac.RoleStaff, ta, fiber.StatusOK},
		{"staff → lecturer", staffActor, rbac.RoleStaff, lecturer, fiber.StatusOK},
		{"admin → admin", adminActor, rbac.RoleAdmin, otherAdmin, fiber.StatusOK},
	} {
		if got := post(tc.actor, tc.role, tc.target); got != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, got, tc.want)
		}
	}
	var enabled bool
	_ = pool.QueryRow(ctx, `SELECT totp_enabled_at IS NOT NULL FROM users WHERE id = $1`, otherStaff).Scan(&enabled)
	if !enabled {
		t.Error("a refused reset must leave the target's 2FA in place")
	}
}
