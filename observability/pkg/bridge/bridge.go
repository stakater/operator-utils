// Package bridge exposes the OTel-side wiring helpers used by the
// observability module: a Prometheus bridge producer that reads from
// controller-runtime's existing prometheus.Registry, and a starter for
// Go runtime instrumentation via otel/contrib.
//
// Both helpers are public so callers wiring their own MeterProvider can
// adopt the module's controller-runtime + Go runtime instrumentation
// without depending on the full publisher.
package bridge

import (
	"fmt"

	bridgeprom "go.opentelemetry.io/contrib/bridges/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/sdk/metric"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// ControllerRuntimeProducer returns a Prometheus bridge Producer that
// reads from controller-runtime's existing prometheus.Registry. Attach
// it to an OTLP PeriodicReader via sdkmetric.WithProducer to push
// controller-runtime metrics through OTLP without touching the registry.
func ControllerRuntimeProducer() metric.Producer {
	return bridgeprom.NewMetricProducer(bridgeprom.WithGatherer(ctrlmetrics.Registry))
}

// StartGoRuntime starts Go runtime metric collection via otel/contrib.
// Must be called AFTER otel.SetMeterProvider so the runtime
// instrumentation finds the configured provider.
func StartGoRuntime() error {
	if err := runtime.Start(); err != nil {
		return fmt.Errorf("start runtime instrumentation: %w", err)
	}
	return nil
}
