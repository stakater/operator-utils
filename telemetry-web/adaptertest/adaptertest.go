// Package adaptertest is the conformance suite for framework adapters. Every
// adapter (gin, echo, future ones) runs Run against its own engine, which
// guarantees the adapters stay behaviorally identical: same metrics, same
// route templating, same panic semantics. It also exports the metric/span
// inspection helpers so adapter modules can write framework-specific tests
// against the same providers.
//
// Usage in an adapter module:
//
//	func TestMain(m *testing.M) {
//	    adaptertest.Setup()
//	    os.Exit(m.Run())
//	}
//
//	func TestConformance(t *testing.T) {
//	    adaptertest.Run(t, func(routes []adaptertest.Route) http.Handler {
//	        engine := gin.New()
//	        h := Instrument(engine)
//	        // register each route with the requested Behavior ...
//	        return h
//	    })
//	}
package adaptertest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Behavior a registered conformance route must implement.
type Behavior int

const (
	// OK responds 200.
	OK Behavior = iota
	// Fail500 responds 500.
	Fail500
	// Panic panics with a non-abort value.
	Panic
	// PanicAbort panics with http.ErrAbortHandler.
	PanicAbort
)

// Route is a GET route the adapter must register on its engine.
type Route struct {
	Template string
	Behavior Behavior
}

// BuildFunc returns the adapter's fully instrumented handler (the equivalent
// of Instrument(engine)) with the given routes registered.
type BuildFunc func(routes []Route) http.Handler

var (
	reader *sdkmetric.ManualReader
	spans  *tracetest.SpanRecorder
)

// Setup installs a ManualReader-backed MeterProvider and a SpanRecorder-backed
// TracerProvider as the process globals. Call once from TestMain before m.Run;
// the endpoint package binds its instruments to the global provider on first
// use, so this must run before any request is served.
func Setup() {
	reader = sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	spans = tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
}

