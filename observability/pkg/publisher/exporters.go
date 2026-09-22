package publisher

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/otlptranslator"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	prometheusexporter "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/stakater/operator-utils/observability/pkg/bridge"
)

// ctrlRegistryClaimed enforces one Prometheus reader per process on
// controller-runtime's registry.
//
// The OTel exporter's collector is unchecked: its Describe emits no Desc,
// so prometheus.Registry.Register files it under uncheckedCollectors and
// returns nil however many times it is called. A second reader therefore
// registers "successfully" and only surfaces later, as a duplicate
// target_info that fails every Gather and turns /metrics into a 500 —
// controller-runtime's own metrics included.
//
// Only the default registry is guarded. A caller who hands the same
// custom registry to two publishers hits the same trap unguarded; see
// PrometheusConfig.Registerer.
var ctrlRegistryClaimed atomic.Bool

func buildOTLPReader(ctx context.Context, cfg Config, g prometheus.Gatherer) (sdkmetric.Reader, error) {
	var exp sdkmetric.Exporter
	var err error
	switch cfg.OTLP.Protocol {
	case "http/protobuf":
		exp, err = newOTLPHTTP(ctx, cfg.OTLP)
	default:
		exp, err = newOTLPGRPC(ctx, cfg.OTLP)
	}
	if err != nil {
		return nil, err
	}

	readerOpts := []sdkmetric.PeriodicReaderOption{
		sdkmetric.WithInterval(cfg.OTLP.Interval),
		sdkmetric.WithTimeout(cfg.OTLP.Timeout),
		sdkmetric.WithProducer(bridge.ProducerFor(g)),
	}
	return sdkmetric.NewPeriodicReader(exp, readerOpts...), nil
}

func newOTLPGRPC(ctx context.Context, o *OTLPConfig) (sdkmetric.Exporter, error) {
	opts := []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithEndpoint(o.Endpoint),
		otlpmetricgrpc.WithTimeout(o.Timeout),
	}
	if o.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	if o.Compression == "gzip" {
		opts = append(opts, otlpmetricgrpc.WithCompressor("gzip"))
	}
	if len(o.Headers) > 0 {
		opts = append(opts, otlpmetricgrpc.WithHeaders(o.Headers))
	}
	return otlpmetricgrpc.New(ctx, opts...)
}

func newOTLPHTTP(ctx context.Context, o *OTLPConfig) (sdkmetric.Exporter, error) {
	opts := []otlpmetrichttp.Option{
		otlpmetrichttp.WithEndpoint(o.Endpoint),
		otlpmetrichttp.WithTimeout(o.Timeout),
	}
	if o.Insecure {
		opts = append(opts, otlpmetrichttp.WithInsecure())
	}
	if o.Compression == "gzip" {
		opts = append(opts, otlpmetrichttp.WithCompression(otlpmetrichttp.GzipCompression))
	}
	if len(o.Headers) > 0 {
		opts = append(opts, otlpmetrichttp.WithHeaders(o.Headers))
	}
	return otlpmetrichttp.New(ctx, opts...)
}

// buildPrometheusReader returns a reader that exposes SDK metrics on the
// configured Prometheus registry.
//
// UnderscoreEscapingWithSuffixes is the full Prometheus-style translation:
// dotted OTel names are escaped and unit and counter suffixes appended.
// The _total suffix is idempotent, so a counter registered as either
// "reconcile" or "reconcile_total" is exposed as reconcile_total.
//
// Scope labels are always suppressed: otel_scope_name / otel_scope_version
// would otherwise land on every series without telling an operator
// anything it does not already know.
//
// The returned capturingRegisterer names the families this module wrote,
// so the OTLP bridge can skip them.
func buildPrometheusReader(cfg Config) (sdkmetric.Reader, *capturingRegisterer, error) {
	p := cfg.Prometheus
	// Comparing against a *prometheus.Registry never panics: an interface
	// holding an incomparable type has a different dynamic type, so the
	// comparison is simply false.
	onCtrlRegistry := p.Registerer == prometheus.Registerer(ctrlmetrics.Registry)
	if onCtrlRegistry && !ctrlRegistryClaimed.CompareAndSwap(false, true) {
		return nil, nil, fmt.Errorf("a Prometheus reader is already registered on controller-runtime's registry: call publisher.New once per process")
	}

	capreg := &capturingRegisterer{Registerer: p.Registerer}
	opts := []prometheusexporter.Option{
		prometheusexporter.WithRegisterer(capreg),
		prometheusexporter.WithoutScopeInfo(),
		prometheusexporter.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithSuffixes),
	}
	if p.DisableTargetInfo {
		opts = append(opts, prometheusexporter.WithoutTargetInfo())
	}
	reader, err := prometheusexporter.New(opts...)
	if err != nil {
		if onCtrlRegistry {
			ctrlRegistryClaimed.Store(false)
		}
		return nil, nil, err
	}
	return reader, capreg, nil
}
