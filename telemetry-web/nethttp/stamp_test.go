package nethttp

import (
	"context"
	"testing"

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
