// Package chitel wires the telemetry library into a chi router: route-tagged
// spans, optional per-endpoint metrics, and the core net/http Handler (spans +
// server metrics + panic recovery). The package name differs from the directory
// so it never collides with go-chi's "chi" — no alias needed:
//
//	import "github.com/stakater/operator-utils/telemetry-web/adapters/chi" // package chitel
package chitel

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/stakater/operator-utils/telemetry-web/nethttp"
)

// Middleware is chi's middleware shape — standard net/http chaining.
type Middleware = func(http.Handler) http.Handler

// Instrument installs RouteTag on r — plus Metrics when
// nethttp.WithEndpointMetrics() is passed — and returns it wrapped in
// nethttp.Handler, ready to serve. Extra nethttp options (e.g.
// nethttp.WithSkipPaths(nethttp.DefaultSkipPaths...)) are forwarded to the
// Handler. Call BEFORE registering routes — chi panics if Use is called after
// the first route. The returned handler wraps the router by reference.
//
// Recovery is not installed here: chi middleware is plain net/http chaining, so
// nethttp.Handler's own recovery already covers the router. Adding chitel
// .Recovery() as well would count each panic twice.
func Instrument(r chi.Router, opts ...nethttp.Option) http.Handler {
	s := nethttp.Resolve(opts...)
	r.Use(RouteTag())
	if s.EndpointMetrics {
		r.Use(Metrics())
	}
	return nethttp.Handler(r, opts...)
}

// Recovery forwards panics to endpoint.RecordPanic and responds 500, with
// http.ErrAbortHandler re-raised. chi middleware is plain net/http chaining,
// so this is the core nethttp.Recovery.
//
// Instrument does not install it — nethttp.Handler already recovers. Use it
// only when building a chain by hand without nethttp.Handler, or alongside
// nethttp.WithoutRecovery().
func Recovery() Middleware {
	return nethttp.Recovery
}

// RouteTag stamps http.route (the matched route pattern) on the server span and
// the otelhttp duration metric, and renames the span to "{method} {route}".
// chi fills in RoutePattern during routing, before the handler runs, so the
// stamp is deferred: a panicking handler still gets its route recorded as the
// stack unwinds. Unmatched requests are skipped.
func RouteTag() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if route := routePattern(r); route != "" {
					nethttp.StampRoute(r.Context(), r.Method, route)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Metrics records one endpoint.requests data point per request, keyed by
// the matched route pattern. Outcome follows nethttp.RecordRoute's shared rule
// (status >= 500 is a failure), so it matches the gin and echo adapters.
// Mounted subrouters record chi's native joined pattern (e.g.
// /api/*/users/{id}) as-is. A panicking handler unwinds past the record call,
// so panics surface on the panic counter, not here.
//
// Opt in via Instrument(r, nethttp.WithEndpointMetrics()).
func Metrics() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			nethttp.RecordRoute(r.Context(), routePattern(r), ww.Status())
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
