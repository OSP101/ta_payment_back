package telemetry

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var outbound = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "ta_outbound_http_duration_seconds", Help: "External HTTP time to response headers.", Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30}}, []string{"integration", "status"})

func init() { Registry.MustRegister(outbound) }

type HTTPTransport struct {
	Integration string
	Base        http.RoundTripper
}

func (t HTTPTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	ctx, span := otel.Tracer("ta-payment/http-client").Start(Parent(r.Context()), "external "+t.Integration, trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	started := time.Now()
	response, err := base.RoundTrip(r.Clone(ctx))
	status := "network_error"
	if response != nil {
		status = strconv.Itoa(response.StatusCode)
		span.SetAttributes(attribute.Int("http.response.status_code", response.StatusCode))
	}
	if err != nil || response != nil && response.StatusCode >= 500 {
		span.SetStatus(codes.Error, "external request failed")
	}
	outbound.WithLabelValues(t.Integration, status).Observe(time.Since(started).Seconds())
	// No URL, path, headers or body: SSO tokens and REG identifiers can be in
	// any of these. Integration names must be constants supplied by code.
	return response, err
}
