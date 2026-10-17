package publisher

import (
	"context"
	"fmt"
	"sync"

	"github.com/prometheus/otlptranslator"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/stakater/operator-utils/observability/pkg/instrument"
	"github.com/stakater/operator-utils/observability/pkg/naming"
)

// promNamer mirrors the translation the Prometheus reader applies in
// buildPrometheusReader. Registration uses it to reject two names that
// would land on one /metrics family; the name handed to the SDK is
// always the one the caller passed.
var promNamer = otlptranslator.NewMetricNamer("", otlptranslator.UnderscoreEscapingWithSuffixes)

// CustomMetrics registers operator-defined metrics on a single OTel Meter.
// All registrations are validated against naming.ValidateMetricName and
// tracked to reject duplicate names across instrument types.
//
// Two distinct names can still collide once the Prometheus reader
// translates them: a counter is served as name+"_total", so "reconcile"
// and "reconcile_total" are one family on /metrics. The whole registry
// fails to gather when that happens, which controller-runtime serves as
// a 500 on every scrape, so reserve rejects the second registration
// instead.
type CustomMetrics struct {
	meter metric.Meter

	mu sync.Mutex
	// names maps every claimed name to the metric that claimed it. A
	// metric claims the name it was registered under and, when the
	// translator rewrites it, the name it will carry on /metrics.
	names map[string]string
}

// reserve claims both the registered name and the name that name will
// carry on /metrics. typ is the OTel metric type, which decides whether
// the translator appends a suffix.
func (c *CustomMetrics) reserve(name, description string, typ otlptranslator.MetricType) error {
	if err := naming.ValidateMetricName(name); err != nil {
		return err
	}
	if description == "" {
		return fmt.Errorf("metric %q: description must not be empty", name)
	}
	// Instruments are registered without a unit, so the only transform
	// the translator can apply to an already-validated name is the
	// counter suffix. An error here means the name is unrepresentable.
	promName, err := promNamer.Build(otlptranslator.Metric{Name: name, Type: typ})
	if err != nil {
		return fmt.Errorf("metric %q: cannot be exposed on /metrics: %w", name, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.names == nil {
		c.names = map[string]string{}
	}
	for _, key := range keysFor(name, promName) {
		owner, exists := c.names[key]
		if !exists {
			continue
		}
		if owner == name {
			return fmt.Errorf("metric %q already registered", name)
		}
		return fmt.Errorf("metric %q collides with metric %q: both are served as %q on /metrics", name, owner, promName)
	}
	for _, key := range keysFor(name, promName) {
		c.names[key] = name
	}
	return nil
}

// release undoes a reserve. Used when the SDK rejects the instrument
// after the name was already claimed.
func (c *CustomMetrics) release(name string, typ otlptranslator.MetricType) {
	promName, err := promNamer.Build(otlptranslator.Metric{Name: name, Type: typ})
	if err != nil {
		promName = name
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, key := range keysFor(name, promName) {
		delete(c.names, key)
	}
}

// keysFor returns the reservation keys for a metric, deduplicated: the
// two are equal whenever the translator leaves the name alone.
func keysFor(name, promName string) []string {
	if name == promName {
		return []string{name}
	}
	return []string{name, promName}
}

// Counter registers a new int64 counter and returns an instrument.Counter handle.
func (c *CustomMetrics) Counter(name, description string) (instrument.Counter, error) {
	if err := c.reserve(name, description, otlptranslator.MetricTypeMonotonicCounter); err != nil {
		return nil, err
	}
	inst, err := c.meter.Int64Counter(name, metric.WithDescription(description))
	if err != nil {
		c.release(name, otlptranslator.MetricTypeMonotonicCounter)
		return nil, fmt.Errorf("create counter %q: %w", name, err)
	}
	return &counterImpl{inst: inst}, nil
}

// Gauge registers a new int64 sync gauge and returns an instrument.Gauge handle.
func (c *CustomMetrics) Gauge(name, description string) (instrument.Gauge, error) {
	if err := c.reserve(name, description, otlptranslator.MetricTypeGauge); err != nil {
		return nil, err
	}
	inst, err := c.meter.Int64Gauge(name, metric.WithDescription(description))
	if err != nil {
		c.release(name, otlptranslator.MetricTypeGauge)
		return nil, fmt.Errorf("create gauge %q: %w", name, err)
	}
	return &gaugeImpl{inst: inst}, nil
}

// Histogram registers a new float64 histogram and returns an instrument.Histogram handle.
func (c *CustomMetrics) Histogram(name, description string) (instrument.Histogram, error) {
	if err := c.reserve(name, description, otlptranslator.MetricTypeHistogram); err != nil {
		return nil, err
	}
	inst, err := c.meter.Float64Histogram(name, metric.WithDescription(description))
	if err != nil {
		c.release(name, otlptranslator.MetricTypeHistogram)
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
