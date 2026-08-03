package telemetry

import (
	"context"
	"net/url"
	"os"
	"strings"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/stakater/operator-utils/telemetry-web/logging"
)

// The OTLP transports this library can export over, spelled as the spec spells
// them in OTEL_EXPORTER_OTLP_PROTOCOL.
const (
	protoGRPC = "grpc"
	protoHTTP = "http/protobuf"
	protoJSON = "http/json"
)

// hasScheme reports whether the endpoint is URL-form (http://host:port) rather
// than bare host:port; the two need different exporter options. A non-empty
// scheme is not enough — "localhost:4317" parses with Scheme="localhost" — so
// the test is that the scheme is followed by "://".
func hasScheme(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && u.Scheme != "" && strings.HasPrefix(endpoint, u.Scheme+"://")
}

// resolveProtocol reports which transport to export one signal over. The
// per-signal variable wins over the generic one, per the OTel spec.
//
// An unset value resolves to gRPC, which deviates from the spec default of
// http/protobuf on purpose: gRPC has been this library's only transport, and
// silently moving every existing deployment from port 4317 to 4318 would break
// working pipelines. An explicit value is always honored, which is what matters
// in practice, since the OpenTelemetry Operator injects one.
func resolveProtocol(signalVar string) string {
	for _, name := range []string{signalVar, "OTEL_EXPORTER_OTLP_PROTOCOL"} {
		p := strings.TrimSpace(os.Getenv(name))
		if p == "" {
			continue
		}
		switch p {
		case protoGRPC, protoHTTP:
			return p
		case protoJSON:
			// The Go SDK ships no JSON encoder. A collector's OTLP/HTTP receiver
			// accepts protobuf on the same port, so this reaches the intended
			// endpoint rather than falling back to a different one.
			logging.Logger().Warn("telemetry: OTLP protocol http/json is not implemented by the Go SDK; "+
				"exporting http/protobuf to the same HTTP endpoint instead",
				"var", name, "value", p)
			return protoHTTP
		default:
			logging.Logger().Warn("telemetry: unrecognized OTLP protocol; exporting over gRPC",
				"var", name, "value", p)
			return protoGRPC
		}
	}
	return protoGRPC
}

// endpointOpts builds the endpoint and TLS options shared by every exporter
// variant. Each exporter package declares its own Option type, so the
// constructors come in as parameters instead of this block being written four
// times.
//
// Only Config.OTLPEndpoint is applied. When it is unset no endpoint option is
// passed at all, leaving the exporter SDK's own env handling in charge
// (OTEL_EXPORTER_OTLP_ENDPOINT, the per-signal overrides, and the default port).
// Reading those here and feeding them to WithEndpoint would break spec-compliant
// URL values.
func endpointOpts[O any](cfg Config, withEndpoint, withEndpointURL func(string) O, withInsecure func() O) []O {
	var opts []O
	if ep := cfg.OTLPEndpoint; ep != "" {
		if hasScheme(ep) {
			opts = append(opts, withEndpointURL(ep))
		} else {
			opts = append(opts, withEndpoint(ep))
		}
	}
	if cfg.Insecure {
		opts = append(opts, withInsecure())
	}
	return opts
}

// newSpanProcessor builds the OTLP trace exporter for the resolved protocol,
// behind a batch span processor.
func newSpanProcessor(ctx context.Context, cfg Config) (sdktrace.SpanProcessor, error) {
	var exp sdktrace.SpanExporter
	var err error
	switch resolveProtocol("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL") {
	case protoHTTP:
		exp, err = otlptracehttp.New(ctx, endpointOpts(cfg,
			otlptracehttp.WithEndpoint, otlptracehttp.WithEndpointURL, otlptracehttp.WithInsecure)...)
	default:
		exp, err = otlptracegrpc.New(ctx, endpointOpts(cfg,
			otlptracegrpc.WithEndpoint, otlptracegrpc.WithEndpointURL, otlptracegrpc.WithInsecure)...)
	}
	if err != nil {
		return nil, err
	}
	return sdktrace.NewBatchSpanProcessor(exp), nil
}

// newMetricReader builds the OTLP metric exporter for the resolved protocol,
// behind a periodic reader.
func newMetricReader(ctx context.Context, cfg Config) (sdkmetric.Reader, error) {
	var exp sdkmetric.Exporter
	var err error
	switch resolveProtocol("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL") {
	case protoHTTP:
		exp, err = otlpmetrichttp.New(ctx, endpointOpts(cfg,
			otlpmetrichttp.WithEndpoint, otlpmetrichttp.WithEndpointURL, otlpmetrichttp.WithInsecure)...)
	default:
		exp, err = otlpmetricgrpc.New(ctx, endpointOpts(cfg,
			otlpmetricgrpc.WithEndpoint, otlpmetricgrpc.WithEndpointURL, otlpmetricgrpc.WithInsecure)...)
	}
	if err != nil {
		return nil, err
	}
	return sdkmetric.NewPeriodicReader(exp), nil
}
