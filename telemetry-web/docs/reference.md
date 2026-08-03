# telemetry — API reference

A small, framework-agnostic Go library that gives any service consistent
OpenTelemetry **traces + metrics + trace-correlated logs** with a two-call setup.
It is *composition over OpenTelemetry*: it wires the OTel SDK, `otelhttp`, and
`contrib/runtime` in one place and exposes a tiny surface. The core imports **no
web framework**; framework glue lives in separate adapter modules.

- **Module:** `github.com/stakater/operator-utils/telemetry-web`
- **Releasing:** [tag ordering and the pin invariant](releasing.md)
- **Guides:** [Gin (via adapter)](guides/gin-adapter.md) · [Echo (via adapter)](guides/echo-adapter.md) · [chi (via adapter)](guides/chi-adapter.md) · [Echo (raw)](guides/echo-raw.md)

---

## Contents

- [Architecture](#architecture)
- [Installation](#installation)
- [Configuration](#configuration)
- [What gets emitted](#what-gets-emitted)
- [Package reference](#package-reference)
  - [`telemetry`](#package-telemetry) — setup
  - [`telemetry-web/logging`](#package-logging) — trace-correlated logs
  - [`telemetry-web/endpoint`](#package-endpoint) — per-endpoint metrics & panics
  - [`telemetry-web/nethttp`](#package-nethttp) — net/http server & client
  - [`adapters/gin`](#package-adaptersgin) — Gin adapter
  - [`adapters/echo`](#package-adaptersecho) — Echo adapter
  - [`adapters/chi`](#package-adapterschi) — chi adapter
- [Correlation model](#correlation-model)
- [Shutdown](#shutdown)
- [Gotchas](#gotchas)

---

## Architecture

```
                          ┌──────────────────────────────────────────┐
  main() ── Init(cfg) ───▶│ global TracerProvider / MeterProvider /   │
                          │ propagator / resource / sampler / runtime │──▶ OTLP ──▶ Collector
                          └──────────────────────────────────────────┘
  inbound   nethttp.Handler(engine) ─ otelhttp span+metrics ─ recovery ─▶ your routes
  outbound  nethttp.HTTPClient().Do(req) ─ injects traceparent ─▶ next hop
  in-handler endpoint.Instrument / endpoint.Record / endpoint.Recovered
  logs      logging.Logger().InfoContext(ctx, …)  (trace_id/span_id stamped)
```

Everything talks to the **global** OTel providers that `Init` installs, so the
helpers work anywhere in the process without threading a client around. The only
value you must thread is `context.Context` — that is what carries the active span
(for exemplars and log correlation).

Dependency direction inside the module (no cycles):

```
adapters/{gin,echo,chi} ─▶ nethttp ─▶ endpoint ─▶ logging ─▶ internal/scope
nethttp ─────────────────▶ logging
telemetry (root) ────────▶ logging, internal/scope
endpoint ────────────────▶ internal/version
```

---

## Installation

The library is a module nested in this repo (not yet tagged), so pin it to a
commit and let Go resolve the pseudo-version:

```sh
go get github.com/stakater/operator-utils/telemetry-web@<commit-sha>
```

which records something like:

```gomod
require github.com/stakater/operator-utils/telemetry-web v0.0.0-20260708082012-8f1fdddd3dca
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
    Insecure       bool     // export without TLS (dev/local collector)
}
```

| Setting | `Config` field | Env var | Default |
| --- | --- | --- | --- |
| OTLP endpoint | `OTLPEndpoint` | `OTEL_EXPORTER_OTLP_ENDPOINT` | `localhost:4317` |
| Trace sample ratio | `SampleRatio` (`*float64`) | `OTEL_TRACES_SAMPLER_ARG` | `1.0` |
| TLS | `Insecure` (set `true` to disable) | — | TLS on |
| Resource attributes | `ServiceName`/`ServiceVersion`/`Environment` | `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_SERVICE_NAME` | process/host/SDK detectors |
| OTLP protocol | — | `OTEL_EXPORTER_OTLP_PROTOCOL` (+ per-signal) | `grpc` |

**Precedence:** `Config` field → env var → built-in default. For `SampleRatio`,
`nil` means "unset" (fall through to env/default); to sample no new roots pass a
pointer to `0.0`. Sampling is `ParentBased(TraceIDRatioBased(ratio))`, so a
service always honors an upstream sampling decision and you never get half a
trace. An unparseable `OTEL_TRACES_SAMPLER_ARG` is warned about and ignored rather
than silently becoming `1.0`, since failing open to full-volume export on a typo
costs money.

`ServiceName` is required — `Init` returns an error if it is empty.

### OTLP transport

`OTEL_EXPORTER_OTLP_PROTOCOL`, and the per-signal
`OTEL_EXPORTER_OTLP_{TRACES,METRICS}_PROTOCOL` which take precedence over it:

| Value | Exporter | Default port |
| --- | --- | --- |
| unset | gRPC | 4317 |
| `grpc` | gRPC | 4317 |
| `http/protobuf` | HTTP | 4318 |
| `http/json` | HTTP, with a warning | 4318 |
| anything else | gRPC, with a warning | 4317 |

The OpenTelemetry Operator's auto-instrumentation injects this variable into pods,
usually as `http/protobuf`, which is why both transports are built. `http/json` is
served over `http/protobuf`: the Go SDK ships no JSON encoder, and a collector's
OTLP/HTTP receiver takes protobuf on the same port, so this reaches the intended
endpoint instead of falling back to a different one.

**Unset means `grpc`, not the spec's `http/protobuf`.** A deliberate deviation:
gRPC was this library's only transport, and following the spec here would silently
move every existing deployment from 4317 to 4318.

### Env vars this library does not read

| Variable | Why | What you get instead |
| --- | --- | --- |
| `OTEL_TRACES_SAMPLER` | The sampler is fixed | `ParentBased(TraceIDRatioBased(ratio))`. `always_on`/`always_off` are ratios of 1 and 0; the `parentbased_*` variants are already the behaviour. |
| `OTEL_PROPAGATORS` | The propagator is fixed | W3C `tracecontext` + `baggage`. b3/jaeger interop is not wireable. |
| `OTEL_LOGS_EXPORTER` | No log export | Structured JSON on stdout, `trace_id` stamped. See `logging.SetDefault`. |
| `OTEL_SDK_DISABLED` | Not honored | Nothing is disabled; the spec kill switch is inert here. |

Note `OTEL_TRACES_SAMPLER_ARG` **is** read even though `OTEL_TRACES_SAMPLER` is
not, because the ratio is the one part of the sampler that is configurable.

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
| `endpoint.requests` | counter | `endpoint`, `outcome` (success\|failure) | `endpoint.Record` / `Instrument`; adapters only under `WithEndpointMetrics` |
| `endpoint.duration` | histogram (s) | `endpoint`, `outcome` | `endpoint.Instrument` |
| `http.server.panics` | counter | `http.route` (when known) | `endpoint.Recovered` / `endpoint.RecordPanic` |
| `go.goroutine.count`, `go.memory.used`, `go.memory.allocated`, `go.memory.gc.goal`, `go.processor.limit`, `go.config.gogc` | various | — | `contrib/runtime` |

\* `http.route` is populated on the duration metric (and the server span) by the
framework adapters' `RouteTag()` middleware, which calls `nethttp.StampRoute`;
with raw `net/http` you stamp it yourself (see the Echo raw guide).

There is **no** separate `requests_total`: the duration histogram's `count`
already gives request rate, and with `http.route` stamped it also gives the
failure rate per route:

```promql
sum by (http_route) (rate(http_server_request_duration_seconds_count{http_response_status_code=~"5.."}[5m]))
```

That makes `endpoint.requests` a strict subset for HTTP, so the adapters
leave it **off** unless you pass `nethttp.WithEndpointMetrics()`. Turn it on when
you want the simpler `{endpoint, outcome}` label pair instead of status-code
regexes. Where it is not redundant is **non-HTTP** work: `endpoint.Instrument`
covers operations that have no otelhttp histogram at all, and pairs the counter
with `endpoint.duration`.

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
var ErrAlreadyInitialized error // second Init without an intervening shutdown
```

Wires the global `TracerProvider`, `MeterProvider`, propagator, resource,
sampler, OTLP exporters, and runtime metrics. **Call once in `main`.** The
returned `shutdown` flushes all providers — `defer` it (with your own bounded
context; the library does not impose a timeout). If a later setup step fails,
`Init` shuts down anything it already started before returning the error.

Calling `Init` twice without an intervening shutdown returns
`ErrAlreadyInitialized`: a second call would orphan the first pair of providers
(their batch processor goroutine and reader ticker become unreachable) and
re-register the runtime instrumentation, which exposes no `Stop`. The returned
shutdown is idempotent, and once it has run `Init` may be called again.

```go
shutdown, err := telemetry.Init(ctx, telemetry.Config{ServiceName: "mto-gateway"})
if err != nil { log.Fatal(err) }
defer func() { // bounded flush — an unreachable collector must not hang exit
    sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    _ = shutdown(sctx)
}()
```

---

### package `logging`

Trace-correlated `slog`. Import path `…/telemetry-web/logging`.

```go
func Logger() *slog.Logger
func SetDefault(l *slog.Logger)
func NewLogHandler(base slog.Handler) slog.Handler
```

- `Logger()` returns the logger **this library writes through** — endpoint
  instrument warnings, recovered panics, setup diagnostics. It defaults to JSON
  on stdout with `trace_id`, `span_id`, and `service.name` added from the
  context/scope, and is cached, not rebuilt per call.
- `SetDefault(l)` replaces it. Call it before `Init`, otherwise a consuming
  operator that already has a `logr`/zap/slog setup gets a second,
  differently-shaped, unsilenceable stream on stdout. `nil` restores the default.
- `NewLogHandler(base)` wraps any `slog.Handler` with the same stamping — use it
  if you want your own base handler (different writer, level, format). The
  stamps stay top-level even when the logger has open `WithGroup` groups.

```go
logging.SetDefault(slog.New(logging.NewLogHandler(myHandler)))
```

**Rule:** use the `…Context` methods so `ctx` reaches the handler:

```go
logging.Logger().InfoContext(ctx, "created tenant", "id", id)
```

---

### package `endpoint`

Per-endpoint metrics and panic recording. Import path `…/telemetry-web/endpoint`.
Instruments are built lazily on first use and **rebuilt whenever `Init` installs
new providers**, so these are safe to call before `Init` — early calls no-op, and
everything after `Init` lands on the real pipeline. (The rebuild is necessary:
otel's global meter delegates to the first real `MeterProvider` and never
re-delegates, so an instrument created earlier would stay bound to it forever.)

```go
func Instrument(ctx context.Context, name string) func(err *error)
func Record(ctx context.Context, name string, failed bool)
func Recovered(ctx context.Context, recovered any, attrs ...attribute.KeyValue) bool
func RecordPanic(ctx context.Context, recovered any, attrs ...attribute.KeyValue)
```

**`Instrument`** — the ergonomic hand-instrumentation helper, and the intended
entry point for **non-HTTP** operations. It records `endpoint.requests`
*and* times the operation into `endpoint.duration`. Returns a finisher that takes
the *address* of the operation's error; outcome is `failure` iff
`err != nil && *err != nil`:

```go
func (c Controller) ListTenants(ctx context.Context) (err error) {
    defer endpoint.Instrument(ctx, "tenants.list")(&err)
    // ... set err on failure paths; outcome follows the final err ...
}
```

**`Record`** — the low-level primitive when you already know the outcome (e.g.
from an HTTP status). Counter only, no duration. Framework middleware reaches it
through `nethttp.RecordRoute`, which applies the shared `status >= 500` rule:

```go
endpoint.Record(ctx, "/api/v1/tenants/:id", resp.StatusCode >= 500)
```

**`Recovered`** — the whole recovery decision for a framework that has its own
middleware. `false` means the value is `http.ErrAbortHandler` and the caller must
re-panic it; `true` means it was recorded. Every adapter's recovery is this one
call plus a framework-specific 500, so a new framework gets identical semantics for
free:

```go
defer func() {
    if rec := recover(); rec != nil {
        if !endpoint.Recovered(ctx, rec, semconv.HTTPRoute(matchedTemplate)) {
            panic(rec)
        }
        // ... respond 500 the framework's way ...
    }
}()
```

**`RecordPanic`** — records an exception + stack on the active span, logs at error
with `trace_id` **and a `stack` field**, and increments `http.server.panics` with
`attrs`. The stack is on the log as well as the span because a span carries nothing
when tracing is off, the trace was not sampled, or the collector is unreachable,
and a panic is the one event where the stack is the point. It does **not** re-raise
or write a response, and does not filter `ErrAbortHandler`; prefer `Recovered`.

Pass the matched route template in `attrs` so a panic spike names an endpoint.
Nothing is derived from the request automatically: a raw path or method would be
unbounded and blow up the series count.

`name` on `Record`/`Instrument` must be a **low-cardinality constant** (a route
template or a stable operation name), never a raw path or an id.

`endpoint.duration` uses explicit second-scale buckets (5ms to 10s), matching
otelhttp's `http.server.request.duration` so the two are comparable. The SDK
default boundaries are millisecond-scale and would put every operation under five
seconds in one bucket.

---

### package `nethttp`

net/http integration. Import path `…/telemetry-web/nethttp`.

```go
func Handler(next http.Handler, opts ...Option) http.Handler // inbound: otelhttp spans+metrics -> recovery -> next
func WithSkipPaths(paths ...string) Option            // opt out exact paths from instrumentation; repeated calls accumulate
var DefaultSkipPaths = []string{...}                  // /healthz /readyz /livez /metrics — pass to WithSkipPaths
func WithEndpointMetrics() Option                     // opt IN to endpoint.requests in an adapter's Instrument
func WithoutRecovery() Option                         // install NO recovery, not even the adapter's (an outer layer owns panics)
func Resolve(opts ...Option) Settings                 // resolved option set, for adapters
func Skipped(ctx context.Context) bool                // was this request excluded by WithSkipPaths?
func StampRoute(ctx context.Context, method, route string) // http.route on span + duration metric; span renamed "{method} {route}"
func RecordRoute(ctx context.Context, route string, status int) // the shared outcome rule: failure iff status >= 500
func Recovery(next http.Handler) http.Handler         // panic -> RecordPanic + 500 if unwritten (ErrAbortHandler re-raised); preserves Flusher/Hijacker
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
  `WithSkipPaths(paths...)` gives excluded paths no span, no `http.server.*` data
  points, and no `endpoint.requests` (they are still served, and recovery
  still applies). Pass `DefaultSkipPaths` to keep k8s probes and `/metrics`
  scrapes from flooding traces and metrics:
  `Handler(mux, WithSkipPaths(nethttp.DefaultSkipPaths...))` — or append your own
  paths to that list. Repeated calls accumulate rather than replacing.
  Middleware running inside a router checks `Skipped(ctx)` to honor the same
  decision, since otelhttp's own filter cannot reach it.
- **A skipped path drops inbound trace context.** The filter runs before
  `propagators.Extract`, so a skipped path will not continue a caller's trace.
  Harmless for probes; do not skip paths that participate in traces.
- **`StampRoute`** is the route-attribution primitive the adapters' `RouteTag`
  middleware uses; call it directly when wiring a framework by hand. It requires
  the request to be inside `Handler` — without the otelhttp labeler in `ctx` the
  span attribute still lands but the metric attribute is dropped, and a warning
  is logged once.
- **`WithoutRecovery`** suppresses recovery everywhere, not just in `Handler`:
  each adapter's `Instrument` consults `Resolve(...).Recovery` too, so the chain
  is left with none at all. Panics then escape to net/http, which closes the
  connection without a response, and `http.server.panics` is not incremented.
  Only pass it when an outer layer recovers and calls `endpoint.RecordPanic`
  itself. See [Recovery layers](#recovery-layers) for why there are normally two.
- **`RecordRoute`** is the single definition of outcome shared by every adapter:
  `failure` iff `status >= 500`, skipping unmatched routes and skipped paths. A
  4xx is a client error and counts as a success.
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
func Instrument(engine *gin.Engine, opts ...nethttp.Option) http.Handler // Recovery + RouteTag (+ Metrics when opted in) + nethttp.Handler
func Recovery() gin.HandlerFunc  // panic -> endpoint.Recovered + 500 (ErrAbortHandler re-raised)
func RouteTag() gin.HandlerFunc  // http.route (c.FullPath()) -> server span + duration metric
func Metrics() gin.HandlerFunc   // endpoint.requests{endpoint,outcome} by matched route template
```

`RouteTag()` stamps `http.route` on the server span and the standard
`http.server.request.duration` metric (via `nethttp.StampRoute`), so traces and
semconv metrics are route-attributed. `Metrics()` records the per-endpoint
counter and is **opt-in**: pass `nethttp.WithEndpointMetrics()`. `Instrument`
forwards `nethttp` options — e.g.
`Instrument(engine, nethttp.WithSkipPaths(nethttp.DefaultSkipPaths...))`.

Gin applies global middleware only to routes registered *after* `Use`, so
`Instrument` **panics** if the engine already has routes rather than silently
instrumenting nothing. See the [Gin guide](guides/gin-adapter.md).

---

### package `adapters/echo`

Separate module `github.com/stakater/operator-utils/telemetry-web/adapters/echo`,
package name **`echotel`** — deliberately different from the directory so it
never collides with labstack's `echo` (no import alias needed):

```go
import "github.com/stakater/operator-utils/telemetry-web/adapters/echo" // package echotel
```

```go
func Instrument(e *echo.Echo, opts ...nethttp.Option) http.Handler // Recovery + RouteTag (+ Metrics when opted in) + nethttp.Handler
func Recovery() echo.MiddlewareFunc  // panic -> endpoint.Recovered + 500 (ErrAbortHandler re-raised)
func RouteTag() echo.MiddlewareFunc  // http.route (c.Path()) -> server span + duration metric
func Metrics() echo.MiddlewareFunc   // endpoint.requests{endpoint,outcome} by matched route template
```

`RouteTag()` and `Metrics()` split the work exactly like the Gin adapter, and
`Metrics()` is opt-in the same way. Echo runs its error handler *after* the
middleware chain, so the adapter resolves the status the request will actually
answer with — a returned `*echo.HTTPError` reports its own code, any other
returned error becomes Echo's default `500` — and then applies the shared
`status >= 500` rule. Unlike Gin there is no ordering rule: Echo applies `Use`
middleware to routes registered before or after the call. See the
[Echo guide](guides/echo-adapter.md).

---

### package `adapters/chi`

Separate module `github.com/stakater/operator-utils/telemetry-web/adapters/chi`,
package name **`chitel`** — deliberately different from the directory so it
never collides with go-chi's `chi` (no import alias needed):

```go
import "github.com/stakater/operator-utils/telemetry-web/adapters/chi" // package chitel
```

```go
func Instrument(r chi.Router, opts ...nethttp.Option) http.Handler // RouteTag (+ Metrics when opted in) + nethttp.Handler
func Recovery() Middleware   // = nethttp.Recovery; NOT installed by Instrument (see below)
func RouteTag() Middleware   // http.route (RoutePattern()) -> server span + duration metric
func Metrics() Middleware    // endpoint.requests{endpoint,outcome} by matched route pattern
```

Same split as the other adapters, with two chi-specific notes. First, chi fills
`RoutePattern()` in *before* invoking the handler, so `RouteTag` stamps from a
`defer` — a panicked request's span still carries `http.route`, matching gin and
echo. Second, `Instrument` deliberately does **not** install `Recovery()`:
`nethttp.Handler` already recovers and `chitel.Recovery` *is* `nethttp.Recovery`,
so installing both would count every panic twice. Use `Recovery()` only when
building a chain by hand or alongside `nethttp.WithoutRecovery()`.

Mounted subrouters record chi's joined pattern (`/api/users/{id}`) as-is. Like
Gin, call `Instrument` **before** registering routes (chi panics on late `Use`).
See the [chi guide](guides/chi-adapter.md).

---

### The adapter contract

Every framework adapter exposes exactly `Instrument` / `Recovery` / `RouteTag`
/ `Metrics` with the semantics above.

**There is one failure rule, not three.** All adapters route their outcome
through `nethttp.RecordRoute`: `outcome=failure` iff the status the request
answers with is ≥ 500. A 4xx is a client error and records `success`.
Framework-native signals that disagree are deliberately *not* consulted — notably
gin's `c.Error(...)`, which would otherwise make a 400 a failure in gin and a
success everywhere else, breaking `sum by (outcome)` across services.

`http.route` is stamped on every request's span and duration metric, including
panicked ones, on all three adapters.

#### Recovery layers

A panic is **counted exactly once** everywhere, but the wiring differs:

| Adapter | Framework `Recovery()` | `Handler`'s recovery |
| --- | --- | --- |
| gin, echo | installed | kept |
| chi | not installed (`chitel.Recovery` *is* `nethttp.Recovery`) | kept |

For gin and echo the framework layer consumes a handler panic before the outer
one sees it, so two layers still yield one count. The outer layer is not
redundant: it is the only one **outside the engine**, so it is what catches a
panic in middleware running before the framework's recovery — echo's `e.Pre(...)`,
or anything registered with `engine.Use(...)` before `gintel.Instrument`. Without
it those escape to net/http: connection torn down, no response, no
`http.server.panics`, no exception on the span.

Adapters are held to the contract by a shared conformance suite —
package `…/telemetry-web/adaptertest` — that each adapter module runs against
its own engine (`adaptertest.Run`): route-templated metrics (never raw paths),
`500 → failure`, `4xx → success`, `http.route` on span + duration metric + the
`"{method} {route}"` span rename, route retained on panicked requests, unmatched
routes skipped, skipped paths recording nothing at all, panic → `500` + panic
counter + no per-endpoint data point, `http.ErrAbortHandler` re-raised untouched.
Frameworks with a different route parameter syntax pass
`adaptertest.WithTemplateRewrite` (chi rewrites `:id` → `{id}`). Every subtest
issues its own requests and asserts on deltas, so the suite is order-independent
and `-shuffle=on` clean. A new adapter is done when it passes `adaptertest.Run`.

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

`shutdown` is idempotent, and after it runs the globals are back on noop providers
with the log scope cleared, so a goroutine that outlives it writes into a noop
rather than into a dead pipeline. A failed `Init` restores the same state, so a
caller that logs the error and keeps serving gets a service that records nothing
rather than one wired to a provider it already shut down.

### Re-`Init` after shutdown

Supported, but with one caveat worth knowing before you build a config-reload path
on it: an `http.Handler` built by `nethttp.Handler` **before** the re-`Init`

- keeps producing **spans**, because otelhttp resolves its tracer per request, and
- stops producing **`http.server.*` metrics**, because it resolves its meter once
  at construction and this library cannot reach inside it to rebind.

`endpoint.*` metrics survive either way, because the `endpoint` package compares the
global `MeterProvider` on each use and rebuilds its instruments when it changes.
Rebuild the handler after a re-`Init` if you depend on `http.server.*`.

---

## Gotchas

- **Propagation is two halves.** `Handler` (inbound) + `HTTPClient`/`Transport`
  (outbound). Miss the outbound half and the trace stops at that service.
- **Background goroutines / queues lose the trace** unless you pass `ctx` in (or,
  for queues, inject/extract trace context in the message metadata).
- **`name` must be low-cardinality.** Use route templates or fixed operation
  names for `endpoint.Record`/`Instrument`, never raw paths or ids.
- **Panicked requests** show on `http.server.panics` (+ a `500` in the otelhttp
  metrics), not as a per-endpoint `endpoint.requests` data point — by design.
- **A panic mid-response keeps the status it already sent.** `nethttp.Recovery`
  writes `500` only if nothing has been committed yet, so a handler that
  panicked after starting its response is left alone rather than producing a
  "superfluous WriteHeader" warning.
