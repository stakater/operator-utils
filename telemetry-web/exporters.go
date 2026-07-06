package telemetry

import (
	"context"
	"os"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const defaultOTLPEndpoint = "localhost:4317"

// resolveEndpoint prefers the config value, then the standard env var, then a
// local-collector default.
func resolveEndpoint(cfg Config) string {
	if cfg.OTLPEndpoint != "" {
		return cfg.OTLPEndpoint
	}
	if env := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); env != "" {
		return env
	}
	return defaultOTLPEndpoint
}

func newTraceExporter(ctx context.Context, cfg Config) (sdktrace.SpanProcessor, error) {
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(resolveEndpoint(cfg))}
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
	opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(resolveEndpoint(cfg))}
	if cfg.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	exp, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return sdkmetric.NewPeriodicReader(exp), nil
}
