package publisher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	otelmetric "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

func newCounterIn(t *testing.T, reg prometheus.Registerer, name string) {
	t.Helper()
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: "h"})
	c.Inc()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
}

func namesFrom(t *testing.T, g prometheus.Gatherer) []string {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	names := make([]string, 0, len(families))
	for _, f := range families {
		names = append(names, f.GetName())
	}
	return names
}

// The bridge must see controller-runtime's families but not the ones this
// module wrote into the same registry.
func TestExceptGatherer_DropsOwnFamilies(t *testing.T) {
	base := prometheus.NewRegistry()
	own := prometheus.NewRegistry()

	newCounterIn(t, base, "controller_runtime_reconcile_total")
	// Same family present in both, standing in for a metric we exported.
	shared := prometheus.NewCounter(prometheus.CounterOpts{Name: "reconcile_total", Help: "h"})
	shared.Inc()
	if err := base.Register(shared); err != nil {
		t.Fatal(err)
	}
	if err := own.Register(shared); err != nil {
		t.Fatalf("a collector must be registerable in two registries: %v", err)
	}

	got := namesFrom(t, exceptGatherer{base: base, own: own})

	if hasName(got, "reconcile_total") {
		t.Errorf("own family leaked through: %v", got)
	}
	if !hasName(got, "controller_runtime_reconcile_total") {
		t.Errorf("controller-runtime family was dropped: %v", got)
	}
}

func TestExceptGatherer_EmptyOwnPassesEverything(t *testing.T) {
	base := prometheus.NewRegistry()
	newCounterIn(t, base, "controller_runtime_reconcile_total")

	got := namesFrom(t, exceptGatherer{base: base, own: prometheus.NewRegistry()})

	if len(got) != 1 || got[0] != "controller_runtime_reconcile_total" {
		t.Fatalf("want the base family untouched, got %v", got)
	}
}

// The wrapper must register for real, not just observe.
func TestCapturingRegisterer_ForwardsAndCaptures(t *testing.T) {
	reg := prometheus.NewRegistry()
	cap := &capturingRegisterer{Registerer: reg}

	newCounterIn(t, cap, "reconcile_total")

	if !hasName(namesFrom(t, reg), "reconcile_total") {
		t.Error("Register did not reach the wrapped registry")
	}
	if len(cap.captured) != 1 {
		t.Fatalf("captured %d collectors, want 1", len(cap.captured))
	}
}

// A failed registration must not be recorded as captured, or the bridge
// would exclude families that were never written.
func TestCapturingRegisterer_DoesNotCaptureOnError(t *testing.T) {
	reg := prometheus.NewRegistry()
	cap := &capturingRegisterer{Registerer: reg}

	newCounterIn(t, cap, "reconcile_total")
	dup := prometheus.NewCounter(prometheus.CounterOpts{Name: "reconcile_total", Help: "h"})
	if err := cap.Register(dup); err == nil {
		t.Fatal("expected duplicate registration to fail")
	}

	if len(cap.captured) != 1 {
		t.Fatalf("captured %d collectors, want 1", len(cap.captured))
	}
}

// Both transports on: the custom metric must reach OTLP once, natively,
// with the operator's own scope, and controller-runtime's must reach it
// once via the bridge.
func TestBothTransports_EachMetricExactlyOnce(t *testing.T) {
	reg := prometheus.NewRegistry()
	newCounterIn(t, reg, "controller_runtime_reconcile_total")

	cfg := Config{
		OperatorName:     "my-operator",
		DisableGoRuntime: true,
		Prometheus:       &PrometheusConfig{Registerer: reg},
		OTLP: &OTLPConfig{Endpoint: "localhost:1", Insecure: true,
			Timeout: time.Second, Interval: time.Hour},
	}
	applyDefaults(&cfg)

	promReader, capreg, err := buildPrometheusReader(cfg)
	if err != nil {
		t.Fatalf("buildPrometheusReader: %v", err)
	}
	own := prometheus.NewRegistry()
	for _, c := range capreg.captured {
		if err := own.Register(c); err != nil {
			t.Fatalf("own registry: %v", err)
		}
	}

	otlpReader, err := buildOTLPReader(context.Background(), cfg,
		exceptGatherer{base: reg, own: own})
	if err != nil {
		t.Fatalf("buildOTLPReader: %v", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(promReader), sdkmetric.WithReader(otlpReader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	c, err := mp.Meter("my-operator").Int64Counter("reconcile_total")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	c.Add(context.Background(), 7)

	var rm metricdata.ResourceMetrics
	if err := otlpReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	counts := map[string]int{}
	scopes := map[string]string{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			counts[m.Name]++
			scopes[m.Name] = sm.Scope.Name
		}
	}

	if counts["reconcile_total"] != 1 {
		t.Errorf("reconcile_total appeared %d times in OTLP, want 1", counts["reconcile_total"])
	}
	if scopes["reconcile_total"] != "my-operator" {
		t.Errorf("reconcile_total scope = %q, want the operator's own scope (it must take the native path)", scopes["reconcile_total"])
	}
	if counts["controller_runtime_reconcile_total"] != 1 {
		t.Errorf("controller_runtime_reconcile_total appeared %d times in OTLP, want 1",
			counts["controller_runtime_reconcile_total"])
	}
	if counts["target_info"] != 0 {
		t.Errorf("target_info reached OTLP as a metric; it is ours and should be excluded")
	}
}

// A View must not resurrect a second copy on the OTLP path. This is the
// failure mode that ruled out reader-level AggregationDrop.
func TestBothTransports_ViewedHistogramNotDuplicated(t *testing.T) {
	reg := prometheus.NewRegistry()

	cfg := Config{
		OperatorName:     "my-operator",
		DisableGoRuntime: true,
		Prometheus:       &PrometheusConfig{Registerer: reg},
		OTLP: &OTLPConfig{Endpoint: "localhost:1", Insecure: true,
			Timeout: time.Second, Interval: time.Hour},
	}
	applyDefaults(&cfg)

	promReader, capreg, err := buildPrometheusReader(cfg)
	if err != nil {
		t.Fatalf("buildPrometheusReader: %v", err)
	}
	own := prometheus.NewRegistry()
	for _, c := range capreg.captured {
		if err := own.Register(c); err != nil {
			t.Fatalf("own registry: %v", err)
		}
	}
	otlpReader, err := buildOTLPReader(context.Background(), cfg,
		exceptGatherer{base: reg, own: own})
	if err != nil {
		t.Fatalf("buildOTLPReader: %v", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(promReader), sdkmetric.WithReader(otlpReader),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "latency"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationBase2ExponentialHistogram{
				MaxSize: 160, MaxScale: 20}})))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	h, err := mp.Meter("my-operator").Float64Histogram("latency", otelmetric.WithUnit("s"))
	if err != nil {
		t.Fatalf("Float64Histogram: %v", err)
	}
	h.Record(context.Background(), 0.03)

	var rm metricdata.ResourceMetrics
	if err := otlpReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	var total int
	var exponential bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "latency" || m.Name == "latency_seconds" {
				total++
				if _, ok := m.Data.(metricdata.ExponentialHistogram[float64]); ok {
					exponential = true
				}
			}
		}
	}
	if total != 1 {
		t.Errorf("the viewed histogram appeared %d times in OTLP, want 1", total)
	}
	if !exponential {
		t.Error("the viewed histogram lost its exponential aggregation, so it did not take the native path")
	}
}