// Collect returns the current cumulative metric state.
func Collect(t *testing.T) metricdata.ResourceMetrics {
	t.Helper()
	if reader == nil {
		t.Fatal("adaptertest.Setup was not called from TestMain")
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	return rm
}

// EndpointOutcome returns the cumulative http.endpoint.requests count for
// {endpoint,outcome}.
func EndpointOutcome(rm metricdata.ResourceMetrics, route, outcome string) int64 {
	var total int64
	eachSum(rm, "http.endpoint.requests", func(dp metricdata.DataPoint[int64]) {
		ep, _ := dp.Attributes.Value(attribute.Key("endpoint"))
		oc, _ := dp.Attributes.Value(attribute.Key("outcome"))
		if ep.AsString() == route && oc.AsString() == outcome {
			total += dp.Value
		}
	})
	return total
}

// EndpointTotal returns the cumulative http.endpoint.requests count across all
// endpoints and outcomes.
func EndpointTotal(rm metricdata.ResourceMetrics) int64 {
	var total int64
	eachSum(rm, "http.endpoint.requests", func(dp metricdata.DataPoint[int64]) { total += dp.Value })
	return total
}

// PanicCount returns the cumulative http.server.panics count.
func PanicCount(rm metricdata.ResourceMetrics) int64 {
	var total int64
	eachSum(rm, "http.server.panics", func(dp metricdata.DataPoint[int64]) { total += dp.Value })
	return total
}

// RouteOnDuration reports whether the otelhttp duration histogram has a data
// point carrying http.route=route.
func RouteOnDuration(rm metricdata.ResourceMetrics, route string) bool {
	for _, sm := range rm.ScopeMetrics {
		for _, mtr := range sm.Metrics {
			if mtr.Name != "http.server.request.duration" {
				continue
			}
			if h, ok := mtr.Data.(metricdata.Histogram[float64]); ok {
				for _, dp := range h.DataPoints {
					if v, ok := dp.Attributes.Value(attribute.Key("http.route")); ok && v.AsString() == route {
						return true
					}
				}
			}
		}
	}
	return false
}

// RouteOnSpan reports whether any recorded span carries http.route=route.
func RouteOnSpan(route string) bool {
	for _, s := range spans.Ended() {
		for _, kv := range s.Attributes() {
			if kv.Key == "http.route" && kv.Value.AsString() == route {
				return true
			}
		}
	}
	return false
}

func eachSum(rm metricdata.ResourceMetrics, name string, fn func(metricdata.DataPoint[int64])) {
	for _, sm := range rm.ScopeMetrics {
		for _, mtr := range sm.Metrics {
			if mtr.Name != name {
				continue
			}
			if sum, ok := mtr.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					fn(dp)
				}
			}
		}
	}
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Run executes the adapter contract against the handler build returns:
//
//   - success on a matched route is recorded on the route TEMPLATE, never the
//     raw path, with outcome=success
//   - a 500 response records outcome=failure
//   - http.route is stamped on the server span and the otelhttp duration metric
//   - unmatched requests record no per-endpoint data point
//   - a panic responds 500, increments http.server.panics, and records NO
//     per-endpoint data point
//   - http.ErrAbortHandler is re-raised untouched and not counted
func Run(t *testing.T, build BuildFunc) {
	if reader == nil {
		t.Fatal("adaptertest.Setup was not called from TestMain")
	}

	h := build([]Route{
		{Template: "/conf/ok/:id", Behavior: OK},
		{Template: "/conf/fail/:id", Behavior: Fail500},
		{Template: "/conf/panic/:id", Behavior: Panic},
		{Template: "/conf/abort", Behavior: PanicAbort},
	})

	t.Run("SuccessRecordedOnRouteTemplate", func(t *testing.T) {
		if rec := get(h, "/conf/ok/42"); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		rm := Collect(t)
		if got := EndpointOutcome(rm, "/conf/ok/:id", "success"); got != 1 {
			t.Errorf("success count for template = %d, want 1", got)
		}
		if got := EndpointOutcome(rm, "/conf/ok/42", "success"); got != 0 {
			t.Errorf("raw path must not be recorded, got %d", got)
		}
	})

	t.Run("Status500RecordsFailure", func(t *testing.T) {
		get(h, "/conf/fail/7")
		if got := EndpointOutcome(Collect(t), "/conf/fail/:id", "failure"); got != 1 {
			t.Errorf("failure count = %d, want 1", got)
		}
	})

	t.Run("HTTPRouteStampedOnMetricAndSpan", func(t *testing.T) {
		rm := Collect(t)
		if !RouteOnDuration(rm, "/conf/ok/:id") {
			t.Error("http.route not stamped on http.server.request.duration")
		}
		if !RouteOnSpan("/conf/ok/:id") {
			t.Error("http.route not stamped on the server span")
		}
	})

	t.Run("UnmatchedRouteNotRecorded", func(t *testing.T) {
		before := EndpointTotal(Collect(t))
		get(h, "/conf/definitely/not/registered")
		if delta := EndpointTotal(Collect(t)) - before; delta != 0 {
			t.Errorf("unmatched route recorded %d data points, want 0", delta)
		}
	})

	t.Run("PanicRecords500AndCounterNotEndpoint", func(t *testing.T) {
		beforePanics := PanicCount(Collect(t))
		beforeEndpoints := EndpointTotal(Collect(t))
		if rec := get(h, "/conf/panic/1"); rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
		rm := Collect(t)
		if delta := PanicCount(rm) - beforePanics; delta != 1 {
			t.Errorf("panic counter delta = %d, want 1", delta)
		}
		if delta := EndpointTotal(rm) - beforeEndpoints; delta != 0 {
			t.Errorf("panicked request recorded %d per-endpoint data points, want 0", delta)
		}
	})

	t.Run("ErrAbortHandlerReRaisedNotCounted", func(t *testing.T) {
		before := PanicCount(Collect(t))
		defer func() {
			if rec := recover(); rec != http.ErrAbortHandler {
				t.Errorf("expected ErrAbortHandler to propagate, got %v", rec)
			}
			if delta := PanicCount(Collect(t)) - before; delta != 0 {
				t.Errorf("ErrAbortHandler must not be counted: delta %d", delta)
			}
		}()
		get(h, "/conf/abort")
	})
}
