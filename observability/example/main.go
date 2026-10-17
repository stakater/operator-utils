// Command example is a tiny standalone program demonstrating how an
// operator wires up the observability module. It does not require a
// running Kubernetes cluster or an OTel collector: metrics are served on
// http://localhost:8080/metrics and also printed to stdout via the
// stdoutmetric exporter.
//
// Run:
//
//	go run .
//	curl -s localhost:8080/metrics | grep reconcile_total
//
// Watch stdout for periodic metric exports (default interval 60 seconds;
// a final flush prints on Ctrl+C).
package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/stakater/operator-utils/observability/pkg/instrument"
	"github.com/stakater/operator-utils/observability/pkg/publisher"
)

func main() {
	// Ctrl+C / SIGTERM cancels this context, which unblocks the main loop
	// and triggers the deferred Shutdowns in reverse order.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Construct the publisher BEFORE any controller-runtime manager would
	// be created. The Prometheus reader is on by default, so the metrics
	// land on controller-runtime's registry with no extra config.
	// Stdout: true additionally dumps them locally, which a real operator
	// would leave off.
	pub, err := publisher.New(ctx, publisher.Config{
		OperatorName: "demo-operator",
		Version:      "0.1.0",
		Stdout:       true,

		// Keep the stdout dump small. A real operator would leave this at
		// its default so Go-runtime metrics are exposed too.
		DisableGoRuntime: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "publisher init: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		_ = pub.Shutdown(shutdownCtx)
	}()

	// Register the three custom metrics this demo will update. Using the
	// Must* variants is idiomatic at startup — a registration error here
	// is a programming bug and should crash the process loudly.
	custom := pub.Custom()
	reconcileTotal := custom.MustCounter(
		"reconcile_total",
		"Total reconciliations attempted by the demo operator",
	)
	activeWorkers := custom.MustGauge(
		"active_workers",
		"Current number of active worker goroutines",
	)
	// Milliseconds, not seconds. The SDK's default histogram boundaries
	// are 0, 5, 10, ... 10000, so a duration recorded in seconds lands in
	// the first bucket every time and the histogram says nothing. Record
	// milliseconds, or attach a View with your own boundaries.
	reconcileDuration := custom.MustHistogram(
		"reconcile_duration_milliseconds",
		"Wall-clock duration of a reconcile call, in milliseconds",
	)

	// A real operator gets this endpoint from the controller-runtime
	// manager. Serving ctrlmetrics.Registry by hand is what the manager
	// does internally, and lets the demo run without a cluster.
	srv := &http.Server{
		Addr:              ":8080",
		Handler:           promhttp.HandlerFor(ctrlmetrics.Registry, promhttp.HandlerOpts{}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "metrics server: %v\n", err)
		}
	}()
	defer func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		_ = srv.Shutdown(shutdownCtx)
	}()

	fmt.Println("demo-operator running. Ctrl+C to stop.")
	fmt.Println("  metrics: http://localhost:8080/metrics")
	fmt.Println("  metrics: also printed to stdout every 60 seconds (and once on shutdown)")

	// Simulate one fake reconcile call per second. Each call updates
	// every metric so the periodic stdout export has interesting data.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runFakeReconcile(ctx, reconcileTotal, activeWorkers, reconcileDuration)
		}
	}
}

// runFakeReconcile simulates a single reconcile call: it varies the
// active-worker gauge, sleeps for a short randomised duration, and then
// records its outcome on the counter and histogram. Roughly one in ten
// calls is reported as an error so the result attribute exercises both
// values.
//
// Real reconcile helpers in operator code would take instrument.Counter
// / instrument.Gauge / instrument.Histogram exactly like this.
func runFakeReconcile(
	ctx context.Context,
	counter instrument.Counter,
	gauge instrument.Gauge,
	histogram instrument.Histogram,
) {
	gauge.Set(ctx, int64(rand.IntN(5)+1))

	start := time.Now()
	time.Sleep(time.Duration(rand.IntN(80)+10) * time.Millisecond)
	durationMillis := float64(time.Since(start).Nanoseconds()) / float64(time.Millisecond)

	outcome := "success"
	if rand.IntN(10) == 0 {
		outcome = "error"
	}

	result := attribute.String("result", outcome)
	counter.Inc(ctx, result)
	histogram.Record(ctx, durationMillis, result)
}
