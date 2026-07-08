# telemetry — API reference

A small, framework-agnostic Go library that gives any service consistent
OpenTelemetry **traces + metrics + trace-correlated logs** with a two-call setup.
It is *composition over OpenTelemetry*: it wires the OTel SDK, `otelhttp`, and
`contrib/runtime` in one place and exposes a tiny surface. The core imports **no
web framework**; framework glue lives in separate adapter modules.

- **Module:** `github.com/stakater/operator-utils/telemetry-web`
- **Guides:** [Gin (via adapter)](guides/gin-adapter.md) · [Echo (via adapter)](guides/echo-adapter.md) · [Echo (raw)](guides/echo-raw.md)

---

## Contents

- [Architecture](#architecture)
- [Installation](#installation)
- [Configuration](#configuration)
- [What gets emitted](#what-gets-emitted)
- [Package reference](#package-reference)
  - [`telemetry`](#package-telemetry) — setup
  - [`telemetry/logging`](#package-logging) — trace-correlated logs
  - [`telemetry/endpoint`](#package-endpoint) — per-endpoint metrics & panics
  - [`telemetry/nethttp`](#package-nethttp) — net/http server & client
  - [`adapters/gin`](#package-adaptersgin) — Gin adapter
  - [`adapters/echo`](#package-adaptersecho) — Echo adapter
- [Correlation model](#correlation-model)
- [Shutdown](#shutdown)
- [Gotchas](#gotchas)

---

## Architecture

```
                          ┌──────────────────────────────────────────┐
  main() ── Init(cfg) ───▶│ global TracerProvider / MeterProvider /   │
                          │ propagator / resource / sampler / runtime │──▶ OTLP/gRPC ──▶ Collector
                          └──────────────────────────────────────────┘
  inbound   nethttp.Handler(engine) ─ otelhttp span+metrics ─ recovery ─▶ your routes
  outbound  nethttp.HTTPClient().Do(req) ─ injects traceparent ─▶ next hop
  in-handler endpoint.Instrument / endpoint.Record / endpoint.RecordPanic
  logs      logging.Logger().InfoContext(ctx, …)  (trace_id/span_id stamped)
```

Everything talks to the **global** OTel providers that `Init` installs, so the
helpers work anywhere in the process without threading a client around. The only
value you must thread is `context.Context` — that is what carries the active span
(for exemplars and log correlation).

Dependency direction inside the module (no cycles):
`adapters/{gin,echo} → nethttp → endpoint → logging → internal/scope → otel`.

---

## Installation

The library is a module nested in this repo (not yet tagged), so pin it to a
commit and let Go resolve the pseudo-version:

```sh
go get github.com/stakater/operator-utils/telemetry-web@<commit-sha>
```

which records something like:

```gomod
require github.com/stakater/operator-utils/telemetry-web v0.0.0-20260707172313-d97fce7d0cfb
```

The framework adapters are **separate** nested modules — add one the same way
only if you use it:

```sh
go get github.com/stakater/operator-utils/telemetry-web/adapters/gin@<commit-sha>   # or adapters/echo
```

Then `go mod tidy`. Prefer this pinned pseudo-version over a local `replace`
directive: it is reproducible for every clone and in CI, while a `replace`
points at a path that only exists on your machine. Reserve `replace` (or a
`go.work` file) for developing the library itself against a consuming service:

```gomod
replace github.com/stakater/operator-utils/telemetry-web => ../operator-utils/telemetry-web   // local dev only
```

---

## Configuration

`Init` reads standard `OTEL_*` env vars where they exist; `Config` fields
override them.

```go
type Config struct {
    ServiceName    string   // REQUIRED — labels every span/metric/log (service.name)
    ServiceVersion string   // optional — service.version resource attribute
    Environment    string   // optional — deployment.environment.name (e.g. "prod")
    OTLPEndpoint   string   // optional — else OTEL_EXPORTER_OTLP_ENDPOINT, else localhost:4317
    SampleRatio    *float64 // optional — nil = use env/default; a set pointer (incl. &0.0) is used verbatim
    Insecure       bool     // gRPC without TLS (dev/local collector)
}
```

| Setting | `Config` field | Env var | Default |
| --- | --- | --- | --- |
| OTLP endpoint | `OTLPEndpoint` | `OTEL_EXPORTER_OTLP_ENDPOINT` | `localhost:4317` |
| Trace sample ratio | `SampleRatio` (`*float64`) | `OTEL_TRACES_SAMPLER_ARG` | `1.0` |
| TLS | `Insecure` (set `true` to disable) | — | TLS on |

**Precedence:** `Config` field → env var → built-in default. For `SampleRatio`,
`nil` means "unset" (fall through to env/default); to sample no new roots pass a
pointer to `0.0`. Sampling is `ParentBased(TraceIDRatioBased(ratio))`, so a
service always honors an upstream sampling decision and you never get half a
trace.

`ServiceName` is required — `Init` returns an error if it is empty.

---

## What gets emitted

**Traces** — one server span per inbound request (via `otelhttp`), propagated
across hops with W3C `traceparent` + `baggage`. Add your own child spans for
high-value operations.

**Metrics** (over OTLP):

| Metric | Type | Attributes | Source |
| --- | --- | --- | --- |
| `http.server.request.duration` | histogram (s) | method, route*, status, … | `otelhttp` (via `nethttp.Handler`) |
| `http.server.active_requests` | up/down counter | method, scheme | `otelhttp` |
| `http.endpoint.requests` | counter | `endpoint`, `outcome` (success\|failure) | `endpoint.Record` / `Instrument` |
| `http.server.panics` | counter | — | `endpoint.RecordPanic` |
| `runtime.*` (goroutines, GC, heap…) | various | — | `contrib/runtime` |

\* `http.route` is populated on the duration metric (and the server span) by
the framework adapters' `Metrics()` middleware; with raw `net/http` you'd have
to stamp it yourself (see the Echo raw guide).

There is **no** separate `requests_total`: the duration histogram's `count`
already gives request rate. `http.endpoint.requests` **deliberately overlaps**
with the route-attributed duration histogram — it exists as a dead-simple
success/failure signal (`outcome` label instead of status-code regexes in every
query), and `endpoint.Record`/`Instrument` also serve non-HTTP operations that
have no otelhttp metric.

Health probes and scrape endpoints are instrumented like any other path unless
you opt out — pass `nethttp.WithSkipPaths(nethttp.DefaultSkipPaths...)` to keep
k8s probe noise out of traces and metrics.

**Logs** — structured JSON to stdout, each record stamped with `trace_id`,
`span_id`, and `service.name` when a span is active.

---

## Package reference

### package `telemetry`

Setup. Import path `github.com/stakater/operator-utils/telemetry-web`.

```go
func Init(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error)
```

Wires the global `TracerProvider`, `MeterProvider`, propagator, resource,
sampler, OTLP exporters, and runtime metrics. **Call once in `main`.** The
returned `shutdown` flushes all providers — `defer` it (with your own bounded
context; the library does not impose a timeout). If a later setup step fails,
`Init` shuts down anything it already started before returning the error.

```go
shutdown, err := telemetry.Init(ctx, telemetry.Config{ServiceName: "mto-gateway"})
if err != nil { log.Fatal(err) }
defer shutdown(context.Background())
```

---

### package `logging`

Trace-correlated `slog`. Import path `…/telemetry/logging`.

```go
func Logger() *slog.Logger
func NewLogHandler(base slog.Handler) slog.Handler
```

- `Logger()` returns a `*slog.Logger` writing JSON to stdout with `trace_id`,
  `span_id`, and `service.name` added from the context/scope.
- `NewLogHandler(base)` wraps any `slog.Handler` with the same stamping — use it
  if you want your own base handler (different writer, level, format).

**Rule:** use the `…Context` methods so `ctx` reaches the handler:

```go
logging.Logger().InfoContext(ctx, "created tenant", "id", id)
```

---

### package `endpoint`

Per-endpoint metrics and panic recording. Import path `…/telemetry/endpoint`.
Instruments are created lazily from the global meter, so these are safe to call
before `Init` (they no-op against the default provider until a real one is set).

```go
func Instrument(ctx context.Context, name string) func(err *error)
func Record(ctx context.Context, name string, failed bool)
func RecordPanic(ctx context.Context, recovered any)
```

**`Instrument`** — the ergonomic hand-instrumentation helper. Returns a finisher
that takes the *address* of the operation's error; outcome is `failure` iff
`err != nil && *err != nil`:

```go
func (c Controller) ListTenants(ctx context.Context) (err error) {
    defer endpoint.Instrument(ctx, "tenants.list")(&err)
    // ... set err on failure paths; outcome follows the final err ...
}
```

**`Record`** — the low-level primitive when you already know the outcome (e.g.
from an HTTP status). Framework middleware uses this:

```go
endpoint.Record(ctx, "/api/v1/tenants/:id", resp.StatusCode >= 500)
```

**`RecordPanic`** — records an exception + stack on the active span, logs at
error with `trace_id`, and increments `http.server.panics`. It does **not**
re-raise or write a response — your recovery middleware decides how to respond.
`name` on `Record`/`Instrument` must be a **low-cardinality constant** (a route
template or a stable operation name), never a raw path or an id.

---

### package `nethttp`

net/http integration. Import path `…/telemetry/nethttp`.

```go
func Handler(next http.Handler, opts ...Option) http.Handler // inbound: otelhttp spans+metrics -> recovery -> next
func WithSkipPaths(paths ...string) Option            // opt out exact paths from instrumentation
var DefaultSkipPaths = []string{...}                  // /healthz /readyz /livez /metrics — pass to WithSkipPaths
func StampRoute(ctx context.Context, route string)    // put http.route on the active span + duration metric
func Recovery(next http.Handler) http.Handler         // panic -> RecordPanic + 500 (ErrAbortHandler re-raised)
func Transport(base http.RoundTripper) http.RoundTripper // outbound: inject trace context
func HTTPClient() *http.Client                        // client whose Transport already propagates
func WrapClient(c *http.Client) *http.Client          // add propagation to an existing client in place
```

- **`Handler`** wraps a whole `http.Handler` (a mux, or any framework engine —
  `*gin.Engine` and `*echo.Echo` both implement `http.Handler`). It gives you
  server spans + the standard `http.server.*` metrics and an innermost panic
  backstop. Recovery sits inside `otelhttp` so the span exists in `ctx` and the
  resulting `500` is measured.
- **Filtering is opt-in.** Every path is instrumented unless you exclude it:
  `WithSkipPaths(paths...)` gives excluded paths no span and no `http.server.*`
  data points (they are still served, and recovery still applies). Pass
  `DefaultSkipPaths` to keep k8s probes and `/metrics` scrapes from flooding
  traces and metrics: `Handler(mux, WithSkipPaths(nethttp.DefaultSkipPaths...))`
  — or append your own paths to that list.
- **`StampRoute`** is the route-attribution primitive the adapters' `RouteTag`
  middleware uses; call it directly when wiring a framework by hand.
- **Outbound propagation is opt-in and non-negotiable:** only calls made through
  `Transport`/`HTTPClient`/`WrapClient` inject `traceparent`. A hop that uses a
  plain client dead-ends the trace even though every service has `Handler`.

```go
http.ListenAndServe(":8080", nethttp.Handler(mux))

req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
resp, err := nethttp.HTTPClient().Do(req) // trace rides along, via req.Context()
```

The outbound request must be built with `http.NewRequestWithContext(ctx, …)` —
the transport injects `traceparent` from the request's context, so a request
made without `ctx` dead-ends the trace even through a propagating client.

---

### package `adapters/gin`

Separate module `github.com/stakater/operator-utils/telemetry-web/adapters/gin`,
package name **`gintel`** — deliberately different from the directory so it never
collides with gin-gonic's `gin` (no import alias needed):

```go
import "github.com/stakater/operator-utils/telemetry-web/adapters/gin" // package gintel
```

```go
func Instrument(engine *gin.Engine, opts ...nethttp.Option) http.Handler // Recovery + RouteTag + Metrics + nethttp.Handler
func Recovery() gin.HandlerFunc  // panic -> endpoint.RecordPanic + 500 (ErrAbortHandler re-raised)
func RouteTag() gin.HandlerFunc  // http.route (c.FullPath()) -> server span + duration metric
func Metrics() gin.HandlerFunc   // http.endpoint.requests{endpoint,outcome} by matched route template
```

`RouteTag()` stamps `http.route` on the server span and the standard
`http.server.request.duration` metric (via `nethttp.StampRoute`), so traces and
semconv metrics are route-attributed; `Metrics()` records the per-endpoint
counter. Use `RouteTag` without `Metrics` for a semconv-only setup. `Instrument`
forwards `nethttp` options — e.g.
`Instrument(engine, nethttp.WithSkipPaths(nethttp.DefaultSkipPaths...))`.
See the [Gin guide](guides/gin-adapter.md).

---

### package `adapters/echo`

Separate module `github.com/stakater/operator-utils/telemetry-web/adapters/echo`,
package name **`echotel`** — deliberately different from the directory so it
never collides with labstack's `echo` (no import alias needed):

```go
import "github.com/stakater/operator-utils/telemetry-web/adapters/echo" // package echotel
```

```go
func Instrument(e *echo.Echo, opts ...nethttp.Option) http.Handler // Recovery + RouteTag + Metrics + nethttp.Handler
func Recovery() echo.MiddlewareFunc  // panic -> endpoint.RecordPanic + 500 (ErrAbortHandler re-raised)
func RouteTag() echo.MiddlewareFunc  // http.route (c.Path()) -> server span + duration metric
func Metrics() echo.MiddlewareFunc   // http.endpoint.requests{endpoint,outcome} by matched route template
```

`RouteTag()` and `Metrics()` split the work exactly like the Gin adapter:
route attribution on span + duration metric, and the per-endpoint counter,
respectively. `Instrument` forwards `nethttp` options the same way. Outcome
classification is Echo-aware: a returned `*echo.HTTPError` counts as `failure`
only when its code is ≥ 500 (a returned 4xx is a client error, hence
`success`), any other returned error is a `failure` (Echo turns it into a
`500`), and a directly written ≥ 500 status is caught too. Unlike the Gin
adapter there is no register-routes-after-instrumenting ordering rule. See the
[Echo guide](guides/echo-adapter.md).

---

### The adapter contract

Every framework adapter exposes exactly `Instrument` / `Recovery` / `RouteTag`
/ `Metrics` with the semantics above, and is held to them by a shared
conformance suite —
package `…/telemetry/adaptertest` — that each adapter module runs against its
own engine (`adaptertest.Run`): route-templated metrics (never raw paths),
`500 → failure`, `http.route` on span + duration metric, unmatched routes
skipped, panic → `500` + panic counter + no per-endpoint data point,
`http.ErrAbortHandler` re-raised untouched. A new adapter is done when it
passes `adaptertest.Run`.

---

## Correlation model

- **metrics ↔ traces:** exemplars, on by default in the SDK whenever a sampled
  span is active in `ctx`. This is why you thread `ctx` into `Record`/`Instrument`
  — never put `trace_id` on a metric as an attribute (unbounded cardinality).
- **traces ↔ logs:** the `trace_id`/`span_id` fields the logging handler stamps
  from `ctx`.

---

## Shutdown

`Init` returns a `shutdown func(context.Context) error`. Call it on process exit
with a bounded context so the last batch of spans/metrics flushes on deploy:

```go
shutdown, _ := telemetry.Init(ctx, cfg)
// ...
sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
_ = shutdown(sctx)
```

Note: shutting down flushes a final metric export; against an unreachable
collector `shutdown` may return a non-nil error — that is expected on teardown
and generally safe to log-and-ignore.

---

## Gotchas

- **Propagation is two halves.** `Handler` (inbound) + `HTTPClient`/`Transport`
  (outbound). Miss the outbound half and the trace stops at that service.
- **Background goroutines / queues lose the trace** unless you pass `ctx` in (or,
  for queues, inject/extract trace context in the message metadata).
- **`name` must be low-cardinality.** Use route templates or fixed operation
  names for `endpoint.Record`/`Instrument`, never raw paths or ids.
- **Panicked requests** show on `http.server.panics` (+ a `500` in the otelhttp
  metrics), not as a per-endpoint `http.endpoint.requests` data point — by design.
- **`nethttp.Recovery` writes `500` unconditionally.** If a handler already wrote
  a status before panicking, that write is a no-op (Go logs "superfluous
  WriteHeader") — inherent to the simple recovery pattern.
