package handler

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestRegEnrolmentStartSuspended(t *testing.T) {
	app := fiber.New()
	// No service/database: a suspended request must never start a fetch job.
	h := &TeachingHandler{}
	app.Post("/terms/:id/reg-enrolment", h.StartRegEnrolment)
	response, err := app.Test(httptest.NewRequest("POST", "/terms/00000000-0000-0000-0000-000000000001/reg-enrolment", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.StatusCode)
	}
}
