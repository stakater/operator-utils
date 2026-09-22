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
	capreg := &capturingRegisterer{Registerer: reg}

	newCounterIn(t, capreg, "reconcile_total")

	if !hasName(namesFrom(t, reg), "reconcile_total") {
		t.Error("Register did not reach the wrapped registry")
	}
	if len(capreg.captured) != 1 {
		t.Fatalf("captured %d collectors, want 1", len(capreg.captured))
	}
}

// A failed registration must not be recorded as captured, or the bridge
// would exclude families that were never written.
func TestCapturingRegisterer_DoesNotCaptureOnError(t *testing.T) {
	reg := prometheus.NewRegistry()
	capreg := &capturingRegisterer{Registerer: reg}

	newCounterIn(t, capreg, "reconcile_total")
	dup := prometheus.NewCounter(prometheus.CounterOpts{Name: "reconcile_total", Help: "h"})
	if err := capreg.Register(dup); err == nil {
		t.Fatal("expected duplicate registration to fail")
	}

	if len(capreg.captured) != 1 {
		t.Fatalf("captured %d collectors, want 1", len(capreg.captured))
	}
}

// registerCtrlStandIn puts a stand-in controller-runtime metric on the
// global registry and takes it back off when the test ends.
func registerCtrlStandIn(t *testing.T, name string) {
	t.Helper()
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: "stand-in for a controller-runtime metric"})
	c.Inc()
	if err := ctrlmetrics.Registry.Register(c); err != nil {
		t.Fatalf("register stand-in %s: %v", name, err)
	}
	t.Cleanup(func() { ctrlmetrics.Registry.Unregister(c) })
}

// buildReadersOnCtrlRegistry drives buildReaders exactly as New does,
// against the registry bridgeGathererFor dedupes against. Everything it
// puts on that global registry comes back off at the end of the test,
// including the one-reader-per-process claim.
func buildReadersOnCtrlRegistry(t *testing.T, cfg Config) readers {
	t.Helper()
	cfg.Prometheus = &PrometheusConfig{Registerer: ctrlmetrics.Registry}
	applyDefaults(&cfg)

	rs := buildReaders(context.Background(), cfg, logr.Discard())
	t.Cleanup(func() {
		if rs.capreg != nil {
			for _, c := range rs.capreg.captured {
				ctrlmetrics.Registry.Unregister(c)
			}
		}
		ctrlRegistryClaimed.Store(false)
	})
	if rs.prometheus == nil {
		t.Fatal("buildReaders did not build a Prometheus reader")
	}
	if cfg.OTLP != nil && rs.otlp == nil {
		t.Fatal("buildReaders did not build an OTLP reader")
	}
	return rs
}

// otlpNames collects from the OTLP reader and counts each metric name.
func otlpNames(t *testing.T, r sdkmetric.Reader) (map[string]int, map[string]metricdata.Aggregation) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	counts := map[string]int{}
	data := map[string]metricdata.Aggregation{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			counts[m.Name]++
			data[m.Name] = m.Data
		}
	}
	return counts, data
}

// Both transports on: the custom metric must reach OTLP once, natively,
// with the operator's own scope, and controller-runtime's must reach it
// once via the bridge.
func TestBothTransports_EachMetricExactlyOnce(t *testing.T) {
	registerCtrlStandIn(t, "eachonce_test_ctrl_total")

	rs := buildReadersOnCtrlRegistry(t, Config{
		OperatorName:     "my-operator",
		DisableGoRuntime: true,
		OTLP: &OTLPConfig{Endpoint: "localhost:1", Insecure: true,
			Timeout: time.Second, Interval: time.Hour},
	})

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(rs.prometheus), sdkmetric.WithReader(rs.otlp))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	c, err := mp.Meter("my-operator").Int64Counter("eachonce_reconcile_total")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	c.Add(context.Background(), 7)

	var rm metricdata.ResourceMetrics
	if err := rs.otlp.Collect(context.Background(), &rm); err != nil {
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

	if counts["eachonce_reconcile_total"] != 1 {
		t.Errorf("custom metric appeared %d times in OTLP, want 1", counts["eachonce_reconcile_total"])
	}
	if scopes["eachonce_reconcile_total"] != "my-operator" {
		t.Errorf("custom metric scope = %q, want the operator's own scope (it must take the native path)",
			scopes["eachonce_reconcile_total"])
	}
	if counts["eachonce_test_ctrl_total"] != 1 {
		t.Errorf("controller-runtime metric appeared %d times in OTLP, want 1", counts["eachonce_test_ctrl_total"])
	}
	if counts["target_info"] != 0 {
		t.Errorf("target_info reached OTLP as a metric; it is ours and should be excluded")
	}
}

// A View must not resurrect a second copy on the OTLP path. This is the
// failure mode that ruled out reader-level AggregationDrop.
func TestBothTransports_ViewedHistogramNotDuplicated(t *testing.T) {
	rs := buildReadersOnCtrlRegistry(t, Config{
		OperatorName:     "my-operator",
		DisableGoRuntime: true,
		OTLP: &OTLPConfig{Endpoint: "localhost:1", Insecure: true,
			Timeout: time.Second, Interval: time.Hour},
	})

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(rs.prometheus), sdkmetric.WithReader(rs.otlp),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "viewed_latency"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationBase2ExponentialHistogram{
				MaxSize: 160, MaxScale: 20}})))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	h, err := mp.Meter("my-operator").Float64Histogram("viewed_latency", otelmetric.WithUnit("s"))
	if err != nil {
		t.Fatalf("Float64Histogram: %v", err)
	}
	h.Record(context.Background(), 0.03)

	counts, data := otlpNames(t, rs.otlp)

	total := counts["viewed_latency"] + counts["viewed_latency_seconds"]
	if total != 1 {
		t.Errorf("the viewed histogram appeared %d times in OTLP, want 1", total)
	}
	if _, ok := data["viewed_latency"].(metricdata.ExponentialHistogram[float64]); !ok {
		t.Error("the viewed histogram lost its exponential aggregation, so it did not take the native path")
	}
}

