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

// Instrument installs Recovery, RouteTag, and Metrics on engine and returns it
// wrapped in nethttp.Handler, ready to serve. Extra nethttp options (e.g.
// nethttp.WithSkipPaths(nethttp.DefaultSkipPaths...)) are forwarded to the
// Handler. Call right after gin.New(), BEFORE registering routes — Gin applies
// global middleware only to routes registered afterward. The returned handler
// wraps the engine by reference, so later routes are served.
func Instrument(engine *gin.Engine, opts ...nethttp.Option) http.Handler {
	engine.Use(Recovery(), RouteTag(), Metrics())
	return nethttp.Handler(engine, opts...)
}

// Recovery forwards panics to endpoint.RecordPanic and responds 500.
// http.ErrAbortHandler is re-raised.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
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
// and the otelhttp duration metric via nethttp.StampRoute. It runs before the
// handler, so even panicked requests carry the route on their span. Unmatched
// requests are skipped. Use it without Metrics for a semconv-only setup.
func RouteTag() gin.HandlerFunc {
	return func(c *gin.Context) {
		if route := c.FullPath(); route != "" {
			nethttp.StampRoute(c.Request.Context(), route)
		}
		c.Next()
	}
}

// Metrics records one per-endpoint data point per request, keyed by the matched
// route template (c.FullPath()), with outcome from the response status. Unmatched
// routes are skipped to avoid 404-scan cardinality. A panicking handler unwinds
// past the record call, so panics surface on the panic counter, not here.
func Metrics() gin.HandlerFunc {
	return func(c *gin.Context) {
		route := c.FullPath()
		c.Next()
		if route == "" {
			return
		}
		failed := c.Writer.Status() >= 500 || len(c.Errors) > 0
		endpoint.Record(c.Request.Context(), route, failed)
	}
}
