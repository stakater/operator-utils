// Package publisher constructs and owns an OpenTelemetry MeterProvider for
// Kubernetes operators. Custom and Go-runtime metrics are exported via
// OTLP only; controller-runtime's existing Prometheus /metrics endpoint
// is left untouched. A Prometheus bridge producer optionally reads from
// controller-runtime's registry and feeds the same metrics into OTLP.
//
// Typical usage in an operator's main.go:
//
//	pub, err := publisher.New(ctx, publisher.Config{
//	    OperatorName: "my-operator",
//	    OTLP:         &publisher.OTLPConfig{Endpoint: "collector:4317", Insecure: true},
//	})
//	if err != nil { ... }
//	defer pub.Shutdown(context.Background())
package publisher

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/stakater/operator-utils/observability/pkg/bridge"
	"github.com/stakater/operator-utils/observability/pkg/resource"
)

// Publisher owns the MeterProvider, custom metrics registry, and any
// active periodic readers. It is constructed once at operator startup
// and shut down once at operator exit.
type Publisher struct {
	cfg      Config
	provider *sdkmetric.MeterProvider
	meter    metric.Meter
	custom   *CustomMetrics
	log      logr.Logger
}

// New constructs the publisher. It is safe to call once per process. It
// does not block on network I/O; OTLP graceful degradation is implicit
// because the exporter lazily dials on first export.
//
// As a side effect, calls otel.SetMeterProvider so package-level
// instruments (via otel.GetMeterProvider) use this provider.
func New(ctx context.Context, cfg Config) (*Publisher, error) {
	if cfg.OperatorName == "" {
		return nil, fmt.Errorf("OperatorName is required")
	}

	applyDefaults(&cfg)
	applyEnvOverrides(&cfg)
	// Re-apply OTLP defaults in case env materialized a fresh OTLP block.
	// applyEnvOverrides already calls applyOTLPDefaults in that path; this
	// call is a no-op safety net.
	if cfg.OTLP != nil {
		applyOTLPDefaults(cfg.OTLP)
	}

	log := namedLogger(cfg.Logger)

	res, err := resource.Build(ctx, cfg.OperatorName, cfg.Version)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	var readers []sdkmetric.Reader

	if cfg.OTLP != nil {
		reader, err := buildOTLPReader(ctx, cfg)
		if err != nil {
			log.Info("OTLP exporter construction failed; continuing without OTLP",
				"err", err.Error(), "endpoint", cfg.OTLP.Endpoint)
		} else {
			readers = append(readers, reader)
		}
	}

	if cfg.Stdout {
		exp, err := stdoutmetric.New()
		if err != nil {
			log.Info("stdout exporter construction failed; continuing without stdout",
				"err", err.Error())
		} else {
			readers = append(readers, sdkmetric.NewPeriodicReader(exp))
		}
	}

	if len(readers) == 0 {
		log.Info("no metric readers configured; custom metrics will not be exported")
	}

	opts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	for _, r := range readers {
		opts = append(opts, sdkmetric.WithReader(r))
	}
	provider := sdkmetric.NewMeterProvider(opts...)
	otel.SetMeterProvider(provider)

	meter := provider.Meter("github.com/stakater/operator-utils/observability/pkg/publisher")
	custom := &CustomMetrics{meter: meter}

	if !cfg.DisableGoRuntime {
		if err := bridge.StartGoRuntime(); err != nil {
			log.Info("Go runtime instrumentation failed to start", "err", err.Error())
		}
	}

	return &Publisher{
		cfg:      cfg,
		provider: provider,
		meter:    meter,
		custom:   custom,
		log:      log,
	}, nil
}

// Custom returns the CustomMetrics registry for operator-defined metrics.
func (p *Publisher) Custom() *CustomMetrics { return p.custom }

// Meter returns the underlying OTel Meter. Provided as an escape hatch for
// advanced users who need direct SDK access; most consumers should use Custom().
func (p *Publisher) Meter() metric.Meter { return p.meter }

// Shutdown flushes pending metrics and releases resources. It honors ctx
// cancellation and always returns nil: errors from the final flush (e.g.
// collector unreachable, or a double-shutdown on the provider) are logged
// at info level and swallowed, consistent with the module's
// graceful-degradation contract.
func (p *Publisher) Shutdown(ctx context.Context) error {
	if p.provider == nil {
		return nil
	}
	if err := p.provider.Shutdown(ctx); err != nil {
		p.log.Info("metric flush on shutdown encountered errors (non-fatal)", "err", err.Error())
	}
	return nil
}

func namedLogger(l logr.Logger) logr.Logger {
	if l.GetSink() == nil {
		return logr.Discard()
	}
	return l.WithName("observability")
}
