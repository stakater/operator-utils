// Package echotel wires the telemetry library into an Echo engine: recovery,
// automatic per-endpoint metrics keyed by the matched route template, and the
// core net/http Handler (spans + server metrics). The package name differs from
// the directory so it never collides with labstack's "echo" — no alias needed:
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

// Instrument installs Recovery, RouteTag, and Metrics on e and returns it
// wrapped in nethttp.Handler, ready to serve. Extra nethttp options (e.g.
// nethttp.WithSkipPaths(nethttp.DefaultSkipPaths...)) are forwarded to the
// Handler. Unlike Gin, Echo applies Use middleware to routes registered before
// or after the call, so ordering doesn't matter. The returned handler wraps
// the engine by reference.
func Instrument(e *echo.Echo, opts ...nethttp.Option) http.Handler {
	s := nethttp.Resolve(opts...)
	if s.Recovery {
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

// RouteTag stamps http.route (the matched route template) on the server span
// and the otelhttp duration metric via nethttp.StampRoute. It runs before the
// handler, so even panicked requests carry the route on their span. Unmatched
// requests are skipped. Use it without Metrics for a semconv-only setup.
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

// Metrics records one endpoint.requests data point per request, keyed by
// the matched route template (c.Path()). Outcome follows nethttp.RecordRoute's
// shared rule (status >= 500 is a failure), so it matches the gin and chi
// adapters. A panicking handler unwinds past the record call, so panics surface
// on the panic counter, not here.
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

// status resolves the status code this request will actually answer with. Echo
// runs its error handler AFTER the middleware chain returns, so for a returned
// error the response status is not written yet and the error carries the
// answer: an *echo.HTTPError reports its own Code, anything else becomes Echo's
// default 500.
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
