package objstate_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stakater/operator-utils/observability/pkg/objstate"
	"github.com/stakater/operator-utils/observability/pkg/publisher"
)

// End to end through the real pipeline: Set -> publisher meter -> Prometheus
// exporter -> registry. The property the whole package exists for must hold in
// the actual exposition: after Delete, the family carries no series for the
// object - the scrape itself forgets it, no DeletePartialMatch anywhere.
func TestSeriesAbsentFromPrometheusExpositionAfterDelete(t *testing.T) {
	reg := prometheus.NewRegistry()
	pub, err := publisher.New(context.Background(), publisher.Config{
		OperatorName: "objstate-test",
		Version:      "0.0.0",
		Prometheus:   &publisher.PrometheusConfig{Registerer: reg},
	})
	if err != nil {
		t.Fatalf("publisher.New: %v", err)
	}
	t.Cleanup(func() { _ = pub.Shutdown(context.Background()) })

	set, err := objstate.NewSet(pub.Meter(), "costjob")
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	age, err := set.Timestamp("costjob_last_success_timestamp_seconds", "when it last worked")
	if err != nil {
		t.Fatalf("Timestamp: %v", err)
	}

	series := func() int {
		t.Helper()
		fams, err := reg.Gather()
		if err != nil {
			t.Fatalf("gather: %v", err)
		}
		for _, f := range fams {
			if f.GetName() == "costjob_last_success_timestamp_seconds" {
				return len(f.GetMetric())
			}
		}
		return 0
	}

	if err := set.Put(objstate.ID{"daily"}, nil, func(st *objstate.State) {
		st.Timestamp(age, time.Unix(600, 0))
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n := series(); n != 1 {
		t.Fatalf("series after Put = %d, want 1", n)
	}

	if err := set.Delete(objstate.ID{"daily"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n := series(); n != 0 {
		t.Fatalf("series after Delete = %d, want 0: the exposition must forget the object", n)
	}
}
