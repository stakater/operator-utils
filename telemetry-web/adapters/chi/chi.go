// Package chitel wires the telemetry library into a chi router: route-tagged
// spans and the core net/http Handler. The package name differs from the
// directory so it never collides with go-chi's "chi" — no alias needed:
//
//	import "github.com/stakater/operator-utils/telemetry-web/adapters/chi" // package chitel
package chitel

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/stakater/operator-utils/telemetry-web/nethttp"
)

// Middleware is chi's middleware shape — standard net/http chaining.
type Middleware = func(http.Handler) http.Handler

// Instrument installs RouteTag on r and returns it wrapped in nethttp.Handler,
// ready to serve. nethttp options are forwarded. Call it before registering
// routes; chi panics if Use runs after the first route.
//
// No Recovery is installed here: chitel.Recovery IS nethttp.Recovery, which
// Handler already applies, so adding it would count every panic twice.
//
// Only the innermost recovery records a panic, since recover() consumes it. Ours
// sits outside the mux, so chi's middleware.Recoverer is always inner to it,
// idiomatic as it is, and takes over: http.server.panics stays at 0 and the span
// carries no error. Unlike gin and echo there is no placement that avoids this.
// Leave the recovering to nethttp.Handler, or own it with
// nethttp.WithoutRecovery() and call endpoint.Recovered yourself.
func Instrument(r chi.Router, opts ...nethttp.Option) http.Handler {
	r.Use(RouteTag())
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
// deferred and survives a panic unwinding past it.
//
// Middleware that answers before chi routes (authentication returning 401,
// say) leaves RoutePattern empty. RouteTag then stamps the pattern chi would
// have matched, so the rejection is attributed to its route. A request that
// matches no route gets none. The lookup needs the router in the routing
// context (chi sets it; a context made with chi.NewRouteContext needs Routes
// set by hand) and is skipped when RouteTag sits inside a mounted router,
// which keeps the mount's pattern.
func RouteTag() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rctx := chi.RouteContext(r.Context())
			// Captured now: routing into a mounted router sets RoutePath.
			topLevel := rctx != nil && rctx.RoutePath == ""
			defer func() {
				if route := matchedRoute(r, topLevel); route != "" {
					nethttp.StampRoute(r.Context(), r.Method, route)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// matchedRoute returns the pattern chi routed on or, when nothing was routed
// and RouteTag runs on the top-level router, the pattern it would have used.
func matchedRoute(r *http.Request, topLevel bool) string {
	if route := routePattern(r); route != "" || !topLevel {
		return route
	}
	return wouldMatch(r)
}

// wouldMatch asks the router in the routing context which pattern r would
// match, with the path chi's own routing uses. Find follows mounted routers
// and returns the joined pattern, or "" when nothing matches.
func wouldMatch(r *http.Request) string {
	rctx := chi.RouteContext(r.Context())
	if rctx == nil || rctx.Routes == nil {
		return ""
	}
	path := r.URL.RawPath
	if path == "" {
		path = r.URL.Path
	}
	found := rctx.Routes.Find(chi.NewRouteContext(), r.Method, path)
	// Find returns the raw pattern; RoutePattern applies the trailing-slash
	// trim a served request gets, so both name the same http.route.
	norm := chi.NewRouteContext()
	norm.RoutePatterns = []string{found}
	return norm.RoutePattern()
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
