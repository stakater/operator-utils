# example

A tiny standalone program that wires up the observability module the way a
real operator would.

## What it does

- Constructs a `publisher.Publisher` with the stdout exporter (no OTel
  collector needed)
- Registers one Counter, one Gauge, one Histogram via `publisher.CustomMetrics`
- Runs a fake reconcile loop once per second that updates all three metrics,
  with `result=success` or `result=error` attributes

## Run

```bash
go run .
```

You'll see:

```
demo-operator running. Ctrl+C to stop.
  metrics: printed to stdout every 60 seconds (and once on shutdown)
```

Wait ~60s for the periodic stdout exporter to flush, then you'll see a JSON
dump of the metrics that have been recorded since the last flush. Ctrl+C
triggers a final flush before the process exits.

## Switching to a real OTLP collector

Drop `Stdout: true`, add an `OTLP` block, and remove the two `Disable*` flags
so controller-runtime and Go-runtime metrics also flow into the collector:

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
