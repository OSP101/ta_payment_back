package handler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"ta-payment-back/internal/rbac"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/telemetry"
)

func TestPanicIsLoggedMeasuredAndTracedAs500(t *testing.T) {
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(&logs, nil))))
	defer slog.SetDefault(oldLogger)
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer provider.Shutdown(context.Background())
	oldProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	defer otel.SetTracerProvider(oldProvider)
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(RequestID(), AccessLog(), Recover())
	app.Get("/panic-test", func(c *fiber.Ctx) error {
		if !trace.SpanContextFromContext(telemetry.Parent(c.Context())).IsValid() {
			t.Error("DB/service context lost request trace")
		}
		panic("ยอดผิดพลาด 1100000000001")
	})
	r := httptest.NewRequest("GET", "/panic-test?password=do-not-log", nil)
	r.Header.Set("X-Request-Id", "forged")
	response, err := app.Test(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 500 || response.Header.Get("X-Request-Id") == "forged" || response.Header.Get("X-Trace-Id") == "" {
		t.Fatalf("missing authoritative reference/status: %v", response)
	}
	text := logs.String()
	if !strings.Contains(text, `"msg":"http_request"`) || !strings.Contains(text, `"status":500`) || strings.Contains(text, "1100000000001") || strings.Contains(text, "do-not-log") {
		t.Fatalf("incomplete or unsafe panic log: %s", text)
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].Name != "GET /panic-test" {
		t.Fatalf("panic trace missing: %#v", spans)
	}
	metrics, err := telemetry.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, family := range metrics {
		if family.GetName() == "ta_http_requests_total" {
			for _, metric := range family.Metric {
				route, status := "", ""
				for _, label := range metric.Label {
					if label.GetName() == "route" {
						route = label.GetValue()
					}
					if label.GetName() == "status" {
						status = label.GetValue()
					}
				}
				if route == "/panic-test" && status == "500" && metric.Counter.GetValue() >= 1 {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("panic missing from 5xx metrics")
	}
}

// The access log's severity is the first filter of any incident, so the status
// it records has to be the one the client was actually told.
//
// Fiber runs ErrorHandler OUTSIDE the middleware chain, after every middleware
// has already returned. A logger that reads c.Response().StatusCode() therefore
// sees the status from before the error was handled — 200 — and every 404 and
// 500 gets filed as a successful request at INFO. errorResponse is the shared
// mapper that makes the log and the response agree.
func TestErrorResponse_MatchesWhatTheClientIsTold(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want int
	}{
		{"fiber error keeps its code", fiber.NewError(fiber.StatusNotFound, "nope"), fiber.StatusNotFound},
		{"not found sentinel", service.ErrNotFound, fiber.StatusNotFound},
		{"forbidden sentinel", service.ErrForbidden, fiber.StatusForbidden},
		{"conflict sentinel", service.ErrConflict, fiber.StatusConflict},
		{"invalid input sentinel", service.ErrInvalidInput, fiber.StatusBadRequest},
		// An ASCII error is an internal fault, not advice for a user.
		{"internal error", errors.New("cannot scan NULL into *time.Time"), fiber.StatusInternalServerError},
		// A Thai error is a business message the user is meant to read.
		{"business message", errors.New("ยอดเกินงบที่ตั้งไว้"), fiber.StatusBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, msg := errorResponse(c.err)
			if got != c.want {
				t.Errorf("status = %d, want %d — the access log would file this "+
					"request under the wrong severity", got, c.want)
			}
			if msg == "" {
				t.Error("empty message: the client is told nothing at all")
			}
		})
	}
}

// The internal-fault message must stay generic: it is what a 500 shows a user,
// and the real text goes to the log with the request id instead.
func TestErrorResponse_InternalFaultsDoNotLeakTheirText(t *testing.T) {
	_, msg := errorResponse(errors.New(`pq: relation "users" does not exist`))
	if msg != "ระบบขัดข้อง กรุณาลองใหม่ภายหลัง" {
		t.Errorf("internal fault answered %q — driver internals must not reach the client", msg)
	}
}

// An audit row records the role that EXPLAINS the permission, not whichever
// role happens to be first in the account's list. An account that is both a
// lecturer and staff did not sign off a payout as a lecturer.
func TestActingRole_RecordsTheMostPrivilegedRole(t *testing.T) {
	for _, c := range []struct {
		roles []string
		want  string
	}{
		{[]string{rbac.RoleTA}, rbac.RoleTA},
		{[]string{rbac.RoleLecturer, rbac.RoleStaff}, rbac.RoleStaff},
		{[]string{rbac.RoleTA, rbac.RoleAdmin, rbac.RoleLecturer}, rbac.RoleAdmin},
		// Nothing to record rather than a guess — a failed login has no role.
		{nil, ""},
		// A role outside the vocabulary is kept, not dropped: an unknown role
		// is still what the request acted under.
		{[]string{"executive"}, "executive"},
	} {
		if got := actingRole(c.roles); got != c.want {
			t.Errorf("actingRole(%v) = %q, want %q", c.roles, got, c.want)
		}
	}
}
