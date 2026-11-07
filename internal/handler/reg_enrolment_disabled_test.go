package handler

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// Validate malformed requests before touching a service/database. The route
// is enabled again rather than returning an unconditional suspension.
func TestRegEnrolmentStartValidatesRequest(t *testing.T) {
	app := fiber.New()
	h := &TeachingHandler{}
	app.Post("/terms/:id/reg-enrolment", h.StartRegEnrolment)
	for _, tc := range []struct{ path, body string }{{"/terms/invalid/reg-enrolment", `{"scope":"all"}`}, {"/terms/00000000-0000-0000-0000-000000000001/reg-enrolment", `{"scope":"invalid"}`}, {"/terms/00000000-0000-0000-0000-000000000001/reg-enrolment", `{"scope":"all","source":"unknown"}`}} {
		req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		res, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 400 {
			t.Fatalf("status=%d, want 400", res.StatusCode)
		}
	}
}
