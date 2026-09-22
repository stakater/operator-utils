package publisher

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/metric"
)

// newPromPublisher builds a Publisher whose Prometheus reader registers
// into a private registry, so tests never touch controller-runtime's
// global one and never collide with each other.
func newPromPublisher(t *testing.T, cfg Config) (*Publisher, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	if cfg.Prometheus == nil {
		cfg.Prometheus = &PrometheusConfig{}
	}
	cfg.Prometheus.Registerer = reg
	if cfg.OperatorName == "" {
		cfg.OperatorName = "op"
	}
	p, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p, reg
}

func gatheredNames(t *testing.T, reg *prometheus.Registry) []string {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	names := make([]string, 0, len(families))
	for _, f := range families {
		names = append(names, f.GetName())
	}
	return names
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// The default reader must expose custom metrics under the exact name they
// were registered with: no _total suffix appended on top of one already
// written by the operator author.
func TestPrometheus_CustomCounterKeepsRegisteredName(t *testing.T) {
	p, reg := newPromPublisher(t, Config{DisableGoRuntime: true})

	p.Custom().MustCounter("reconcile_total", "total reconciliations").
		Inc(context.Background())

	names := gatheredNames(t, reg)
	if !hasName(names, "reconcile_total") {
		t.Fatalf("want reconcile_total on /metrics, got %v", names)
	}
	if hasName(names, "reconcile_total_total") {
		t.Fatalf("counter suffix applied twice, got %v", names)
	}
}

// Scope labels name the instrumentation library, which is always this
// module. They add a label to every series and tell an operator nothing.
func TestPrometheus_NoScopeLabelsOnSeries(t *testing.T) {
	p, reg := newPromPublisher(t, Config{DisableGoRuntime: true})

	p.Custom().MustGauge("active_workers", "active workers").
		Set(context.Background(), 3)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "otel_scope_name" || l.GetName() == "otel_scope_version" {
					t.Fatalf("%s carries scope label %s", f.GetName(), l.GetName())
				}
			}
		}
	}
}

// The translator appends _total only when the name lacks it, so either
// spelling lands on the same series. Registration has to know that: both
// spellings on one publisher would be one Prometheus family fed by two
// SDK instruments, which fails the whole Gather and turns /metrics into
// a 500 for controller-runtime's metrics too.
func TestPrometheus_CounterTotalSuffixIsIdempotent(t *testing.T) {
	for _, registered := range []string{"reconcile", "reconcile_total"} {
		t.Run(registered, func(t *testing.T) {
			p, reg := newPromPublisher(t, Config{DisableGoRuntime: true})

			p.Custom().MustCounter(registered, "total reconciliations").
				Inc(context.Background())

			names := gatheredNames(t, reg)
			if !hasName(names, "reconcile_total") {
				t.Fatalf("registered %q, want reconcile_total, got %v", registered, names)
			}
			if hasName(names, "reconcile_total_total") {
				t.Fatalf("registered %q, suffix applied twice: %v", registered, names)
			}
		})
	}
}

func TestPrometheus_CollidingTranslatedNamesAreRejected(t *testing.T) {
	for _, order := range [][2]string{
		{"reconcile", "reconcile_total"},
		{"reconcile_total", "reconcile"},
	} {
		t.Run(order[0]+"_then_"+order[1], func(t *testing.T) {
			p, reg := newPromPublisher(t, Config{DisableGoRuntime: true})

			first, err := p.Custom().Counter(order[0], "d")
			if err != nil {
				t.Fatalf("Counter(%q): %v", order[0], err)
			}
			first.Inc(context.Background())

			if _, err := p.Custom().Counter(order[1], "d"); err == nil {
				t.Fatalf("Counter(%q) was accepted; both are served as reconcile_total", order[1])
			}

			// The rejection is the point: /metrics must still gather.
			if _, err := reg.Gather(); err != nil {
				t.Fatalf("/metrics is broken: %v", err)
			}
		})
	}
}

// A gauge and a counter can differ only by the suffix the translator
// adds, so the collision is not counter-specific.
func TestPrometheus_GaugeCollidingWithCounterIsRejected(t *testing.T) {
	p, reg := newPromPublisher(t, Config{DisableGoRuntime: true})

	g := p.Custom().MustGauge("widgets_total", "d")
	g.Set(context.Background(), 1)

	if _, err := p.Custom().Counter("widgets", "d"); err == nil {
		t.Fatal("Counter(\"widgets\") was accepted; it is served as widgets_total")
	}
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("/metrics is broken: %v", err)
	}
}

func TestPrometheus_DisabledRegistersNothing(t *testing.T) {
	_, reg := newPromPublisher(t, Config{
		DisableGoRuntime:  true,
		DisablePrometheus: true,
	})

	if names := gatheredNames(t, reg); len(names) != 0 {
		t.Fatalf("expected an empty registry, got %v", names)
	}
}

func TestPrometheus_TargetInfoOptOut(t *testing.T) {
	p, reg := newPromPublisher(t, Config{
		DisableGoRuntime: true,
		Prometheus:       &PrometheusConfig{DisableTargetInfo: true},
	})

	p.Custom().MustCounter("reconcile_total", "total reconciliations").
		Inc(context.Background())

	if names := gatheredNames(t, reg); hasName(names, "target_info") {
		t.Fatalf("target_info should be suppressed, got %v", names)
	}
}

// Go-runtime instrumentation emits OTel-style dotted names. They must be
// escaped to Prometheus-legal names, otherwise a scrape yields UTF-8
// names that only Prometheus 3.x and newer can query comfortably. Uses
// the Meter escape hatch because naming.ValidateMetricName forbids dots
// on custom metrics.
func TestPrometheus_DottedNamesAreUnderscoreEscaped(t *testing.T) {
	p, reg := newPromPublisher(t, Config{DisableGoRuntime: true})

	g, err := p.Meter().Int64UpDownCounter("go.memory.used", metric.WithUnit("By"))
	if err != nil {
		t.Fatalf("Int64UpDownCounter: %v", err)
	}
	g.Add(context.Background(), 1024)

	names := gatheredNames(t, reg)
	if !hasName(names, "go_memory_used_bytes") {
		t.Fatalf("want go_memory_used_bytes, got %v", names)
	}
}
