package endpoint

import (
	"context"
	"errors"
	"sync"
	"testing"

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
			if m.Name != "http.endpoint.requests" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("http.endpoint.requests is not Sum[int64]")
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
