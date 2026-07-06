// Package nethttp provides net/http integration for the telemetry library:
// an inbound Handler (spans + server metrics + panic recovery) and an outbound
// Transport that propagates trace context.
package nethttp

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"

	"github.com/stakater/operator-utils/telemetry-web/endpoint"
)

// Handler is the composed inbound chain: otelhttp (spans + metrics) -> recovery
// -> next. Frameworks that don't expose http.Handler chaining should instead
// call endpoint.RecordPanic and endpoint.Record from their own middleware.
func Handler(next http.Handler) http.Handler {
	inner := Recovery(next)
	return otelhttp.NewHandler(inner, "server",
		otelhttp.WithPropagators(otel.GetTextMapPropagator()),
		otelhttp.WithMeterProvider(otel.GetMeterProvider()),
		otelhttp.WithTracerProvider(otel.GetTracerProvider()),
	)
}

// Recovery recovers panics, records telemetry via endpoint.RecordPanic, and
// writes 500. http.ErrAbortHandler is re-raised. Must sit inside the otelhttp
// handler so the span exists in ctx and the 500 is measured.
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				endpoint.RecordPanic(r.Context(), rec)
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Transport wraps a RoundTripper so outbound requests inject trace context.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base,
		otelhttp.WithPropagators(otel.GetTextMapPropagator()))
}

// HTTPClient returns a client whose Transport already propagates trace context.
func HTTPClient() *http.Client { return &http.Client{Transport: Transport(nil)} }

// WrapClient adds propagation to an existing client in place.
func WrapClient(c *http.Client) *http.Client {
	c.Transport = Transport(c.Transport)
	return c
}