func TestCapturingRegisterer_MustRegisterAlsoCaptures(t *testing.T) {
	reg := prometheus.NewRegistry()
	capreg := &capturingRegisterer{Registerer: reg}

	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "reconcile_total", Help: "h"})
	c.Inc()
	capreg.MustRegister(c)

	if !hasName(namesFrom(t, reg), "reconcile_total") {
		t.Error("MustRegister did not reach the wrapped registry")
	}
	if len(capreg.captured) != 1 {
		t.Fatalf("captured %d collectors, want 1: the embedded MustRegister bypassed capture", len(capreg.captured))
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

// The three branches New relies on to decide what the bridge reads.
// Each case asserts what the bridge actually sees, not just which type
// came back: a filter built against the wrong registry still type-checks
// as an exceptGatherer while matching nothing.
func TestBridgeGathererFor(t *testing.T) {
	registerCtrlStandIn(t, "bridgefor_test_ctrl_total")

	// Stands in for what the Prometheus reader wrote onto the shared
	// registry, so the mirror has a family to exclude.
	ours := prometheus.NewCounter(prometheus.CounterOpts{Name: "bridgefor_test_ours_total", Help: "h"})
	ours.Inc()
	if err := ctrlmetrics.Registry.Register(ours); err != nil {
		t.Fatalf("register ours: %v", err)
	}
	t.Cleanup(func() { ctrlmetrics.Registry.Unregister(ours) })
	capreg := &capturingRegisterer{captured: []prometheus.Collector{ours}}

	cases := []struct {
		name       string
		registerer prometheus.Registerer
		capreg     *capturingRegisterer
		wantOurs   bool
	}{
		{
			name:       "no Prometheus reader: nothing of ours is there to filter",
			registerer: nil,
			capreg:     nil,
			wantOurs:   true,
		},
		{
			name:       "reader on controller-runtime's registry: our families are excluded",
			registerer: ctrlmetrics.Registry,
			capreg:     capreg,
			wantOurs:   false,
		},
		{
			// Filtering here would drop controller-runtime families that
			// merely share a name with ours.
			name:       "reader on some other registry: no filtering",
			registerer: prometheus.NewRegistry(),
			capreg:     capreg,
			wantOurs:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := namesFrom(t, bridgeGathererFor(tc.registerer, tc.capreg))

			if !hasName(got, "bridgefor_test_ctrl_total") {
				t.Errorf("controller-runtime family was dropped: %v", got)
			}
			if hasName(got, "bridgefor_test_ours_total") != tc.wantOurs {
				t.Errorf("our family present = %v, want %v: %v",
					!tc.wantOurs, tc.wantOurs, got)
			}
		})
	}
}

// OTLP-only: nothing of ours may touch the registry controller-runtime
// serves, but its own metrics must still reach the collector.
func TestOTLPOnly_LeavesSharedRegistryAloneAndStillBridges(t *testing.T) {
	shared := prometheus.NewRegistry()
	newCounterIn(t, shared, "controller_runtime_reconcile_total")

	cfg := Config{
		OperatorName:      "my-operator",
		DisableGoRuntime:  true,
		DisablePrometheus: true,
		OTLP: &OTLPConfig{Endpoint: "localhost:1", Insecure: true,
			Timeout: time.Second, Interval: time.Hour},
	}
	applyDefaults(&cfg)

	otlpReader, err := buildOTLPReader(context.Background(), cfg, shared)
	if err != nil {
		t.Fatalf("buildOTLPReader: %v", err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(otlpReader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	c, err := mp.Meter("my-operator").Int64Counter("reconcile_total")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	c.Add(context.Background(), 1)

	var rm metricdata.ResourceMetrics
	if err := otlpReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var sawCustom, sawCtrl bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "reconcile_total":
				sawCustom = true
			case "controller_runtime_reconcile_total":
				sawCtrl = true
			}
		}
	}
	if !sawCustom {
		t.Error("custom metric did not reach OTLP natively")
	}
	if !sawCtrl {
		t.Error("controller-runtime metric did not reach OTLP via the bridge")
	}
	if hasName(namesFrom(t, shared), "reconcile_total") {
		t.Error("OTLP-only mode wrote an SDK metric into the shared registry")
	}
}

// target_info belongs on /metrics. Its absence from OTLP is asserted in
// TestBothTransports_EachMetricExactlyOnce.
func TestTargetInfo_PresentOnMetrics(t *testing.T) {
	p, reg := newPromPublisher(t, Config{DisableGoRuntime: true})
	p.Custom().MustCounter("reconcile_total", "d").Inc(context.Background())

	if !hasName(gatheredNames(t, reg), "target_info") {
		t.Error("target_info should be on /metrics by default")
	}
}
