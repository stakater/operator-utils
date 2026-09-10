# Extension points

The `Publisher` is the default-experience composition of every other
package in this module. If you need behaviour the publisher doesn't
expose — custom views, observable instruments, a different reader
topology — you have two paths:

1. **Escape-hatch into the SDK.** `pub.Meter()` returns the underlying
   `metric.Meter`, so you can use OTel SDK APIs directly while keeping
   the publisher's resource attributes, OTLP wiring, and shutdown.
2. **Bypass the publisher entirely.** Wire your own `MeterProvider` and
   use the `resource` and `bridge` packages as standalone building
   blocks to keep the module's conventions.

This page documents the second path.

## resource.Build

```go
import "github.com/stakater/operator-utils/observability/pkg/resource"

res, err := resource.Build(ctx, "my-operator", "1.2.3")
```

Returns a `*sdkresource.Resource` carrying:

| Attribute | Source |
|---|---|
| `service.name` | The `operatorName` argument |
| `service.version` | The `version` argument |
| `service.instance.id` | `POD_NAME` env, falls back to `os.Hostname()` |
| `k8s.pod.name` | Same as `service.instance.id` |
| `k8s.namespace.name` | `POD_NAMESPACE` env (omitted if unset) |

The schema URL is set to `semconv.SchemaURL` (currently `v1.26.0`).

### When to use

When you're building a `MeterProvider` yourself and want the module's
k8s-pod resource conventions without going through `publisher.New`. For
example, if you need to share a single `MeterProvider` between metrics
and traces:

```go
import (
    sdkmetric "go.opentelemetry.io/otel/sdk/metric"
    sdktrace "go.opentelemetry.io/otel/sdk/trace"

    "github.com/stakater/operator-utils/observability/pkg/resource"
)

res, err := resource.Build(ctx, "my-operator", "1.2.3")
if err != nil { /* handle */ }

mp := sdkmetric.NewMeterProvider(
    sdkmetric.WithResource(res),
    // ... your own readers ...
)
tp := sdktrace.NewTracerProvider(
    sdktrace.WithResource(res),
    // ... your own span processors ...
)
```

### Customising the resource

`resource.Build` is intentionally a black box for the five attributes
above. To add more, wrap it:

```go
base, err := resource.Build(ctx, "my-op", "1.2.3")
if err != nil { /* handle */ }

extra := sdkresource.NewWithAttributes(semconv.SchemaURL,
    attribute.String("deployment.environment", "prod"),
    attribute.String("cloud.region", "us-east-1"),
)

merged, err := sdkresource.Merge(base, extra)
```

The right-hand resource's attributes overwrite the left-hand resource's
on key conflict — see [the OTel resource docs](https://opentelemetry.io/docs/specs/otel/resource/sdk/#merge)
for the merge semantics.

## bridge.ControllerRuntimeProducer

```go
import "github.com/stakater/operator-utils/observability/pkg/bridge"

producer := bridge.ControllerRuntimeProducer()
```

Returns a `metric.Producer` that reads from controller-runtime's existing
`prometheus.Registry`. Attach it to a `PeriodicReader` via
`sdkmetric.WithProducer` to push controller-runtime metrics through OTLP
(or any other Reader) without touching the registry or the `/metrics`
endpoint.

### When to use

When you're wiring your own `MeterProvider` and want the same
controller-runtime metric flow that `publisher.New` sets up by default:

```go
reader := sdkmetric.NewPeriodicReader(otlpExporter,
    sdkmetric.WithInterval(30*time.Second),
    sdkmetric.WithProducer(bridge.ControllerRuntimeProducer()),
)
```

### Why this flow exists

The bridge is the only mechanism this module uses to get
controller-runtime's Prometheus-native metrics into OTLP. The alternative
— writing OTel metrics back into the Prometheus registry via the OTel
Prometheus exporter — would create a duplication loop: every custom
metric would round-trip through the registry and be re-exported via the
bridge, ending up in OTLP twice.

By keeping the flow one-directional (Prometheus → bridge → OTLP), the
module avoids the loop. There is intentionally no support in this
module for the reverse direction.

## bridge.StartGoRuntime

```go
import "github.com/stakater/operator-utils/observability/pkg/bridge"

if err := bridge.StartGoRuntime(); err != nil { /* handle */ }
```

Starts Go runtime metric collection via
`go.opentelemetry.io/contrib/instrumentation/runtime`. Publishes metrics
through the **global** OTel `MeterProvider` (i.e. whatever
`otel.GetMeterProvider()` returns).

### When to use

When you're wiring your own `MeterProvider` and want the same Go-runtime
metrics that the publisher's `DisableGoRuntime: false` would give you:

```go
mp := sdkmetric.NewMeterProvider(/* ... */)
otel.SetMeterProvider(mp)             // must be set first
_ = bridge.StartGoRuntime()           // then start runtime instrumentation
```

The runtime instrumentation reads the global MeterProvider once at start
time. If you call `bridge.StartGoRuntime` before
`otel.SetMeterProvider`, the metrics flow to the SDK's no-op default
provider and never reach your exporter.

### What it collects

The set of metrics is owned by `otel/contrib/instrumentation/runtime` —
see [its documentation](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/runtime)
for the full list. As of `v0.68.0` it covers GC pauses, heap allocations,
goroutine count, and other standard Go runtime telemetry.

This module does not expose a knob for the runtime instrumentation's
collection interval. Use the upstream API directly if you need that:

```go
_ = runtime.Start(runtime.WithMinimumReadMemStatsInterval(5*time.Second))
```

In that case, skip `bridge.StartGoRuntime` and call `runtime.Start`
yourself with whatever options you want.

## Putting it together

A custom `MeterProvider` that uses all three extension points:

```go
import (
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
    sdkmetric "go.opentelemetry.io/otel/sdk/metric"

    "github.com/stakater/operator-utils/observability/pkg/bridge"
    "github.com/stakater/operator-utils/observability/pkg/resource"
)

func setupMetrics(ctx context.Context) (*sdkmetric.MeterProvider, error) {
    res, err := resource.Build(ctx, "my-op", "1.2.3")
    if err != nil { return nil, err }

    exp, err := otlpmetricgrpc.New(ctx,
        otlpmetricgrpc.WithEndpoint("collector:4317"),
        otlpmetricgrpc.WithInsecure(),
    )
    if err != nil { return nil, err }

    reader := sdkmetric.NewPeriodicReader(exp,
        sdkmetric.WithInterval(30*time.Second),
        sdkmetric.WithProducer(bridge.ControllerRuntimeProducer()),
    )

    mp := sdkmetric.NewMeterProvider(
        sdkmetric.WithResource(res),
        sdkmetric.WithReader(reader),
        // ... any Views, additional Readers, etc.
    )
    otel.SetMeterProvider(mp)

    if err := bridge.StartGoRuntime(); err != nil {
        // log and continue; runtime metrics being absent is non-fatal
    }
    return mp, nil
}
```

This is essentially what `publisher.New` does internally, minus the
config-merging, env-override, and graceful-degradation glue. Reach for
the publisher first; reach for these pieces only when its abstractions
get in your way.
