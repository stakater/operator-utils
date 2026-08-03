// Package endpoint times named operations and records panics against the global
// OpenTelemetry meter. Instruments are created lazily on first use and rebuilt
// whenever the global MeterProvider changes.
package endpoint

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/stakater/operator-utils/telemetry-web/internal/ident"
	"github.com/stakater/operator-utils/telemetry-web/logging"
)

// instruments is swapped as a unit so a recorder sees either the old set or the
// new one, never a half-updated mix.
type instruments struct {
	duration metric.Float64Histogram
	panics   metric.Int64Counter
}

// durationBounds are the explicit second-scale buckets for endpoint.duration. The
// SDK's default boundaries run 5..10000, which is useless for a unit of seconds.
// These match otelhttp's http.server.request.duration set, so the two histograms
// are directly comparable.
var durationBounds = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}

// bound pairs an instrument set with the MeterProvider it was created from, so a
// provider swap is noticed by comparison rather than by notification.
type bound struct {
	mp metric.MeterProvider
	i  *instruments
}

var (
	current atomic.Pointer[bound]
	buildMu sync.Mutex
	// Replaced on every rebuild so the warning is once per provider generation,
	// not once per process. Guarded by buildMu; a pointer because assigning a
	// sync.Once by value copies a lock.
	warnOnce = new(sync.Once)
)

// get returns instruments bound to the MeterProvider that is global right now,
// rebuilding them when it has changed.
//
// The provider is re-checked per call rather than cached: otel's global meter
// delegates to the first real provider installed and never re-delegates, so an
// instrument set kept across a swap would go on writing into the retired one.
//
// Assumes a comparable MeterProvider — every SDK implementation is a pointer or
// an empty struct.
func get() *instruments {
	mp := otel.GetMeterProvider()
	if b := current.Load(); b != nil && b.mp == mp {
		return b.i
	}
	buildMu.Lock()
	defer buildMu.Unlock()
	if b := current.Load(); b != nil && b.mp == mp {
		return b.i
	}
	warnOnce = new(sync.Once)
	b := &bound{mp: mp, i: build(mp)}
	current.Store(b)
	return b.i
}

// build creates the instrument set for mp. A creation failure is warned about
// once per provider generation and leaves that instrument nil, so the metric is
// skipped rather than panicking.
//
// mp is passed in rather than read from the global so it is the same provider get
// compared against, with no window for a swap in between.
func build(mp metric.MeterProvider) *instruments {
	m := mp.Meter(
		ident.ModulePath,
		metric.WithInstrumentationVersion(ident.Version()),
	)
	var i instruments
	var errs []error

	var err error
	i.duration, err = m.Float64Histogram("endpoint.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of a named operation, split by success/failure outcome."),
		metric.WithExplicitBucketBoundaries(durationBounds...),
	)
	if err != nil {
		i.duration, errs = nil, append(errs, err)
	}
	i.panics, err = m.Int64Counter("http.server.panics",
		metric.WithUnit("{panic}"),
		metric.WithDescription("Panics recovered from HTTP handlers."),
	)
	if err != nil {
		i.panics, errs = nil, append(errs, err)
	}

	if len(errs) > 0 {
		// Joined, not a []error: slog's JSON handler renders a slice of errors as
		// [{},{}], losing every message.
		warnOnce.Do(func() {
			logging.Logger().Warn("telemetry: some endpoint instruments could not be created; those metrics are disabled",
				"err", errors.Join(errs...))
		})
	}
	return &i
}

// Instrument times a named operation and returns a finisher that takes the
// ADDRESS of the operation's error (nil on success):
//
//	func ListTenants(ctx context.Context) (err error) {
//	    defer endpoint.Instrument(ctx, "tenants.list")(&err)
//	    // ... set err on failure paths; outcome follows the final err ...
//	}
//
// The pointer is what makes the one-line defer work: &err is bound at defer time,
// but *err is read when the finisher runs, after the named return is set. A plain
// nil records a success.
//
// This is the entry point for NON-HTTP operations. Handlers served through
// nethttp.Handler already get latency per route from http.server.request.duration.
func Instrument(ctx context.Context, name string) func(err *error) {
	start := time.Now()
	return func(err *error) {
		i := get()
		if i.duration == nil {
			return
		}
		failed := err != nil && *err != nil
		i.duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(
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
