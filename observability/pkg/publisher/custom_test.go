package publisher

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric"
)

func newTestCustomMetrics(t *testing.T) *CustomMetrics {
	t.Helper()
	mp := metric.NewMeterProvider()
	return &CustomMetrics{meter: mp.Meter("test")}
}

func TestCustomMetrics_CounterHappyPath(t *testing.T) {
	c := newTestCustomMetrics(t)
	ctr, err := c.Counter("reconcile_total", "total reconciliations")
	if err != nil {
		t.Fatalf("Counter: %v", err)
	}
	ctr.Inc(context.Background())
}

func TestCustomMetrics_GaugeHappyPath(t *testing.T) {
	c := newTestCustomMetrics(t)
	g, err := c.Gauge("active_workers", "current active workers")
	if err != nil {
		t.Fatalf("Gauge: %v", err)
	}
	g.Set(context.Background(), 5)
}

func TestCustomMetrics_HistogramHappyPath(t *testing.T) {
	c := newTestCustomMetrics(t)
	h, err := c.Histogram("reconcile_duration_seconds", "reconcile duration")
	if err != nil {
		t.Fatalf("Histogram: %v", err)
	}
	h.Record(context.Background(), 0.123)
}

func TestCustomMetrics_DuplicateNameRejected(t *testing.T) {
	c := newTestCustomMetrics(t)
	if _, err := c.Counter("dupe", "first"); err != nil {
		t.Fatalf("first Counter: %v", err)
	}
	_, err := c.Counter("dupe", "second")
	if err == nil {
		t.Fatal("expected error on duplicate name, got nil")
	}
}

func TestCustomMetrics_DuplicateAcrossInstrumentTypes(t *testing.T) {
	c := newTestCustomMetrics(t)
	if _, err := c.Counter("metric_x", "a"); err != nil {
		t.Fatalf("Counter: %v", err)
	}
	if _, err := c.Gauge("metric_x", "b"); err == nil {
		t.Fatal("expected duplicate-name error when registering gauge with same name as counter")
	}
}

func TestCustomMetrics_ReservedPrefixRejected(t *testing.T) {
	c := newTestCustomMetrics(t)
	_, err := c.Counter("controller_runtime_x", "bad")
	if err == nil {
		t.Fatal("expected reserved-prefix error")
	}
}

func TestCustomMetrics_InvalidNameRejected(t *testing.T) {
	c := newTestCustomMetrics(t)
	_, err := c.Counter("BAD-NAME", "bad")
	if err == nil {
		t.Fatal("expected name-validation error")
	}
}

func TestCustomMetrics_EmptyDescriptionRejected(t *testing.T) {
	c := newTestCustomMetrics(t)
	_, err := c.Counter("ok_name", "")
	if err == nil || !strings.Contains(err.Error(), "description") {
		t.Fatalf("want description error, got %v", err)
	}
}

func TestCustomMetrics_MustCounterPanicsOnInvalid(t *testing.T) {
	c := newTestCustomMetrics(t)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustCounter did not panic on invalid name")
		}
	}()
	c.MustCounter("BAD", "x")
}
