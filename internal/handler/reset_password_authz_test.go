package handler

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/rbac"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/storage"
	"ta-payment-back/internal/testutil"
)

// AUTHZ-02: POST /users/:id/reset-password was gated by adminOrStaff with no
// restriction on the TARGET's role — a staff member could reset an admin's
// password and read the temp password back in the response body. This pins
// that staff is now refused against admin/staff targets, while admin keeps
// working against any target and staff keeps working against a TA.
func TestResetPassword_StaffCannotResetPrivilegedAccounts(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	aud := audit.New(pool)
	svc := service.NewContainer(pool, store, mail.New(config.Config{}), aud, config.Config{}, nil, nil)

	mkUser := func(role string) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, email, first_name, last_name, password_hash) VALUES ($1, $2, 'ท', 'ท', 'x')`,
			id, id.String()+"@test.local"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO user_roles (user_id, role) VALUES ($1, $2::role_code)`, id, role); err != nil {
			t.Fatal(err)
		}
		return id
	}
	staffActor := mkUser(rbac.RoleStaff)
	adminActor := mkUser(rbac.RoleAdmin)
	otherAdmin := mkUser(rbac.RoleAdmin)
	otherStaff := mkUser(rbac.RoleStaff)
	taAccount := mkUser(rbac.RoleTA)

	uh := &UserHandler{Svc: svc}
	appFor := func(actor uuid.UUID, role string) *fiber.App {
		app := fiber.New()
		app.Post("/users/:id/reset-password", func(c *fiber.Ctx) error {
			c.Locals(string(CtxUserID), actor)
			c.Locals(string(CtxRoles), []string{role})
			return c.Next()
		}, uh.ResetPassword)
		return app
	}

	post := func(app *fiber.App, targetID uuid.UUID) int {
		t.Helper()
		req := httptest.NewRequest("POST", "/users/"+targetID.String()+"/reset-password", nil)
		res, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	if got := post(appFor(staffActor, rbac.RoleStaff), otherAdmin); got != fiber.StatusForbidden {
		t.Errorf("staff resetting admin: status = %d, want 403", got)
	}
	if got := post(appFor(staffActor, rbac.RoleStaff), otherStaff); got != fiber.StatusForbidden {
		t.Errorf("staff resetting another staff: status = %d, want 403", got)
	}
	if got := post(appFor(staffActor, rbac.RoleStaff), taAccount); got != fiber.StatusOK {
		t.Errorf("staff resetting a TA: status = %d, want 200", got)
	}
	if got := post(appFor(adminActor, rbac.RoleAdmin), otherAdmin); got != fiber.StatusOK {
		t.Errorf("admin resetting another admin: status = %d, want 200", got)
	}
}
