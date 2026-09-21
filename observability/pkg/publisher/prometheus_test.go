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

// The translator appends _total only when the name lacks it, so both
// spellings land on the same series. This is why the module needs no
// counter-suffix knob and no naming rule about _total.
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
