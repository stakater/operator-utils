# telemetry

A small, **framework-agnostic** Go library that gives any service consistent
OpenTelemetry **traces + metrics + trace-correlated logs** with a two-call setup.
It is *composition over OpenTelemetry* — it leans on the OTel SDK, `otelhttp`, and
`contrib/runtime`; it does not hand-roll spans, propagation, or instruments, and
it imports **no web framework**.

```
import (
    "github.com/stakater/operator-utils/telemetry-web"
    "github.com/stakater/operator-utils/telemetry-web/endpoint"
    "github.com/stakater/operator-utils/telemetry-web/logging"
    "github.com/stakater/operator-utils/telemetry-web/nethttp"
)
```

Telemetry is exported over **OTLP/gRPC** to an OpenTelemetry Collector. Logs go to
structured JSON on stdout with `trace_id` stamped in.

## Documentation

- [API reference](docs/reference.md) — configuration, emitted signals, full package-by-package API.
- [Gin integration (via adapter)](docs/guides/gin-adapter.md) — the one-call `teleg.Instrument(engine)` path.
- [Echo integration (raw)](docs/guides/echo-raw.md) — wiring Echo with the core primitives.

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
    defer shutdown(context.Background()) // flushes the last batch on exit

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
- `OTEL_TRACES_SAMPLER_ARG` — head-sampling ratio (default `1.0`).

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
  in `main`. `defer shutdown(...)` to flush on deploy. Returns an error if
  `ServiceName` is empty or the resource can't be built; if a later setup step
  fails, already-started providers are cleaned up before returning.

### Inbound HTTP (net/http) — `nethttp`

- **`Handler(next http.Handler) http.Handler`**
  The composed inbound chain: `otelhttp` (spans + server metrics) → `Recovery`
  (panic → 500) → your handler. Wrap your router once. `gin.Engine`,
  `chi.Mux`, etc. all satisfy `http.Handler`, so you can wrap them too:
  `nethttp.Handler(ginEngine)`.
- **`Recovery(next http.Handler) http.Handler`**
  Exported separately for teams composing their own chain — it must sit **inside**
  the `otelhttp` handler. `Handler` already includes it. It calls `endpoint.RecordPanic`
  then writes `500`; it re-raises `http.ErrAbortHandler` for net/http to handle.

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

It records one `http.endpoint.requests` data point per call with two
low-cardinality attributes: `endpoint` (the `name` you pass) and `outcome`
(`"failure"` iff `err != nil && *err != nil`, else `"success"`).

- **RPS per endpoint:** `sum by (endpoint) (rate(http_endpoint_requests_total[5m]))`
- **Success/failure split:** add `outcome` to the `by(...)` clause.

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
  `http.endpoint.requests` data point as `Instrument`, but takes the outcome as a
  plain `bool` instead of deferring on an `*error`. This is what framework
  adapters (e.g. `adapters/gin`) use internally once they've already determined
  success/failure from the response status.

### Trace-correlated logging — `logging`

- **`Logger() *slog.Logger`** — a `*slog.Logger` writing trace-correlated JSON to
  stdout. Use the `...Context` methods so `ctx` reaches the handler:
  `logging.Logger().InfoContext(ctx, "created tenant", "id", id)`.
- **`NewLogHandler(base slog.Handler) slog.Handler`** — wrap your own base
  `slog.Handler` to add `trace_id`, `span_id`, and `service.name` from the context
  on every record.

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

### Gin adapter — `adapters/gin`

For Gin services, `github.com/stakater/operator-utils/telemetry-web/adapters/gin`
wires `nethttp` and `endpoint` into a Gin engine in a couple of calls, so you
don't hand-roll the middleware above. Because gin-gonic is conventionally
imported as `gin`, import the adapter under an alias such as `teleg`:

```go
import (
    "github.com/gin-gonic/gin"
    teleg "github.com/stakater/operator-utils/telemetry-web/adapters/gin"
)

func main() {
    ctx := context.Background()
    shutdown, err := telemetry.Init(ctx, telemetry.Config{ServiceName: "svc"})
    if err != nil {
        log.Fatal(err)
    }
    defer shutdown(context.Background())

    engine := gin.New()
    engine.Use(teleg.Recovery(), teleg.Metrics())
    h := teleg.Instrument(engine)
    // ... register routes on engine ...

    http.ListenAndServe(":8080", h)
    // or: (&http.Server{Handler: h}).ListenAndServe()
}
```

- **`Instrument(*gin.Engine) http.Handler`** — wraps the engine with inbound
  spans/metrics/recovery (equivalent to `nethttp.Handler`), returning a plain
  `http.Handler` to serve.
- **`Recovery() gin.HandlerFunc`** — Gin middleware calling `endpoint.RecordPanic`
  on panic.
- **`Metrics() gin.HandlerFunc`** — Gin middleware calling `endpoint.Record` with
  the route name and outcome for per-endpoint metrics.

---

## Metrics emitted

| Metric                     | Type      | Source                                    |
|----------------------------|-----------|-------------------------------------------|
| `http.server.request.duration` & other `http.server.*` | histogram/… | `otelhttp` (via `nethttp.Handler`) |
| `http.endpoint.requests`   | counter   | `endpoint.Instrument` / `endpoint.Record` (`endpoint`, `outcome`) |
| `http.server.panics`       | counter   | `endpoint.RecordPanic` / `nethttp.Recovery` |
| runtime (goroutines/GC/heap) | gauges/counters | `contrib/runtime` (via `Init`)      |

Metrics ↔ traces are correlated via **exemplars** (on by default when a sampled
span is active in `ctx`). `trace_id` is **never** put on a metric as an attribute —
exemplars only — to keep cardinality bounded.

---

## Notes & limits

- **`http.route` is not populated** by bare `otelhttp` on a wrapped engine, so the
  built-in HTTP server metrics don't break down per route. Use `endpoint.Instrument`
  (or `endpoint.Record` / the `adapters/gin` middleware) for per-endpoint breakdown,
  or add your router's OTel contrib middleware (`otelgin`, `otelchi`, …) alongside
  this library.
- **Recovery writes 500 unconditionally.** If a handler already wrote a
  status/body before panicking, that 500 is a no-op (standard net/http behavior).
- Async boundaries (queues) and background goroutines don't carry `ctx` — pass it
  through, or inject/extract trace context in message metadata, to keep the trace
  alive.

## Local verification

Run a collector locally (`otel/opentelemetry-collector` with an OTLP receiver on
`:4317` and a debug exporter), start your service with
`OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317` and `Insecure: true`, send a few
requests, and confirm traces, `http.server.*` + `http.endpoint.requests` metrics,
runtime metrics, and `trace_id`-carrying logs appear.

## Tests

```sh
cd telemetry
go test ./... -shuffle=on
```
