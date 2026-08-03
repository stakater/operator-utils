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
