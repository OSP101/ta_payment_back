package handler

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/storage"
	"ta-payment-back/internal/testutil"
)

// SEC-06: AccountGuard's query joined `sessions` by id alone
// (`LEFT JOIN sessions s ON s.id = $2`), with nothing checking that the
// session actually belongs to the user the token claims. A request carrying
// uid=A but jti=B's live session id would find B's session, read its
// last_activity_at as non-nil/recent, and be let through as if it were A's
// own valid session — defense in depth against a token minted with the
// wrong session bound to it (a future issuance path, an SSOCallback stub,
// etc.), not exploitable via today's login flow alone. This pins that the
// join now requires s.user_id = u.id, so a cross-user (uid, jti) pair is
// rejected with session_revoked.
func TestAccountGuard_RejectsTokenWhoseSessionBelongsToAnotherUser(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.NewContainer(pool, store, mail.New(config.Config{}), audit.New(pool), config.Config{}, nil, nil)

	mkUser := func() uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, email, first_name, last_name, is_active) VALUES ($1, $2, 'ท', 'ท', TRUE)`,
			id, id.String()+"@test.local"); err != nil {
			t.Fatal(err)
		}
		return id
	}
	userA := mkUser()
	userB := mkUser()

	// A live, non-revoked, recently-active session — but it belongs to B.
	sessionB := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO sessions (id, user_id, expires_at, last_activity_at)
		 VALUES ($1, $2, $3, NOW())`,
		sessionB, userB, time.Now().Add(12*time.Hour)); err != nil {
		t.Fatal(err)
	}

	app := fiber.New()
	app.Get("/probe", func(c *fiber.Ctx) error {
		// Stand-in for Authenticated: a token claiming uid=A but carrying
		// B's session id — exactly the mismatch a correct token issuer would
		// never produce today, and exactly what the DB-level check exists to
		// catch regardless.
		c.Locals(string(CtxUserID), userA)
		c.Locals(string(CtxSessionID), sessionB)
		c.Locals(string(CtxRoles), []string{"ta"})
		return c.Next()
	}, AccountGuard(svc), func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	req := httptest.NewRequest("GET", "/probe", nil)
	res, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}