func TestCapturingRegisterer_MustRegisterAlsoCaptures(t *testing.T) {
	reg := prometheus.NewRegistry()
	cap := &capturingRegisterer{Registerer: reg}

	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "reconcile_total", Help: "h"})
	c.Inc()
	cap.MustRegister(c)

	if !hasName(namesFrom(t, reg), "reconcile_total") {
		t.Error("MustRegister did not reach the wrapped registry")
	}
	if len(cap.captured) != 1 {
		t.Fatalf("captured %d collectors, want 1: the embedded MustRegister bypassed capture", len(cap.captured))
	}
}

type failingGatherer struct{ err error }

func (f failingGatherer) Gather() ([]*dto.MetricFamily, error) { return nil, f.err }

func TestExceptGatherer_PropagatesGatherErrors(t *testing.T) {
	boom := errors.New("boom")
	ok := prometheus.NewRegistry()

	if _, err := (exceptGatherer{base: failingGatherer{boom}, own: ok}).Gather(); !errors.Is(err, boom) {
		t.Errorf("base error not propagated, got %v", err)
	}
	if _, err := (exceptGatherer{base: ok, own: failingGatherer{boom}}).Gather(); !errors.Is(err, boom) {
		t.Errorf("own error not propagated, got %v", err)
	}
}

// The three branches New relies on to decide what the bridge reads:
// no Prometheus reader running, mirroring the captured collectors
// succeeded, and mirroring failed.
func TestBridgeGathererFor(t *testing.T) {
	dupCounter := func() prometheus.Collector {
		return prometheus.NewCounter(prometheus.CounterOpts{Name: "reconcile_total", Help: "h"})
	}

	cases := []struct {
		name   string
		capreg *capturingRegisterer
		check  func(t *testing.T, g prometheus.Gatherer)
	}{
		{
			name:   "no Prometheus reader: bridge reads the registry unfiltered",
			capreg: nil,
			check: func(t *testing.T, g prometheus.Gatherer) {
				if g != prometheus.Gatherer(ctrlmetrics.Registry) {
					t.Errorf("got %v, want ctrlmetrics.Registry", g)
				}
			},
		},
		{
			name:   "mirroring succeeds: bridge excludes our families",
			capreg: &capturingRegisterer{captured: []prometheus.Collector{dupCounter()}},
			check: func(t *testing.T, g prometheus.Gatherer) {
				if g == nil {
					t.Fatal("got nil, want a non-nil exceptGatherer")
				}
				if _, ok := g.(exceptGatherer); !ok {
					t.Fatalf("got %T, want exceptGatherer", g)
				}
			},
		},
		{
			// Two collectors describing the same metric: the first
			// Register into the fresh mirror registry succeeds, the
			// second fails as a duplicate.
			name: "mirroring fails: bridge is skipped entirely",
			capreg: &capturingRegisterer{captured: []prometheus.Collector{
				dupCounter(), dupCounter(),
			}},
			check: func(t *testing.T, g prometheus.Gatherer) {
				if g != nil {
					t.Errorf("got %v, want nil", g)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bridgeGathererFor(tc.capreg, logr.Discard())
			tc.check(t, got)
		})
	}
}
