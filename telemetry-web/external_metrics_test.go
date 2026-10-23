package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/stakater/operator-utils/telemetry-web/endpoint"
	"github.com/stakater/operator-utils/telemetry-web/nethttp"
)

// ownMeterProvider installs a provider standing in for the component that
// owns metrics when ExternalMetrics is set, such as observability's
// Publisher, and restores the previous global afterwards.
func ownMeterProvider(t *testing.T) (*sdkmetric.MeterProvider, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	return mp, reader
}

func initExternal(t *testing.T) func(context.Context) error {
	t.Helper()
	shutdown, err := Init(context.Background(), Config{
		ServiceName:     "test-svc",
		Insecure:        true,
		ExternalMetrics: true,
	})
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	return shutdown
}

func metricNames(t *testing.T, r *sdkmetric.ManualReader) map[string]bool {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	names := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
		}
	}
	return names
}

func TestExternalMetrics_InitKeepsTheCallersMeterProvider(t *testing.T) {
	mp, _ := ownMeterProvider(t)
	shutdown := initExternal(t)
	defer func() { _ = shutdown(bounded(t)) }()

	if got := otel.GetMeterProvider(); got != metric.MeterProvider(mp) {
		t.Errorf("global MeterProvider is %T after Init, want the caller's", got)
	}
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Errorf("global TracerProvider is %T after Init, want *sdktrace.TracerProvider", otel.GetTracerProvider())
	}
}

func TestExternalMetrics_ShutdownLeavesTheCallersMeterProvider(t *testing.T) {
	mp, _ := ownMeterProvider(t)
	shutdown := initExternal(t)
	_ = shutdown(bounded(t))

	if got := otel.GetMeterProvider(); got != metric.MeterProvider(mp) {
		t.Errorf("global MeterProvider is %T after shutdown, want the caller's", got)
	}
	if !isNoopTracerProvider(otel.GetTracerProvider()) {
		t.Errorf("global TracerProvider is %T after shutdown, want the noop", otel.GetTracerProvider())
	}
}

// No input a test can provide fails Init after it starts changing global state
// (the endpoint check runs first), so the failure path's reset is tested on
// retire itself.
func TestRetireLeavesAMeterProviderItDoesNotOwn(t *testing.T) {
	mp, _ := ownMeterProvider(t)

	retire(false)

	if got := otel.GetMeterProvider(); got != metric.MeterProvider(mp) {
		t.Errorf("global MeterProvider is %T after retire(false), want the caller's", got)
	}
	if !isNoopTracerProvider(otel.GetTracerProvider()) {
		t.Errorf("global TracerProvider is %T after retire(false), want the noop", otel.GetTracerProvider())
	}
}

func TestExternalMetrics_InstrumentationRecordsIntoTheCallersProvider(t *testing.T) {
	_, reader := ownMeterProvider(t)
	shutdown := initExternal(t)
	defer func() { _ = shutdown(bounded(t)) }()

	h := nethttp.Handler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		endpoint.Instrument(r.Context(), "probe")(nil)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/probe", nil))

	names := metricNames(t, reader)
	for _, want := range []string{"http.server.request.duration", "endpoint.duration"} {
		if !names[want] {
			t.Errorf("caller's provider has no %s; got %v", want, names)
		}
	}
	for name := range names {
		if strings.HasPrefix(name, "go.") {
			t.Errorf("telemetry-web started runtime metric %s on the caller's provider", name)
		}
	}
}
