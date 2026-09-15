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

// GET /teaching-courses/import/history and /teaching-courses/import/history/:id
// — new routes for TOR §3.3 ข.5. Confirms they're wired with the same
// admin/staff gate as every other registrar-import route, and that they
// don't collide with the pre-existing "/teaching-courses/:id" route.
func TestImportHistoryRoutes_RoleGateAndNoCollisionWithGetByID(t *testing.T) {
	pool := testutil.NewPool(t)
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.NewContainer(pool, store, mail.New(config.Config{}), audit.New(pool), config.Config{}, nil, nil)
	th := &TeachingHandler{Svc: svc}

	// ImportHistory/ImportHistoryDetail check isPrivileged() against the DB
	// too (defense-in-depth, same as ReplaceLecturers and friends), so the
	// admin/staff success-path assertions below need a REAL row — a bare
	// uuid.New() with role only in fiber.Locals passes the route's own
	// RequireRole gate but is then refused by the service as "not actually
	// admin/staff in the database".
	mkUser := func(role string) uuid.UUID {
		id := uuid.New()
		ctx := context.Background()
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, email, first_name, last_name, is_active) VALUES ($1, $2, 'ท', 'ท', TRUE)`,
			id, id.String()+"@example.test"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO user_roles (user_id, role) VALUES ($1, $2::role_code)`, id, role); err != nil {
			t.Fatal(err)
		}
		return id
	}

	appWithRole := func(role string) *fiber.App {
		app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
		actor := mkUser(role)
		withActor := func(c *fiber.Ctx) error {
			c.Locals(string(CtxUserID), actor)
			c.Locals(string(CtxRoles), []string{role})
			return c.Next()
		}
		app.Get("/teaching-courses/:id", withActor, th.Get)
		app.Get("/teaching-courses/import/history",
			withActor, RequireRole(rbac.RoleAdmin, rbac.RoleStaff), th.ImportHistory)
		app.Get("/teaching-courses/import/history/:id",
			withActor, RequireRole(rbac.RoleAdmin, rbac.RoleStaff), th.ImportHistoryDetail)
		return app
	}

	get := func(app *fiber.App, path string) int {
		t.Helper()
		res, err := app.Test(httptest.NewRequest("GET", path, nil))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	if got := get(appWithRole(rbac.RoleLecturer), "/teaching-courses/import/history"); got != fiber.StatusForbidden {
		t.Errorf("lecturer listing history: status = %d, want 403", got)
	}
	if got := get(appWithRole(rbac.RoleTA), "/teaching-courses/import/history/"+uuid.New().String()); got != fiber.StatusForbidden {
		t.Errorf("ta reading detail: status = %d, want 403", got)
	}
	// Admin gets past the gate to a real (empty) 200 list — no rows yet, but
	// the request must reach the handler, not be misrouted to Get(:id) with
	// id="import" (which would fail uuid.Parse and return 400 instead).
	if got := get(appWithRole(rbac.RoleAdmin), "/teaching-courses/import/history"); got != fiber.StatusOK {
		t.Errorf("admin listing history: status = %d, want 200 (proves no collision with /teaching-courses/:id)", got)
	}
	// A random id: reaches ImportHistoryDetail (not Get), which returns 404
	// via ErrNotFound rather than Get's own not-found shape — either way this
	// must be routed to the history handler, not silently succeed.
	if got := get(appWithRole(rbac.RoleStaff), "/teaching-courses/import/history/"+uuid.New().String()); got != fiber.StatusNotFound {
		t.Errorf("staff reading a nonexistent import id: status = %d, want 404", got)
	}
}
