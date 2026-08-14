package handler

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestBind(t *testing.T) {
	type req struct {
		Email string `json:"email" validate:"required,email"`
		Name  string `json:"name" validate:"required,min=2,max=50"`
	}

	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Post("/probe", func(c *fiber.Ctx) error {
		var in req
		if err := Bind(c, &in); err != nil {
			return err
		}
		return c.JSON(in)
	})

	post := func(body string) (int, string) {
		t.Helper()
		httpReq := httptest.NewRequest("POST", "/probe", strings.NewReader(body))
		httpReq.Header.Set("Content-Type", "application/json")
		res, err := app.Test(httpReq)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		buf := make([]byte, 1024)
		n, _ := res.Body.Read(buf)
		return res.StatusCode, string(buf[:n])
	}

	if status, body := post(`{"email":"a@b.com","name":"Ann"}`); status != 200 {
		t.Errorf("valid body should pass, got %d: %s", status, body)
	}
	if status, body := post(`{"name":"Ann"}`); status != 400 || !strings.Contains(body, "email") {
		t.Errorf("missing required email should 400 and name the field, got %d: %s", status, body)
	}
	if status, body := post(`{"email":"not-an-email","name":"Ann"}`); status != 400 || !strings.Contains(body, "email") {
		t.Errorf("invalid email should 400 and name the field, got %d: %s", status, body)
	}
	if status, body := post(`{"email":"a@b.com","name":"A"}`); status != 400 || !strings.Contains(body, "name") {
		t.Errorf("too-short name should 400 and name the field, got %d: %s", status, body)
	}
	if status, _ := post(`not json`); status != 400 {
		t.Errorf("malformed JSON should still 400, got %d", status)
	}
}
