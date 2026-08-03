// Package echotel wires the telemetry library into an Echo engine: recovery,
// route-tagged spans, optional per-endpoint metrics, and the core net/http
// Handler. The package name differs from the directory so it never collides
// with labstack's "echo" — no alias needed:
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

// Recovery forwards panics to endpoint.Recovered, tagging the panic counter with
// the matched route, and responds 500. Use it instead of Echo's
// middleware.Recover, which would swallow the panic before telemetry sees it.
// http.ErrAbortHandler is re-raised.
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

// status resolves the code the request actually answers with, mirroring Echo's
// DefaultHTTPErrorHandler rather than approximating it. Each step matters:
//
//   - Committed first. Once a handler has written, Echo's error handler returns
//     without touching the response, so the status already sent is the answer even
//     though a non-nil error came back.
//   - A plain type assertion, not errors.As. Echo does not unwrap, so
//     fmt.Errorf("...: %w", echo.NewHTTPError(404)) answers 500; errors.As would
//     find the 404 and record a server fault as a client one.
//   - he.Internal unwrapped one level, because Echo does that and answers with the
//     inner code. The idiomatic NewHTTPError(500).SetInternal(NewHTTPError(404))
//     answers 404.
//
// This tracks Echo's DEFAULT handler. A consumer that sets its own
// HTTPErrorHandler can answer with anything, and the outcome recorded here will
// follow the default's rules instead.
func status(c echo.Context, err error) int {
	if err == nil || c.Response().Committed {
		return c.Response().Status
	}
	he, ok := err.(*echo.HTTPError) //nolint:errorlint // mirrors Echo, which does not unwrap
	if !ok {
		return http.StatusInternalServerError
	}
	if inner, ok := he.Internal.(*echo.HTTPError); ok { //nolint:errorlint // one level, as Echo does
		return inner.Code
	}
	return he.Code
}
