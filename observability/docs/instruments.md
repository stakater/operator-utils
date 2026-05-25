# Instruments

Three instrument types are supported: **Counter**, **Gauge**, and
**Histogram**. The interfaces live in the `instrument` package and are
returned by `publisher.CustomMetrics` registration methods.

```go
import "github.com/stakater/operator-utils/observability/pkg/instrument"
```

All three accept variadic `attribute.KeyValue` arguments
(`go.opentelemetry.io/otel/attribute`) so per-call attributes can be
attached without going through the OTel SDK directly.

## Counter

Monotonically increasing integer counter. Use for things that only ever
grow during a process's lifetime.

```go
type Counter interface {
    Inc(ctx context.Context, attrs ...attribute.KeyValue)
    Add(ctx context.Context, value int64, attrs ...attribute.KeyValue)
}
```

**`Inc`** — increments by 1. Equivalent to `Add(ctx, 1, attrs...)`.

**`Add`** — adds `value` to the counter. `value` **must** be non-negative;
the OTel SDK silently drops negative additions and emits a warning on its
internal logger.

### When to use

- "How many times has reconcile been called?"
- "How many errors have been observed?"
- "How many bytes have been processed?"

### Example

```go
reconcileTotal := pub.Custom().MustCounter(
    "reconcile_total",
    "Total reconciliations attempted by the controller",
)

// At call sites:
reconcileTotal.Inc(ctx, attribute.String("result", "success"))
reconcileTotal.Add(ctx, batchSize, attribute.String("queue", queueName))
```

## Gauge

Instantaneous integer measurement (sync gauge). Use for values that can
go up or down and represent a current state rather than a cumulative total.

```go
type Gauge interface {
    Set(ctx context.Context, value int64, attrs ...attribute.KeyValue)
}
```

**`Set`** — records the current value. The most recent `Set` wins for a
given attribute set within an export interval; intermediate values are
not exported.

Implemented on top of the OTel SDK's native sync `Int64Gauge`
(stable as of `otel/sdk/metric` v1.28).

### When to use

- "How many workers are currently active?"
- "How many items are in the work queue right now?"
- "What is the current size of the cache?"

### Example

```go
activeWorkers := pub.Custom().MustGauge(
    "active_workers",
    "Current number of active worker goroutines",
)

activeWorkers.Set(ctx, int64(workers.Len()))
```

## Histogram

Records a distribution of `float64` measurements. Use for things where
the *shape* of values matters (latency, sizes, durations).

```go
type Histogram interface {
    Record(ctx context.Context, value float64, attrs ...attribute.KeyValue)
}
```

**`Record`** — adds a sample. The OTel SDK aggregates samples into
configurable buckets and reports count, sum, min, max, and bucket counts
to the exporter.

Bucket boundaries come from the OTel SDK default (see
[opentelemetry.io](https://opentelemetry.io/docs/specs/otel/metrics/sdk/#explicit-bucket-histogram-aggregation)).
This module does not currently expose a knob to customise them; if you
need custom buckets, use `pub.Meter()` to obtain the underlying OTel
`Meter` and register a histogram directly with a `View`.

### When to use

- "How long does a reconcile take?"
- "How large are the API responses?"
- "How long do items sit in the queue before processing?"

### Example

```go
reconcileDuration := pub.Custom().MustHistogram(
    "reconcile_duration_seconds",
    "Wall-clock duration of a reconcile call, in seconds",
)

start := time.Now()
// ... do reconcile ...
reconcileDuration.Record(ctx, time.Since(start).Seconds(),
    attribute.String("result", "success"),
)
```

## Attributes

All three instruments accept `attribute.KeyValue` arguments on every
recording call. Attributes split a single metric into per-attribute-set
time series in the backend.

```go
import "go.opentelemetry.io/otel/attribute"

counter.Add(ctx, 1,
    attribute.String("namespace", req.Namespace),
    attribute.String("result", "success"),
)
```

### Cardinality warning

Every distinct combination of attribute values produces a separate time
series in the OTLP backend. High-cardinality attribute values (UIDs,
request IDs, full namespaces in a large cluster) can explode storage
costs downstream.

Prefer bounded enumerations: `result=success|error`, `kind=Pod|Deployment`,
etc.

### Attribute key validation

Attribute keys are not validated by `Counter.Add` / `Gauge.Set` /
`Histogram.Record` at runtime — the SDK takes whatever you pass. If you
want to enforce keys at startup, call `naming.ValidateAttributeKey`
yourself in init:

```go
if err := naming.ValidateAttributeKey("my_attr"); err != nil {
    log.Fatalf("invalid attribute key: %v", err)
}
```

See [custom-metrics.md](custom-metrics.md) for the validation rules.

## Implementation details

The `instrument` package defines **only** interfaces. The concrete types
(`counterImpl`, `gaugeImpl`, `histogramImpl`) live unexported inside
`publisher` because they're constructed alongside the underlying OTel
instrument. Consumers receive only the interface — they cannot reach the
underlying OTel instrument from the returned handle.

If you need direct SDK access (e.g. to register an observable callback or
a `View`), use `pub.Meter()` instead of `pub.Custom()`. See the publisher
section of [configuration.md](configuration.md) for the escape hatch.

## What is *not* supported

- **Observable / async instruments.** No observable counters, observable
  gauges, or observable up-down counters. Only synchronous instruments.
- **UpDownCounter.** The Gauge interface covers most use cases that
  would otherwise want an `UpDownCounter`.
- **Float64Counter / Int64Histogram.** Counters are int64-only;
  histograms are float64-only. If your use case needs the opposite, use
  `pub.Meter()` directly.
