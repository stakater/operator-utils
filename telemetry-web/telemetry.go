package telemetry

import (
	"context"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

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

	"github.com/stakater/operator-utils/telemetry-web/internal/ident"
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

// ErrAlreadyInitialized is returned by a second Init not preceded by the first
// one's shutdown, which would orphan the previous providers' goroutines and
// register the runtime instrumentation twice.
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
// after it runs, Init may be called again.
//
// Re-Init caveat: an http.Handler built before the re-Init keeps producing spans
// but stops producing http.server.* metrics, because otelhttp resolves its meter
// once at construction. Rebuild the handler if you depend on those metrics.
func Init(ctx context.Context, cfg Config) (shutdown func(context.Context) error, retErr error) {
	if cfg.ServiceName == "" {
		return nil, errors.New("telemetry: Config.ServiceName is required")
	}

	initMu.Lock()
	defer initMu.Unlock()
	if initialized {
		return nil, ErrAlreadyInitialized
	}

	// Init mutates process-wide state before anything can fail, so a failure has
	// to put it all back — otherwise a caller that logs and keeps serving is left
	// wired to a shut-down pipeline instead of to a noop.
	defer func() {
		if retErr != nil {
			retire()
		}
	}()

	// Cleanup must not depend on ctx still being live: a provider whose Shutdown
	// is refused for a cancelled context latches shut without stopping its
	// goroutines, and nothing can stop them afterwards.
	cleanupCtx := context.WithoutCancel(ctx)

	// Routes SDK-internal failures into the consumer's logger instead of otel's
	// default stderr handler, which would produce a second log format. Set before
	// the exporters are built so their errors are caught too.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logging.Logger().Error("telemetry: sdk error", "err", err)
	}))

	ident.SetServiceName(cfg.ServiceName)

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
		_ = tp.Shutdown(cleanupCtx)
		return nil, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	// Bound to mp rather than the global provider so mp.Shutdown retires the
	// runtime callbacks along with it.
	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		_ = mp.Shutdown(cleanupCtx)
		_ = tp.Shutdown(cleanupCtx)
		return nil, err
	}

	initialized = true

	var once sync.Once
	var shutdownErr error
	return func(shutdownCtx context.Context) error {
		once.Do(func() {
			flushCtx, cancel := flushContext(shutdownCtx)
			defer cancel()
			shutdownErr = errors.Join(tp.Shutdown(flushCtx), mp.Shutdown(flushCtx))
			retire()

			initMu.Lock()
			initialized = false
			initMu.Unlock()
		})
		return shutdownErr
	}, nil
}

// flushFloor bounds a flush whose caller left it unbounded, or handed over a
// context that was already cancelled or expired. Long enough for one OTLP attempt,
// short enough not to stall a pod past its termination grace period.
const flushFloor = 5 * time.Second

// flushContext derives the context a provider Shutdown runs under: the caller's
// deadline if it is still in the future, and never the caller's cancellation.
//
// Cancellation is dropped because TracerProvider.Shutdown marks itself shut down
// BEFORE flushing and returns at its first ctx.Done() check, leaving the batch
// processor unflushed and unstoppable. Callers pass the context SIGTERM just
// cancelled, so honoring it would lose every pending span on graceful shutdown.
// The deadline is kept, since that is how a caller bounds how long exit may stall.
func flushContext(ctx context.Context) (context.Context, context.CancelFunc) {
	uncancellable := context.WithoutCancel(ctx)
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) > 0 {
		return context.WithDeadline(uncancellable, dl)
	}
	return context.WithTimeout(uncancellable, flushFloor)
}

// retire puts the process back in its pre-Init state, so a lingering goroutine
// writes into a noop rather than a dead pipeline. Cached instruments follow on
// their own, because they compare the global provider on each use.
func retire() {
	otel.SetTracerProvider(tracenoop.NewTracerProvider())
	otel.SetMeterProvider(metricnoop.NewMeterProvider())
	ident.SetServiceName("")
}

// resolveRatio: config value if set, else OTEL_TRACES_SAMPLER_ARG, else 1.0.
//
// Only the _ARG variable is read, since the sampler is always
// ParentBased(TraceIDRatioBased(ratio)) — always_on and always_off are ratios of
// 1 and 0, and the parentbased_* variants are already the behaviour.
//
// A bad value is warned about and ignored. Silently falling back to 1.0 on a typo
// would turn a sampling config into full-volume export.
func resolveRatio(cfg Config) float64 {
	if cfg.SampleRatio != nil {
		return *cfg.SampleRatio
	}
	const v = "OTEL_TRACES_SAMPLER_ARG"
	arg := strings.TrimSpace(os.Getenv(v))
	if arg == "" {
		return 1.0
	}
	r, err := strconv.ParseFloat(arg, 64)
	switch {
	case err != nil:
		logging.Logger().Warn("telemetry: ignoring unparseable sampler argument; sampling every trace",
			"var", v, "value", arg, "err", err)
		return 1.0
	case math.IsNaN(r):
		// ParseFloat accepts "NaN", which compares false against every bound and
		// so would slip past the range check into TraceIDRatioBased.
		logging.Logger().Warn("telemetry: ignoring NaN sampler argument; sampling every trace",
			"var", v, "value", arg)
		return 1.0
	case r < 0 || r > 1:
		// TraceIDRatioBased clamps, so behaviour is fine; the config is not.
		logging.Logger().Warn("telemetry: sampler argument out of range, clamped to [0,1]",
			"var", v, "value", arg)
	}
	return r
}
