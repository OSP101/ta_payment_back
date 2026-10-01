package handler

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/service"
)

// A screen that polls an endpoint its user may not call must file one refusal
// an hour, not one every poll: nothing can be removed from audit_logs.
func TestDeniedAuditThrottle_RecordsOncePerWindowPerRoute(t *testing.T) {
	actor := uuid.New()
	k := deniedAuditKey{actor: actor, method: "GET", route: "/api/v1/test-denied"}
	t0 := time.Now()

	if !deniedAuditDue(k, t0) {
		t.Fatal("the first refusal was not recorded")
	}
	if deniedAuditDue(k, t0.Add(deniedAuditWindow-time.Second)) {
		t.Error("a repeat inside the window was recorded again")
	}
	if !deniedAuditDue(k, t0.Add(deniedAuditWindow+time.Second)) {
		t.Error("a refusal after the window was suppressed")
	}
	// A different route, a different method, a different person: different acts.
	for _, other := range []deniedAuditKey{
		{actor: actor, method: "GET", route: "/api/v1/test-denied-2"},
		{actor: actor, method: "POST", route: "/api/v1/test-denied"},
		{actor: uuid.New(), method: "GET", route: "/api/v1/test-denied"},
	} {
		if !deniedAuditDue(other, t0) {
			t.Errorf("%+v was suppressed by another key's window", other)
		}
	}

	SweepDeniedAudit(t0.Add(3 * deniedAuditWindow))
	if _, kept := deniedAuditSeen.Load(k); kept {
		t.Error("the sweep kept a key whose window had passed")
	}
}

// The middleware must never change what the client is told, and must stay out
// of the way of everything that is not a refusal of a signed-in user. The
// auditor is nil here on purpose: any path that reached it would panic.
func TestAuditDenied_PassesEveryResponseThroughUnchanged(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(AuditDenied(nil))
	app.Get("/ok", func(c *fiber.Ctx) error { return c.SendString("fine") })
	app.Get("/missing", func(c *fiber.Ctx) error { return service.ErrNotFound })
	app.Get("/boom", func(c *fiber.Ctx) error { return errors.New("internal detail") })
	// A 403 with nobody signed in has no one to attribute it to.
	app.Get("/forbidden-anon", func(c *fiber.Ctx) error { return service.ErrForbidden })

	for path, want := range map[string]int{
		"/ok": 200, "/missing": 404, "/boom": 500, "/forbidden-anon": 403,
	} {
		res, err := app.Test(httptest.NewRequest("GET", path, nil))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if res.StatusCode != want {
			t.Errorf("%s: status %d, want %d", path, res.StatusCode, want)
		}
	}
}

// The export is opened in Excel by the people whose machines matter. A cell
// that begins with = + - @ is a formula there, and notes and names are text
// somebody else typed.
func TestCSVSafeRow_DefusesFormulas(t *testing.T) {
	got := csvSafeRow([]string{"=HYPERLINK(\"http://x\")", "+1", "-2", "@cmd", "\tx", "ปกติ", "", "a=b", "10.0.0.1"})
	want := []string{"'=HYPERLINK(\"http://x\")", "'+1", "'-2", "'@cmd", "'\tx", "ปกติ", "", "a=b", "10.0.0.1"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cell %d = %q, want %q", i, got[i], want[i])
		}
	}
}
