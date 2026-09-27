package handler

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// UAT DEF-001: a brand-new staff account (temporary password, no TOTP) under
// MFA_MANDATORY_ENFORCED=true could do nothing at all — /me/password was
// refused with mfa_setup_required and /me/2fa/setup with
// password_change_required. The two forced flows must run in order:
// password first, then enrolment.
func TestAccountGuard_NewStaffChangesPasswordThenEnrols(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.NewContainer(pool, store, mail.New(config.Config{}), audit.New(pool),
		config.Config{MFAMandatoryEnforced: true}, nil, nil)

	uid, sid := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active, must_change_password)
		 VALUES ($1, $2, 'ท', 'ท', TRUE, TRUE)`, uid, uid.String()+"@test.local"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO sessions (id, user_id, expires_at, last_activity_at) VALUES ($1, $2, $3, NOW())`,
		sid, uid, time.Now().Add(12*time.Hour)); err != nil {
		t.Fatal(err)
	}

	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(string(CtxUserID), uid)
		c.Locals(string(CtxSessionID), sid)
		c.Locals(string(CtxRoles), []string{rbac.RoleStaff})
		return c.Next()
	}, AccountGuard(svc))
	app.All("/*", func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	call := func(method, path string) (int, string) {
		t.Helper()
		res, err := app.Test(httptest.NewRequest(method, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	expect := func(method, path string, wantStatus int, wantBody string) {
		t.Helper()
		st, body := call(method, path)
		if st != wantStatus || !strings.Contains(body, wantBody) {
			t.Errorf("%s %s = %d %q, want %d containing %q", method, path, st, body, wantStatus, wantBody)
		}
	}

	// Step 1: temporary password pending — the password change goes through,
	// enrolment and everything else wait.
	expect("POST", "/api/v1/me/password", fiber.StatusOK, "")
	expect("POST", "/api/v1/me/2fa/setup", fiber.StatusForbidden, "password_change_required")
	expect("GET", "/api/v1/users", fiber.StatusForbidden, "password_change_required")

	// Step 2: password changed — now enrolment is the only way forward.
	if _, err := pool.Exec(ctx, `UPDATE users SET must_change_password = FALSE WHERE id = $1`, uid); err != nil {
		t.Fatal(err)
	}
	expect("POST", "/api/v1/me/2fa/setup", fiber.StatusOK, "")
	expect("GET", "/api/v1/users", fiber.StatusForbidden, "mfa_setup_required")
}
