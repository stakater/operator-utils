package objstate

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type harness struct {
	set    *Set
	reader *sdkmetric.ManualReader
}

func newHarness(t *testing.T, idKeys ...string) *harness {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	set, err := NewSet(mp.Meter("test"), idKeys...)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	return &harness{set: set, reader: reader}
}

// collect returns every data point of the named metric as attrs-map -> value.
func (h *harness) collect(t *testing.T, name string) map[string]float64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					out[p.Attributes.Encoded(attribute.DefaultEncoder())] = p.Value
				}
			case metricdata.Sum[float64]:
				for _, p := range d.DataPoints {
					out[p.Attributes.Encoded(attribute.DefaultEncoder())] = p.Value
				}
			}
		}
	}
	return out
}

func enc(kvs ...attribute.KeyValue) string {
	set := attribute.NewSet(kvs...)
	return set.Encoded(attribute.DefaultEncoder())
}

// The package's reason to exist: a series exists exactly while its object
// does. No DeletePartialMatch choreography, no stale series after deletion.
func TestSeriesVanishWithTheObject(t *testing.T) {
	h := newHarness(t, "costjob", "namespace")
	age, err := h.set.Timestamp("costjob_last_success_timestamp_seconds", "when it last worked")
	if err != nil {
		t.Fatalf("Timestamp: %v", err)
	}

	id := ID{"daily", "finops"}
	if err := h.set.Put(id, nil, func(st *State) {
		st.Timestamp(age, time.Unix(600, 0))
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got := h.collect(t, "costjob_last_success_timestamp_seconds")
	want := enc(attribute.String("costjob", "daily"), attribute.String("namespace", "finops"))
	if got[want] != 600 {
		t.Fatalf("timestamp = %v, want 600 at %s (have %v)", got[want], want, got)
	}

	if err := h.set.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := h.collect(t, "costjob_last_success_timestamp_seconds"); len(got) != 0 {
		t.Fatalf("series survived object deletion: %v - a deleted job would page forever", got)
	}
}

// One-hot enum: every registered value emitted per object, all-zero before the
// first transition (the presence signal "tracked but never ran").
func TestEnumIsOneHotWithAllZeroPresence(t *testing.T) {
	h := newHarness(t, "costjob")
	status, err := h.set.Enum("costjob_last_execution_status", "one-hot", "status",
		"Success", "Failed")
	if err != nil {
		t.Fatalf("Enum: %v", err)
	}

	if err := h.set.Put(ID{"new-job"}, nil, nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got := h.collect(t, "costjob_last_execution_status")
	if len(got) != 2 {
		t.Fatalf("want 2 one-hot series for a tracked-but-never-run object, got %v", got)
	}
	for k, v := range got {
		if v != 0 {
			t.Fatalf("expected all-zero before first transition, got %v at %s", v, k)
		}
	}

	if err := h.set.Put(ID{"new-job"}, nil, func(st *State) { st.Enum(status, "Failed") }); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got = h.collect(t, "costjob_last_execution_status")
	failed := enc(attribute.String("costjob", "new-job"), attribute.String("status", "Failed"))
	success := enc(attribute.String("costjob", "new-job"), attribute.String("status", "Success"))
	if got[failed] != 1 || got[success] != 0 {
		t.Fatalf("one-hot wrong after transition: %v", got)
	}
}

// Put replaces state wholesale: a stale condition reason cannot linger.
func TestPutReplacesStateSoStaleReasonsCannotLinger(t *testing.T) {
	h := newHarness(t, "supervisor")
	ready, err := h.set.Condition("supervisor_ready", "ready with reason", "reason")
	if err != nil {
		t.Fatalf("Condition: %v", err)
	}

	put := func(ok bool, reason string) {
		if err := h.set.Put(ID{"sup"}, nil, func(st *State) { st.Condition(ready, ok, reason) }); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	put(false, "WakeFailed")
	put(true, "Reconciled")

	got := h.collect(t, "supervisor_ready")
	if len(got) != 1 {
		t.Fatalf("exactly one reason series may exist, got %v", got)
	}
	cur := enc(attribute.String("supervisor", "sup"), attribute.String("reason", "Reconciled"))
	if got[cur] != 1 {
		t.Fatalf("current reason series wrong: %v", got)
	}
}

// Counters survive Put (a state republish must never reset a reconcile
// counter) and die with the object; counting may precede any Put.
func TestCountersSurvivePutAndDieWithTheObject(t *testing.T) {
	h := newHarness(t, "instance", "namespace")
	reconciles, err := h.set.Counter("instance_reconciles_total", "storm signal")
	if err != nil {
		t.Fatalf("Counter: %v", err)
	}
	id := ID{"ti", "team-a"}

	if err := h.set.Inc(reconciles, id); err != nil { // before any Put
		t.Fatalf("Inc: %v", err)
	}
	if err := h.set.Put(id, nil, nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := h.set.Inc(reconciles, id); err != nil {
		t.Fatalf("Inc: %v", err)
	}

	key := enc(attribute.String("instance", "ti"), attribute.String("namespace", "team-a"))
	if got := h.collect(t, "instance_reconciles_total"); got[key] != 2 {
		t.Fatalf("counter = %v, want 2 (Put must not reset it)", got[key])
	}

	if err := h.set.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := h.collect(t, "instance_reconciles_total"); len(got) != 0 {
		t.Fatalf("counter series survived deletion: %v", got)
	}
}

// Extra per-object attributes ride on every series of the object.
func TestExtraAttributesApplyToEverySeries(t *testing.T) {
	h := newHarness(t, "supervisor", "namespace")
	ns, err := h.set.Value("supervisor_sleeping_namespaces", "count")
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if err := h.set.Put(ID{"sup", "team-a"},
		[]attribute.KeyValue{attribute.String("kind", "resourcesupervisor")},
		func(st *State) { st.Value(ns, 3) }); err != nil {
		t.Fatalf("Put: %v", err)
	}
	key := enc(
		attribute.String("supervisor", "sup"),
		attribute.String("namespace", "team-a"),
		attribute.String("kind", "resourcesupervisor"),
	)
	if got := h.collect(t, "supervisor_sleeping_namespaces"); got[key] != 3 {
		t.Fatalf("value with extra attrs = %v, want 3 (have %v)", got[key], got)
	}
}

func TestValidation(t *testing.T) {
	h := newHarness(t, "name")
	if _, err := h.set.Value("go_bad_prefix", "reserved"); err == nil {
		t.Fatal("reserved prefix must be rejected")
	}
	if _, err := h.set.Value("dupe", "a"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := h.set.Timestamp("dupe", "b"); err == nil {
		t.Fatal("duplicate name across instrument kinds must be rejected")
	}
	if err := h.set.Put(ID{"a", "b"}, nil, nil); err == nil {
		t.Fatal("identity arity mismatch must be rejected")
	}
	if _, err := NewSet(nil, "x"); err == nil {
		t.Fatal("nil meter must be rejected")
	}
}
