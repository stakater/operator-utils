package endpoint

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/stakater/operator-utils/telemetry-web/internal/rebind"
	"github.com/stakater/operator-utils/telemetry-web/logging"
)

// resetInstruments rebinds the instruments to the current global MeterProvider
// so each test's ManualReader sees its own data. This is the same path
// telemetry.Init drives via rebind.Notify.
func resetInstruments() {
	rebind.Notify()
}

func TestRecordOutcomes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	resetInstruments()

	Record(context.Background(), "tenants.list", false) // success
	Record(context.Background(), "tenants.list", true)  // failure
	Record(context.Background(), "tenants.list", true)  // failure

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	success, failure := outcomeCounts(t, rm, "tenants.list")
	if success != 1 {
		t.Errorf("success = %d, want 1", success)
	}
	if failure != 2 {
		t.Errorf("failure = %d, want 2", failure)
	}
}

func TestInstrumentOutcome(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	resetInstruments()

	fail1 := errors.New("boom")
	fail2 := errors.New("again")
	Instrument(context.Background(), "orders.get")(nil)    // success (nil pointer)
	Instrument(context.Background(), "orders.get")(&fail1) // failure
	Instrument(context.Background(), "orders.get")(&fail2) // failure

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	success, failure := outcomeCounts(t, rm, "orders.get")
	if success != 1 {
		t.Errorf("success = %d, want 1", success)
	}
	if failure != 2 {
		t.Errorf("failure = %d, want 2", failure)
	}
}

func hasMetric(rm metricdata.ResourceMetrics, name string) bool {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return true
			}
		}
	}
	return false
}

func outcomeCounts(t *testing.T, rm metricdata.ResourceMetrics, endpoint string) (success, failure int64) {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "endpoint.requests" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("endpoint.requests is not Sum[int64]")
			}
			for _, dp := range sum.DataPoints {
				if ep, _ := dp.Attributes.Value(attribute.Key("endpoint")); ep.AsString() != endpoint {
					continue
				}
				oc, _ := dp.Attributes.Value(attribute.Key("outcome"))
				switch oc.AsString() {
				case "success":
					success += dp.Value
				case "failure":
					failure += dp.Value
				}
			}
		}
	}
	return success, failure
}

// histogram returns the endpoint.duration data point count and sum for
// {endpoint,outcome}, plus whether the instrument was found at all.
func histogram(t *testing.T, reader *sdkmetric.ManualReader, name, outcome string) (count uint64, sum float64, found bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "endpoint.duration" {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("endpoint.duration is %T, want Histogram[float64]", m.Data)
			}
			for _, dp := range h.DataPoints {
				ep, _ := dp.Attributes.Value(attribute.Key("endpoint"))
				oc, _ := dp.Attributes.Value(attribute.Key("outcome"))
				if ep.AsString() == name && oc.AsString() == outcome {
					count += dp.Count
					sum += dp.Sum
					found = true
				}
			}
		}
	}
	return count, sum, found
}

// The histogram is in seconds, so it needs explicit boundaries: the SDK default
// set starts at 5 and runs to 10000, which puts every operation faster than five
// seconds in one bucket and makes every quantile meaningless. Nothing else in the
// suite would notice the advice being dropped.
func TestDurationHistogramBuckets(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	resetInstruments()

	var err error
	Instrument(context.Background(), "orders.get")(&err)

	var rm metricdata.ResourceMetrics
	if cErr := reader.Collect(context.Background(), &rm); cErr != nil {
		t.Fatalf("collect: %v", cErr)
	}

	var dp metricdata.HistogramDataPoint[float64]
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "endpoint.duration" {
				continue
			}
			if m.Unit != "s" {
				t.Errorf("unit = %q, want \"s\"", m.Unit)
			}
			h := m.Data.(metricdata.Histogram[float64])
			dp, found = h.DataPoints[0], true
		}
	}
	if !found {
		t.Fatal("endpoint.duration not recorded")
	}

	if !slices.Equal(dp.Bounds, durationBounds) {
		t.Errorf("bounds = %v, want %v", dp.Bounds, durationBounds)
	}
	// The sub-millisecond observation above must land in the first bucket, which
	// is what the default boundaries got wrong.
	if dp.BucketCounts[0] != 1 {
		t.Errorf("a sub-millisecond observation landed in bucket %v, not the first: counts=%v",
			firstNonZero(dp.BucketCounts), dp.BucketCounts)
	}
}

func firstNonZero(counts []uint64) int {
	for i, c := range counts {
		if c > 0 {
			return i
		}
	}
	return -1
}

// Instrument is the one API that records latency, which is the whole point of
// it existing for non-HTTP operations.
func TestInstrumentRecordsDuration(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	resetInstruments()

	func() (err error) {
		defer Instrument(context.Background(), "queue.consume")(&err)
		time.Sleep(2 * time.Millisecond)
		return nil
	}()

	count, sum, found := histogram(t, reader, "queue.consume", "success")
	if !found {
		t.Fatal("endpoint.duration recorded no data point")
	}
	if count != 1 {
		t.Errorf("observation count = %d, want 1", count)
	}
	if sum <= 0 {
		t.Errorf("recorded duration = %v, want a positive elapsed time", sum)
	}
	// Seconds, per the instrument's unit — a 2ms sleep must not read as 2.
	if sum >= 1 {
		t.Errorf("recorded duration = %v s, implausible for a 2ms operation; unit may be wrong", sum)
	}
}

