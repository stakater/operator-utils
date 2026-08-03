// Package adaptertest is the conformance suite for framework adapters. Every
// adapter (gin, echo, chi, future ones) runs Run against its own engine, which
// guarantees the adapters stay behaviorally identical: same metrics, same
// route templating, same outcome classification, same panic semantics. It also
// exports the metric/span inspection helpers so adapter modules can write
// framework-specific tests against the same providers.
//
// Usage in an adapter module:
//
//	func TestMain(m *testing.M) {
//	    adaptertest.Setup()
//	    os.Exit(m.Run())
//	}
//
//	func TestConformance(t *testing.T) {
//	    adaptertest.Run(t, func(routes []adaptertest.Route, opts ...nethttp.Option) http.Handler {
//	        engine := gin.New()
//	        h := Instrument(engine, opts...)
//	        // register each route with the requested Behavior ...
//	        return h
//	    })
//	}
//
// Every subtest issues its own requests and asserts on deltas, so subtests may
// be run individually or in any order.
//
// Not parallel-safe. The reader and span recorder are process-wide globals shared
// by every helper, and Reset clears spans for whoever calls it next, so an adapter
// author must not add t.Parallel() to a test that uses these helpers. The metric
// deltas would interleave and the span assertions would see another test's spans.
package adaptertest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/stakater/operator-utils/telemetry-web/nethttp"
)

// Behavior a registered conformance route must implement.
type Behavior int

const (
	// OK responds 200.
	OK Behavior = iota
	// Fail500 responds 500.
	Fail500
	// Fail400 responds 400 — a client error, which must count as a success.
	Fail400
	// Panic panics with a non-abort value.
	Panic
	// PanicAbort panics with http.ErrAbortHandler.
	PanicAbort
	// Stream flushes a chunked response, requiring the ResponseWriter handed to
	// the handler to still implement http.Flusher.
	Stream
)

// StreamBody is what a Stream route must write, one byte at a time with a flush
// between each.
const StreamBody = "abc"

// Route is a route the adapter must register on its engine.
type Route struct {
	Template string
	Behavior Behavior
	// Method defaults to GET when empty. The same Template may appear more than
	// once with different methods.
	Method string
}

// MethodOf returns the HTTP method a Route must be registered for. Adapters call
// it instead of reading Method directly, so an empty value keeps meaning GET.
func MethodOf(r Route) string {
	if r.Method == "" {
		return http.MethodGet
	}
	return r.Method
}

// SkipPath is the exact path the suite excludes via nethttp.WithSkipPaths. The
// adapter must register it like any other route; the suite asserts nothing is
// recorded for it.
const SkipPath = "/conf/skipped"

// BuildFunc returns the adapter's fully instrumented handler (the equivalent of
// Instrument(engine, opts...)) with the given routes registered. It MUST
// forward opts to Instrument — the suite uses them to turn on endpoint metrics
// and to configure the skip path.
type BuildFunc func(routes []Route, opts ...nethttp.Option) http.Handler

// RunOption configures Run.
type RunOption func(*runConfig)

type runConfig struct {
	rewrite func(string) string
}

// WithTemplateRewrite translates the suite's canonical ":param" route
// templates into the framework's syntax (e.g. chi: "/x/:id" -> "/x/{id}").
// The rewritten form is used both for the routes handed to build and for the
// endpoint values the suite expects on metrics. Default is identity (gin,
// echo).
func WithTemplateRewrite(fn func(string) string) RunOption {
	return func(c *runConfig) { c.rewrite = fn }
}

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

