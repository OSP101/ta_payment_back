// Package telemetry owns bounded, asynchronous diagnostics. It never records
// SQL text, query arguments, request bodies, credentials or customer documents.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// ContextKey bridges Fiber's RequestCtx (used by existing services) to an
// immutable OTel context. Do not put a Fiber context inside an exported span.
const ContextKey = "ta_payment.telemetry.context"

func Parent(ctx context.Context) context.Context {
	if parent, ok := ctx.Value(ContextKey).(context.Context); ok {
		// Keep the caller's deadline/cancellation and an already-active child
		// span. Replacing ctx with parent would discard service timeouts.
		if !trace.SpanContextFromContext(ctx).IsValid() {
			ctx = trace.ContextWithSpan(ctx, trace.SpanFromContext(parent))
		}
		if ctx.Value(requestKey{}) == nil {
			if id, ok := parent.Value(requestKey{}).(string); ok {
				ctx = WithRequest(ctx, id)
			}
		}
	}
	return ctx
}

var Registry = prometheus.NewRegistry()
var HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "ta_http_requests_total", Help: "API requests by matched route and outcome."}, []string{"method", "route", "status"})
var HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "ta_http_request_duration_seconds", Help: "API request duration.", Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}}, []string{"method", "route"})
var HTTPActive = prometheus.NewGauge(prometheus.GaugeOpts{Name: "ta_http_requests_active", Help: "Requests currently executing."})
var DBQueries = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "ta_db_query_duration_seconds", Help: "Database round trip including row consumption.", Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 15, 60}}, []string{"operation", "outcome"})
var Operations = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "ta_operations_total", Help: "Business and background operation outcomes."}, []string{"operation", "outcome"})
var JobLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "ta_job_last_success_timestamp_seconds", Help: "Last completed successful background run."}, []string{"task"})
var NotificationPending = prometheus.NewGauge(prometheus.GaugeOpts{Name: "ta_notification_outbox_pending", Help: "Pending or processing notification messages."})
var NotificationFailed = prometheus.NewGauge(prometheus.GaugeOpts{Name: "ta_notification_outbox_failed", Help: "Notification messages requiring review after retries."})
var NotificationOldest = prometheus.NewGauge(prometheus.GaugeOpts{Name: "ta_notification_outbox_oldest_seconds", Help: "Age of the oldest pending or processing message."})
var ExportErrors = prometheus.NewCounter(prometheus.CounterOpts{Name: "ta_trace_export_errors_total", Help: "Failed OpenTelemetry trace exports."})

func init() {
	JobLastSuccess.WithLabelValues("hourly").Set(0)
	Registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), HTTPRequests, HTTPDuration, HTTPActive, DBQueries, Operations, JobLastSuccess)
	Registry.MustRegister(NotificationPending, NotificationFailed, NotificationOldest)
	Registry.MustRegister(ExportErrors)
}

// Start validates configuration before serving. Callers keep the application
// available if telemetry cannot start; the explicit error must be logged.
func Start(ctx context.Context, env, release string) (func(context.Context) error, error) {
	var provider *sdktrace.TracerProvider
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	if endpoint != "" {
		rate := 1.0
		if raw := os.Getenv("OTEL_TRACES_SAMPLER_ARG"); raw != "" {
			var err error
			rate, err = strconv.ParseFloat(raw, 64)
			if err != nil || !(rate >= 0 && rate <= 1) {
				return nil, fmt.Errorf("invalid OTEL_TRACES_SAMPLER_ARG")
			}
		}
		exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint), otlptracehttp.WithTimeout(3*time.Second))
		if err != nil {
			return nil, fmt.Errorf("trace exporter initialization failed")
		}
		provider = sdktrace.NewTracerProvider(
			sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "ta-payment-api"), attribute.String("service.version", release), attribute.String("deployment.environment.name", env))),
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(rate))),
			sdktrace.WithBatcher(exporter, sdktrace.WithMaxQueueSize(2048), sdktrace.WithExportTimeout(3*time.Second)),
		)
		otel.SetTracerProvider(provider)
		otel.SetTextMapPropagator(propagation.TraceContext{})
		// Exporter errors can contain endpoints/headers. Emit a fixed message.
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) { ExportErrors.Inc(); slog.Warn("telemetry_export_failed") }))
	}
	var server *http.Server
	shutdown := func(ctx context.Context) error {
		var result error
		if server != nil {
			result = server.Shutdown(ctx)
		}
		if provider != nil {
			if err := provider.Shutdown(ctx); err != nil {
				result = err
			}
		}
		return result
	}
	if addr := os.Getenv("METRICS_ADDR"); addr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(Registry, promhttp.HandlerOpts{Timeout: 5 * time.Second, MaxRequestsInFlight: 2}))
		server = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 3 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			_ = shutdown(ctx)
			return nil, fmt.Errorf("metrics listener failed")
		}
		go func() {
			if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
				slog.Error("metrics_server_failed")
			}
		}()
	}
	slog.Info("telemetry_started", "tracing", provider != nil, "metrics", server != nil)
	return shutdown, nil
}

func TraceIDs(ctx context.Context) (string, string) {
	sc := trace.SpanContextFromContext(Parent(ctx))
	if !sc.IsValid() {
		return "", ""
	}
	return sc.TraceID().String(), sc.SpanID().String()
}
