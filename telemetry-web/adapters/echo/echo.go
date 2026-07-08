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
	e.Use(Recovery(), RouteTag(), Metrics())
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
					if rec == http.ErrAbortHandler {
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
				nethttp.StampRoute(c.Request().Context(), route)
			}
			return next(c)
		}
	}
}

// Metrics records one per-endpoint data point per request, keyed by the matched
// route template (c.Path()), with outcome from the handler's returned error and
// response status. Unmatched routes are skipped to avoid 404-scan cardinality.
// A panicking handler unwinds past the record call, so panics surface on the
// panic counter, not here.
func Metrics() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			route := c.Path()
			err := next(c)
			if route == "" {
				return err
			}
			endpoint.Record(c.Request().Context(), route, failed(c, err))
			return err
		}
	}
}

// failed classifies the outcome: only server-side failures (5xx) count, so a
// returned 4xx *echo.HTTPError is a success, any other returned error maps to
// Echo's default 500, and a directly written 5xx status is caught too. Echo runs
// its error handler after the middleware chain returns, so the returned error —
// not the not-yet-written status — is the reliable signal.
func failed(c echo.Context, err error) bool {
	if err != nil {
		if he, ok := errors.AsType[*echo.HTTPError](err); ok {
			return he.Code >= 500
		}
		return true
	}
	return c.Response().Status >= 500
}
