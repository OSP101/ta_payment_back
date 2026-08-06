package handler

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/rbac"
	"ta-payment-back/internal/testutil"
)

// RequireExecutiveView has two gates and both carry real weight: the JWT
// claims path (staff/admin/executive in the token) and the live-DB fallback
// that makes a freshly ticked flag work without a re-login. The 06/08/2026
// browser check found exactly this hole — the flag was set, the token was
// old, and the endpoint 403'd — so the fallback is what this test pins.
func TestRequireExecutiveView(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()

	mkUser := func(exec bool) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, email, first_name, last_name, is_executive)
			 VALUES ($1, $2, 'ท', 'ท', $3)`,
			id, id.String()+"@test.local", exec); err != nil {
			t.Fatal(err)
		}
		return id
	}
	flagged := mkUser(true)
	plain := mkUser(false)

	app := fiber.New()
	// Stand-in for Authenticated: puts the claims under test into locals.
	app.Get("/probe", func(c *fiber.Ctx) error {
		c.Locals(string(CtxUserID), uuid.MustParse(c.Get("X-Test-User")))
		roles := []string{}
		if r := c.Get("X-Test-Role"); r != "" {
			roles = append(roles, r)
		}
		c.Locals(string(CtxRoles), roles)
		return c.Next()
	}, RequireExecutiveView(pool), func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	probe := func(user uuid.UUID, role string) int {
		t.Helper()
		req := httptest.NewRequest("GET", "/probe", nil)
		req.Header.Set("X-Test-User", user.String())
		if role != "" {
			req.Header.Set("X-Test-Role", role)
		}
		res, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	// Claims path: each privileged role passes without touching the flag.
	for _, role := range []string{rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleExecutive} {
		if got := probe(plain, role); got != 200 {
			t.Errorf("role %q should pass on claims alone, got %d", role, got)
		}
	}
	// Fallback path: a lecturer token issued BEFORE the flag was ticked.
	if got := probe(flagged, rbac.RoleLecturer); got != 200 {
		t.Errorf("flagged user with a stale token should pass via the DB fallback, got %d", got)
	}
	// And the gate still holds for everyone else.
	if got := probe(plain, rbac.RoleLecturer); got != 403 {
		t.Errorf("plain lecturer should be refused, got %d", got)
	}
	if got := probe(plain, rbac.RoleTA); got != 403 {
		t.Errorf("TA should be refused, got %d", got)
	}
}
