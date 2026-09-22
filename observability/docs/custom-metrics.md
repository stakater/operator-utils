# CustomMetrics

`publisher.CustomMetrics` is the registry for operator-defined metrics.
Obtain it via `pub.Custom()`. It wraps a single OTel `Meter` and tracks
registered names to reject duplicates across instrument types.

```go
custom := pub.Custom()
```

## API surface

```go
type CustomMetrics struct { /* unexported fields */ }

func (c *CustomMetrics) Counter(name, description string)   (instrument.Counter,   error)
func (c *CustomMetrics) Gauge(name, description string)     (instrument.Gauge,     error)
func (c *CustomMetrics) Histogram(name, description string) (instrument.Histogram, error)

func (c *CustomMetrics) MustCounter(name, description string)   instrument.Counter
func (c *CustomMetrics) MustGauge(name, description string)     instrument.Gauge
func (c *CustomMetrics) MustHistogram(name, description string) instrument.Histogram
```

All six functions are safe to call concurrently. The registry uses a
mutex around its name table.

## Registration parameters

### `name string`

Must satisfy `naming.ValidateMetricName`:

- Match the regex `^[a-z][a-z0-9_]*$` (lowercase ASCII, digits,
  underscores; must start with a letter).
- Must not start with any **reserved prefix**:
  - `otel_`
  - `go_`
  - `process_`
  - `controller_runtime_`
  - `workqueue_`
  - `rest_client_`

Reserved prefixes match metrics that come from the Go runtime,
controller-runtime, or other OTel-internal sources. Allowing custom
metrics with these prefixes would create ambiguous time series
downstream, and would now genuinely collide: the Prometheus reader
exposes custom metrics on the same registry those sources use. The same
prefixes also keep the OTLP bridge able to tell our families apart from
controller-runtime's by name alone.

Counter names may end in `_total` or not: the Prometheus exporter appends
that suffix only when it is missing, so `reconcile` and `reconcile_total`
are both served as `reconcile_total`.

Pick one spelling per metric. Because both land on the same `/metrics`
family, registering both on one publisher would give that family two
sources, which fails the whole registry's gather and makes controller-runtime
serve a 500 on every scrape. Registration therefore reserves the translated
name as well as the one you passed, and the second registration fails:

```
metric "reconcile_total" collides with metric "reconcile": both are served as "reconcile_total" on /metrics
```

The same applies across instrument types, since only counters get the
suffix: a Gauge named `widgets_total` and a Counter named `widgets` are
one family on `/metrics`, and the second one is rejected. The name you
pass is never rewritten, only checked.

Names are tracked across instrument types. Registering a Counter named
`"reconcile_total"` makes a subsequent Gauge or Histogram registration
with the same name fail — even though they are different instrument
shapes at the OTel level.

### `description string`

Must be non-empty. The description is passed through to the OTel SDK as
the instrument's description metadata, which most OTLP backends surface
as the metric's help text.

There is no length limit enforced by this module.

## Error returns

`Counter` / `Gauge` / `Histogram` return non-nil errors for:

| Cause | Error wording |
|---|---|
| Empty name | `metric name must not be empty` |
| Name fails regex | `metric name %q must match ^[a-z][a-z0-9_]*$` |
| Name uses reserved prefix | `metric name %q uses reserved prefix %q` |
| Empty description | `metric %q: description must not be empty` |
| Name already registered | `metric %q already registered` |
| OTel SDK rejects the instrument | `create counter %q: %w` (similarly for gauge/histogram) |

The last one shouldn't happen in practice — anything that fails the OTel
SDK's instrument creation would already have failed `ValidateMetricName`
first. But it is wrapped properly with `%w` for completeness.

## Must* variants

`MustCounter`, `MustGauge`, and `MustHistogram` panic on any error
returned by the non-`Must` variant. They are intended for **package-level
var initialisation**, where a registration failure is a programming bug
and should crash the process loudly at startup rather than be silently
ignored.

```go
package mycontroller

var (
    reconcileTotal = pub.Custom().MustCounter(
        "reconcile_total",
        "Total reconciliations attempted by the controller",
    )
    activeWorkers = pub.Custom().MustGauge(
        "active_workers",
        "Current number of active worker goroutines",
    )
)
```

Do not use `Must*` for runtime-driven registration (e.g. metrics whose
names depend on user input) — that's what the error-returning variants
are for.

## Duplicate-name semantics

Name reservation happens atomically before the OTel instrument is
created. The first successful registration of a name "claims" it for the
lifetime of the `CustomMetrics` instance.

OTel's own meter would happily return the same underlying instrument if
you call `Int64Counter("x", ...)` twice — but the *second* description
silently wins, which has bitten users in practice. By tracking names
explicitly, this module surfaces accidental double-registration as an
error.

The reservation is per-`CustomMetrics` instance. A second `Publisher`
constructed in the same process (don't do this) would have its own
registry.

## Validating names at startup

Names typically come from package-level var initialisation, which means
errors only surface when the var-init runs. If you want a single up-front
validation pass — for instance, an operator that loads metric names from
configuration — use `naming.ValidateMetricName` directly:

```go
import "github.com/stakater/operator-utils/observability/pkg/naming"

for _, name := range cfg.MetricNames {
    if err := naming.ValidateMetricName(name); err != nil {
        log.Fatalf("invalid metric name %q: %v", name, err)
    }
}
```

`naming.ValidateAttributeKey` is also exported for symmetric reasons,
though attribute keys are not validated by the instrument recording calls
at runtime.
