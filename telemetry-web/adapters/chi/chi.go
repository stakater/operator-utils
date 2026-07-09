// Package chitel wires the telemetry library into a chi router: recovery,
// automatic per-endpoint metrics keyed by the matched route pattern, and the
// core net/http Handler (spans + server metrics). The package name differs from
// the directory so it never collides with go-chi's "chi" — no alias needed:
//
//	import "github.com/stakater/operator-utils/telemetry-web/adapters/chi" // package chitel
package chitel

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/stakater/operator-utils/telemetry-web/endpoint"
	"github.com/stakater/operator-utils/telemetry-web/nethttp"
)

// Middleware is chi's middleware shape — standard net/http chaining.
type Middleware = func(http.Handler) http.Handler

// Instrument installs Recovery, RouteTag, and Metrics on r and returns it
// wrapped in nethttp.Handler, ready to serve. Extra nethttp options (e.g.
// nethttp.WithSkipPaths(nethttp.DefaultSkipPaths...)) are forwarded to the
// Handler. Call BEFORE registering routes — chi panics if Use is called after
// the first route. The returned handler wraps the router by reference.
func Instrument(r chi.Router, opts ...nethttp.Option) http.Handler {
	r.Use(Recovery(), RouteTag(), Metrics())
	return nethttp.Handler(r, opts...)
}

// Recovery forwards panics to endpoint.RecordPanic and responds 500, with
// http.ErrAbortHandler re-raised. chi middleware is plain net/http chaining,
// so this is the core nethttp.Recovery.
func Recovery() Middleware {
	return nethttp.Recovery
}

// RouteTag stamps http.route (the matched route pattern) on the server span
// and the otelhttp duration metric via nethttp.StampRoute. chi assembles
// RoutePattern during routing, so — unlike Gin/Echo — the stamp happens AFTER
// the handler returns; panicked requests therefore carry no http.route on
// their span. Unmatched requests are skipped. Use it without Metrics for a
// semconv-only setup.
func RouteTag() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			if route := routePattern(r); route != "" {
				nethttp.StampRoute(r.Context(), route)
			}
		})
	}
}

// Metrics records one per-endpoint data point per request, keyed by the
// matched route pattern, with outcome from the response status (chi handlers
// have no error channel, so failure means status >= 500). Mounted subrouters
// record chi's native joined pattern (e.g. /api/*/users/{id}) as-is. Unmatched
// routes are skipped to avoid 404-scan cardinality. A panicking handler
// unwinds past the record call, so panics surface on the panic counter, not
// here.
func Metrics() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			if route := routePattern(r); route != "" {
				endpoint.Record(r.Context(), route, ww.Status() >= 500)
			}
		})
	}
}

// routePattern returns the matched route pattern, or "" when routing found no
// match or the middleware runs outside a chi router.
func routePattern(r *http.Request) string {
	rctx := chi.RouteContext(r.Context())
	if rctx == nil {
		return ""
	}
	return rctx.RoutePattern()
}
