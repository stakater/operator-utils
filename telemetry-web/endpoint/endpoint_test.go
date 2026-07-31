package endpoint

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// resetInstruments rebinds the counters to the current global MeterProvider so
// each test's ManualReader sees its own data. Test-only.
func resetInstruments() {
	once = sync.Once{}
	ensure()
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
