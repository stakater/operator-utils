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

	"github.com/prometheus/client_golang/prometheus"
	bridgeprom "go.opentelemetry.io/contrib/bridges/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/sdk/metric"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// ProducerFor returns a Prometheus bridge Producer that reads from g. Use
// it when the metrics to bridge are not simply everything in
// controller-runtime's registry, for example when SDK metrics share that
// registry and must be filtered out.
func ProducerFor(g prometheus.Gatherer) metric.Producer {
	return bridgeprom.NewMetricProducer(bridgeprom.WithGatherer(g))
}

// ControllerRuntimeProducer returns a Prometheus bridge Producer that
// reads from controller-runtime's existing prometheus.Registry. Attach it
// to a Reader via sdkmetric.WithProducer to pull controller-runtime
// metrics into OTel without writing to the registry.
func ControllerRuntimeProducer() metric.Producer {
	return ProducerFor(ctrlmetrics.Registry)
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
