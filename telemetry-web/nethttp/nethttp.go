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

// DefaultSkipPaths are the usual noise endpoints — k8s health probes and the
// Prometheus scrape path. Handler does NOT skip them on its own; opt in with
//
//	nethttp.Handler(mux, nethttp.WithSkipPaths(nethttp.DefaultSkipPaths...))
//
// or append your own: WithSkipPaths(append(nethttp.DefaultSkipPaths, "/ping")...).
var DefaultSkipPaths = []string{"/healthz", "/readyz", "/livez", "/metrics"}

// WithSkipPaths excludes the given exact request paths from instrumentation:
// no span and no http.server.* metrics, but the request is still served (and
// recovery still applies). By default nothing is skipped.
func WithSkipPaths(paths ...string) Option {
	return func(c *config) { c.skipPaths = paths }
}

// Handler is the composed inbound chain: otelhttp (spans + metrics) -> recovery
// -> next. Frameworks that don't expose http.Handler chaining should instead
// call endpoint.RecordPanic and endpoint.Record from their own middleware.
// Every path is instrumented unless excluded via WithSkipPaths.
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

// StampRoute puts http.route on the active server span and on the otelhttp
// metric attributes (via the request Labeler) for this request, and renames
// the span to the semconv form "{method} {route}" (method may be empty to
// leave the span name alone). The framework adapters call it from their
// RouteTag middleware once the matched route template is known; call it
// yourself when integrating a framework by hand.
func StampRoute(ctx context.Context, method, route string) {
	attr := semconv.HTTPRoute(route)
	labeler, _ := otelhttp.LabelerFromContext(ctx)
	labeler.Add(attr)
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attr)
	if method != "" {
		span.SetName(method + " " + route)
	}
}
