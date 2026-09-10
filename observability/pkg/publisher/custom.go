package publisher

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/stakater/operator-utils/observability/pkg/instrument"
	"github.com/stakater/operator-utils/observability/pkg/naming"
)

// CustomMetrics registers operator-defined metrics on a single OTel Meter.
// All registrations are validated against naming.ValidateMetricName and
// tracked to reject duplicate names across instrument types.
type CustomMetrics struct {
	meter metric.Meter

	mu    sync.Mutex
	names map[string]struct{}
}

func (c *CustomMetrics) reserve(name, description string) error {
	if err := naming.ValidateMetricName(name); err != nil {
		return err
	}
	if description == "" {
		return fmt.Errorf("metric %q: description must not be empty", name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.names == nil {
		c.names = map[string]struct{}{}
	}
	if _, exists := c.names[name]; exists {
		return fmt.Errorf("metric %q already registered", name)
	}
	c.names[name] = struct{}{}
	return nil
}

// Counter registers a new int64 counter and returns an instrument.Counter handle.
func (c *CustomMetrics) Counter(name, description string) (instrument.Counter, error) {
	if err := c.reserve(name, description); err != nil {
		return nil, err
	}
	inst, err := c.meter.Int64Counter(name, metric.WithDescription(description))
	if err != nil {
		c.mu.Lock()
		delete(c.names, name)
		c.mu.Unlock()
		return nil, fmt.Errorf("create counter %q: %w", name, err)
	}
	return &counterImpl{inst: inst}, nil
}

// Gauge registers a new int64 sync gauge and returns an instrument.Gauge handle.
func (c *CustomMetrics) Gauge(name, description string) (instrument.Gauge, error) {
	if err := c.reserve(name, description); err != nil {
		return nil, err
	}
	inst, err := c.meter.Int64Gauge(name, metric.WithDescription(description))
	if err != nil {
		c.mu.Lock()
		delete(c.names, name)
		c.mu.Unlock()
		return nil, fmt.Errorf("create gauge %q: %w", name, err)
	}
	return &gaugeImpl{inst: inst}, nil
}

// Histogram registers a new float64 histogram and returns an instrument.Histogram handle.
func (c *CustomMetrics) Histogram(name, description string) (instrument.Histogram, error) {
	if err := c.reserve(name, description); err != nil {
		return nil, err
	}
	inst, err := c.meter.Float64Histogram(name, metric.WithDescription(description))
	if err != nil {
		c.mu.Lock()
		delete(c.names, name)
		c.mu.Unlock()
		return nil, fmt.Errorf("create histogram %q: %w", name, err)
	}
	return &histogramImpl{inst: inst}, nil
}

// MustCounter is like Counter but panics on error. Intended for package-level
// var initialization.
func (c *CustomMetrics) MustCounter(name, description string) instrument.Counter {
	v, err := c.Counter(name, description)
	if err != nil {
		panic(err)
	}
	return v
}

// MustGauge is like Gauge but panics on error.
func (c *CustomMetrics) MustGauge(name, description string) instrument.Gauge {
	v, err := c.Gauge(name, description)
	if err != nil {
		panic(err)
	}
	return v
}

// MustHistogram is like Histogram but panics on error.
func (c *CustomMetrics) MustHistogram(name, description string) instrument.Histogram {
	v, err := c.Histogram(name, description)
	if err != nil {
		panic(err)
	}
	return v
}

// Concrete instrument implementations. They satisfy the interfaces in the
// instrument package and are constructed alongside the underlying OTel
// instrument in Counter/Gauge/Histogram above.

type counterImpl struct{ inst metric.Int64Counter }

func (c *counterImpl) Inc(ctx context.Context, attrs ...attribute.KeyValue) {
	c.inst.Add(ctx, 1, metric.WithAttributes(attrs...))
}
func (c *counterImpl) Add(ctx context.Context, value int64, attrs ...attribute.KeyValue) {
	c.inst.Add(ctx, value, metric.WithAttributes(attrs...))
}

type gaugeImpl struct{ inst metric.Int64Gauge }

func (g *gaugeImpl) Set(ctx context.Context, value int64, attrs ...attribute.KeyValue) {
	g.inst.Record(ctx, value, metric.WithAttributes(attrs...))
}

type histogramImpl struct{ inst metric.Float64Histogram }

func (h *histogramImpl) Record(ctx context.Context, value float64, attrs ...attribute.KeyValue) {
	h.inst.Record(ctx, value, metric.WithAttributes(attrs...))
}
