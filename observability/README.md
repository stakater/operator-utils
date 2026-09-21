# observability

OpenTelemetry metrics publisher for Kubernetes operators built on
`controller-runtime`. Custom and Go-runtime metrics are exposed on the
Prometheus registry the manager already serves at `/metrics`, so a
zero-config `Publisher` works with your existing `ServiceMonitor` and
needs no collector, no extra port and no RBAC change. Setting `OTLP`
additionally pushes the same metrics to a collector.

This README is the quickstart. For per-package reference docs (every
config field, every instrument method, exporter behaviour,
extension points), see [`docs/`](docs/README.md). For a runnable program,
see [`example/`](example/).

## Architecture

```
            ┌──────────────────────────────────────┐
            │      instruments (OTel Meter API)    │
            └───────────┬──────────────┬───────────┘
                        │              │ native
       Prometheus exporter             │
                        ▼              ▼
   ┌──────────────────────────┐    OTLP exporter ──► [collector]
   │ controller-runtime Prom  │        ▲
   │ Registry  ──► /metrics   │────────┘
   └──────────────────────────┘   bridge, minus our own families
```

| Metric source | `/metrics` | OTLP |
|---|---|---|
| Custom (operator-defined) | yes | yes, natively |
| Go runtime | yes | yes, natively |
| Controller-runtime native | yes | yes, via the bridge |

### Why nothing is suppressed

The Prometheus exporter and the bridge share one registry, so a naive
bridge would re-export every SDK metric that the exporter just wrote, and
each custom metric would reach OTLP twice under one name.

Rather than switch a route off, the bridge is given a filtered view: it
exports only the families this module did not write. Every transport
therefore carries every metric class exactly once, in every
configuration, and custom metrics keep their native OTel form on the push
path, including exponential histograms and the operator's own
instrumentation scope.

## Package layout

| Import path | What's there |
|---|---|
| `.../observability/pkg/publisher` | `Publisher`, `Config`, `OTLPConfig`, `PrometheusConfig`, `CustomMetrics`. The default entry point. |
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

    // 1. Construct publisher BEFORE the manager. With no OTLP block,
    //    metrics are served on the manager's /metrics endpoint only.
    pub, err := publisher.New(ctx, publisher.Config{
        OperatorName: "my-operator",
        Version:      "1.2.3",
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
| `Prometheus` | `*PrometheusConfig` | nil | Tunes the `/metrics` reader; nil means defaults, not disabled |
| `DisablePrometheus` | bool | false | When true, this module's metrics are no longer exposed on `/metrics`, though controller-runtime's own metrics still are, and the bridge stays on |
| `Stdout` | bool | false | Enables stdout exporter (dev only) |
| `DisableGoRuntime` | bool | false | When true, Go runtime metrics are not collected |
| `Logger` | logr.Logger | discard | Logger for warnings |

### `publisher.PrometheusConfig`

| Field | Default | Purpose |
|---|---|---|
| `Registerer` | controller-runtime's registry | Where to expose metrics; override to serve them elsewhere |
| `DisableTargetInfo` | false | Drop the `target_info` series carrying `service.name` / `service.version` |

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
# Controller-runtime metrics, as before
curl -s localhost:8080/metrics | grep controller_runtime_reconcile_total

# Custom metrics, under the exact name you registered
curl -s localhost:8080/metrics | grep reconcile_total

# Go runtime metrics, dotted OTel names escaped to Prometheus form
curl -s localhost:8080/metrics | grep go_memory_used_bytes

# Resource attributes
curl -s localhost:8080/metrics | grep target_info
```

## Caveats

- **Cardinality.** Custom metric attribute values become OTLP labels.
  High-cardinality values (full namespaces, UIDs, request IDs) explode
  storage cost downstream. Prefer bounded enumerations.
- **OTLP graceful degradation.** If the collector is unreachable, exports
  fail in the background and the operator keeps running. `/metrics` is
  unaffected.
- **Counter naming.** Names are translated to Prometheus style on
  export, and the `_total` suffix is appended only when missing.
  Registering `reconcile` or `reconcile_total` yields the same
  `reconcile_total` series.
- **Histogram buckets.** The SDK default bucket boundaries are
  millisecond-oriented (`0, 5, 10, ... 10000`), so a histogram recorded
  in seconds buckets poorly on `/metrics`. Record milliseconds, or wire
  a custom View through `pkg/bridge` and your own Reader.
