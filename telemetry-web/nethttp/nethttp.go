// Package nethttp provides net/http integration for the telemetry library:
// an inbound Handler (spans + server metrics + panic recovery) and an outbound
// Transport that propagates trace context.
package nethttp

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"sync"

	"github.com/felixge/httpsnoop"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/stakater/operator-utils/telemetry-web/endpoint"
	"github.com/stakater/operator-utils/telemetry-web/logging"
)

// Option configures Handler.
type Option func(*config)

type config struct {
	skipPaths       []string
	noRecover       bool
	endpointMetrics bool
}

// Settings is the resolved option set. Adapters call Resolve to learn which
// middleware their Instrument should install.
type Settings struct {
	// SkipPaths are the exact paths excluded from instrumentation.
	SkipPaths []string
	// Recovery reports whether the built-in recovery middleware is installed.
	Recovery bool
	// EndpointMetrics reports whether WithEndpointMetrics was requested.
	EndpointMetrics bool
}

// Resolve applies opts and reports the result, so an adapter and the Handler
// it builds agree on one configuration.
func Resolve(opts ...Option) Settings {
	cfg := config{}
	for _, opt := range opts {
		opt(&cfg)
	}
	return Settings{
		SkipPaths:       cfg.skipPaths,
		Recovery:        !cfg.noRecover,
		EndpointMetrics: cfg.endpointMetrics,
	}
}

// WithEndpointMetrics opts in to the per-endpoint endpoint.requests counter
// in an adapter's Instrument. It is off by default because
// http.server.request.duration already records per-route request counts and
// outcomes — it carries http.route (see StampRoute) plus the status code and
// method, so it is a strict superset:
//
//	sum by (http_route) (rate(http_server_request_duration_count{http_response_status_code=~"5.."}[5m]))
//
// Turn it on when you want the simpler {endpoint,outcome} label pair.
func WithEndpointMetrics() Option {
	return func(c *config) { c.endpointMetrics = true }
}

// DefaultSkipPaths are the usual noise endpoints — k8s health probes and the
// Prometheus scrape path. Handler does NOT skip them on its own; opt in with
//
//	nethttp.Handler(mux, nethttp.WithSkipPaths(nethttp.DefaultSkipPaths...))
//
// or append your own: WithSkipPaths(append(nethttp.DefaultSkipPaths, "/ping")...).
var DefaultSkipPaths = []string{"/healthz", "/readyz", "/livez", "/metrics"}

// WithSkipPaths excludes the given exact request paths from instrumentation:
// no span, no http.server.* metrics, and no endpoint.requests from an
// adapter's Metrics middleware. The request is still served, and recovery
// still applies. Repeated calls accumulate. By default nothing is skipped.
//
// Because the filter runs before trace context is extracted, a skipped path
// also drops any inbound traceparent — it will not continue a caller's trace.
// That is harmless for probes; do not skip paths that participate in traces.
func WithSkipPaths(paths ...string) Option {
	return func(c *config) { c.skipPaths = append(c.skipPaths, paths...) }
}

// WithoutRecovery omits the built-in recovery middleware. Use it when an outer
// layer already recovers panics and calls endpoint.RecordPanic, so the panic is
// not counted twice.
func WithoutRecovery() Option {
	return func(c *config) { c.noRecover = true }
}

// skipKey marks a request that WithSkipPaths excluded, so middleware running
// inside the router (which otelhttp's own filter cannot reach) can bail out too.
type skipKey struct{}

// Skipped reports whether this request's path was excluded via WithSkipPaths.
// Adapter metrics middleware consults it; otelhttp's filter handles the span
// and http.server.* metrics on its own.
func Skipped(ctx context.Context) bool {
	v, _ := ctx.Value(skipKey{}).(bool)
	return v
}

// Handler is the composed inbound chain: otelhttp (spans + metrics) -> recovery
// -> next. Frameworks that don't expose http.Handler chaining should instead
// call endpoint.RecordPanic and endpoint.Record from their own middleware.
// Every path is instrumented unless excluded via WithSkipPaths.
func Handler(next http.Handler, opts ...Option) http.Handler {
	s := Resolve(opts...)

	inner := next
	if s.Recovery {
		inner = Recovery(next)
	}

	// otelhttp reads the globals in its own defaults, so passing them here
	// would only restate them.
	instrumented := otelhttp.NewHandler(inner, "server",
		otelhttp.WithFilter(func(r *http.Request) bool { return !Skipped(r.Context()) }),
	)

	if len(s.SkipPaths) == 0 {
		return instrumented
	}
	skip := make(map[string]bool, len(s.SkipPaths))
	for _, p := range s.SkipPaths {
		skip[p] = true
	}
	// Marking outside otelhttp is what lets both its filter and the adapters'
	// middleware see the same decision.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skip[r.URL.Path] {
			r = r.WithContext(context.WithValue(r.Context(), skipKey{}, true))
		}
		instrumented.ServeHTTP(w, r)
	})
}

