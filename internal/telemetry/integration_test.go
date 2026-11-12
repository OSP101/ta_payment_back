package telemetry_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"ta-payment-back/internal/db"
	"ta-payment-back/internal/telemetry"
)

// Invoked by deploy/scripts/test-observability.py against an isolated Tempo.
func TestTempoIntegration(t *testing.T) {
	endpoint := os.Getenv("OTEL_SMOKE_ENDPOINT")
	if endpoint == "" {
		t.Skip("requires isolated observability integration stack")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", endpoint+"/v1/traces")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")
	t.Setenv("METRICS_ADDR", "")
	old := otel.GetTracerProvider()
	defer otel.SetTracerProvider(old)
	stop, err := telemetry.Start(context.Background(), "smoke", "integration")
	if err != nil {
		t.Fatal(err)
	}
	ctx, root := otel.Tracer("smoke").Start(context.Background(), "observability smoke")
	id, _ := telemetry.TraceIDs(ctx)
	pool, err := db.Connect(ctx, os.Getenv("OTEL_SMOKE_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var value string
	if err := pool.QueryRow(ctx, "SELECT $1::text", "never-export-this").Scan(&value); err != nil || value != "never-export-this" {
		t.Fatal("fixture database query failed")
	}

	root.End()
	flush, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := stop(flush); err != nil {
		t.Fatal(err)
	}
	query := os.Getenv("OTEL_SMOKE_QUERY") + "/api/traces/" + id
	client := http.Client{Timeout: 3 * time.Second}
	for attempt := 0; attempt < 30; attempt++ {
		resp, err := client.Get(query)
		if err == nil {
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				if strings.Contains(string(data), "never-export-this") || strings.Contains(string(data), "SELECT secret_value") {
					t.Fatal("sensitive query data exported")
				}
				if !strings.Contains(string(data), "db SELECT") {
					t.Fatal("child span missing")
				}
				t.Log("Go OTLP exporter -> Tempo -> trace query verified; trace_id=" + id)
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatal("exported trace not queryable in Tempo")
}
