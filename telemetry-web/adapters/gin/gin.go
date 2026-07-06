// Package gin wires the telemetry library into a Gin engine: recovery,
// automatic per-endpoint metrics keyed by the matched route template, and the
// core net/http Handler (spans + server metrics). Import it under an alias since
// gin-gonic is usually imported as "gin":
//
//	import teleg "github.com/stakater/operator-utils/telemetry-web/adapters/gin"
package gin

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/stakater/operator-utils/telemetry-web/endpoint"
	"github.com/stakater/operator-utils/telemetry-web/nethttp"
)

// Instrument installs Recovery and Metrics on engine and returns it wrapped in
// nethttp.Handler, ready to serve. Call right after gin.New(), BEFORE registering
// routes — Gin applies global middleware only to routes registered afterward.
// The returned handler wraps the engine by reference, so later routes are served.
func Instrument(engine *gin.Engine) http.Handler {
	engine.Use(Recovery(), Metrics())
	return nethttp.Handler(engine)
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
