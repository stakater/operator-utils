// Package nethttp provides net/http integration for the telemetry library:
// an inbound Handler (spans + server metrics + panic recovery) and an outbound
// Transport that propagates trace context.
package nethttp

import (
	"context"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/stakater/operator-utils/telemetry-web/endpoint"
)

// Option configures Handler.
type Option func(*config)

type config struct {
	skipPaths []string
}

// WithSkipPaths replaces the default skip list (health probes and /metrics)
// with the given exact request paths. Skipped paths get no span and no
// http.server.* metrics but are still served (and recovery still applies).
// Call with no arguments to instrument everything.
func WithSkipPaths(paths ...string) Option {
	return func(c *config) { c.skipPaths = paths }
}

// Handler is the composed inbound chain: otelhttp (spans + metrics) -> recovery
// -> next. Frameworks that don't expose http.Handler chaining should instead
// call endpoint.RecordPanic and endpoint.Record from their own middleware.
// By default health probes and /metrics are not instrumented — see WithSkipPaths.
func Handler(next http.Handler, opts ...Option) http.Handler {
	cfg := config{}
	for _, opt := range opts {
		opt(&cfg)
	}
	skip := make(map[string]bool, len(cfg.skipPaths))
	for _, p := range cfg.skipPaths {
		skip[p] = true
	}

	inner := Recovery(next)
	return otelhttp.NewHandler(inner, "server",
		otelhttp.WithPropagators(otel.GetTextMapPropagator()),
		otelhttp.WithMeterProvider(otel.GetMeterProvider()),
		otelhttp.WithTracerProvider(otel.GetTracerProvider()),
		otelhttp.WithFilter(func(r *http.Request) bool { return !skip[r.URL.Path] }),
	)
}

// Recovery recovers panics, records telemetry via endpoint.RecordPanic, and
// writes 500. http.ErrAbortHandler is re-raised. Must sit inside the otelhttp
// handler so the span exists in ctx and the 500 is measured.
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				endpoint.RecordPanic(r.Context(), rec)
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Transport wraps a RoundTripper so outbound requests inject trace context.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base,
		otelhttp.WithPropagators(otel.GetTextMapPropagator()))
}

// HTTPClient returns a client whose Transport already propagates trace context.
func HTTPClient() *http.Client { return &http.Client{Transport: Transport(nil)} }

// WrapClient adds propagation to an existing client in place.
func WrapClient(c *http.Client) *http.Client {
	c.Transport = Transport(c.Transport)
	return c
}

// stampRoute puts http.route on the active server span and on the otelhttp
// metric attributes for this request. Stamped before the handler runs so even
// panicked requests carry the route on their span.
func StampRoute(ctx context.Context, route string) {
	attr := semconv.HTTPRoute(route)
	labeler, _ := otelhttp.LabelerFromContext(ctx)
	labeler.Add(attr)
	trace.SpanFromContext(ctx).SetAttributes(attr)
}
