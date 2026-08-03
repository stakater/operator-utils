// Package echotel wires the telemetry library into an Echo engine: recovery,
// route-tagged spans, and the core net/http Handler. The package name differs
// from the directory so it never collides with labstack's "echo" — no alias
// needed:
//
//	import "github.com/stakater/operator-utils/telemetry-web/adapters/echo" // package echotel
package echotel

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"

	"github.com/stakater/operator-utils/telemetry-web/endpoint"
	"github.com/stakater/operator-utils/telemetry-web/nethttp"
)

// Instrument installs Recovery and RouteTag on e and returns it wrapped in
// nethttp.Handler, ready to serve. nethttp options are forwarded. Unlike Gin,
// Echo applies Use middleware to routes registered before or after the call, so
// ordering doesn't matter. The handler wraps the engine by reference.
func Instrument(e *echo.Echo, opts ...nethttp.Option) http.Handler {
	s := nethttp.Resolve(opts...)
	if s.Recovery {
		// Two layers on purpose. Echo's consumes handler panics first, so the
		// count stays at one; nethttp.Handler's is the only one outside the
		// engine, and so the only thing covering e.Pre middleware.
		e.Use(Recovery())
	}
	e.Use(RouteTag())
	return nethttp.Handler(e, opts...)
}

// Recovery forwards panics to endpoint.Recovered, tagging the panic counter with
// the matched route, and responds 500. http.ErrAbortHandler is re-raised.
//
// It replaces Echo's middleware.Recover rather than sitting under it: recover()
// consumes the panic, so only the innermost recovery records anything. Echo runs
// Use middleware outermost-first, so a middleware.Recover registered after
// Instrument is inner to this one and silently takes over the count.
func Recovery() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			defer func() {
				if rec := recover(); rec != nil {
					if !endpoint.Recovered(c.Request().Context(), rec, routeAttrs(c)...) {
						panic(rec)
					}
					_ = c.NoContent(http.StatusInternalServerError)
				}
			}()
			return next(c)
		}
	}
}

// routeAttrs tags the panic counter with the matched template, so a spike points
// at an endpoint. Empty for an unmatched request, which keeps 404 scans from
// creating a time series each.
func routeAttrs(c echo.Context) []attribute.KeyValue {
	if route := c.Path(); route != "" {
		return []attribute.KeyValue{semconv.HTTPRoute(route)}
	}
	return nil
}

// RouteTag stamps http.route (c.Path()) on the server span and the duration
// metric, and renames the span to "{method} {route}". It runs before the
// handler, so panicked requests keep their route. Unmatched requests are
// skipped.
func RouteTag() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if route := c.Path(); route != "" {
				nethttp.StampRoute(c.Request().Context(), c.Request().Method, route)
			}
			return next(c)
		}
	}
}
