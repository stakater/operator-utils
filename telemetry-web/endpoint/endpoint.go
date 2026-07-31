// Package endpoint records per-endpoint request metrics and panics against the
// global OpenTelemetry meter. Instruments are created lazily on first use.
package endpoint

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/stakater/operator-utils/telemetry-web/internal/version"
	"github.com/stakater/operator-utils/telemetry-web/logging"
)

var (
	once     sync.Once
	requests metric.Int64Counter
	duration metric.Float64Histogram
	panics   metric.Int64Counter
)

// ensure lazily creates the instruments from the global MeterProvider.
// Idempotent; safe to call before telemetry.Init (records no-op against the
// default provider until a real one is installed and this first runs).
func ensure() {
	once.Do(func() {
		m := otel.GetMeterProvider().Meter(
			version.ModulePath,
			metric.WithInstrumentationVersion(version.Version()),
		)
		var err error
		requests, err = m.Int64Counter("endpoint.requests",
			metric.WithUnit("{request}"),
			metric.WithDescription("Per-endpoint request count, split by success/failure outcome."),
		)
		if err != nil {
			logging.Logger().Warn("failed to create endpoint.requests counter; per-endpoint metrics disabled", "err", err)
		}
		duration, err = m.Float64Histogram("endpoint.duration",
			metric.WithUnit("s"),
			metric.WithDescription("Duration of a named operation, split by success/failure outcome."),
		)
		if err != nil {
			logging.Logger().Warn("failed to create endpoint.duration histogram; operation latency disabled", "err", err)
		}
		panics, err = m.Int64Counter("http.server.panics",
			metric.WithUnit("{panic}"),
			metric.WithDescription("Panics recovered from HTTP handlers."),
		)
		if err != nil {
			logging.Logger().Warn("failed to create http.server.panics counter; panic metrics disabled", "err", err)
		}
	})
}

// Record emits one endpoint.requests data point for a named endpoint.
// failed=true => outcome "failure", else "success". Framework adapters call this
// with an outcome derived from the response status; hand-written code should
// prefer Instrument.
func Record(ctx context.Context, name string, failed bool) {
	ensure()
	if requests == nil {
		return
	}
	requests.Add(ctx, 1, metric.WithAttributes(
		attribute.String("endpoint", name),
		attribute.String("outcome", outcomeOf(failed)),
	))
}

// Instrument times a named operation and returns a finisher that takes the
// ADDRESS of the operation's error (nil on success):
//
//	func ListTenants(ctx context.Context) (err error) {
//	    defer endpoint.Instrument(ctx, "tenants.list")(&err)
//	    // ... set err on failure paths; outcome follows the final err ...
//	}
//
// It records both endpoint.requests and endpoint.duration. The pointer
// makes the one-line defer work: &err is bound at defer time, but *err is read
// when the finisher runs, after the named return is set. Passing a plain nil
// records a success.
//
// This is the intended entry point for NON-HTTP operations, which have no
// otelhttp histogram covering them. For HTTP handlers served through
// nethttp.Handler, http.server.request.duration already records latency per
// route.
func Instrument(ctx context.Context, name string) func(err *error) {
	start := time.Now()
	return func(err *error) {
		failed := err != nil && *err != nil
		Record(ctx, name, failed) // also runs ensure()
		if duration == nil {
			return
		}
		duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(
			attribute.String("endpoint", name),
			attribute.String("outcome", outcomeOf(failed)),
		))
	}
}

// outcomeOf maps the boolean failure flag to the metric attribute value.
func outcomeOf(failed bool) string {
	if failed {
		return "failure"
	}
	return "success"
}
