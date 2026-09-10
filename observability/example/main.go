// Command example is a tiny standalone program demonstrating how an
// operator wires up the observability module. It does not require a
// running Kubernetes cluster or an OTel collector — metrics are printed
// to stdout via the stdoutmetric exporter so you can see them locally.
//
// Run:
//
//	go run .
//
// Watch stdout for periodic metric exports (default interval 60 seconds;
// a final flush prints on Ctrl+C).
package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/stakater/operator-utils/observability/pkg/instrument"
	"github.com/stakater/operator-utils/observability/pkg/publisher"
)

func main() {
	// Ctrl+C / SIGTERM cancels this context, which unblocks the main loop
	// and triggers the deferred Shutdowns in reverse order.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Construct the publisher BEFORE any controller-runtime manager would
	// be created. Stdout: true sends metrics to stdout instead of OTLP,
	// which is convenient for a local demo. In a real operator you would
	// set OTLP and leave Stdout off.
	pub, err := publisher.New(ctx, publisher.Config{
		OperatorName: "demo-operator",
		Version:      "0.1.0",
		Stdout:       true,

		// Keep the demo output small. A real operator would leave these
		// at their defaults so controller-runtime and Go-runtime metrics
		// also flow into OTLP.
		DisableControllerRuntimeBridge: true,
		DisableGoRuntime:               true,
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
	reconcileDuration := custom.MustHistogram(
		"reconcile_duration_seconds",
		"Wall-clock duration of a reconcile call, in seconds",
	)

	fmt.Println("demo-operator running. Ctrl+C to stop.")
	fmt.Println("  metrics: printed to stdout every 60 seconds (and once on shutdown)")

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
	duration := time.Since(start).Seconds()

	outcome := "success"
	if rand.IntN(10) == 0 {
		outcome = "error"
	}

	result := attribute.String("result", outcome)
	counter.Inc(ctx, result)
	histogram.Record(ctx, duration, result)
}
