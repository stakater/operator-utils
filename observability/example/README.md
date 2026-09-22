# example

A tiny standalone program that wires up the observability module the way a
real operator would.

## What it does

- Constructs a `publisher.Publisher` with the default Prometheus reader
  and the stdout exporter (no OTel collector needed)
- Serves `/metrics` on `:8080`, which is what the controller-runtime
  manager would do for you in a real operator
- Registers one Counter, one Gauge, one Histogram via `publisher.CustomMetrics`.
  The histogram records **milliseconds**, because the SDK's default bucket
  boundaries are millisecond-oriented — recording seconds would put every
  observation in the first bucket
- Runs a fake reconcile loop once per second that updates all three metrics,
  with `result=success` or `result=error` attributes

## Run

```bash
go run .
```

You'll see:

```
demo-operator running. Ctrl+C to stop.
  metrics: http://localhost:8080/metrics
  metrics: also printed to stdout every 60 seconds (and once on shutdown)
```

Scrape it immediately — the Prometheus reader collects on each scrape, so
there is nothing to wait for:

```bash
curl -s localhost:8080/metrics | grep -E '^reconcile_total|^active_workers|^target_info'
```

```
active_workers 4
reconcile_total{result="success"} 2
target_info{service_name="demo-operator",service_version="0.1.0",...} 1
```

The stdout exporter is the slow path: wait ~60s for its periodic flush and
you'll get a JSON dump of the same metrics. Ctrl+C triggers a final flush
before the process exits.

## Switching to a real OTLP collector

Drop `Stdout: true`, add an `OTLP` block, and remove `DisableGoRuntime` so
Go-runtime metrics are exported too. `/metrics` keeps working alongside it;
controller-runtime metrics are served there for scraping and, via the
bridge, also pushed over OTLP:

```go
pub, err := publisher.New(ctx, publisher.Config{
    OperatorName: "demo-operator",
    Version:      "0.1.0",
    OTLP: &publisher.OTLPConfig{
        Endpoint: "otel-collector.observability.svc:4317",
        Insecure: true,
    },
})
```

Or set `OTEL_EXPORTER_OTLP_ENDPOINT` in the environment to enable OTLP without
recompiling.

## Why this is a separate module

`example/` has its own `go.mod` with a `replace` directive pointing at `../`,
so it tracks the working tree rather than the published version of the
library. Keeping it in a separate module also means consumers of
`github.com/stakater/operator-utils/observability` don't transitively pull
in the example's dependencies.
