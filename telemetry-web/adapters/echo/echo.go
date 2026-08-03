// Package echotel wires the telemetry library into an Echo engine: recovery,
// route-tagged spans, optional per-endpoint metrics, and the core net/http
// Handler. The package name differs from the directory so it never collides
// with labstack's "echo" — no alias needed:
//
//	import "github.com/stakater/operator-utils/telemetry-web/adapters/echo" // package echotel
package echotel

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/stakater/operator-utils/telemetry-web/endpoint"
	"github.com/stakater/operator-utils/telemetry-web/nethttp"
)

// Instrument installs Recovery and RouteTag on e — plus Metrics under
// nethttp.WithEndpointMetrics() — and returns it wrapped in nethttp.Handler,
// ready to serve. Other nethttp options are forwarded. Unlike Gin, Echo applies
// Use middleware to routes registered before or after the call, so ordering
// doesn't matter. The handler wraps the engine by reference.
func Instrument(e *echo.Echo, opts ...nethttp.Option) http.Handler {
	s := nethttp.Resolve(opts...)
	if s.Recovery {
		// Two layers on purpose. Echo's consumes handler panics first, so the
		// count stays at one; nethttp.Handler's is the only one outside the
		// engine, and so the only thing covering e.Pre middleware.
		e.Use(Recovery())
	}
	e.Use(RouteTag())
	if s.EndpointMetrics {
		e.Use(Metrics())
	}
	return nethttp.Handler(e, opts...)
}

// Recovery forwards panics to endpoint.RecordPanic and responds 500. Use it
// instead of Echo's middleware.Recover, which would swallow the panic before
// telemetry sees it. http.ErrAbortHandler is re-raised.
func Recovery() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			defer func() {
				if rec := recover(); rec != nil {
					// Sentinel arrives by identity, never wrapped.
					if rec == http.ErrAbortHandler { //nolint:errorlint
						panic(rec)
					}
					endpoint.RecordPanic(c.Request().Context(), rec)
					_ = c.NoContent(http.StatusInternalServerError)
				}
			}()
			return next(c)
		}
	}
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

// Metrics records one endpoint.requests data point per request, keyed by the
// matched route template. Outcome follows nethttp.RecordRoute's shared rule, so
// it matches the gin and chi adapters. A panicking handler unwinds past the
// record call, so panics land on the panic counter instead.
//
// Opt in via Instrument(e, nethttp.WithEndpointMetrics()).
func Metrics() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			route := c.Path()
			err := next(c)
			nethttp.RecordRoute(c.Request().Context(), route, status(c, err))
			return err
		}
	}
}

// status resolves the code the request will answer with. Echo runs its error
// handler after the middleware chain, so on a returned error nothing is written
// yet and the error carries the answer: an *echo.HTTPError reports its own
// Code, anything else becomes Echo's default 500.
func status(c echo.Context, err error) int {
	if err != nil {
		var he *echo.HTTPError
		if errors.As(err, &he) {
			return he.Code
		}
		return http.StatusInternalServerError
	}
	return c.Response().Status
}
