package handler

import (
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

// Found during the 2026-09-14 TOR acceptance review (§3.3 ข.1): the route for
// POST /teaching-courses/import listed rbac.RoleLecturer alongside admin/staff,
// but TeachingService.PreviewImport/CommitImport have always checked
// isPrivileged() (admin/staff only) — a lecturer got past the route only to be
// refused by the service with ErrForbidden. Router and service now agree: this
// test proves the REJECTION happens at the role-gate middleware, before the
// request body is even read (a lecturer POST with no body still gets 403, not
// the 400 "term_id required" ImportExcel would return if it were reached).
func TestImportRoute_LecturerRejectedByRoleGate(t *testing.T) {
	pool := testutil.NewPool(t)
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.NewContainer(pool, store, mail.New(config.Config{}), audit.New(pool), config.Config{}, nil, nil)
	th := &TeachingHandler{Svc: svc}

	appWithRole := func(role string) *fiber.App {
		app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
		app.Post("/teaching-courses/import",
			func(c *fiber.Ctx) error {
				c.Locals(string(CtxUserID), uuid.New())
				c.Locals(string(CtxRoles), []string{role})
				return c.Next()
			},
			// The exact role list router.go registers this route with, as of
			// the 15/09/2026 fix — kept in sync deliberately: if router.go's
			// list drifts from this one, this test stops meaning anything, so
			// any change to that line should update this line too.
			RequireRole(rbac.RoleAdmin, rbac.RoleStaff),
			th.ImportExcel,
		)
		return app
	}

	post := func(app *fiber.App) int {
		t.Helper()
		// Deliberately empty body: if this reaches ImportExcel at all it fails
		// on "term_id required" (400), so seeing anything other than 403 here
		// would mean the role gate let the request through.
		req := httptest.NewRequest("POST", "/teaching-courses/import", nil)
		res, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	if got := post(appWithRole(rbac.RoleLecturer)); got != fiber.StatusForbidden {
		t.Errorf("lecturer: status = %d, want 403 (rejected by the role gate, matching the service's own isPrivileged check)", got)
	}
	if got := post(appWithRole(rbac.RoleTA)); got != fiber.StatusForbidden {
		t.Errorf("ta: status = %d, want 403", got)
	}
	// Staff/admin must still get PAST the gate — a 400 here (from the missing
	// term_id/file) proves the middleware let them through to the handler.
	if got := post(appWithRole(rbac.RoleStaff)); got != fiber.StatusBadRequest {
		t.Errorf("staff: status = %d, want 400 (past the gate, refused by ImportExcel itself for a missing body)", got)
	}
	if got := post(appWithRole(rbac.RoleAdmin)); got != fiber.StatusBadRequest {
		t.Errorf("admin: status = %d, want 400 (past the gate, refused by ImportExcel itself for a missing body)", got)
	}
}
