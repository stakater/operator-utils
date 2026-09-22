package publisher

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

func TestApplyDefaults_ZeroValueGetsDefaults(t *testing.T) {
	cfg := Config{OperatorName: "my-op"}
	applyDefaults(&cfg)

	if cfg.Version != "unknown" {
		t.Errorf("Version = %q, want %q", cfg.Version, "unknown")
	}
	if cfg.DisableGoRuntime {
		t.Errorf("DisableGoRuntime = true, want false")
	}
	if cfg.Prometheus == nil {
		t.Fatal("Prometheus = nil, want a default config: the reader is on by default")
	}
	if cfg.Prometheus.Registerer != ctrlmetrics.Registry {
		t.Errorf("Registerer = %v, want controller-runtime's registry", cfg.Prometheus.Registerer)
	}
}

// A disabled reader must not be materialised into a default config, since
// New() keys the bridge decision off whether one was built.
func TestApplyDefaults_DisabledPrometheusStaysNil(t *testing.T) {
	cfg := Config{OperatorName: "my-op", DisablePrometheus: true}
	applyDefaults(&cfg)

	if cfg.Prometheus != nil {
		t.Errorf("Prometheus = %+v, want nil", cfg.Prometheus)
	}
}

// An explicit registry must survive defaulting, otherwise an operator
// cannot serve metrics anywhere but controller-runtime's endpoint.
func TestApplyDefaults_KeepsExplicitRegisterer(t *testing.T) {
	reg := prometheus.NewRegistry()
	cfg := Config{OperatorName: "my-op", Prometheus: &PrometheusConfig{Registerer: reg}}
	applyDefaults(&cfg)

	if cfg.Prometheus.Registerer != reg {
		t.Errorf("Registerer was overwritten by the default")
	}
}

func TestApplyDefaults_RespectsExplicitDisables(t *testing.T) {
	cfg := Config{
		OperatorName:     "my-op",
		DisableGoRuntime: true,
	}
	applyDefaults(&cfg)

	if !cfg.DisableGoRuntime {
		t.Errorf("DisableGoRuntime flipped back to false")
	}
}

func TestApplyDefaults_OTLPSubconfig(t *testing.T) {
	cfg := Config{
		OperatorName: "my-op",
		OTLP:         &OTLPConfig{Endpoint: "collector:4317"},
	}
	applyDefaults(&cfg)

	if cfg.OTLP.Protocol != "grpc" {
		t.Errorf("Protocol = %q, want %q", cfg.OTLP.Protocol, "grpc")
	}
	if cfg.OTLP.Timeout != 10*time.Second {
		t.Errorf("Timeout = %v, want %v", cfg.OTLP.Timeout, 10*time.Second)
	}
	if cfg.OTLP.Interval != 30*time.Second {
		t.Errorf("Interval = %v, want %v", cfg.OTLP.Interval, 30*time.Second)
	}
	if cfg.OTLP.Compression != "gzip" {
		t.Errorf("Compression = %q, want %q", cfg.OTLP.Compression, "gzip")
	}
}

func TestApplyEnvOverrides_ServiceName(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "from-env")
	cfg := Config{OperatorName: "from-code"}
	applyEnvOverrides(&cfg)
	if cfg.OperatorName != "from-env" {
		t.Errorf("OperatorName = %q, want %q", cfg.OperatorName, "from-env")
	}
	if cfg.OTLP != nil {
		t.Errorf("OTLP materialized unexpectedly when only OTEL_SERVICE_NAME is set: %+v", cfg.OTLP)
	}
}

func TestApplyEnvOverrides_OTLPEndpointCreatesConfig(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "envcollector:4317")
	cfg := Config{OperatorName: "op"}
	applyEnvOverrides(&cfg)
	if cfg.OTLP == nil {
		t.Fatal("OTLP is nil; expected env to materialize a config")
	}
	if cfg.OTLP.Endpoint != "envcollector:4317" {
		t.Errorf("Endpoint = %q, want %q", cfg.OTLP.Endpoint, "envcollector:4317")
	}
	if cfg.OTLP.Protocol != "grpc" {
		t.Errorf("env-materialized Protocol = %q, want default %q", cfg.OTLP.Protocol, "grpc")
	}
	if cfg.OTLP.Timeout != 10*time.Second {
		t.Errorf("env-materialized Timeout = %v, want default 10s", cfg.OTLP.Timeout)
	}
}

func TestApplyEnvOverrides_OTLPProtocolOverridesStruct(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	cfg := Config{
		OperatorName: "op",
		OTLP:         &OTLPConfig{Endpoint: "x:4317", Protocol: "grpc"},
	}
	applyEnvOverrides(&cfg)
	if cfg.OTLP.Protocol != "http/protobuf" {
		t.Errorf("Protocol = %q, want %q", cfg.OTLP.Protocol, "http/protobuf")
	}
}

func TestApplyEnvOverrides_Headers(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer xyz,X-Tenant=acme")
	cfg := Config{
		OperatorName: "op",
		OTLP:         &OTLPConfig{Endpoint: "x:4317"},
	}
	applyEnvOverrides(&cfg)
	if got := cfg.OTLP.Headers["Authorization"]; got != "Bearer xyz" {
		t.Errorf("Headers[Authorization] = %q, want %q", got, "Bearer xyz")
	}
	if got := cfg.OTLP.Headers["X-Tenant"]; got != "acme" {
		t.Errorf("Headers[X-Tenant] = %q, want %q", got, "acme")
	}
}