// Reset drops recorded spans so a subtest sees only the spans it produced.
// Metrics stay cumulative — the instruments are bound to the provider installed
// by Setup and cannot be rebound — so assert on deltas instead.
func Reset() {
	if spans != nil {
		spans.Reset()
	}
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

// EndpointOutcome returns the cumulative endpoint.requests count for
// {endpoint,outcome}.
func EndpointOutcome(rm metricdata.ResourceMetrics, route, outcome string) int64 {
	var total int64
	eachSum(rm, "endpoint.requests", func(dp metricdata.DataPoint[int64]) {
		ep, _ := dp.Attributes.Value(attribute.Key("endpoint"))
		oc, _ := dp.Attributes.Value(attribute.Key("outcome"))
		if ep.AsString() == route && oc.AsString() == outcome {
			total += dp.Value
		}
	})
	return total
}

// EndpointTotal returns the cumulative endpoint.requests count across all
// endpoints and outcomes.
func EndpointTotal(rm metricdata.ResourceMetrics) int64 {
	var total int64
	eachSum(rm, "endpoint.requests", func(dp metricdata.DataPoint[int64]) { total += dp.Value })
	return total
}

// PanicCount returns the cumulative http.server.panics count.
func PanicCount(rm metricdata.ResourceMetrics) int64 {
	var total int64
	eachSum(rm, "http.server.panics", func(dp metricdata.DataPoint[int64]) { total += dp.Value })
	return total
}

// PanicCountForRoute returns the cumulative http.server.panics count carrying
// http.route=route.
func PanicCountForRoute(rm metricdata.ResourceMetrics, route string) int64 {
	var total int64
	eachSum(rm, "http.server.panics", func(dp metricdata.DataPoint[int64]) {
		if v, ok := dp.Attributes.Value(attribute.Key("http.route")); ok && v.AsString() == route {
			total += dp.Value
		}
	})
	return total
}

// DurationCount returns the cumulative http.server.request.duration observation
// count carrying http.route=route.
func DurationCount(rm metricdata.ResourceMetrics, route string) uint64 {
	var total uint64
	eachHist(rm, "http.server.request.duration", func(dp metricdata.HistogramDataPoint[float64]) {
		if v, ok := dp.Attributes.Value(attribute.Key("http.route")); ok && v.AsString() == route {
			total += dp.Count
		}
	})
	return total
}

// DurationTotal returns the cumulative http.server.request.duration observation
// count across all attribute sets.
func DurationTotal(rm metricdata.ResourceMetrics) uint64 {
	var total uint64
	eachHist(rm, "http.server.request.duration", func(dp metricdata.HistogramDataPoint[float64]) {
		total += dp.Count
	})
	return total
}

// RouteOnDuration reports whether the otelhttp duration histogram has a data
// point carrying http.route=route.
func RouteOnDuration(rm metricdata.ResourceMetrics, route string) bool {
	return DurationCount(rm, route) > 0
}

// ended returns the spans recorded since the last Reset. Setup installs the
// recorder, so forgetting it in TestMain would otherwise nil-panic here instead of
// producing the same clear message Collect gives.
func ended() []sdktrace.ReadOnlySpan {
	if spans == nil {
		panic("adaptertest.Setup was not called from TestMain")
	}
	return spans.Ended()
}

// RouteOnSpan reports whether any span recorded since the last Reset carries
// http.route=route.
func RouteOnSpan(route string) bool {
	for _, s := range ended() {
		for _, kv := range s.Attributes() {
			if kv.Key == "http.route" && kv.Value.AsString() == route {
				return true
			}
		}
	}
	return false
}

// SpanNamed reports whether any span recorded since the last Reset has the
// given name.
func SpanNamed(name string) bool {
	for _, s := range ended() {
		if s.Name() == name {
			return true
		}
	}
	return false
}

// SpanCount returns the number of spans recorded since the last Reset.
func SpanCount() int { return len(ended()) }

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

func eachHist(rm metricdata.ResourceMetrics, name string, fn func(metricdata.HistogramDataPoint[float64])) {
	for _, sm := range rm.ScopeMetrics {
		for _, mtr := range sm.Metrics {
			if mtr.Name != name {
				continue
			}
			if h, ok := mtr.Data.(metricdata.Histogram[float64]); ok {
				for _, dp := range h.DataPoints {
					fn(dp)
				}
			}
		}
	}
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	return do(h, http.MethodGet, path)
}

func do(h http.Handler, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Run executes the adapter contract against the handler build returns:
//
//   - success on a matched route is recorded on the route TEMPLATE, never the
//     raw path, with outcome=success
//   - a 500 response records outcome=failure
//   - a 4xx response records outcome=SUCCESS — only server-side faults count
//   - http.route is stamped on the server span and the otelhttp duration metric,
//     and the span is renamed to the semconv "{method} {route}" form
//   - a panicked request still carries http.route on its span
//   - unmatched requests record no per-endpoint data point
//   - a path excluded by WithSkipPaths records nothing at all: no span, no
//     duration observation, no per-endpoint data point
//   - a panic responds 500, increments http.server.panics, and records NO
//     per-endpoint data point
//   - http.ErrAbortHandler is re-raised untouched and not counted
//   - the ResponseWriter reaching the handler still implements http.Flusher, so
//     SSE and other streaming responses work through the instrumented chain
//   - the panic counter carries http.route, so a spike names an endpoint
//   - POST, HEAD, and OPTIONS are instrumented the same as GET, span name included
func Run(t *testing.T, build BuildFunc, opts ...RunOption) {
	if reader == nil {
		t.Fatal("adaptertest.Setup was not called from TestMain")
	}
	cfg := runConfig{rewrite: func(s string) string { return s }}
	for _, opt := range opts {
		opt(&cfg)
	}
	okT, failT := cfg.rewrite("/conf/ok/:id"), cfg.rewrite("/conf/fail/:id")
	clientT := cfg.rewrite("/conf/client/:id")
	panicT, abortT := cfg.rewrite("/conf/panic/:id"), cfg.rewrite("/conf/abort")
	streamT := cfg.rewrite("/conf/stream")
	methodT := cfg.rewrite("/conf/method")

	h := build([]Route{
		{Template: okT, Behavior: OK},
		{Template: failT, Behavior: Fail500},
		{Template: clientT, Behavior: Fail400},
		{Template: panicT, Behavior: Panic},
		{Template: abortT, Behavior: PanicAbort},
		{Template: streamT, Behavior: Stream},
		{Template: SkipPath, Behavior: OK},
		// Same template, three methods: everything else in the suite is a GET, so
		// nothing would notice a chain that only works for one method.
		{Template: methodT, Behavior: OK, Method: http.MethodPost},
		{Template: methodT, Behavior: OK, Method: http.MethodHead},
		{Template: methodT, Behavior: OK, Method: http.MethodOptions},
	},
		nethttp.WithEndpointMetrics(),
		nethttp.WithSkipPaths(SkipPath),
	)

	t.Run("SuccessRecordedOnRouteTemplate", func(t *testing.T) {
		Reset()
		before := Collect(t)
		if rec := get(h, "/conf/ok/42"); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		after := Collect(t)
		if got := EndpointOutcome(after, okT, "success") - EndpointOutcome(before, okT, "success"); got != 1 {
			t.Errorf("success count for template = %d, want 1", got)
		}
		if got := EndpointOutcome(after, "/conf/ok/42", "success"); got != 0 {
			t.Errorf("raw path must not be recorded, got %d", got)
		}
	})

	t.Run("Status500RecordsFailure", func(t *testing.T) {
		Reset()
		before := Collect(t)
		get(h, "/conf/fail/7")
		after := Collect(t)
		if got := EndpointOutcome(after, failT, "failure") - EndpointOutcome(before, failT, "failure"); got != 1 {
			t.Errorf("failure count = %d, want 1", got)
		}
	})

	t.Run("ClientErrorRecordsSuccess", func(t *testing.T) {
		Reset()
		before := Collect(t)
		if rec := get(h, "/conf/client/7"); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		after := Collect(t)
		if got := EndpointOutcome(after, clientT, "success") - EndpointOutcome(before, clientT, "success"); got != 1 {
			t.Errorf("4xx must record outcome=success, got delta %d", got)
		}
		if got := EndpointOutcome(after, clientT, "failure") - EndpointOutcome(before, clientT, "failure"); got != 0 {
			t.Errorf("4xx must not record outcome=failure, got delta %d", got)
		}
	})

	t.Run("HTTPRouteStampedOnMetricAndSpan", func(t *testing.T) {
		Reset()
		before := Collect(t)
		get(h, "/conf/ok/42")
		after := Collect(t)
		if DurationCount(after, okT)-DurationCount(before, okT) != 1 {
			t.Error("http.route not stamped on http.server.request.duration")
		}
		if !RouteOnSpan(okT) {
			t.Error("http.route not stamped on the server span")
		}
		if !SpanNamed("GET " + okT) {
			t.Errorf("server span not renamed to semconv form %q", "GET "+okT)
		}
	})

	t.Run("PanickedRequestStillCarriesRouteOnSpan", func(t *testing.T) {
		Reset()
		get(h, "/conf/panic/1")
		if !RouteOnSpan(panicT) {
			t.Error("panicked request lost http.route on its span")
		}
	})

	// A panic spike is only actionable if the counter says which endpoint spiked.
	t.Run("PanicCounterCarriesRoute", func(t *testing.T) {
		Reset()
		before := PanicCountForRoute(Collect(t), panicT)
		get(h, "/conf/panic/1")
		if delta := PanicCountForRoute(Collect(t), panicT) - before; delta != 1 {
			t.Errorf("http.server.panics{http.route=%q} delta = %d, want 1", panicT, delta)
		}
	})

	// Every other case here is a GET, so a chain that mishandled another method
	// would go unnoticed. HEAD in particular is where response-writer wrapping
	// tends to break.
	t.Run("NonGETMethodsInstrumented", func(t *testing.T) {
		for _, method := range []string{http.MethodPost, http.MethodHead, http.MethodOptions} {
			t.Run(method, func(t *testing.T) {
				Reset()
				before := Collect(t)
				if rec := do(h, method, "/conf/method"); rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200", rec.Code)
				}
				after := Collect(t)
				if got := EndpointOutcome(after, methodT, "success") - EndpointOutcome(before, methodT, "success"); got != 1 {
					t.Errorf("success delta = %d, want 1", got)
				}
				if DurationCount(after, methodT)-DurationCount(before, methodT) != 1 {
					t.Error("http.route not stamped on http.server.request.duration")
				}
				// The span name is "{method} {route}", so the method has to reach
				// StampRoute rather than being hardcoded to GET.
				if want := method + " " + methodT; !SpanNamed(want) {
					t.Errorf("server span not named %q", want)
				}
			})
		}
	})

	t.Run("UnmatchedRouteNotRecorded", func(t *testing.T) {
		Reset()
		before := EndpointTotal(Collect(t))
		get(h, "/conf/definitely/not/registered")
		if delta := EndpointTotal(Collect(t)) - before; delta != 0 {
			t.Errorf("unmatched route recorded %d data points, want 0", delta)
		}
	})

	t.Run("SkippedPathRecordsNothing", func(t *testing.T) {
		Reset()
		before := Collect(t)
		if rec := get(h, SkipPath); rec.Code != http.StatusOK {
			t.Fatalf("skipped path must still be served, status = %d", rec.Code)
		}
		after := Collect(t)
		if delta := EndpointTotal(after) - EndpointTotal(before); delta != 0 {
			t.Errorf("skipped path recorded %d endpoint.requests points, want 0", delta)
		}
		if delta := DurationTotal(after) - DurationTotal(before); delta != 0 {
			t.Errorf("skipped path recorded %d duration observations, want 0", delta)
		}
		if n := SpanCount(); n != 0 {
			t.Errorf("skipped path produced %d spans, want 0", n)
		}
	})

	t.Run("PanicRecords500AndCounterNotEndpoint", func(t *testing.T) {
		Reset()
		before := Collect(t)
		if rec := get(h, "/conf/panic/1"); rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
		after := Collect(t)
		if delta := PanicCount(after) - PanicCount(before); delta != 1 {
			t.Errorf("panic counter delta = %d, want 1", delta)
		}
		if delta := EndpointTotal(after) - EndpointTotal(before); delta != 0 {
			t.Errorf("panicked request recorded %d per-endpoint data points, want 0", delta)
		}
	})

	// Guards against any adapter (or the core Recovery) wrapping the
	// ResponseWriter in a way that drops http.Flusher. gin's and echo's Flush
	// type-assert, so losing it panics at runtime rather than degrading.
	t.Run("StreamingResponseWorks", func(t *testing.T) {
		Reset()
		srv := httptest.NewServer(h)
		defer srv.Close()

		resp, err := http.Get(srv.URL + streamT)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(body) != StreamBody {
			t.Errorf("streamed body = %q, want %q — the handler's ResponseWriter likely lost http.Flusher", body, StreamBody)
		}
	})

	t.Run("ErrAbortHandlerReRaisedNotCounted", func(t *testing.T) {
		Reset()
		before := PanicCount(Collect(t))
		defer func() {
			if rec := recover(); rec != http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity
				t.Errorf("expected ErrAbortHandler to propagate, got %v", rec)
			}
			if delta := PanicCount(Collect(t)) - before; delta != 0 {
				t.Errorf("ErrAbortHandler must not be counted: delta %d", delta)
			}
		}()
		get(h, "/conf/abort")
	})
}
