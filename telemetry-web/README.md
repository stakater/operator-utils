# telemetry

A small, **framework-agnostic** Go library that gives any service consistent
OpenTelemetry **traces + metrics + trace-correlated logs** with a two-call setup.
It is *composition over OpenTelemetry* — it leans on the OTel SDK, `otelhttp`, and
`contrib/runtime`; it does not hand-roll spans, propagation, or instruments, and
it imports **no web framework**. Its only non-OTel dependency is
[`felixge/httpsnoop`](https://github.com/felixge/httpsnoop), already in the graph
via `otelhttp`, used so panic recovery can wrap a `ResponseWriter` without
dropping `Flusher`/`Hijacker`.

```
import (
    "github.com/stakater/operator-utils/telemetry-web"
    "github.com/stakater/operator-utils/telemetry-web/endpoint"
    "github.com/stakater/operator-utils/telemetry-web/logging"
    "github.com/stakater/operator-utils/telemetry-web/nethttp"
)
```

> The root import path ends in `telemetry-web`, but the package it declares is
> **`telemetry`** — a hyphen is not a legal Go identifier. Refer to it as
> `telemetry.Init`, no alias needed.

Telemetry is exported over **OTLP/gRPC only** to an OpenTelemetry Collector. Logs
go to structured JSON on stdout with `trace_id` stamped in, unless you redirect
them with `logging.SetDefault`.

## Documentation

- [API reference](docs/reference.md) — configuration, emitted signals, full package-by-package API.
- [Gin adapter](docs/guides/gin-adapter.md) — the one-call `gintel.Instrument(engine)` path.
- [Echo adapter](docs/guides/echo-adapter.md) — the one-call `echotel.Instrument(e)` path.
- [chi adapter](docs/guides/chi-adapter.md) — the one-call `chitel.Instrument(r)` path.
- [Echo integration (raw)](docs/guides/echo-raw.md) — wiring a framework with the core primitives by hand.

---

## Quick start

```go
func main() {
    ctx := context.Background()

    shutdown, err := telemetry.Init(ctx, telemetry.Config{
        ServiceName:    "mto-gateway",
        ServiceVersion: version,   // optional
        Environment:    "prod",    // optional
        Insecure:       true,      // dev/local collector without TLS
    })
    if err != nil {
        log.Fatal(err)
    }
    defer func() { // bounded flush — an unreachable collector must not hang exit
        sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        _ = shutdown(sctx)
    }()

    mux := http.NewServeMux()
    // ... register routes ...

    // Handler adds spans, HTTP server metrics, and panic recovery.
    http.ListenAndServe(":8080", nethttp.Handler(mux))
}
```

Two rules to get value out of it:

1. Wrap your server with `nethttp.Handler` (inbound spans/metrics) **and** make
   cross-service calls through `nethttp.HTTPClient()` (outbound propagation).
   Propagation has two halves — a hop that skips the client dead-ends the trace.
2. Log with the `...Context` methods (`InfoContext`, `ErrorContext`) so `ctx`
   reaches the handler and `trace_id` lands in the log line.

---

## Configuration

`Config` is the entire configuration surface. `ServiceName` is required; everything
else is optional and falls back to standard `OTEL_*` env vars, then defaults.

| Field            | Type       | Meaning                                                                 |
|------------------|------------|-------------------------------------------------------------------------|
| `ServiceName`    | `string`   | **Required.** How this service labels itself on every span/metric/log.  |
| `ServiceVersion` | `string`   | Optional build/version tag. Omitted from the resource when empty.       |
| `Environment`    | `string`   | Optional `"prod"`/`"staging"`/… Omitted from the resource when empty.   |
| `OTLPEndpoint`   | `string`   | Optional collector endpoint. Overrides `OTEL_EXPORTER_OTLP_ENDPOINT`.   |
| `SampleRatio`    | `*float64` | `nil` = unset; a non-nil pointer is used verbatim, **including `0`** to never sample new roots. |
| `Insecure`       | `bool`     | gRPC without TLS (local/dev collector).                                 |

**Environment variables** (config fields take precedence):

- `OTEL_EXPORTER_OTLP_ENDPOINT` — collector address (default `localhost:4317`).
  Spec URL form (`http://host:4317`) and bare `host:port` are both accepted.
- `OTEL_EXPORTER_OTLP_{TRACES,METRICS}_ENDPOINT` — per-signal overrides.
- `OTEL_TRACES_SAMPLER_ARG` — head-sampling ratio (default `1.0`).
- `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_SERVICE_NAME` — merged into the resource;
  explicit `Config` fields win.

**Not supported:** `OTEL_EXPORTER_OTLP_PROTOCOL` (and its per-signal variants).
This library builds gRPC exporters only. Any other value logs a warning naming
the variable; `Init` still succeeds and the service still boots — telemetry just
will not reach the collector.

> Worth knowing before you deploy: the OpenTelemetry Operator's
> auto-instrumentation injects `OTEL_EXPORTER_OTLP_PROTOCOL` into pods, often as
> `http/protobuf`. That is why this warns instead of failing — a library whose job
> is observability must not be able to crash-loop the service it observes. If you
> see that warning, point the variable at a gRPC (OTLP) receiver or remove it.

Sampling is `ParentBased(TraceIDRatioBased(ratio))`, so a service honors an upstream
sampling decision — you never get half a trace.

---

## Exported API

This is the **entire** public surface. Everything else in the package is
unexported on purpose (providers, exporter constructors, instruments, the slog
handler type, ratio/endpoint resolution) — consumers never touch it.

### Setup

- **`Init(ctx, Config) (shutdown func(context.Context) error, err error)`**
  Wires the global tracer/meter providers, the W3C propagator, the resource, the
  sampler, OTLP exporters, and runtime (goroutine/GC/heap) metrics. Call **once**
  in `main`, and `defer shutdown(...)` under a bounded context. Returns an error if
  `ServiceName` is empty or the resource can't be built; if a later setup step
  fails, already-started providers are cleaned up before returning.

  Calling it twice without an intervening shutdown returns `ErrAlreadyInitialized`
  rather than orphaning the first pair of providers. The returned shutdown is
  idempotent, and after it runs `Init` may be called again.

### Inbound HTTP (net/http) — `nethttp`

- **`Handler(next http.Handler, opts ...Option) http.Handler`**
  The composed inbound chain: `otelhttp` (spans + server metrics) → `Recovery`
  (panic → 500) → your handler. Wrap your router once. `gin.Engine`,
  `chi.Mux`, etc. all satisfy `http.Handler`, so you can wrap them too:
  `nethttp.Handler(ginEngine)`.
- **`Recovery(next http.Handler) http.Handler`**
  Exported separately for teams composing their own chain — it must sit **inside**
  the `otelhttp` handler. `Handler` already includes it. It calls `endpoint.RecordPanic`
  then writes `500` **if nothing has been written yet**; it re-raises
  `http.ErrAbortHandler` for net/http to handle. The `ResponseWriter` it passes
  down keeps whatever optional interfaces the original had (`Flusher`,
  `Hijacker`, …), so SSE, flushing, and WebSocket upgrades work through the
  instrumented chain.
- **`StampRoute(ctx, method, route string)`**
  Puts `http.route` on the active span *and* on the `http.server.*` metrics, and
  renames the span to the semconv `"{method} {route}"` form. The adapters call it
  from `RouteTag`; call it yourself when wiring a framework by hand. The request
  must be inside `nethttp.Handler` — without it the metric attribute is dropped
  and a warning is logged once.
- **`RecordRoute(ctx, route string, status int)`**
  The one shared definition of outcome: records `endpoint.requests` with
  `failure` iff `status >= 500`. Skips unmatched routes and skipped paths.

#### Options

- **`WithSkipPaths(paths ...string)`** — exclude exact paths from instrumentation:
  no span, no `http.server.*`, and no `endpoint.requests`. Still served, and
  recovery still applies. Repeated calls accumulate. Because the filter runs before
  extraction, a skipped path also drops any inbound `traceparent` — fine for
  probes, wrong for real traced paths.
- **`DefaultSkipPaths`** — `/healthz`, `/readyz`, `/livez`, `/metrics`. Nothing is
  skipped unless you opt in: `nethttp.Handler(mux, nethttp.WithSkipPaths(nethttp.DefaultSkipPaths...))`.
- **`WithEndpointMetrics()`** — turn on the per-endpoint counter in an adapter's
  `Instrument`. Off by default, see [Metrics emitted](#metrics-emitted).
- **`WithoutRecovery()`** — install **no** recovery at all: not `Handler`'s, and
  not the adapter's framework middleware either. Panics then escape to net/http
  (which closes the connection) and are not counted. Use it only when an outer
  layer recovers and calls `endpoint.RecordPanic` itself.
- **`Skipped(ctx) bool`** / **`Resolve(opts ...Option) Settings`** — for adapters
  and hand-rolled middleware that need the same decisions `Handler` made.

### Outbound HTTP (trace propagation) — `nethttp`

- **`HTTPClient() *http.Client`** — a client whose transport already injects the
  `traceparent` header. Use it (or `Transport`) for **all** cross-service calls.
- **`Transport(base http.RoundTripper) http.RoundTripper`** — wrap a specific
  RoundTripper (nil → `http.DefaultTransport`).
- **`WrapClient(c *http.Client) *http.Client`** — add propagation to an existing
  client in place.

### Per-endpoint metrics — `endpoint`

Drop this into **any** handler or function to emit per-endpoint request metrics.
It is HTTP/framework-agnostic: it needs only a `context.Context` and, at the end,
a pointer to the operation's `error`.

- **`Instrument(ctx, name string) func(err *error)`**

```go
func ListTenants(ctx context.Context) (err error) {
    defer endpoint.Instrument(ctx, "tenants.list")(&err)
    // ... work; set err on any failure path ...
    return err
}
```

It records one `endpoint.requests` data point **and** one `endpoint.duration`
observation per call, both with the same two low-cardinality attributes:
`endpoint` (the `name` you pass) and `outcome` (`"failure"` iff
`err != nil && *err != nil`, else `"success"`).

- **RPS per endpoint:** `sum by (endpoint) (rate(endpoint_requests_total[5m]))`
- **Success/failure split:** add `outcome` to the `by(...)` clause.
- **Latency:** `histogram_quantile(0.95, sum by (le, endpoint) (rate(endpoint_duration_bucket[5m])))`

> `Instrument` is aimed at **non-HTTP** operations, which have no `otelhttp`
> histogram covering them. For HTTP handlers served through `nethttp.Handler`,
> `http.server.request.duration` already records latency per route.

Why a **pointer**? `&err` is bound when the `defer` statement runs, but `*err` is
read when the deferred finisher executes — *after* your named return is set — so it
sees the final error. Passing a plain `nil` records a success.

> `name` **must be a low-cardinality constant** (e.g. `"tenants.list"`), never a
> value derived from the request (path, id, user). A dynamic name would explode
> metric cardinality.

Using Gin? Call it from the handler with the request context:

```go
func (h *TenantController) GetAllTenants(c *gin.Context) {
    var err error
    defer endpoint.Instrument(c.Request.Context(), "tenants.list")(&err)
    // ... set err on failures ...
}
```

**Note — `endpoint.Record`.** Adapters that already know the outcome (no named
return to defer against) call the lower-level primitive directly instead of
`Instrument`:

- **`Record(ctx context.Context, name string, failed bool)`** — records the same
  `endpoint.requests` data point as `Instrument`, but takes the outcome as a
  plain `bool` instead of deferring on an `*error`. This is what framework
  adapters (e.g. `adapters/gin`) use internally once they've already determined
  success/failure from the response status.

### Trace-correlated logging — `logging`

- **`Logger() *slog.Logger`** — the logger this library writes through, and a
  convenient one for your own code. Defaults to trace-correlated JSON on stdout.
  Use the `...Context` methods so `ctx` reaches the handler:
  `logging.Logger().InfoContext(ctx, "created tenant", "id", id)`.
- **`NewLogHandler(base slog.Handler) slog.Handler`** — wrap your own base
  `slog.Handler` to add `trace_id`, `span_id`, and `service.name` from the context
  on every record. The stamps stay top-level even under `WithGroup`.
- **`SetDefault(*slog.Logger)`** — redirect everything the library logs (endpoint
  warnings, recovered panics, setup diagnostics) into your own logger. Call it
  before `Init`, otherwise an operator that already has a `logr`/zap/slog setup
  gets a second, differently-shaped stream on stdout. Pass `nil` to restore the
  default.

```go
// Keep trace correlation by wrapping your handler.
logging.SetDefault(slog.New(logging.NewLogHandler(myHandler)))
```

### Panic recording (custom frameworks) — `endpoint`

- **`RecordPanic(ctx, recovered any)`** — records the exception on the active span,
  logs at error with `trace_id`, and increments `http.server.panics`. `nethttp.Recovery`
  calls this for you; call it directly from your own framework's recovery
  middleware (it does **not** re-raise or write a response — you decide how to
  respond). Gin example:

```go
func telemetryRecovery() gin.HandlerFunc {
    return func(c *gin.Context) {
        defer func() {
            if rec := recover(); rec != nil {
                if rec == http.ErrAbortHandler {
                    panic(rec)
                }
                endpoint.RecordPanic(c.Request.Context(), rec)
                c.AbortWithStatus(http.StatusInternalServerError)
            }
        }()
        c.Next()
    }
}
```

### Framework adapters — `adapters/{gin,echo,chi}`

Each adapter is **its own Go module** and wires `nethttp` into that framework in
one call, so you don't hand-roll the middleware above.

| Framework | Module | Package | Guide |
|---|---|---|---|
| Gin | `…/telemetry-web/adapters/gin` | `gintel` | [gin-adapter.md](docs/guides/gin-adapter.md) |
| Echo | `…/telemetry-web/adapters/echo` | `echotel` | [echo-adapter.md](docs/guides/echo-adapter.md) |
| chi | `…/telemetry-web/adapters/chi` | `chitel` | [chi-adapter.md](docs/guides/chi-adapter.md) |

Each package name deliberately differs from its directory so it never collides
with the framework's own package — **no import alias is needed**:

```go
import (
    "github.com/gin-gonic/gin"
    "github.com/stakater/operator-utils/telemetry-web/adapters/gin" // package gintel
)

func main() {
    ctx := context.Background()
    shutdown, err := telemetry.Init(ctx, telemetry.Config{ServiceName: "svc"})
    if err != nil {
        log.Fatal(err)
    }
    defer func() {
        sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        _ = shutdown(sctx)
    }()

    engine := gin.New()
    h := gintel.Instrument(engine) // installs Recovery and RouteTag
    // ... register routes on engine ...

    http.ListenAndServe(":8080", h)
    // or: (&http.Server{Handler: h}).ListenAndServe()
}
```

Every adapter exposes the same four things:

- **`Instrument(engine, opts ...nethttp.Option) http.Handler`** — installs the
  middleware below, wraps the engine in `nethttp.Handler`, and returns a plain
  `http.Handler` to serve. `nethttp` options are forwarded.
- **`Recovery()`** — panic → `endpoint.RecordPanic` + `500`. Installed by
  `Instrument` on gin and echo; on chi it *is* `nethttp.Recovery`, which
  `Handler` already applies.
- **`RouteTag()`** — stamps `http.route` on the span and the duration metric and
  renames the span to `"{method} {route}"`.
- **`Metrics()`** — records `endpoint.requests`, keyed by the matched route
  template. Opt-in via `nethttp.WithEndpointMetrics()`.

Ordering: **gin** and **chi** need `Instrument` *before* any route is registered
and panic if you call it late, rather than instrumenting nothing. **Echo** applies
`Use` middleware to routes registered either side of the call, so it doesn't care.

All three classify outcome identically (`status >= 500` is a failure) via
`nethttp.RecordRoute`, and a panic is counted exactly once on each. The shared
conformance suite in `adaptertest` fails the build if one of them drifts. See
[the adapter contract](docs/reference.md#the-adapter-contract) for the failure
rule and how the recovery layers fit together.

---

## Metrics emitted

| Metric                     | Type      | Source                                    |
|----------------------------|-----------|-------------------------------------------|
| `http.server.request.duration` & other `http.server.*` | histogram/… | `otelhttp` (via `nethttp.Handler`); carries `http.route` once `RouteTag`/`StampRoute` runs |
| `endpoint.requests`   | counter   | `endpoint.Instrument` / `endpoint.Record`, and adapters under `WithEndpointMetrics` (`endpoint`, `outcome`) |
| `endpoint.duration`        | histogram | `endpoint.Instrument` (`endpoint`, `outcome`) |
| `http.server.panics`       | counter   | `endpoint.RecordPanic` / `nethttp.Recovery` |
| runtime (goroutines/GC/heap) | gauges/counters | `contrib/runtime` (via `Init`)      |

Metrics ↔ traces are correlated via **exemplars** (on by default when a sampled
span is active in `ctx`). `trace_id` is **never** put on a metric as an attribute —
exemplars only — to keep cardinality bounded.

### Why `endpoint.requests` is opt-in for HTTP

Once `RouteTag` stamps `http.route`, `http.server.request.duration` carries
route, method, and status code, and its `_count` gives you request and failure
rates — a strict superset:

```promql
sum by (http_route) (rate(http_server_request_duration_count{http_response_status_code=~"5.."}[5m]))
```

The counter buys one simpler label pair (`endpoint`, `outcome`) at the cost of a
second instrument recording the same events, so adapters leave it off unless you
pass `nethttp.WithEndpointMetrics()`. It remains the right tool for **non-HTTP**
operations via `endpoint.Instrument`, where no `otelhttp` histogram exists.

---

## Notes & limits

- **You do not need `otelgin`/`otelchi`.** Bare `otelhttp` cannot know the matched
  template, which is what `StampRoute` supplies; once stamped, the built-in
  `http.server.*` metrics break down per route on their own.
- **A skipped path loses inbound trace context.** `WithSkipPaths` filters before
  extraction, so a skipped path will not continue a caller's trace. Correct for
  probes, wrong for real paths.
- **`Init` is once per process.** A second call without an intervening shutdown
  returns `ErrAlreadyInitialized`; the runtime instrumentation has no `Stop`, so
  re-registering it is not recoverable.
- Async boundaries (queues) and background goroutines don't carry `ctx` — pass it
  through, or inject/extract trace context in message metadata, to keep the trace
  alive.

## Local verification

Run a collector locally (`otel/opentelemetry-collector` with an OTLP receiver on
`:4317` and a debug exporter), start your service with
`OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317` and `Insecure: true`, send a few
requests, and confirm traces, `http.server.*` + `endpoint.requests` metrics,
runtime metrics, and `trace_id`-carrying logs appear.

## Tests

```sh
cd telemetry-web
go test ./... -shuffle=on
```

### Working on the adapters

Each adapter (`adapters/gin`, `adapters/echo`, `adapters/chi`) is its own module
and pins a published pseudo-version of the core, which is what consumers of the
adapter resolve. `go.work` overrides that pin locally so the adapters compile
against the in-tree core, otherwise a breaking core change stays invisible until
after it is published.

That means a core API change must be applied to the adapters in the same commit.
CI builds both ways — once through the workspace and once with `GOWORK=off` —
and **both are blocking**, so neither a broken adapter nor a stale pin can slip
through:

```sh
cd telemetry-web/adapters/gin
go build ./...              # against the in-tree core
GOWORK=off go build ./...   # against the pinned core, as a consumer sees it
```

There is a bootstrap consequence worth knowing: a core change that the adapters
need cannot be pinned in the same commit, because the pseudo-version only exists
once the core commit is pushed. Land the core change first, then repin in a
follow-up commit — the `GOWORK=off` build is red in between, which is correct.

Repin with:

```sh
cd telemetry-web/adapters/gin
GOWORK=off go get github.com/stakater/operator-utils/telemetry-web@<sha>
GOWORK=off go mod tidy
```

Once the core carries a release tag, pin to the tag instead of a pseudo-version.
