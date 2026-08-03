// Package endpoint records per-endpoint request metrics and panics against the
// global OpenTelemetry meter. Instruments are created lazily on first use and
// rebuilt whenever the global MeterProvider changes.
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

	"github.com/stakater/operator-utils/telemetry-web/internal/version"
	"github.com/stakater/operator-utils/telemetry-web/logging"
)

// instruments is swapped as a unit so a rebuild is atomic from a recorder's
// point of view: a caller sees either the old set or the new one, never a
// half-updated mix.
type instruments struct {
	requests metric.Int64Counter
	duration metric.Float64Histogram
	panics   metric.Int64Counter
}

// durationBounds are the explicit second-scale buckets for endpoint.duration.
// Without advice the SDK applies its default boundaries, which start at 5 and
// run to 10000 — sane for milliseconds, useless for a unit of seconds, where
// every operation under five seconds falls in one bucket and the quantiles say
// nothing. These match otelhttp's http.server.request.duration set, so the two
// histograms are directly comparable.
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
	// warnOnce is replaced on every rebuild, so the warning is once per provider
	// generation rather than once per process: a failure against the second
	// MeterProvider must not be hidden by one that already fired against the
	// first. Guarded by buildMu, which every build path holds. A pointer because
	// assigning a sync.Once by value copies a lock.
	warnOnce = new(sync.Once)
)

// get returns instruments bound to the MeterProvider that is global right now,
// rebuilding them when it has changed.
//
// The provider has to be re-checked rather than cached once: otel's global meter
// delegates to the first real provider installed and never re-delegates, so an
// instrument set kept across a provider swap would go on writing into the retired
// one. Comparing the provider per call keeps them current, and it does so for any
// installer, not only telemetry.Init.
//
// The comparison assumes a comparable MeterProvider. Every implementation in the
// SDK is a pointer or an empty struct; a provider declared as a struct containing
// a map or slice would panic here.
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

// build creates the instrument set from whatever MeterProvider is global now.
// A creation failure is warned about once per provider generation and leaves that
// instrument nil, so the corresponding metric is skipped rather than panicking.
//
// mp is passed in rather than read from the global so it is the same provider get
// compared against, with no window for a swap in between.
func build(mp metric.MeterProvider) *instruments {
	m := mp.Meter(
		version.ModulePath,
		metric.WithInstrumentationVersion(version.Version()),
	)
	var i instruments
	var errs []error

	var err error
	i.requests, err = m.Int64Counter("endpoint.requests",
		metric.WithUnit("{request}"),
		metric.WithDescription("Per-endpoint request count, split by success/failure outcome."),
	)
	if err != nil {
		i.requests, errs = nil, append(errs, err)
	}
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
		// Joined, not a []error: slog's JSON handler renders a slice of errors
		// as [{},{}], losing every message. Warned once per process, since a
		// rebuild runs on every Init.
		warnOnce.Do(func() {
			logging.Logger().Warn("telemetry: some endpoint instruments could not be created; those metrics are disabled",
				"err", errors.Join(errs...))
		})
	}
	return &i
}

// Record emits one endpoint.requests data point for a named endpoint.
// failed=true => outcome "failure", else "success". Framework adapters call this
// with an outcome derived from the response status; hand-written code should
// prefer Instrument.
func Record(ctx context.Context, name string, failed bool) {
	get().record(ctx, name, failed)
}

// record emits the counter data point against one instrument set.
func (i *instruments) record(ctx context.Context, name string, failed bool) {
	if i.requests == nil {
		return
	}
	i.requests.Add(ctx, 1, metric.WithAttributes(
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
// It records both endpoint.requests and endpoint.duration. The pointer is what
// makes the one-line defer work: &err is bound at defer time, but *err is read
// when the finisher runs, after the named return is set. A plain nil records a
// success.
//
// This is the entry point for NON-HTTP operations, which no otelhttp histogram
// covers. HTTP handlers served through nethttp.Handler already get latency per
// route from http.server.request.duration.
func Instrument(ctx context.Context, name string) func(err *error) {
	start := time.Now()
	return func(err *error) {
		failed := err != nil && *err != nil

		// One get(): a rebuild between the two records would otherwise split
		// them across providers.
		i := get()
		i.record(ctx, name, failed)
		if i.duration == nil {
			return
		}
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
