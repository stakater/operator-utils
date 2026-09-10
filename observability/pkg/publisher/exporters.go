package publisher

import (
	"context"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/stakater/operator-utils/observability/pkg/bridge"
)

func buildOTLPReader(ctx context.Context, cfg Config) (sdkmetric.Reader, error) {
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
	}
	if !cfg.DisableControllerRuntimeBridge {
		readerOpts = append(readerOpts, sdkmetric.WithProducer(bridge.ControllerRuntimeProducer()))
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
