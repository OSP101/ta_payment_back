package handler

import (
	"errors"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/google/uuid"

	"ta-payment-back/internal/errtrack"
)

// ctxPanicReported marks a request whose panic Recover() already sent to
// GlitchTip. fiber's recover middleware turns the panic into an ordinary
// error that then reaches ErrorHandler as a 500; without this mark the same
// failure would be reported twice, the second time without the stack trace
// that makes it worth reading.
const ctxPanicReported ctxKey = "panic_reported"

// Recover is fiber's recover middleware with the panic reported to
// GlitchTip on the way through. The report has to happen HERE, inside the
// deferred recover: this is the last point where the goroutine's stack still
// runs through the line that panicked. By the time ErrorHandler sees the
// resulting error, that stack is gone.
func Recover() fiber.Handler {
	return recover.New(recover.Config{
		EnableStackTrace: true,
		StackTraceHandler: func(c *fiber.Ctx, e any) {
			c.Locals(string(ctxPanicReported), true)
			errtrack.CapturePanic(e, errtrackRequest(c, fiber.StatusInternalServerError))
		},
	})
}

// reportServerError sends a 5xx to GlitchTip. 4xx are the client's mistake
// (bad input, no permission, stale page) and are already in the access log;
// sending them would bury the real faults.
func reportServerError(c *fiber.Ctx, err error, status int) {
	if status < 500 {
		return
	}
	if done, _ := c.Locals(string(ctxPanicReported)).(bool); done {
		return
	}
	errtrack.CaptureError(err, errtrackRequest(c, status))
}

func errtrackRequest(c *fiber.Ctx, status int) errtrack.Request {
	r := errtrack.Request{
		ID:       ReqID(c).String(),
		Method:   c.Method(),
		Route:    c.Route().Path,
		Status:   status,
		UserRole: actingRole(Roles(c)),
	}
	if id := UserID(c); id != uuid.Nil {
		r.UserID = id.String()
	}
	return r
}

// ErrorReportTest fails on purpose so an admin can check, on a live
// deployment, that a 5xx and a panic both reach GlitchTip (the page is
// /staff/error-report-test). Admin only: it does nothing but produce an
// error, but there is no reason for anyone else to fill the issue list.
func ErrorReportTest(c *fiber.Ctx) error {
	var in struct {
		Kind string `json:"kind"`
	}
	if err := Bind(c, &in); err != nil {
		return err
	}
	switch in.Kind {
	case "error":
		return errors.New("error-report test: handler returned an error (ignore, sent on purpose)")
	case "panic":
		panic("error-report test: handler panicked (ignore, sent on purpose)")
	default:
		return fiber.NewError(fiber.StatusBadRequest, "kind ต้องเป็น error หรือ panic")
	}
}
