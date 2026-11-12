package telemetry

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// StartOperation names must be constants from code, never user input. Finish
// reports an outcome without serializing an error (which may contain PII).
func StartOperation(ctx context.Context, name string) (context.Context, func(error)) {
	ctx, span := otel.Tracer("ta-payment/operations").Start(Parent(ctx), name)
	started := time.Now()
	return ctx, func(err error) {
		defer span.End()
		outcome, level := "success", slog.LevelInfo
		if err != nil {
			outcome, level = "error", slog.LevelError
			span.SetStatus(codes.Error, "operation failed")
		}
		Operations.WithLabelValues(name, outcome).Inc()
		slog.Log(ctx, level, "operation_completed", "operation", name, "outcome", outcome, "duration_ms", time.Since(started).Milliseconds())
	}
}
