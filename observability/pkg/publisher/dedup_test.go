package publisher

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
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
