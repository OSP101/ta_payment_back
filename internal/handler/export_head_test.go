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
