package telemetry

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// hasScheme reports whether the endpoint is URL-form (http://host:port) rather
// than bare host:port. The two need different exporter options.
func hasScheme(endpoint string) bool { return strings.Contains(endpoint, "://") }

// Endpoint resolution: only Config.OTLPEndpoint is applied here — as
// WithEndpointURL for URL-form values, WithEndpoint for bare host:port. When
// unset, NO endpoint option is passed, so the exporter SDK's own env handling
// applies: OTEL_EXPORTER_OTLP_ENDPOINT (URL-form per spec), the per-signal
// OTEL_EXPORTER_OTLP_{TRACES,METRICS}_ENDPOINT overrides, and the
// localhost:4317 default. Re-reading the env here and feeding it to
// WithEndpoint would break spec-compliant URL values.

func newTraceExporter(ctx context.Context, cfg Config) (sdktrace.SpanProcessor, error) {
	var opts []otlptracegrpc.Option
	if ep := cfg.OTLPEndpoint; ep != "" {
		if hasScheme(ep) {
			opts = append(opts, otlptracegrpc.WithEndpointURL(ep))
		} else {
			opts = append(opts, otlptracegrpc.WithEndpoint(ep))
		}
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	exp, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return sdktrace.NewBatchSpanProcessor(exp), nil
}

func newMetricReader(ctx context.Context, cfg Config) (sdkmetric.Reader, error) {
	var opts []otlpmetricgrpc.Option
	if ep := cfg.OTLPEndpoint; ep != "" {
		if hasScheme(ep) {
			opts = append(opts, otlpmetricgrpc.WithEndpointURL(ep))
		} else {
			opts = append(opts, otlpmetricgrpc.WithEndpoint(ep))
		}
	}
	if cfg.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	exp, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return sdkmetric.NewPeriodicReader(exp), nil
}
