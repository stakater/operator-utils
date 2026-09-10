// Package instrument defines the Counter, Gauge, and Histogram interfaces
// returned by publisher.CustomMetrics. They are factored out into this
// small package so callers that only need to type-annotate metric handles
// (e.g. helper function signatures) can import a stable, dependency-light
// surface without pulling in the full publisher.
package instrument

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
)

// Counter is a monotonically increasing integer counter.
type Counter interface {
	// Inc increments the counter by 1.
	Inc(ctx context.Context, attrs ...attribute.KeyValue)
	// Add adds value to the counter. value must be non-negative.
	Add(ctx context.Context, value int64, attrs ...attribute.KeyValue)
}

// Gauge represents an instantaneous integer measurement.
type Gauge interface {
	// Set records the current value of the gauge.
	Set(ctx context.Context, value int64, attrs ...attribute.KeyValue)
}

// Histogram records a distribution of float64 measurements.
type Histogram interface {
	// Record adds a sample to the histogram.
	Record(ctx context.Context, value float64, attrs ...attribute.KeyValue)
}
