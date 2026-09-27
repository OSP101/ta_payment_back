package handler

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// Fiber's Get() registers the SAME handler for HEAD:
//
//	func (app *App) Get(path string, handlers ...Handler) Router {
//	    return app.Head(path, handlers...).Add(MethodGet, path, handlers...)
//	}
//
// That is right for a read. It is not right for the payout ZIP, which locks
// every approved month, writes the PII access trail and records the batch —
// so a link prefetcher or a scanner issuing HEAD would irreversibly freeze a
// course while transferring no file. Verified live on 04/08/2026: HEAD
// returned 200 and left a locked course and a history row behind.
// The guard must be the FIRST thing the handler does: it has to reject before
// the id is parsed, or a malformed id would mask it behind a 400.
//
// Kept ahead of the valid-id case on purpose. Without the guard this one
// returns a clean 400 and fails loudly; the valid-id case reaches the service
// and panics, which aborts the whole test binary and reports nothing.
func TestCourseZip_RefusesHeadBeforeValidatingTheID(t *testing.T) {
	app := fiber.New()
	h := &ExportHandler{}
	app.Get("/z/:id.zip", h.CourseZip)

	res, err := app.Test(httptest.NewRequest(fiber.MethodHead, "/z/not-a-uuid.zip", nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != fiber.StatusMethodNotAllowed {
		t.Errorf("HEAD status = %d, want %d", res.StatusCode, fiber.StatusMethodNotAllowed)
	}
}

func TestCourseZip_RefusesHead(t *testing.T) {
	app := fiber.New()
	h := &ExportHandler{}
	app.Get("/z/:id.zip", h.CourseZip)

	res, err := app.Test(httptest.NewRequest(fiber.MethodHead, "/z/"+
		"11111111-1111-1111-1111-111111111111.zip", nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != fiber.StatusMethodNotAllowed {
		t.Errorf("HEAD status = %d, want %d — HEAD must not run an export",
			res.StatusCode, fiber.StatusMethodNotAllowed)
	}
}

// A cross-site link can only make the browser issue a GET (with the Lax
// cookie attached); the export must not run for one.
func TestCourseZip_RefusesGet(t *testing.T) {
	app := fiber.New()
	h := &ExportHandler{}
	app.Get("/z/:id.zip", h.CourseZip)

	res, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/z/"+
		"11111111-1111-1111-1111-111111111111.zip", nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != fiber.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want %d — a GET must not run an export",
			res.StatusCode, fiber.StatusMethodNotAllowed)
	}
}

// The forced password-change / 2FA allowlists must match /me exactly, not any
// route that happens to end in /me.
func TestIsOwnAccountPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/api/v1/me":                    true,
		"/api/v1/me/password":           false,
		"/api/demo/w/3/me":              true,
		"/api/v1/dashboard/ta/me":       false,
		"/api/v1/dashboard/lecturer/me": false,
	} {
		if got := isOwnAccountPath(p, "/me"); got != want {
			t.Errorf("isOwnAccountPath(%q, /me) = %v, want %v", p, got, want)
		}
	}
	if !isOwnAccountPath("/api/v1/me/password", "/me/password") {
		t.Error("/api/v1/me/password not matched")
	}
}