// trackWrites returns w wrapped so responded reports whether anything has been
// committed to the client yet.
//
// httpsnoop generates a wrapper exposing exactly the optional interfaces w
// already implements — Flusher, Hijacker, ReadFrom, and so on. That matters:
// embedding the http.ResponseWriter interface directly would promote only
// Header/Write/WriteHeader and silently drop the rest, and gin's
// responseWriter.Flush and echo's Response.Flush type-assert for Flusher, so
// streaming handlers would panic and WebSocket upgrades would fail.
func trackWrites(w http.ResponseWriter) (wrapped http.ResponseWriter, responded func() bool) {
	var wrote bool
	wrapped = httpsnoop.Wrap(w, httpsnoop.Hooks{
		WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(code int) { wrote = true; next(code) }
		},
		Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(b []byte) (int, error) { wrote = true; return next(b) }
		},
		// A hijacked connection is no longer ours to write a status to.
		Hijack: func(next httpsnoop.HijackFunc) httpsnoop.HijackFunc {
			return func() (net.Conn, *bufio.ReadWriter, error) { wrote = true; return next() }
		},
	})
	return wrapped, func() bool { return wrote }
}

// Recovery recovers panics, records telemetry via endpoint.RecordPanic, and
// writes 500 if nothing has been written yet. http.ErrAbortHandler is
// re-raised. Must sit inside the otelhttp handler so the span exists in ctx and
// the response is measured.
//
// The response writer handed to next preserves whatever optional interfaces the
// original had, so SSE, flushing, and connection hijacking keep working.
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracked, responded := trackWrites(w)
		defer func() {
			if p := recover(); p != nil {
				// recover() yields the sentinel by identity, never wrapped —
				// net/http compares it the same way.
				if p == http.ErrAbortHandler { //nolint:errorlint
					panic(p)
				}
				endpoint.RecordPanic(r.Context(), p)
				// A handler that panicked mid-stream already sent its status;
				// overwriting it is impossible and only produces log noise.
				if !responded() {
					tracked.WriteHeader(http.StatusInternalServerError)
				}
			}
		}()
		next.ServeHTTP(tracked, r)
	})
}

// Transport wraps a RoundTripper so outbound requests inject trace context.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base)
}

// HTTPClient returns a client whose Transport already propagates trace context.
func HTTPClient() *http.Client { return &http.Client{Transport: Transport(nil)} }

// WrapClient adds propagation to an existing client in place.
func WrapClient(c *http.Client) *http.Client {
	c.Transport = Transport(c.Transport)
	return c
}

// RecordRoute emits one endpoint.requests data point for a matched route.
// It is the single definition of "failure" shared by every adapter: a
// server-side status (5xx). Client errors are the caller's fault, not the
// service's, so a 4xx is a success.
//
// Unmatched routes (empty template) are skipped to keep 404 scans from
// exploding cardinality, and paths excluded by WithSkipPaths are skipped so the
// exclusion covers this counter too, not just otelhttp's own metrics.
//
// Adapters call this from their Metrics middleware; call it directly when
// wiring a framework by hand.
func RecordRoute(ctx context.Context, route string, status int) {
	if route == "" || Skipped(ctx) {
		return
	}
	endpoint.Record(ctx, route, status >= 500)
}

// warnNoLabeler fires at most once per process: the condition is a wiring
// mistake, not a per-request event.
var warnNoLabeler sync.Once

// StampRoute puts http.route on the active server span and on the otelhttp
// metric attributes (via the request Labeler) for this request, and renames
// the span to the semconv form "{method} {route}" (method may be empty to
// leave the span name alone). The framework adapters call it from their
// RouteTag middleware once the matched route template is known; call it
// yourself when integrating a framework by hand.
//
// The request must be inside nethttp.Handler. Without it there is no otelhttp
// labeler in ctx, so the span attribute still lands but the metric attribute
// is silently dropped; StampRoute logs a warning once when it detects this.
func StampRoute(ctx context.Context, method, route string) {
	attr := semconv.HTTPRoute(route)
	labeler, ok := otelhttp.LabelerFromContext(ctx)
	if !ok {
		warnNoLabeler.Do(func() {
			logging.Logger().Warn("nethttp.StampRoute: no otelhttp labeler in context; " +
				"http.route will not reach http.server.* metrics. Serve through nethttp.Handler.")
		})
	}
	labeler.Add(attr)
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attr)
	if method != "" {
		span.SetName(method + " " + route)
	}
}
