package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestFiberBridgeKeepsCancellationAndNestedSpan(t *testing.T) {
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}})
	parent := trace.ContextWithSpanContext(context.Background(), sc)
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), ContextKey, parent), time.Second)
	bridged := Parent(ctx)
	cancel()
	if bridged.Err() != context.Canceled {
		t.Fatal("telemetry discarded caller cancellation")
	}
	child := trace.NewSpanContext(trace.SpanContextConfig{TraceID: sc.TraceID(), SpanID: trace.SpanID{3}})
	if got := trace.SpanContextFromContext(Parent(trace.ContextWithSpanContext(ctx, child))); got.SpanID() != child.SpanID() {
		t.Fatal("telemetry replaced a child span with the root")
	}
}

func TestLogScrubbingAndCorrelation(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewLogHandler(slog.NewJSONHandler(&buf, nil))).With("password", "secret-value")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}})
	ctx := WithRequest(trace.ContextWithSpanContext(context.Background(), sc), "550e8400-e29b-41d4-a716-446655440000")
	logger.ErrorContext(ctx, "failure for person@example.com", "error", errors.New("postgres://user:password@db failed citizen 1100000000001 account 123-4-56789-0 Bearer eyJhello.world.signature https://app.test/api/public/private-link?code=private-query"), "cookie", "session-value")
	text := buf.String()
	for _, secret := range []string{"person@example.com", "user:password", "1100000000001", "123-4-56789-0", "eyJhello", "secret-value", "session-value", "private-link", "private-query"} {
		if strings.Contains(text, secret) {
			t.Fatalf("log leaked %q: %s", secret, text)
		}
	}
	for _, expected := range []string{sc.TraceID().String(), "550e8400-e29b-41d4-a716-446655440000"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("lost correlation key %q: %s", expected, text)
		}
	}
}

func TestDatabaseSpansAreChildrenWithoutSQLOrArguments(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	defer otel.SetTracerProvider(old)
	ctx, root := provider.Tracer("test").Start(context.Background(), "request")
	tracer := DBTracer{}
	query := tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "SELECT secret FROM users WHERE citizen_id=$1", Args: []any{"1100000000001"}})
	tracer.TraceQueryEnd(query, nil, pgx.TraceQueryEndData{Err: &pgconn.PgError{Code: "23505", Message: "1100000000001 already exists"}})
	root.End()
	spans := exporter.GetSpans()
	if len(spans) != 2 || spans[0].Parent.SpanID() != spans[1].SpanContext.SpanID() {
		t.Fatalf("database span not connected: %#v", spans)
	}
	for _, attr := range spans[0].Attributes {
		if strings.Contains(attr.Value.AsString(), "1100000000001") || strings.Contains(attr.Value.AsString(), "SELECT secret") {
			t.Fatal("sensitive SQL data in span")
		}
	}
	if spans[0].Name != "db SELECT" {
		t.Fatalf("unexpected span name %q", spans[0].Name)
	}
}

func TestStartRejectsInvalidSamplingConfiguration(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://localhost:4318/v1/traces")
	for _, rate := range []string{"-1", "2", "NaN", "invalid"} {
		t.Setenv("OTEL_TRACES_SAMPLER_ARG", rate)
		if _, err := Start(context.Background(), "test", "test"); err == nil {
			t.Fatalf("accepted invalid sample rate %q", rate)
		}
	}
}
