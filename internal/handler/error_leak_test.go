package handler

import (
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgconn"

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

// Exercise the real HTTP error boundary: raw details must never occur in the
// response even when an internal error contains Thai or a server status.
func TestErrorHandler_HidesWrappedAndServerErrors(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"Thai wrapper", fmt.Errorf("อ่านข้อมูลไม่สำเร็จ: %w", errors.New("dial tcp 10.0.0.8:5432: refused")), 500},
		{"Thai path", &os.PathError{Op: "open", Path: "/private/uploads/เอกสาร.pdf", Err: os.ErrPermission}, 500},
		{"joined errors", errors.Join(errors.New("อ่านข้อมูลไม่สำเร็จ"), errors.New("private driver details")), 500},
		{"explicit server error", fiber.NewError(503, "open /private/template.pdf: permission denied"), 503},
		{"wrapped database error", fmt.Errorf("อ่านข้อมูลไม่สำเร็จ: %w", &pgconn.PgError{Code: "XX000", Message: "private schema details"}), 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
			app.Get("/", func(c *fiber.Ctx) error { return tc.err })
			res, err := app.Test(httptest.NewRequest("GET", "/", nil))
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != tc.status || string(body) != `{"error":"ระบบขัดข้อง กรุณาลองใหม่ภายหลัง"}` {
				t.Fatalf("unsafe response: status=%d body=%s", res.StatusCode, body)
			}
		})
	}
}

func TestErrorHandler_ThaiPanicIsInternal(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(Recover())
	app.Get("/", func(c *fiber.Ctx) error {
		panic("อ่านไฟล์ /private/เอกสาร.pdf ไม่สำเร็จ")
	})
	res, err := app.Test(httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 500 || string(body) != `{"error":"ระบบขัดข้อง กรุณาลองใหม่ภายหลัง"}` {
		t.Fatalf("panic details leaked: status=%d body=%s", res.StatusCode, body)
	}
}

func TestErrorResponse_PreservesExplicitBusinessErrors(t *testing.T) {
	business := service.Conflict("มีข้อมูลนี้อยู่แล้ว")
	status, msg := errorResponse(fmt.Errorf("operation: %w", business))
	if status != 409 || msg != business.Msg {
		t.Fatalf("lost business error: %d %q", status, msg)
	}
}
