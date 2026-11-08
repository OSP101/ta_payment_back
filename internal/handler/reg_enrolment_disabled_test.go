package handler

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// No service/database is provided: suspension must return before starting
// jobs or probing either REG website, even for clients calling the API directly.
func TestRegEnrolmentSuspendedForHandover(t *testing.T) {
	app := fiber.New()
	h := &TeachingHandler{}
	app.Get("/reg-enrolment/sources", h.RegSources)
	app.Post("/terms/:id/reg-enrolment", h.StartRegEnrolment)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/reg-enrolment/sources", ""},
		{"POST", "/terms/00000000-0000-0000-0000-000000000001/reg-enrolment", `{"scope":"all","source":"new"}`},
		{"POST", "/terms/00000000-0000-0000-0000-000000000001/reg-enrolment", `{"scope":"requested","source":"legacy"}`},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		res, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != fiber.StatusServiceUnavailable {
			t.Fatalf("%s %s status=%d, want 503", tc.method, tc.path, res.StatusCode)
		}
	}
}
