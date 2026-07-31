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
// than bare host:port. The two need different exporter options.
// A bare "localhost:4317" parses with Scheme="localhost", so a non-empty scheme
// is not enough. The exact test is whether the scheme is followed by "://",
// which also rejects a separator buried later ("host:4317/x://y") and correctly
// classifies "unix:///path" as URL-form so the exporter reports its own error
// instead of silently receiving it as a host.
func hasScheme(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && u.Scheme != "" && strings.HasPrefix(endpoint, u.Scheme+"://")
}

// warnProtocol logs once per signal when the environment asks for a transport
// this library does not build. Only OTLP/gRPC is supported.
//
// This warns rather than failing Init on purpose. The OpenTelemetry Operator's
// auto-instrumentation injects OTEL_EXPORTER_OTLP_PROTOCOL into pods, often as
// http/protobuf, so hard-failing would let an unrelated pod-spec change
// crash-loop a service that was previously healthy. A library whose job is
// observability must not be able to take down what it observes: telemetry
// degrades, the service keeps serving, and the warning names the cause.
//
// The per-signal variable wins over the generic one, matching the OTel spec.
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

// newSpanProcessor builds the OTLP/gRPC trace exporter and wraps it in a batch
// span processor.
//
// Endpoint resolution: only Config.OTLPEndpoint is applied here — as
// WithEndpointURL for URL-form values, WithEndpoint for bare host:port. When
// unset, NO endpoint option is passed, so the exporter SDK's own env handling
// applies: OTEL_EXPORTER_OTLP_ENDPOINT (URL-form per spec), the per-signal
// OTEL_EXPORTER_OTLP_{TRACES,METRICS}_ENDPOINT overrides, and the
// localhost:4317 default. Re-reading the env here and feeding it to
// WithEndpoint would break spec-compliant URL values.
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
