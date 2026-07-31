// Package gintel wires the telemetry library into a Gin engine: recovery,
// automatic per-endpoint metrics keyed by the matched route template, and the
// core net/http Handler (spans + server metrics). The package name differs from
// the directory so it never collides with gin-gonic's "gin" — no alias needed:
//
//	import "github.com/stakater/operator-utils/telemetry-web/adapters/gin" // package gintel
package gintel

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/stakater/operator-utils/telemetry-web/endpoint"
	"github.com/stakater/operator-utils/telemetry-web/nethttp"
)

// Instrument installs Recovery and RouteTag on engine — plus Metrics when
// nethttp.WithEndpointMetrics() is passed — and returns it wrapped in
// nethttp.Handler, ready to serve. Extra nethttp options (e.g.
// nethttp.WithSkipPaths(nethttp.DefaultSkipPaths...)) are forwarded to the
// Handler. The returned handler wraps the engine by reference, so later routes
// are served.
//
// Call right after gin.New(), BEFORE registering routes: Gin applies global
// middleware only to routes registered afterward, so a late call silently
// instruments nothing. Instrument panics rather than let that happen quietly.
func Instrument(engine *gin.Engine, opts ...nethttp.Option) http.Handler {
	if len(engine.Routes()) > 0 {
		panic("gintel.Instrument must be called before registering routes: " +
			"Gin applies global middleware only to routes registered after Use")
	}
	s := nethttp.Resolve(opts...)
	if s.Recovery {
		engine.Use(Recovery())
	}
	engine.Use(RouteTag())
	if s.EndpointMetrics {
		engine.Use(Metrics())
	}
	return nethttp.Handler(engine, opts...)
}

// Recovery forwards panics to endpoint.RecordPanic and responds 500.
// http.ErrAbortHandler is re-raised.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				// Sentinel arrives by identity, never wrapped.
				if rec == http.ErrAbortHandler { //nolint:errorlint
					panic(rec)
				}
				endpoint.RecordPanic(c.Request.Context(), rec)
				c.AbortWithStatus(http.StatusInternalServerError)
			}
		}()
		c.Next()
	}
}

// RouteTag stamps http.route (the matched route template) on the server span
// and the otelhttp duration metric, and renames the span to "{method} {route}".
// It runs before the handler, so even panicked requests carry the route on
// their span. Unmatched requests are skipped.
func RouteTag() gin.HandlerFunc {
	return func(c *gin.Context) {
		if route := c.FullPath(); route != "" {
			nethttp.StampRoute(c.Request.Context(), c.Request.Method, route)
		}
		c.Next()
	}
}

// Metrics records one endpoint.requests data point per request, keyed by
// the matched route template (c.FullPath()). Outcome follows
// nethttp.RecordRoute's shared rule (status >= 500 is a failure), so it matches
// the echo and chi adapters. A panicking handler unwinds past the record call,
// so panics surface on the panic counter, not here.
//
// Note that c.Errors is deliberately NOT consulted: a handler that calls
// c.Error() while still returning 4xx has recorded a client error, and counting
// it as a server-side failure would make this metric mean something different
// here than in the other adapters.
//
// Opt in via Instrument(engine, nethttp.WithEndpointMetrics()).
func Metrics() gin.HandlerFunc {
	return func(c *gin.Context) {
		route := c.FullPath()
		c.Next()
		nethttp.RecordRoute(c.Request.Context(), route, c.Writer.Status())
	}
}
