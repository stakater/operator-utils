// Package endpoint records per-endpoint request metrics and panics against the
// global OpenTelemetry meter. Instruments are created lazily on first use.
package endpoint

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	once     sync.Once
	requests metric.Int64Counter
	panics   metric.Int64Counter
)

// ensure lazily creates the counters from the global MeterProvider. Idempotent;
// safe to call before telemetry.Init (records no-op against the default provider
// until a real one is installed and this first runs).
func ensure() {
	once.Do(func() {
		m := otel.GetMeterProvider().Meter("telemetry")
		requests, _ = m.Int64Counter("http.endpoint.requests",
			metric.WithUnit("{request}"),
			metric.WithDescription("Per-endpoint request count, split by success/failure outcome."),
		)
		panics, _ = m.Int64Counter("http.server.panics",
			metric.WithUnit("{panic}"),
			metric.WithDescription("Panics recovered from HTTP handlers."),
		)
	})
}

// Record emits one http.endpoint.requests data point for a named endpoint.
// failed=true => outcome "failure", else "success". Framework adapters call this
// with an outcome derived from the response status; hand-written code should
// prefer Instrument.
func Record(ctx context.Context, name string, failed bool) {
	ensure()
	if requests == nil {
		return
	}
	outcome := "success"
	if failed {
		outcome = "failure"
	}
	requests.Add(ctx, 1, metric.WithAttributes(
		attribute.String("endpoint", name),
		attribute.String("outcome", outcome),
	))
}

// Instrument marks the start of a named endpoint (or any operation) and returns
// a finisher that takes the ADDRESS of the operation's error (nil on success):
//
//	func ListTenants(ctx context.Context) (err error) {
//	    defer endpoint.Instrument(ctx, "tenants.list")(&err)
//	    // ... set err on failure paths; outcome follows the final err ...
//	}
//
// The pointer makes the one-line defer work: &err is bound at defer time, but
// *err is read when the finisher runs, after the named return is set. Passing a
// plain nil records a success.
func Instrument(ctx context.Context, name string) func(err *error) {
	return func(err *error) {
		Record(ctx, name, err != nil && *err != nil)
	}
}
