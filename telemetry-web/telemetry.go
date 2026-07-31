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
	"sync"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/stakater/operator-utils/telemetry-web/internal/scope"
	"github.com/stakater/operator-utils/telemetry-web/logging"
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

// ErrAlreadyInitialized is returned by a second Init that is not preceded by
// the first one's shutdown. Re-initializing would orphan the previous
// providers — their batch processor goroutine and reader ticker have no
// remaining path to Shutdown — and would register the runtime instrumentation
// callbacks a second time.
var ErrAlreadyInitialized = errors.New("telemetry: Init already called; run the returned shutdown before re-initializing")

var (
	initMu      sync.Mutex
	initialized bool
)

// Init wires the global TracerProvider, MeterProvider, propagator, resource,
// sampler, OTLP exporters, and runtime metrics. Call once in main. The returned
// shutdown flushes all providers; defer it under a bounded context so an
// unreachable collector cannot stall process exit.
//
// Calling Init twice without an intervening shutdown returns
// ErrAlreadyInitialized. The returned shutdown is safe to call more than once;
// after it runs, Init may be called again (tests rely on this).
func Init(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	if cfg.ServiceName == "" {
		return nil, errors.New("telemetry: Config.ServiceName is required")
	}

	initMu.Lock()
	defer initMu.Unlock()
	if initialized {
		return nil, ErrAlreadyInitialized
	}

	scope.Set(cfg.ServiceName)

	attrs := []attribute.KeyValue{semconv.ServiceName(cfg.ServiceName)}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	if cfg.Environment != "" {
		attrs = append(attrs, semconv.DeploymentEnvironmentName(cfg.Environment))
	}

	// WithFromEnv first so OTEL_RESOURCE_ATTRIBUTES / OTEL_SERVICE_NAME are
	// honored, WithAttributes last so explicit Config values win the merge.
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithHost(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(attrs...),
	)
	if err != nil {
		// A schema-URL conflict yields a usable resource alongside the error;
		// only a nil resource is fatal.
		if res == nil {
			return nil, err
		}
		logging.Logger().Warn("telemetry: resource built with conflicts", "err", err)
	}

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(resolveRatio(cfg)))

	spanProcessor, err := newSpanProcessor(ctx, cfg)
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

	// Bound to mp rather than the global provider, so mp.Shutdown below retires
	// the runtime callbacks with it. The contrib package exposes no Stop, which
	// is the other reason Init refuses to run twice.
	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		_ = mp.Shutdown(ctx)
		_ = tp.Shutdown(ctx)
		return nil, err
	}

	initialized = true

	var once sync.Once
	var shutdownErr error
	return func(shutdownCtx context.Context) error {
		once.Do(func() {
			shutdownErr = errors.Join(tp.Shutdown(shutdownCtx), mp.Shutdown(shutdownCtx))
			// Retire the globals too: leaving them pointing at shut-down
			// providers means anything still recording — a lingering goroutine,
			// or code running before a re-Init — writes into a dead pipeline.
			// Noop providers make that a visible no-op instead.
			otel.SetTracerProvider(tracenoop.NewTracerProvider())
			otel.SetMeterProvider(metricnoop.NewMeterProvider())

			initMu.Lock()
			initialized = false
			initMu.Unlock()
		})
		return shutdownErr
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
