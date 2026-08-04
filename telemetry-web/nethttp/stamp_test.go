package nethttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// StampRoute must set the semconv span name ("{method} {route}") and the
// http.route attribute on the active span.
func TestStampRouteSetsSpanNameAndAttribute(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	ctx, span := tp.Tracer("test").Start(context.Background(), "server")

	StampRoute(ctx, "GET", "/users/:id")
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("expected 1 span, got %d", len(ended))
	}
	if got := ended[0].Name(); got != "GET /users/:id" {
		t.Errorf("span name = %q, want %q", got, "GET /users/:id")
	}
	var route string
	for _, kv := range ended[0].Attributes() {
		if kv.Key == attribute.Key("http.route") {
			route = kv.Value.AsString()
		}
	}
	if route != "/users/:id" {
		t.Errorf("http.route = %q, want %q", route, "/users/:id")
	}
}

// A skipped path must make StampRoute a no-op. otelhttp's filter short-circuits
// before it injects the labeler, so a skipped request has no labeler and a
// non-recording span — but an adapter's RouteTag still runs, because a skip path
// is a registered route with a real template. Without the early return the
// recommended WithSkipPaths(DefaultSkipPaths...) setup warns about a wiring
// mistake on the first health probe.
//
// Asserted on the labeler rather than the log: the warning is behind a
// process-wide sync.Once, so a log assertion could pass vacuously depending on
// test order, while the labeler is exact either way.
func TestStampRouteSkipsExcludedPaths(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	ctx, span := tp.Tracer("test").Start(context.Background(), "server")

	// A labeler IS present here, unlike the real skipped request, so a StampRoute
	// that did any work at all would be visible.
	labeler := &otelhttp.Labeler{}
	ctx = otelhttp.ContextWithLabeler(ctx, labeler)
	ctx = context.WithValue(ctx, skipKey{}, true)

	StampRoute(ctx, "GET", "/healthz")
	span.End()

	if got := labeler.Get(); len(got) != 0 {
		t.Errorf("skipped request stamped %v on the metric labeler, want nothing", got)
	}
	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("expected 1 span, got %d", len(ended))
	}
	if got := ended[0].Name(); got != "server" {
		t.Errorf("span renamed to %q, want it left as %q", got, "server")
	}
	for _, kv := range ended[0].Attributes() {
		if kv.Key == attribute.Key("http.route") {
			t.Errorf("skipped request stamped http.route = %q on the span", kv.Value.AsString())
		}
	}
}

// Without a method, StampRoute must still attach the attribute but leave the
// span name alone (no " /route" names).
func TestStampRouteWithoutMethodKeepsSpanName(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	ctx, span := tp.Tracer("test").Start(context.Background(), "server")

	StampRoute(ctx, "", "/users/:id")
	span.End()

	if got := recorder.Ended()[0].Name(); got != "server" {
		t.Errorf("span name = %q, want unchanged %q", got, "server")
	}
}

// A request served through Handler with no route ever stamped must still get a
// method-named span, not the "server" operation string. Guards the semconv
// naming against an otelhttp upgrade or a stray WithSpanNameFormatter.
func TestHandlerNamesSpanByMethodWithoutRoute(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))

	h := Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/anything", nil))

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("expected 1 span, got %d", len(ended))
	}
	if got := ended[0].Name(); got != http.MethodPost {
		t.Errorf("span name = %q, want %q", got, http.MethodPost)
	}
}

// Mounting an instrumented router under a mux pattern must not cost the span its
// route-derived name. otelhttp re-sets the name after the handler returns whenever
// r.Pattern != "", so without Handler's spanName formatter the outer mux's coarse
// "/api/" would overwrite the template StampRoute recorded, leaving the span name
// and http.route on the SAME span disagreeing. Nothing else in the suite catches
// this, because everywhere else serves the handler at the root.
func TestMountedHandlerKeepsStampedSpanName(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))

	const template = "/api/users/{id}"
	inner := Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		StampRoute(r.Context(), r.Method, template)
		w.WriteHeader(http.StatusOK)
	}))
	outer := http.NewServeMux()
	outer.Handle("/api/", inner)

	outer.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/users/42", nil))

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("expected 1 span, got %d", len(ended))
	}
	if got, want := ended[0].Name(), "GET "+template; got != want {
		t.Errorf("span name = %q, want %q", got, want)
	}
	var route string
	for _, kv := range ended[0].Attributes() {
		if kv.Key == attribute.Key("http.route") {
			route = kv.Value.AsString()
		}
	}
	if route != template {
		t.Errorf("http.route = %q, want %q", route, template)
	}
	if name := ended[0].Name(); route != "" && name != "GET "+route {
		t.Errorf("span name %q and http.route %q disagree on the same span", name, route)
	}
}

// With nothing stamped, the mux pattern is the only route available and must still
// be used — that is otelhttp's own behavior and the reason a bare net/http mux
// gets route-attributed for free. Guards the formatter's fallback.
func TestMountedHandlerFallsBackToMuxPattern(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))

	mux := http.NewServeMux()
	mux.Handle("/items/{id}", Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/items/7", nil))

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("expected 1 span, got %d", len(ended))
	}
	if got, want := ended[0].Name(), "GET /items/{id}"; got != want {
		t.Errorf("span name = %q, want %q", got, want)
	}
}

// A non-standard method must collapse to "HTTP" rather than becoming part of the
// span name, so a client cannot mint a span name per request. semconv does this;
// the formatter has to keep doing it.
func TestSpanNameCollapsesNonStandardMethod(t *testing.T) {
	if got := spanName("", httptest.NewRequest("PROPFIND", "/x", nil)); got != "HTTP" {
		t.Errorf("span name = %q, want %q", got, "HTTP")
	}
	if got := spanName("", httptest.NewRequest(http.MethodGet, "/x", nil)); got != http.MethodGet {
		t.Errorf("span name = %q, want %q", got, http.MethodGet)
	}
}

// patternRoute mirrors otelhttp's internal httpRoute, which a mux pattern needs
// because it may carry a method and a host that do not belong in a route.
func TestPatternRoute(t *testing.T) {
	for _, tc := range []struct{ pattern, want string }{
		{"", ""},
		{"/foo/{id}", "/foo/{id}"},
		{"GET /foo/{id}", "/foo/{id}"},
		{"example.com/foo/{id}", "/foo/{id}"},
		{"GET example.com/foo/{id}", "/foo/{id}"},
		{"nohostnoslash", ""},
	} {
		if got := patternRoute(tc.pattern); got != tc.want {
			t.Errorf("patternRoute(%q) = %q, want %q", tc.pattern, got, tc.want)
		}
	}
}
