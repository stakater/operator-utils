package endpoint

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"

	"github.com/stakater/operator-utils/telemetry-web/logging"
)

func TestRecordPanicIncrementsCounter(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	resetInstruments()

	RecordPanic(context.Background(), "boom")

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !hasMetric(rm, "http.server.panics") {
		t.Errorf("http.server.panics not recorded")
	}
}

// A panic spike is only actionable if it says which endpoint spiked.
func TestRecordPanicCarriesAttributes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	resetInstruments()

	RecordPanic(context.Background(), "boom", semconv.HTTPRoute("/users/{id}"))

	if got := panicRoute(t, reader); got != "/users/{id}" {
		t.Errorf("http.route on http.server.panics = %q, want %q", got, "/users/{id}")
	}
}

// No attributes must stay legal: a hand-wired recovery that knows no route still
// has to be able to count the panic.
func TestRecordPanicWithoutAttributes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	resetInstruments()

	RecordPanic(context.Background(), "boom")

	if got := panicRoute(t, reader); got != "" {
		t.Errorf("http.route = %q, want it absent", got)
	}
}

// The stack must reach the log, not only the span: with tracing off or the trace
// unsampled the span carries nothing, and a panic without a stack is not
// debuggable.
func TestRecordPanicLogsAStack(t *testing.T) {
	var buf bytes.Buffer
	logging.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { logging.SetDefault(nil) })

	otel.SetMeterProvider(sdkmetric.NewMeterProvider())
	resetInstruments()

	RecordPanic(context.Background(), "boom")

	out := buf.String()
	if !strings.Contains(out, `"stack"`) {
		t.Errorf("no stack on the panic log: %s", out)
	}
	// The frame that called RecordPanic must be in it, or the stack was captured
	// somewhere useless.
	if !strings.Contains(out, "TestRecordPanicLogsAStack") {
		t.Errorf("stack does not reach the calling frame: %s", out)
	}
}

// Recovered is the shared rule every adapter's recovery defers to, so both halves
// are asserted here rather than three times over.
func TestRecoveredHandlesAbortSentinel(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	resetInstruments()

	// ErrAbortHandler means "drop the connection silently"; it is not a panic to
	// report, and the caller must re-raise it.
	if Recovered(context.Background(), http.ErrAbortHandler) {
		t.Error("Recovered(ErrAbortHandler) = true, want false so the caller re-panics")
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if hasMetric(rm, "http.server.panics") {
		t.Error("ErrAbortHandler must not be counted as a panic")
	}

	if !Recovered(context.Background(), "boom") {
		t.Error("Recovered(other) = false, want true")
	}
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !hasMetric(rm, "http.server.panics") {
		t.Error("a real panic must be counted")
	}
}

// panicRoute returns the http.route attribute on the panic counter's single data
// point, or "" when absent.
func panicRoute(t *testing.T, reader *sdkmetric.ManualReader) string {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http.server.panics" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || len(sum.DataPoints) == 0 {
				t.Fatalf("http.server.panics has no int64 data points")
			}
			v, found := sum.DataPoints[0].Attributes.Value(semconv.HTTPRouteKey)
			if !found {
				return ""
			}
			return v.AsString()
		}
	}
	t.Fatal("http.server.panics not recorded")
	return ""
}
