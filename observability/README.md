# observability

OpenTelemetry metrics publisher for Kubernetes operators built on
`controller-runtime`. Custom and Go-runtime metrics are exported over OTLP
only. Controller-runtime's existing Prometheus `/metrics` endpoint is
**not touched** by this module; instead a one-directional bridge producer
reads from that registry and feeds the same metrics into the OTLP push
path.

This README is the quickstart. For per-package reference docs (every
config field, every instrument method, exporter behaviour,
extension points), see [`docs/`](docs/README.md). For a runnable program,
see [`example/`](example/).

## Architecture

```
                ┌──────────────────────────────────────────┐
                │           OTel MeterProvider             │
                │   custom metrics + Go runtime metrics    │
                │              │                            │
                │              ▼                            │
                │      PeriodicReader                       │
                │       │                ▲                  │
                │       │ (also pulls    │                  │
                │       │  from) ────────┘                  │
                │       │      Prometheus bridge producer   │
                │       ▼                ▲                  │
                │   OTLP exporter        │                  │
                └────────┼───────────────┼──────────────────┘
                         │               │
                         ▼               │
                    [collector]          │
                                         │
              ┌──────────────────────────┴──────┐
              │ controller-runtime Prom Registry │
              │ (populated by client_golang,     │
              │  served at /metrics — UNTOUCHED) │
              └─────────────────────────────────┘
```

| Metric source | `/metrics` | OTLP |
|---|---|---|
| Custom (operator-defined) | no | yes (native OTel SDK) |
| Go runtime | no | yes (via `otel/contrib/instrumentation/runtime`) |
| Controller-runtime native | yes (existing) | yes (via bridge producer) |

### Design rationale

Earlier designs considered writing OTel-instrumented metrics into
controller-runtime's Prometheus registry (via the OTel Prometheus
exporter) so they would appear on `/metrics` alongside controller-runtime
metrics. This was rejected because the Prometheus bridge producer also
reads from that registry to feed OTLP, which would cause every custom
metric to reach OTLP twice — once natively from the SDK, once
round-tripped through the registry. The current design uses
one-directional, non-overlapping flows: OTel SDK metrics go to OTLP only;
Prometheus registry metrics stay on `/metrics` (untouched) and reach OTLP
only via the bridge.

## Package layout

| Import path | What's there |
|---|---|
| `.../observability/pkg/publisher` | `Publisher`, `Config`, `OTLPConfig`, `CustomMetrics`. The default entry point. |
| `.../observability/pkg/instrument` | `Counter`, `Gauge`, `Histogram` interfaces. Import this when type-annotating helper signatures. |
| `.../observability/pkg/naming` | `ValidateMetricName`, `ValidateAttributeKey`. Useful at operator startup. |
| `.../observability/pkg/resource` | `Build(operatorName, version)` — exposes the module's k8s pod resource conventions for callers wiring a custom MeterProvider. |
| `.../observability/pkg/bridge` | `ControllerRuntimeProducer`, `StartGoRuntime` — extension points for callers wiring a custom Reader. |

## Integration

```go
package main

import (
    "context"
    "os"
    "os/signal"
    "syscall"

    ctrl "sigs.k8s.io/controller-runtime"

    "github.com/stakater/operator-utils/observability/pkg/publisher"
)

func main() {
    ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer cancel()

    // 1. Construct publisher BEFORE the manager.
    pub, err := publisher.New(ctx, publisher.Config{
        OperatorName: "my-operator",
        Version:      "1.2.3",
        OTLP: &publisher.OTLPConfig{
            Endpoint: "otel-collector.observability.svc:4317",
            Insecure: true,
        },
    })
    if err != nil {
        // Only fatal on misconfiguration (e.g., empty OperatorName).
        // OTLP unreachability is logged, not returned.
        os.Exit(1)
    }
    defer func() { _ = pub.Shutdown(context.Background()) }()

    // 2. Build and run the controller-runtime Manager.
    //    Set ctrl.Options{HealthProbeBindAddress, PprofBindAddress} on the
    //    Manager if you want probes/pprof — controller-runtime owns those.
    mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{})
    if err != nil {
        os.Exit(1)
    }
    if err := mgr.Start(ctx); err != nil {
        os.Exit(1)
    }
}
```

### Defining custom metrics

Register metrics once, ideally as package-level vars in the controller package:

```go
var reconcileTotal = pub.Custom().MustCounter(
    "reconcile_total",
    "Total reconciliations attempted by the controller",
)
```

Then call them from reconcile loops:
```go
reconcileTotal.Inc(ctx, attribute.String("result", "success"))
```

## Configuration reference

### `publisher.Config`

| Field | Type | Default | Purpose |
|---|---|---|---|
| `OperatorName` | string | (required) | `service.name` resource attribute |
| `Version` | string | `"unknown"` | `service.version` resource attribute |
| `OTLP` | `*OTLPConfig` | nil | OTLP exporter; if nil, OTLP disabled unless env enables it |
| `Stdout` | bool | false | Enables stdout exporter (dev only) |
| `DisableControllerRuntimeBridge` | bool | false | When true, ctrl-runtime metrics do not flow into OTLP |
| `DisableGoRuntime` | bool | false | When true, Go runtime metrics are not collected |
| `Logger` | logr.Logger | discard | Logger for warnings |

### `publisher.OTLPConfig`

| Field | Default | Purpose |
|---|---|---|
| `Endpoint` | "" (required when OTLP set) | Collector address (host:port or scheme://host:port/path) |
| `Protocol` | `"grpc"` | `"grpc"` or `"http/protobuf"` |
| `Insecure` | false | Disable TLS entirely (plaintext); typical for in-cluster |
| `Headers` | nil | Per-export headers (e.g. auth tokens) |
| `Timeout` | 10s | Per-export timeout |
| `Compression` | `"gzip"` | `"gzip"` or `""` |
| `Interval` | 30s | Periodic push interval |

## Environment variable overrides

These standard OTel SDK env vars override the Go config:

- `OTEL_SERVICE_NAME` → `OperatorName`
- `OTEL_EXPORTER_OTLP_ENDPOINT` → `OTLP.Endpoint` (also creates `OTLP` if nil)
- `OTEL_EXPORTER_OTLP_PROTOCOL` → `OTLP.Protocol`
- `OTEL_EXPORTER_OTLP_HEADERS` → merged into `OTLP.Headers` (format: `k1=v1,k2=v2`)

## Verification

```bash
# Controller-runtime metrics still on /metrics (unchanged behavior)
curl -s localhost:8080/metrics | grep controller_runtime_reconcile_total

# Custom metrics are NOT on /metrics — they are OTLP-only
curl -s localhost:8080/metrics | grep my_custom_metric  # should be empty

# To see custom metrics during local dev, set Stdout: true and check the operator's stdout.
```

## Caveats

- **Cardinality.** Custom metric attribute values become OTLP labels.
  High-cardinality values (full namespaces, UIDs, request IDs) explode
  storage cost downstream. Prefer bounded enumerations.
- **OTLP graceful degradation.** If the collector is unreachable, exports
  fail in the background and the operator keeps running. `/metrics` is
  unaffected.
- **Custom metrics do not appear on `/metrics`.** This is intentional.
  Use Prometheus's native OTLP ingestion (v2.47+) or scrape the
  collector if you need them in Prometheus.
