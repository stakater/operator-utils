// Package chitel wires the telemetry library into a chi router: route-tagged
// spans, optional per-endpoint metrics, and the core net/http Handler. The
// package name differs from the directory so it never collides with go-chi's
// "chi" — no alias needed:
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

// Instrument installs RouteTag on r — plus Metrics under
// nethttp.WithEndpointMetrics() — and returns it wrapped in nethttp.Handler,
// ready to serve. Other nethttp options are forwarded. Call it before
// registering routes; chi panics if Use runs after the first route.
//
// No Recovery is installed here: chitel.Recovery IS nethttp.Recovery, which
// Handler already applies, so adding it would count every panic twice.
//
// Do NOT add chi's middleware.Recoverer either, idiomatic as it is. Registered on
// the mux it is always inner to nethttp.Recovery, which sits outside, so it
// consumes the panic first and the recorded telemetry inverts relative to gin and
// echo: http.server.panics stays at 0, the span carries no error, and because
// Metrics returns normally the request lands on
// endpoint.requests{outcome=failure} instead. Leave the recovering to
// nethttp.Handler, or own it with nethttp.WithoutRecovery().
func Instrument(r chi.Router, opts ...nethttp.Option) http.Handler {
	s := nethttp.Resolve(opts...)
	r.Use(RouteTag())
	if s.EndpointMetrics {
		r.Use(Metrics())
	}
	return nethttp.Handler(r, opts...)
}

// Recovery forwards panics to endpoint.Recovered and responds 500, with
// http.ErrAbortHandler re-raised. chi middleware is plain net/http chaining, so
// this is literally nethttp.Recovery. Instrument does not install it; use it
// only when building a chain without nethttp.Handler, or alongside
// nethttp.WithoutRecovery().
func Recovery() Middleware {
	return nethttp.Recovery
}

// RouteTag stamps http.route (the matched route pattern) on the server span and
// the duration metric, and renames the span to "{method} {route}". chi fills in
// RoutePattern during routing, before the handler runs, so the stamp is
// deferred and survives a panic unwinding past it. Unmatched requests are
// skipped.
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

// Metrics records one endpoint.requests data point per request, keyed by the
// matched route pattern. Outcome follows nethttp.RecordRoute's shared rule, so
// it matches the gin and echo adapters. Mounted subrouters record chi's joined
// pattern (e.g. /api/users/{id}) as-is. A panicking handler unwinds past the
// record call, so panics land on the panic counter instead.
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
