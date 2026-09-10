package publisher

import (
	"maps"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
)

// Config configures a Publisher. The zero value with only OperatorName set
// is a valid configuration that enables both default instrumentations
// (controller-runtime bridge and Go runtime).
type Config struct {
	// OperatorName is required. Used for the service.name resource attribute.
	OperatorName string

	// Version is optional. Used for service.version. Defaults to "unknown".
	Version string

	// OTLP, if non-nil, configures the OTLP exporter. If nil and
	// OTEL_EXPORTER_OTLP_ENDPOINT is set in env, an OTLP exporter is
	// constructed from env-derived defaults.
	OTLP *OTLPConfig

	// Stdout enables a stdout metric exporter for local development. Default false.
	Stdout bool

	// DisableControllerRuntimeBridge disables the bridge producer that feeds
	// controller-runtime metrics into OTLP. Zero value means bridge enabled.
	DisableControllerRuntimeBridge bool

	// DisableGoRuntime disables Go runtime instrumentation. Zero value means
	// runtime metrics enabled.
	DisableGoRuntime bool

	// Logger is optional. Defaults to logr.Discard().
	Logger logr.Logger
}

// OTLPConfig configures the OTLP exporter.
type OTLPConfig struct {
	// Endpoint is the collector endpoint, e.g. "otel-collector.observability.svc:4317".
	// Accepts host:port or scheme://host:port/path; passed through to the exporter.
	Endpoint string

	// Protocol is "grpc" (default) or "http/protobuf".
	Protocol string

	// Insecure disables TLS entirely (plaintext). Typical for in-cluster traffic.
	Insecure bool

	// Headers is an optional map of headers sent on each export, e.g. auth tokens.
	Headers map[string]string

	// Timeout is the per-export timeout. Defaults to 10s.
	Timeout time.Duration

	// Compression is "gzip" or "". Defaults to "gzip".
	Compression string

	// Interval is the periodic push interval. Defaults to 30s.
	Interval time.Duration
}

func applyDefaults(cfg *Config) {
	if cfg.Version == "" {
		cfg.Version = "unknown"
	}
	if cfg.OTLP != nil {
		applyOTLPDefaults(cfg.OTLP)
	}
}

func applyOTLPDefaults(o *OTLPConfig) {
	if o.Protocol == "" {
		o.Protocol = "grpc"
	}
	if o.Timeout == 0 {
		o.Timeout = 10 * time.Second
	}
	if o.Interval == 0 {
		o.Interval = 30 * time.Second
	}
	if o.Compression == "" {
		o.Compression = "gzip"
	}
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("OTEL_SERVICE_NAME"); v != "" {
		cfg.OperatorName = v
	}
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	protocol := os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	headers := os.Getenv("OTEL_EXPORTER_OTLP_HEADERS")

	if endpoint != "" || protocol != "" || headers != "" {
		if cfg.OTLP == nil {
			cfg.OTLP = &OTLPConfig{}
			applyOTLPDefaults(cfg.OTLP)
		}
		if endpoint != "" {
			cfg.OTLP.Endpoint = endpoint
		}
		if protocol != "" {
			cfg.OTLP.Protocol = protocol
		}
		if headers != "" {
			parsed := parseHeaders(headers)
			if cfg.OTLP.Headers == nil {
				cfg.OTLP.Headers = parsed
			} else {
				maps.Copy(cfg.OTLP.Headers, parsed)
			}
		}
	}
}

func parseHeaders(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			continue
		}
		out[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
	}
	return out
}
