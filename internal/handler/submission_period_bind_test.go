package handler

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/service"
)

func TestSubmissionPeriodManualSaveBinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		idJSON string
		wantID uuid.UUID
		status int
	}{
		{"create without ID", "", uuid.Nil, 200},
		{"edit with UUID", `"id":"22222222-2222-4222-8222-222222222222",`, uuid.MustParse("22222222-2222-4222-8222-222222222222"), 200},
		{"empty ID reproduces reported error", `"id":"",`, uuid.Nil, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := fiber.New()
			app.Post("/submission-periods", func(c *fiber.Ctx) error {
				var in service.SubmissionPeriod
				if err := Bind(c, &in); err != nil {
					return err
				}
				if in.ID != tc.wantID || in.YearMonth != "2569-12" || in.StartsOn != "2026-12-01" || in.DueDate != "2026-12-31" {
					t.Errorf("unexpected bound period: %+v", in)
				}
				return c.SendStatus(200)
			})
			body := fmt.Sprintf(`{%s"term_id":"11111111-1111-4111-8111-111111111111","year_month":"2569-12","starts_on":"2026-12-01","due_date":"2026-12-31","label":"ธันวาคม 2569","remind_days_before":3,"is_closed":false}`, tc.idJSON)
			req := httptest.NewRequest("POST", "/submission-periods", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			res, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != tc.status {
				t.Fatalf("got %d, want %d", res.StatusCode, tc.status)
			}
		})
	}
}
