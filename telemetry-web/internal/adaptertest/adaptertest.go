// Package adaptertest is the conformance suite for framework adapters. Every
// adapter (gin, echo, chi, future ones) runs Run against its own engine, which
// guarantees the adapters stay behaviorally identical: same metrics, same
// route templating, same status attribution, same panic semantics. It also
// exports the metric/span inspection helpers so adapter modules can write
// framework-specific tests against the same providers.
//
// Under internal/ deliberately. It is test scaffolding, not consumer API, and
// keeping it out of the public surface stops it appearing on pkg.go.dev and stops
// its imports (testing, sdk/metric, sdk/trace/tracetest) reading as part of the
// library's contract. The adapter modules can still import it: Go's internal rule
// is lexical on import path, not on module boundary, so anything under
// github.com/stakater/operator-utils/telemetry-web/ has access — including
// .../telemetry-web/adapters/gin, which is its own module. An out-of-tree adapter
// could not, which is accepted: a new adapter belongs in this repo.
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
	// Fail400 responds 400 — recorded as a 400, never promoted to a server fault.
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
// forward opts to Instrument — the suite uses them to configure the skip path.
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

// DurationStatusCount returns the cumulative http.server.request.duration
// observation count carrying both http.route=route and the given
// http.response.status_code. It is how the suite checks that the status a
// framework answers with reaches otelhttp, which is the only place an outcome is
// now recorded.
func DurationStatusCount(rm metricdata.ResourceMetrics, route string, status int) uint64 {
	var total uint64
	eachHist(rm, "http.server.request.duration", func(dp metricdata.HistogramDataPoint[float64]) {
		// Both presence checks matter: a missing key yields a zero Value, so
		// dropping ok would make route="" match every unrouted data point.
		r, okR := dp.Attributes.Value(attribute.Key("http.route"))
		c, okC := dp.Attributes.Value(attribute.Key("http.response.status_code"))
		if okR && okC && r.AsString() == route && c.AsInt64() == int64(status) {
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
//   - a matched route is recorded on the route TEMPLATE, never the raw path
//   - the status the request answers with reaches the duration metric, for a
//     handler-written 500 and a 4xx alike
//   - http.route is stamped on the server span and the otelhttp duration metric,
//     and the span is renamed to the semconv "{method} {route}" form
//   - a panicked request still carries http.route on its span
//   - unmatched requests carry no http.route at all, so a 404 scan mints no series
//   - a path excluded by WithSkipPaths records nothing at all: no span, no
//     duration observation
//   - a panic responds 500, increments http.server.panics, and lands on the
//     duration metric as a 500
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
		nethttp.WithSkipPaths(SkipPath),
	)

	t.Run("RecordedOnRouteTemplateNotRawPath", func(t *testing.T) {
		Reset()
		before := Collect(t)
		if rec := get(h, "/conf/ok/42"); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		after := Collect(t)
		if got := DurationCount(after, okT) - DurationCount(before, okT); got != 1 {
			t.Errorf("observation count for template = %d, want 1", got)
		}
		// The whole point of route templates: /conf/ok/42 and /conf/ok/43 must not
		// each get a time series.
		if RouteOnDuration(after, "/conf/ok/42") {
			t.Error("raw path recorded as http.route")
		}
	})

	// The status a framework answers with has to reach otelhttp, which is now the
	// only place it is recorded. Echo is the case that matters: its error handler
	// runs after the middleware chain, so a 500 originates outside the handler.
	t.Run("StatusRecordedOnDurationMetric", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			path   string
			route  string
			status int
		}{
			{"ServerError", "/conf/fail/7", failT, http.StatusInternalServerError},
			{"ClientError", "/conf/client/7", clientT, http.StatusBadRequest},
		} {
			t.Run(tc.name, func(t *testing.T) {
				Reset()
				before := Collect(t)
				if rec := get(h, tc.path); rec.Code != tc.status {
					t.Fatalf("status = %d, want %d", rec.Code, tc.status)
				}
				after := Collect(t)
				got := DurationStatusCount(after, tc.route, tc.status) -
					DurationStatusCount(before, tc.route, tc.status)
				if got != 1 {
					t.Errorf("observations at {route=%s, status=%d} = %d, want 1", tc.route, tc.status, got)
				}
			})
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

	// Every other case serves the handler at the root, which hides a whole failure
	// mode: otelhttp re-names the span after the handler returns whenever
	// r.Pattern != "", so an instrumented router mounted under a mux pattern can end
	// up named after the coarse outer pattern while http.route on the same span
	// holds the real template. Mounting an API router beside /healthz is ordinary
	// wiring, so the adapter has to survive it.
	t.Run("MountedUnderMuxKeepsRouteNamedSpan", func(t *testing.T) {
		Reset()
		outer := http.NewServeMux()
		outer.Handle("/mounted/", http.StripPrefix("/mounted", h))

		rec := httptest.NewRecorder()
		outer.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mounted/conf/ok/42", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if want := "GET " + okT; !SpanNamed(want) {
			t.Errorf("span not named %q; the outer mux pattern overwrote the route", want)
		}
		if !RouteOnSpan(okT) {
			t.Errorf("http.route %q missing from the span", okT)
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

	// A 404 scan must not mint a time series per probed path, so an unmatched
	// request has to be recorded with no http.route at all rather than with the raw
	// URL. The adapters skip StampRoute when the framework matched nothing.
	t.Run("UnmatchedRouteCarriesNoRoute", func(t *testing.T) {
		Reset()
		const raw = "/conf/definitely/not/registered"
		get(h, raw)
		after := Collect(t)
		if RouteOnDuration(after, raw) {
			t.Error("unmatched request recorded the raw path as http.route")
		}
		if RouteOnSpan(raw) {
			t.Error("unmatched request stamped the raw path on its span")
		}
		// The raw-path checks alone would pass an adapter that dropped its
		// route != "" guard and stamped the empty template: that mints an
		// http_route="" series rather than leaving the attribute off.
		if got := DurationCount(after, ""); got != 0 {
			t.Errorf("unmatched request recorded %d observations at http.route=\"\", want 0", got)
		}
	})

	t.Run("SkippedPathRecordsNothing", func(t *testing.T) {
		Reset()
		before := Collect(t)
		if rec := get(h, SkipPath); rec.Code != http.StatusOK {
			t.Fatalf("skipped path must still be served, status = %d", rec.Code)
		}
		after := Collect(t)
		if delta := DurationTotal(after) - DurationTotal(before); delta != 0 {
			t.Errorf("skipped path recorded %d duration observations, want 0", delta)
		}
		if n := SpanCount(); n != 0 {
			t.Errorf("skipped path produced %d spans, want 0", n)
		}
	})

	t.Run("PanicRecords500AndCounter", func(t *testing.T) {
		Reset()
		before := Collect(t)
		if rec := get(h, "/conf/panic/1"); rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
		after := Collect(t)
		if delta := PanicCount(after) - PanicCount(before); delta != 1 {
			t.Errorf("panic counter delta = %d, want 1", delta)
		}
		// Recovery writes the 500 before otelhttp records, so the panicked request
		// is a 500 on the duration metric rather than missing from it.
		if got := DurationStatusCount(after, panicT, http.StatusInternalServerError) -
			DurationStatusCount(before, panicT, http.StatusInternalServerError); got != 1 {
			t.Errorf("panicked request observations at status=500 = %d, want 1", got)
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
