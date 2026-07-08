package nethttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var reader *sdkmetric.ManualReader

func TestMain(m *testing.M) {
	reader = sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	os.Exit(m.Run())
}

// durationCount sums the request counts recorded on the otelhttp server
// duration histogram — one per instrumented request.
func durationCount(t *testing.T) uint64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var total uint64
	for _, sm := range rm.ScopeMetrics {
		for _, mtr := range sm.Metrics {
			if mtr.Name != "http.server.request.duration" {
				continue
			}
			if h, ok := mtr.Data.(metricdata.Histogram[float64]); ok {
				for _, dp := range h.DataPoints {
					total += dp.Count
				}
			}
		}
	}
	return total
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func get(h http.Handler, path string) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
}

func TestHandlerWithSkipPathsReplacesDefault(t *testing.T) {
	h := Handler(okHandler(), WithSkipPaths("/internal/ping"))

	before := durationCount(t)
	get(h, "/internal/ping")
	if got := durationCount(t); got != before {
		t.Errorf("custom skip path must not be instrumented: delta = %d, want 0", got-before)
	}
}

func TestHandlerWithSkipPathsEmptyInstrumentsEverything(t *testing.T) {
	h := Handler(okHandler(), WithSkipPaths())

	before := durationCount(t)
	get(h, "/healthz")
	if got := durationCount(t); got != before+1 {
		t.Errorf("WithSkipPaths() must disable filtering: /healthz delta = %d, want 1", got-before)
	}
}

func TestHandlerSkippedPathsStillServed(t *testing.T) {
	h := Handler(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("skipped path must still reach the handler: status = %d, want 200", rec.Code)
	}
}
