package handler

import (
	"bytes"
	"context"
	"image/color"
	"mime/multipart"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/storage"
	"ta-payment-back/internal/testutil"

	"ta-payment-back/internal/rbac"
)

// AUTHZ-01: a lecturer could POST /users/:id/avatar for ANY user id — nothing
// checked that the target was a TA account the lecturer is allowed to touch.
// This pinned the fix: a lecturer may set an avatar on a `ta` account, and is
// refused on an `admin` account.
func TestUploadAvatarFor_LecturerLimitedToTAAccounts(t *testing.T) {
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
			`INSERT INTO users (id, email, first_name, last_name) VALUES ($1, $2, 'ท', 'ท')`,
			id, id.String()+"@test.local"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO user_roles (user_id, role) VALUES ($1, $2::role_code)`, id, role); err != nil {
			t.Fatal(err)
		}
		return id
	}
	lecturer := mkUser(rbac.RoleLecturer)
	taAccount := mkUser(rbac.RoleTA)
	adminAccount := mkUser(rbac.RoleAdmin)

	uh := &UserHandler{Svc: svc}
	app := fiber.New()
	app.Post("/users/:id/avatar", func(c *fiber.Ctx) error {
		c.Locals(string(CtxUserID), lecturer)
		c.Locals(string(CtxRoles), []string{rbac.RoleLecturer})
		return c.Next()
	}, uh.UploadAvatarFor)

	post := func(targetID uuid.UUID) int {
		t.Helper()
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		part, err := w.CreateFormFile("file", "avatar.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(pngBytes(t, 8, 8, color.NRGBA{R: 1, G: 2, B: 3, A: 255})); err != nil {
			t.Fatal(err)
		}
		w.Close()
		req := httptest.NewRequest("POST", "/users/"+targetID.String()+"/avatar", &body)
		req.Header.Set("Content-Type", w.FormDataContentType())
		res, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	if got := post(adminAccount); got != fiber.StatusForbidden {
		t.Errorf("lecturer setting admin's avatar: status = %d, want 403", got)
	}
	if got := post(taAccount); got != fiber.StatusCreated {
		t.Errorf("lecturer setting a TA's avatar: status = %d, want 201", got)
	}
}
