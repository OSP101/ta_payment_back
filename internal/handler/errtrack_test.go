package handler

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/errtrack"
)

// captureTransport keeps events in memory instead of sending them.
type captureTransport struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (t *captureTransport) Configure(sentry.ClientOptions)          {}
func (t *captureTransport) Flush(time.Duration) bool                { return true }
func (t *captureTransport) FlushWithContext(_ context.Context) bool { return true }
func (t *captureTransport) Close()                                  {}
func (t *captureTransport) SendEvent(e *sentry.Event) {
	t.mu.Lock()
	t.events = append(t.events, e)
	t.mu.Unlock()
}
func (t *captureTransport) taken() []*sentry.Event { t.mu.Lock(); defer t.mu.Unlock(); return t.events }
func (t *captureTransport) reset()                 { t.mu.Lock(); t.events = nil; t.mu.Unlock() }

// What reaches GlitchTip from a failing request: one event per fault, with
// the route pattern and ids but none of the request's contents.
func TestErrtrackReportsServerErrorsOnly(t *testing.T) {
	tr := &captureTransport{}
	if err := errtrack.Init(errtrack.Options{
		DSN:       "http://public@glitchtip.invalid/1",
		Transport: tr,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sentry.Init(sentry.ClientOptions{}) })

	user := uuid.New()
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(Recover())
	app.Use(RequestID())
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(string(CtxUserID), user)
		c.Locals(string(CtxRoles), []string{"ta"})
		return c.Next()
	})
	app.Get("/boom/:id", func(c *fiber.Ctx) error { panic("nil map in claim " + c.Params("id")) })
	app.Post("/fail/:id", func(c *fiber.Ctx) error {
		return errors.New("export claim " + c.Params("id") + " failed")
	})
	app.Get("/missing", func(c *fiber.Ctx) error { return fiber.ErrNotFound })
	app.Post("/selftest", ErrorReportTest)

	do := func(method, target, body string) {
		t.Helper()
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Cookie", "access_token=secret-jwt")
		req.Header.Set("Content-Type", "application/json")
		if _, err := app.Test(req, -1); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("4xx is not reported", func(t *testing.T) {
		tr.reset()
		do("GET", "/missing", "")
		if n := len(tr.taken()); n != 0 {
			t.Fatalf("got %d events for a 404, want 0", n)
		}
	})

	t.Run("returned 5xx is reported once, without request contents", func(t *testing.T) {
		tr.reset()
		do("POST", "/fail/123?citizen_id=1100000000001", `{"bank_account":"1234567890"}`)
		ev := tr.taken()
		if len(ev) != 1 {
			t.Fatalf("got %d events, want 1", len(ev))
		}
		e := ev[0]
		if e.Tags["route"] != "/fail/:id" || e.Tags["role"] != "ta" || e.Tags["request_id"] == "" {
			t.Errorf("tags = %v", e.Tags)
		}
		if e.User.ID != user.String() {
			t.Errorf("user id = %q, want %q", e.User.ID, user)
		}
		if e.Request != nil {
			t.Errorf("event carries the request: %+v", e.Request)
		}
		if got := strings.Join(e.Fingerprint, "|"); got != "POST|/fail/:id|export claim <n> failed" {
			t.Errorf("fingerprint = %q", got)
		}
		if e.Transaction != "POST /fail/:id" {
			t.Errorf("transaction = %q", e.Transaction)
		}
	})

	t.Run("admin self-test produces one report per kind", func(t *testing.T) {
		for _, kind := range []string{"error", "panic"} {
			tr.reset()
			do("POST", "/selftest", `{"kind":"`+kind+`"}`)
			if n := len(tr.taken()); n != 1 {
				t.Errorf("kind=%s: got %d events, want 1", kind, n)
			}
		}
		tr.reset()
		do("POST", "/selftest", `{"kind":"other"}`)
		if n := len(tr.taken()); n != 0 {
			t.Errorf("bad kind: got %d events, want 0 (it is a 400)", n)
		}
	})

	t.Run("panic is reported once, with the panic tag", func(t *testing.T) {
		tr.reset()
		do("GET", "/boom/7", "")
		ev := tr.taken()
		if len(ev) != 1 {
			t.Fatalf("got %d events, want 1 (panic must not also be reported by ErrorHandler)", len(ev))
		}
		if ev[0].Tags["panic"] != "true" || ev[0].Level != sentry.LevelFatal {
			t.Errorf("tags = %v level = %v", ev[0].Tags, ev[0].Level)
		}
	})
}
