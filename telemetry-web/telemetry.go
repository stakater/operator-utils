// Package telemetry is a framework-agnostic house-style wrapper over
// OpenTelemetry. Call Init once in main; use Handler to wrap a net/http
// handler, InstrumentEndpoint for per-endpoint metrics, and Transport/HTTPClient
// for outbound trace propagation. No web framework is imported here — consumers
// call the exported functions from their own middleware.
package telemetry

import (
	"context"
	"errors"
	"os"
	"strconv"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"

	"github.com/stakater/operator-utils/telemetry-web/internal/scope"
)

// Config is the entire consumer configuration surface. ServiceName is required;
// everything else is optional and falls back to OTEL_* env vars or defaults.
type Config struct {
	ServiceName    string
	ServiceVersion string
	Environment    string
	OTLPEndpoint   string
	// SampleRatio: nil = unset (falls back to OTEL_TRACES_SAMPLER_ARG, then 1.0);
	// a non-nil pointer is used verbatim, including 0 to never sample new roots.
	SampleRatio *float64
	Insecure    bool
}

// Init wires the global TracerProvider, MeterProvider, propagator, resource,
// sampler, OTLP exporters, and runtime metrics. Call once in main. The returned
// shutdown flushes all providers; defer it.
func Init(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	if cfg.ServiceName == "" {
		return nil, errors.New("telemetry: Config.ServiceName is required")
	}
	scope.Set(cfg.ServiceName)

	attrs := []attribute.KeyValue{semconv.ServiceName(cfg.ServiceName)}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	if cfg.Environment != "" {
		attrs = append(attrs, semconv.DeploymentEnvironmentName(cfg.Environment))
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(attrs...),
		resource.WithProcess(),
		resource.WithHost(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, err
	}

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(resolveRatio(cfg)))

	spanProcessor, err := newTraceExporter(ctx, cfg)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(spanProcessor),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)
	otel.SetTracerProvider(tp)

	reader, err := newMetricReader(ctx, cfg)
	if err != nil {
		_ = tp.Shutdown(ctx)
		return nil, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		_ = mp.Shutdown(ctx)
		_ = tp.Shutdown(ctx)
		return nil, err
	}

	return func(shutdownCtx context.Context) error {
		return errors.Join(tp.Shutdown(shutdownCtx), mp.Shutdown(shutdownCtx))
	}, nil
}

// (The shutdown timeout is chosen by the consumer in main via
// context.WithTimeout — the library does not impose one.)

// resolveRatio: config value if set, else OTEL_TRACES_SAMPLER_ARG, else 1.0.
func resolveRatio(cfg Config) float64 {
	if cfg.SampleRatio != nil {
		return *cfg.SampleRatio
	}
	if arg := os.Getenv("OTEL_TRACES_SAMPLER_ARG"); arg != "" {
		if r, err := strconv.ParseFloat(arg, 64); err == nil {
			return r
		}
	}
	return 1.0
}
