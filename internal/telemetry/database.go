package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type queryKey struct{}
type queryState struct {
	start                  time.Time
	operation, fingerprint string
}
type DBTracer struct{}

func queryOperation(sql string) string {
	fields := strings.Fields(sql)
	if len(fields) == 0 {
		return "OTHER"
	}
	switch op := strings.ToUpper(fields[0]); op {
	case "SELECT", "INSERT", "UPDATE", "DELETE", "BEGIN", "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE", "WITH", "COPY", "BATCH", "ACQUIRE", "CONNECT":
		return op
	default:
		return "OTHER"
	}
}

func (t DBTracer) TraceBatchStart(ctx context.Context, conn *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return t.TraceQueryStart(ctx, conn, pgx.TraceQueryStartData{SQL: "BATCH"})
}
func (DBTracer) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if data.Err != nil {
		trace.SpanFromContext(ctx).SetStatus(codes.Error, "batch query failed")
	}
}
func (t DBTracer) TraceBatchEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchEndData) {
	t.TraceQueryEnd(ctx, conn, pgx.TraceQueryEndData{Err: data.Err})
}
func (t DBTracer) TraceCopyFromStart(ctx context.Context, conn *pgx.Conn, _ pgx.TraceCopyFromStartData) context.Context {
	return t.TraceQueryStart(ctx, conn, pgx.TraceQueryStartData{SQL: "COPY"})
}
func (t DBTracer) TraceCopyFromEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceCopyFromEndData) {
	t.TraceQueryEnd(ctx, conn, pgx.TraceQueryEndData{Err: data.Err})
}
func (t DBTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return t.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "ACQUIRE"})
}
func (t DBTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	t.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: data.Err})
}

func (DBTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	op := queryOperation(data.SQL)
	hash := sha256.Sum256([]byte(data.SQL))
	fingerprint := hex.EncodeToString(hash[:8])
	ctx, _ = otel.Tracer("ta-payment/db").Start(Parent(ctx), "db "+op, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attribute.String("db.system.name", "postgresql"), attribute.String("db.operation.name", op), attribute.String("db.query.fingerprint", fingerprint)))
	return context.WithValue(ctx, queryKey{}, queryState{time.Now(), op, fingerprint})
}

func (DBTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	s, ok := ctx.Value(queryKey{}).(queryState)
	if !ok {
		return
	}
	span := trace.SpanFromContext(ctx)
	defer span.End()
	duration := time.Since(s.start)
	outcome := "success"
	failed := data.Err != nil && !errors.Is(data.Err, pgx.ErrNoRows)
	if failed {
		outcome = "error"
		span.SetStatus(codes.Error, "database operation failed")
	}
	DBQueries.WithLabelValues(s.operation, outcome).Observe(duration.Seconds())
	if failed || duration >= time.Second {
		state := ""
		var pgerr *pgconn.PgError
		if errors.As(data.Err, &pgerr) {
			state = pgerr.Code
			span.SetAttributes(attribute.String("db.response.status_code", state))
		}
		slog.WarnContext(ctx, "database_operation", "operation", s.operation, "outcome", outcome, "duration_ms", duration.Milliseconds(), "query_fingerprint", s.fingerprint, "sqlstate", state)
	}
}

// ObservePool exposes capacity without credentials or database names.
func ObservePool(pool *pgxpool.Pool) {
	for _, item := range []struct {
		name  string
		help  string
		value func() float64
	}{
		{"connections", "Open pool connections.", func() float64 { return float64(pool.Stat().TotalConns()) }},
		{"acquired_connections", "Connections currently acquired.", func() float64 { return float64(pool.Stat().AcquiredConns()) }},
		{"max_connections", "Configured maximum pool size.", func() float64 { return float64(pool.Stat().MaxConns()) }},
		{"empty_acquire_total", "Acquisitions that waited for a connection.", func() float64 { return float64(pool.Stat().EmptyAcquireCount()) }},
		{"canceled_acquire_total", "Canceled pool acquisitions.", func() float64 { return float64(pool.Stat().CanceledAcquireCount()) }},
	} {
		if strings.HasSuffix(item.name, "_total") {
			Registry.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "ta_db_pool_" + item.name, Help: item.help}, item.value))
		} else {
			Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "ta_db_pool_" + item.name, Help: item.help}, item.value))
		}
	}
}
