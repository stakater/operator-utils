// Package gintel wires the telemetry library into a Gin engine: recovery,
// route-tagged spans, optional per-endpoint metrics, and the core net/http
// Handler. The package name differs from the directory so it never collides
// with gin-gonic's "gin" — no alias needed:
//
//	import "github.com/stakater/operator-utils/telemetry-web/adapters/gin" // package gintel
package gintel

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"

	"github.com/stakater/operator-utils/telemetry-web/endpoint"
	"github.com/stakater/operator-utils/telemetry-web/nethttp"
)

// Instrument installs Recovery and RouteTag on engine — plus Metrics under
// nethttp.WithEndpointMetrics() — and returns it wrapped in nethttp.Handler,
// ready to serve. Other nethttp options are forwarded. The handler wraps the
// engine by reference, so routes registered later are still served.
//
// Call it right after gin.New(), before registering routes: Gin applies global
// middleware only to routes registered afterward, so a late call would
// instrument nothing. Instrument panics rather than fail silently.
func Instrument(engine *gin.Engine, opts ...nethttp.Option) http.Handler {
	if len(engine.Routes()) > 0 {
		panic("gintel.Instrument must be called before registering routes: " +
			"Gin applies global middleware only to routes registered after Use")
	}
	s := nethttp.Resolve(opts...)
	if s.Recovery {
		// Two layers on purpose. Gin's consumes handler panics first, so the
		// count stays at one; nethttp.Handler's is the only one outside the
		// engine, and so the only thing covering middleware registered before
		// this call.
		engine.Use(Recovery())
	}
	engine.Use(RouteTag())
	if s.EndpointMetrics {
		engine.Use(Metrics())
	}
	return nethttp.Handler(engine, opts...)
}

// Recovery forwards panics to endpoint.Recovered, tagging the panic counter with
// the matched route, and responds 500. http.ErrAbortHandler is re-raised.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				if !endpoint.Recovered(c.Request.Context(), rec, routeAttrs(c)...) {
					panic(rec)
				}
				c.AbortWithStatus(http.StatusInternalServerError)
			}
		}()
		c.Next()
	}
}

// routeAttrs tags the panic counter with the matched template, so a spike points
// at an endpoint. Empty for an unmatched request, which keeps 404 scans from
// creating a time series each.
func routeAttrs(c *gin.Context) []attribute.KeyValue {
	if route := c.FullPath(); route != "" {
		return []attribute.KeyValue{semconv.HTTPRoute(route)}
	}
	return nil
}

// RouteTag stamps http.route (c.FullPath()) on the server span and the duration
// metric, and renames the span to "{method} {route}". It runs before the
// handler, so panicked requests keep their route. Unmatched requests are
// skipped.
func RouteTag() gin.HandlerFunc {
	return func(c *gin.Context) {
		if route := c.FullPath(); route != "" {
			nethttp.StampRoute(c.Request.Context(), c.Request.Method, route)
		}
		c.Next()
	}
}

// Metrics records one endpoint.requests data point per request, keyed by the
// matched route template. Outcome follows nethttp.RecordRoute's shared rule, so
// it matches the echo and chi adapters. A panicking handler unwinds past the
// record call, so panics land on the panic counter instead.
//
// c.Errors is deliberately not consulted: treating it as a failure would make a
// gin 4xx mean something different from an echo or chi 4xx.
//
// Opt in via Instrument(engine, nethttp.WithEndpointMetrics()).
func Metrics() gin.HandlerFunc {
	return func(c *gin.Context) {
		route := c.FullPath()
		c.Next()
		nethttp.RecordRoute(c.Request.Context(), route, c.Writer.Status())
	}
}
