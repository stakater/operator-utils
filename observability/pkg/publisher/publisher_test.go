package publisher

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

func TestNew_EmptyOperatorNameFails(t *testing.T) {
	_, err := New(context.Background(), Config{})
	if err == nil {
		t.Fatal("expected error for empty OperatorName")
	}
}

func TestNew_NoReadersSucceedsWithWarn(t *testing.T) {
	// No OTLP, no Stdout, no env vars, and the default Prometheus reader
	// turned off, so the provider genuinely has nothing attached.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "")

	p, err := New(context.Background(), Config{OperatorName: "op", DisablePrometheus: true})
	if err != nil {
		t.Fatalf("expected success with no readers, got %v", err)
	}
	defer p.Shutdown(context.Background())

	if p.Custom() == nil {
		t.Fatal("Custom() returned nil")
	}
}

// Construction must not block on the network even when the OTLP endpoint
// is unreachable. Real-export-time graceful degradation happens inside
// the PeriodicReader's background goroutine and is not exercised here.
func TestNew_UnreachableOTLPDoesNotBlockConstruction(t *testing.T) {
	cfg := Config{
		OperatorName:      "op",
		DisablePrometheus: true,
		OTLP: &OTLPConfig{
			Endpoint: "localhost:1", // guaranteed-unused
			Insecure: true,
			Timeout:  100 * time.Millisecond,
			Interval: time.Second,
		},
	}
	p, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("expected graceful degradation, got error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestNew_EnvOnlyOTLPEnablement(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:1")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")

	p, err := New(context.Background(), Config{OperatorName: "op", DisablePrometheus: true})
	if err != nil {
		t.Fatalf("expected env-only OTLP to construct, got error: %v", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	defer p.Shutdown(shutdownCtx)
}

func TestNew_CustomMetricSurvivesShutdown(t *testing.T) {
	p, err := New(context.Background(), Config{
		OperatorName:      "op",
		Stdout:            true,
		DisablePrometheus: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctr, err := p.Custom().Counter("things_total", "counts things")
	if err != nil {
		t.Fatalf("Counter: %v", err)
	}
	ctr.Inc(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// Every other New test disables Prometheus, so none of them exercise the
// branches inside New that build bridgeGatherer when the Prometheus reader
// is actually on alongside OTLP. This is a construction smoke test: it only
// proves /metrics still carries the counter once when both transports are
// wired up. It does not inspect OTLP; TestBuildReaders_BridgeExcludesOwnFamilies
// covers the dedup guarantee end to end.
func TestNew_BothTransportsConstructWithPrometheusUnaffected(t *testing.T) {
	reg := prometheus.NewRegistry()
	cfg := Config{
		OperatorName:     "op",
		DisableGoRuntime: true,
		Prometheus:       &PrometheusConfig{Registerer: reg},
		OTLP: &OTLPConfig{
			Endpoint: "localhost:1",
			Insecure: true,
			Timeout:  100 * time.Millisecond,
			Interval: time.Hour,
		},
	}
	p, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Shutdown(context.Background())

	ctr, err := p.Custom().Counter("reconcile_total", "total reconciliations")
	if err != nil {
		t.Fatalf("Counter: %v", err)
	}
	ctr.Inc(context.Background())

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var series int
	for _, f := range families {
		if f.GetName() == "reconcile_total" {
			series += len(f.GetMetric())
		}
	}
	if series != 1 {
		t.Fatalf("reconcile_total has %d series on /metrics, want exactly 1", series)
	}
}

// buildReaders is the line New relies on to keep OTLP from re-exporting
// what the Prometheus reader already wrote. This test drives buildReaders
// directly, the way New does, and Collects from the OTLP reader it returns
// rather than hand-building exceptGatherer as the dedup tests do. bridgeGathererFor
// hardcodes controller-runtime's registry as the base it dedupes against,
// so this test uses that same registry as the Prometheus target, with a
// stand-in controller-runtime metric registered and cleaned up alongside it.
func TestBuildReaders_BridgeExcludesOwnFamilies(t *testing.T) {
	ctrlCounter := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "buildreaders_test_ctrl_total", Help: "stand-in for a controller-runtime metric",
	})
	ctrlCounter.Inc()
	if err := ctrlmetrics.Registry.Register(ctrlCounter); err != nil {
		t.Fatalf("register stand-in controller-runtime metric: %v", err)
	}
	t.Cleanup(func() { ctrlmetrics.Registry.Unregister(ctrlCounter) })

	cfg := Config{
		OperatorName: "op",
		Prometheus:   &PrometheusConfig{Registerer: ctrlmetrics.Registry},
		OTLP: &OTLPConfig{
			Endpoint: "localhost:1", Insecure: true,
			Timeout: time.Second, Interval: time.Hour,
		},
	}
	applyDefaults(&cfg)

	rs := buildReaders(context.Background(), cfg, logr.Discard())
	if rs.prometheus == nil {
		t.Fatal("buildReaders did not build a Prometheus reader")
	}
	if rs.otlp == nil {
		t.Fatal("buildReaders did not build an OTLP reader")
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(rs.prometheus), sdkmetric.WithReader(rs.otlp))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	c, err := mp.Meter("op").Int64Counter("reconcile_total")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	c.Add(context.Background(), 1)

	var rm metricdata.ResourceMetrics
	if err := rs.otlp.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	counts := map[string]int{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			counts[m.Name]++
		}
	}
	if counts["reconcile_total"] != 1 {
		t.Errorf("reconcile_total appeared %d times in OTLP, want exactly 1", counts["reconcile_total"])
	}
	if counts["buildreaders_test_ctrl_total"] != 1 {
		t.Errorf("controller-runtime stand-in appeared %d times in OTLP, want exactly 1",
			counts["buildreaders_test_ctrl_total"])
	}
}
