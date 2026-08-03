package telemetry

import (
	"context"
	"net/url"
	"os"
	"strings"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/stakater/operator-utils/telemetry-web/logging"
)

// hasScheme reports whether the endpoint is URL-form (http://host:port) rather
// than bare host:port; the two need different exporter options. A non-empty
// scheme is not enough — "localhost:4317" parses with Scheme="localhost" — so
// the test is that the scheme is followed by "://".
func hasScheme(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && u.Scheme != "" && strings.HasPrefix(endpoint, u.Scheme+"://")
}

// warnProtocol warns when the environment asks for a transport this library does
// not build; only OTLP/gRPC is supported. The per-signal variable wins over the
// generic one, per the OTel spec.
//
// It warns rather than failing Init on purpose: the OpenTelemetry Operator
// injects OTEL_EXPORTER_OTLP_PROTOCOL into pods, often as http/protobuf, and a
// library whose job is observability must not crash-loop what it observes.
// Telemetry degrades, the service keeps serving.
//
// Called once per signal per Init, and deliberately not deduplicated across
// signals — a sync.Once would hide the second one.
func warnProtocol(signalVar string) {
	for _, name := range []string{signalVar, "OTEL_EXPORTER_OTLP_PROTOCOL"} {
		p := strings.TrimSpace(os.Getenv(name))
		if p == "" {
			continue
		}
		if p != "grpc" {
			logging.Logger().Warn(
				"telemetry: requested OTLP protocol is not supported; this library exports OTLP/gRPC only. "+
					"Traces and metrics will not reach the collector until the collector exposes a gRPC (OTLP) receiver "+
					"and this variable is set to grpc or removed.",
				"var", name, "value", p)
		}
		return
	}
}

// newSpanProcessor builds the OTLP/gRPC trace exporter behind a batch span
// processor.
//
// Only Config.OTLPEndpoint is applied here. When it is unset no endpoint option
// is passed at all, leaving the exporter SDK's own env handling in charge
// (OTEL_EXPORTER_OTLP_ENDPOINT, the per-signal overrides, and the
// localhost:4317 default). Reading those here and feeding them to WithEndpoint
// would break spec-compliant URL values.
func newSpanProcessor(ctx context.Context, cfg Config) (sdktrace.SpanProcessor, error) {
	warnProtocol("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")
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

// newMetricReader builds the OTLP/gRPC metric exporter behind a periodic
// reader. Endpoint resolution matches newSpanProcessor.
func newMetricReader(ctx context.Context, cfg Config) (sdkmetric.Reader, error) {
	warnProtocol("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")
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