// The duration carries the same outcome attribute as the counter, so latency
// can be split by success/failure.
func TestInstrumentDurationCarriesFailureOutcome(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	resetInstruments()

	func() (err error) {
		defer Instrument(context.Background(), "queue.consume")(&err)
		return errors.New("boom")
	}()

	if count, _, found := histogram(t, reader, "queue.consume", "failure"); !found || count != 1 {
		t.Errorf("failure duration count = %d (found=%v), want 1", count, found)
	}
	if count, _, _ := histogram(t, reader, "queue.consume", "success"); count != 0 {
		t.Errorf("success duration count = %d, want 0", count)
	}
}

// Record is the counter-only primitive: it must not emit a duration, otherwise
// adapters would report a meaningless latency for every request.
func TestRecordDoesNotEmitDuration(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	resetInstruments()

	Record(context.Background(), "/users/:id", false)

	if count, _, found := histogram(t, reader, "/users/:id", "success"); found || count != 0 {
		t.Errorf("Record emitted %d duration observations, want 0", count)
	}
}

// M1: otel's global meter delegates to the FIRST real MeterProvider and never
// re-delegates, so instruments cached here would stay bound to it forever —
// silently writing into a retired pipeline after a shutdown, and recording
// nothing into the provider a second Init installs. rebind.Notify (which
// telemetry.Init calls) must move them to the current provider.
func TestRebindMovesInstrumentsToTheCurrentProvider(t *testing.T) {
	first := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(first)))
	resetInstruments()

	Record(context.Background(), "probe", false)
	if got := counterValue(t, first, "probe", "success"); got != 1 {
		t.Fatalf("first provider count = %d, want 1", got)
	}

	// Stand in for a second Init: new provider, then the rebind Init performs.
	second := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(second)))
	rebind.Notify()

	Record(context.Background(), "probe", false)

	if got := counterValue(t, second, "probe", "success"); got != 1 {
		t.Errorf("second provider count = %d, want 1 — instruments did not rebind", got)
	}
	if got := counterValue(t, first, "probe", "success"); got != 1 {
		t.Errorf("first provider count = %d, want it frozen at 1 — still receiving after rebind", got)
	}
}

// Without a rebind the instruments must still work: the lazy first build binds
// to whatever provider is global at that moment.
func TestInstrumentsBuildLazilyWithoutRebind(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	resetInstruments()

	Record(context.Background(), "lazy", false)
	if got := counterValue(t, reader, "lazy", "success"); got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
}

// counterValue collects from reader and returns the endpoint.requests value for
// {endpoint,outcome}.
func counterValue(t *testing.T, reader *sdkmetric.ManualReader, endpoint, outcome string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	success, failure := outcomeCounts(t, rm, endpoint)
	if outcome == "failure" {
		return failure
	}
	return success
}

// failingMeter reports an error for every instrument, standing in for a
// MeterProvider that rejects them (duplicate registration, bad configuration).
type failingMeter struct{ metricnoop.Meter }

func (failingMeter) Int64Counter(string, ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	return nil, errors.New("instrument rejected")
}

func (failingMeter) Float64Histogram(string, ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return nil, errors.New("instrument rejected")
}

type failingProvider struct{ metricnoop.MeterProvider }

func (failingProvider) Meter(string, ...metric.MeterOption) metric.Meter { return failingMeter{} }

// If the provider refuses to create instruments, the affected metrics are
// skipped and the library keeps working. Recording must not panic on the nil
// instrument — a telemetry failure must never become an application failure.
func TestInstrumentCreationFailureDegradesGracefully(t *testing.T) {
	otel.SetMeterProvider(failingProvider{})
	resetInstruments()

	ctx := context.Background()
	Record(ctx, "degraded", false)
	func() (err error) {
		defer Instrument(ctx, "degraded")(&err)
		return errors.New("boom")
	}()
	RecordPanic(ctx, "kaboom")

	i := get()
	if i.requests != nil || i.duration != nil || i.panics != nil {
		t.Error("instruments should be nil when the provider rejects them")
	}

	// And recovery is automatic: a working provider plus a rebind restores them.
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	rebind.Notify()

	Record(ctx, "recovered", false)
	if got := counterValue(t, reader, "recovered", "success"); got != 1 {
		t.Errorf("count after recovery = %d, want 1", got)
	}
}

// The diagnostic exists to name which instrument failed. A []error value is
// marshalled by slog's JSON handler as [{},{}] — the messages vanish — so the
// errors must be joined into a single error value.
func TestInstrumentFailureWarningNamesTheErrors(t *testing.T) {
	var buf bytes.Buffer
	logging.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { logging.SetDefault(nil) })

	// No manual reset: rebuild re-arms warnOnce per provider generation, so this
	// warns even though an earlier test in the run already triggered one. That is
	// the point — a failure against this provider must not be hidden by a warning
	// that fired against a previous one.
	otel.SetMeterProvider(failingProvider{})
	rebind.Notify()

	out := buf.String()
	if out == "" {
		t.Fatal("no warning logged")
	}
	if strings.Contains(out, "[{}") {
		t.Errorf("errors marshalled as empty objects, messages lost: %s", out)
	}
	if !strings.Contains(out, "instrument rejected") {
		t.Errorf("warning does not name the failure: %s", out)
	}
}
