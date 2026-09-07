package handler

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"ta-payment-back/internal/service"
)

// ErrorHandler's last branch used to answer EVERY remaining plain error with a
// 400 carrying err.Error() verbatim, on the assumption that anything reaching it
// was a Thai business message. Internal failures land there too — a pgx row scan
// carries no *PgError — so a driver's own words were rendered to the user as if
// they were advice about the form they had just filled in.
//
// The split is by script: Thai text is a message somebody wrote for a user, ASCII
// is machinery talking to itself.
func TestErrorHandler_DoesNotLeakInternalErrors(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Get("/scan", func(c *fiber.Ctx) error {
		// Verbatim shape of the failure a waived makeup used to produce.
		return errors.New("can't scan into dest[1] (col: makeup_date): cannot scan NULL into *time.Time")
	})
	app.Get("/business", func(c *fiber.Ctx) error {
		return errors.New("วันที่ทำงานต้องอยู่ในช่วงภาคการศึกษา")
	})
	app.Get("/typed", func(c *fiber.Ctx) error {
		return service.Invalid("จำนวนชั่วโมงต้องมากกว่า 0")
	})

	get := func(path string) (int, string) {
		t.Helper()
		res, err := app.Test(httptest.NewRequest("GET", path, nil))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}

	status, body := get("/scan")
	if status != fiber.StatusInternalServerError {
		t.Errorf("internal error status = %d, want 500", status)
	}
	for _, leak := range []string{"scan", "makeup_date", "time.Time", "dest["} {
		if strings.Contains(body, leak) {
			t.Errorf("response leaked internal detail %q: %s", leak, body)
		}
	}
	if !strings.Contains(body, "ระบบขัดข้อง") {
		t.Errorf("internal error should read as a generic outage, got: %s", body)
	}

	// Thai business messages must survive untouched — they are the whole reason
	// the plain-error branch exists.
	if status, body := get("/business"); status != fiber.StatusBadRequest ||
		!strings.Contains(body, "ภาคการศึกษา") {
		t.Errorf("plain Thai error = %d %s, want 400 with its own message", status, body)
	}
	if status, body := get("/typed"); status != fiber.StatusBadRequest ||
		!strings.Contains(body, "จำนวนชั่วโมง") {
		t.Errorf("UserError = %d %s, want 400 with its own message", status, body)
	}
}
