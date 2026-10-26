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
	"github.com/stakater/operator-utils/telemetry-web/internal/ident"
	"github.com/stakater/operator-utils/telemetry-web/nethttp"
)

// newMeterProvider builds a provider standing in for the component that owns
// metrics, such as observability's Publisher, and shuts it down afterwards.
func newMeterProvider(t *testing.T) (*sdkmetric.MeterProvider, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return mp, reader
}

// ownMeterProvider is newMeterProvider, installed as the global one, with the
// previous global restored afterwards.
func ownMeterProvider(t *testing.T) (*sdkmetric.MeterProvider, *sdkmetric.ManualReader) {
	t.Helper()
	mp, reader := newMeterProvider(t)
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	return mp, reader
}

// initExternal runs Init with the owner's provider, restoring the global
// MeterProvider that Init replaces.
func initExternal(t *testing.T, mp metric.MeterProvider) func(context.Context) error {
	t.Helper()
	prev := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	shutdown, err := Init(context.Background(), Config{
		ServiceName:   "test-svc",
		Insecure:      true,
		MeterProvider: mp,
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

func TestMeterProvider_InitMakesItGlobal(t *testing.T) {
	ownMeterProvider(t) // a different provider is global before Init
	mp, _ := newMeterProvider(t)
	shutdown := initExternal(t, mp)
	defer func() { _ = shutdown(bounded(t)) }()

	if got := otel.GetMeterProvider(); got != metric.MeterProvider(mp) {
		t.Errorf("global MeterProvider is %T after Init, want the caller's", got)
	}
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Errorf("global TracerProvider is %T after Init, want *sdktrace.TracerProvider", otel.GetTracerProvider())
	}
}

func TestMeterProvider_ShutdownLeavesTheCallersMeterProvider(t *testing.T) {
	mp, _ := newMeterProvider(t)
	shutdown := initExternal(t, mp)
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
	prevTP, prevName := otel.GetTracerProvider(), ident.ServiceName()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		ident.SetServiceName(prevName)
	})
	mp, _ := ownMeterProvider(t)

	retire(false)

	if got := otel.GetMeterProvider(); got != metric.MeterProvider(mp) {
		t.Errorf("global MeterProvider is %T after retire(false), want the caller's", got)
	}
	if !isNoopTracerProvider(otel.GetTracerProvider()) {
		t.Errorf("global TracerProvider is %T after retire(false), want the noop", otel.GetTracerProvider())
	}
}

func TestMeterProvider_InstrumentationRecordsIntoTheCallersProvider(t *testing.T) {
	mp, reader := newMeterProvider(t)
	shutdown := initExternal(t, mp)
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

func TestInitRejectsANilPointerMeterProvider(t *testing.T) {
	var mp *sdkmetric.MeterProvider
	tp := otel.GetTracerProvider()
	shutdown, err := Init(context.Background(), Config{ServiceName: "svc", MeterProvider: mp})
	if err == nil || !strings.Contains(err.Error(), "nil pointer") {
		if err == nil {
			_ = shutdown(context.Background())
		}
		t.Fatalf("Init error = %v, want a nil pointer error", err)
	}
	if got := otel.GetTracerProvider(); got != tp {
		t.Errorf("global TracerProvider changed to %T", got)
	}
}
