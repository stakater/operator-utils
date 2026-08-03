# Guide: integrating telemetry with Gin (via the adapter)

The `adapters/gin` module wires the whole library into a Gin service in **one
call**: framework-native panic recovery, server spans and metrics keyed by the
*matched route template*, and trace-correlated logging. This is the
recommended path for a Gin app.

See also: [API reference](../reference.md) · [Echo adapter guide](echo-adapter.md) ·
[Echo (raw) guide](echo-raw.md).

---

## 1. Add the modules

Both the core and the adapter are nested modules in this repo (not yet tagged),
so pin them to a commit — Go resolves it to a pseudo-version:

```sh
go get github.com/stakater/operator-utils/telemetry-web@<commit-sha>
go get github.com/stakater/operator-utils/telemetry-web/adapters/gin@<commit-sha>
go mod tidy
```

Your `go.mod` then records something like:

```gomod
require (
    github.com/stakater/operator-utils/telemetry-web v0.0.0-20260708082012-8f1fdddd3dca
    github.com/stakater/operator-utils/telemetry-web/adapters/gin v0.0.0-20260708082012-8f1fdddd3dca
)
```

Prefer this pinned version over a local `replace` directive — it is reproducible
for every clone and in CI. Reserve `replace`/`go.work` for developing the
library itself.

---

## 2. Wire `main`

Two calls do it: `telemetry.Init` once, and `gintel.Instrument(engine)` to get the
servable handler.

```go
package main

import (
    "context"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/gin-gonic/gin"

    "github.com/stakater/operator-utils/telemetry-web"
    "github.com/stakater/operator-utils/telemetry-web/adapters/gin" // package gintel — no alias needed
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()

    // 1. Initialise telemetry once.
    shutdown, err := telemetry.Init(ctx, telemetry.Config{
        ServiceName: "mto-gateway",
        Environment: os.Getenv("ENVIRONMENT"),                 // optional
        Insecure:    os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true",
    })
    if err != nil {
        panic(err)
    }

    // 2. Build the engine and instrument it BEFORE registering routes.
    engine := gin.New()
    engine.Use(gin.Logger()) // optional; prefer gin.New() + Logger over gin.Default(), see below
    handler := gintel.Instrument(engine)

    // ... register routes on `engine` here ...
    engine.GET("/api/v1/tenants/:id", getTenant)

    // 3. Serve the wrapped handler + shut down cleanly.
    srv := &http.Server{Addr: ":8080", Handler: handler}
    go func() {
        if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            panic(err)
        }
    }()

    <-ctx.Done()
    stop()
    sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    _ = srv.Shutdown(sctx)
    _ = shutdown(sctx) // flush the last telemetry batch
}
```

**Ordering rule:** call `gintel.Instrument(engine)` right after `gin.New()`, before
`engine.GET/POST/...`. Gin binds a route's middleware chain at registration time,
so global middleware must be installed first. The returned `handler` wraps the
engine *by reference*, so routes you register afterward are still served.

### If you already use `gin.Recovery()` or `gin.Default()`

`recover()` consumes the panic, so **the innermost recovery is the only one that
records it**. Gin runs `Use` middleware outermost-first, so placement decides:

```go
engine.Use(gin.Recovery())          // OUTER to ours: harmless, panic still counted
handler := gintel.Instrument(engine)
```

```go
handler := gintel.Instrument(engine)
engine.Use(gin.Recovery())          // INNER to ours: swallows it, http.server.panics stays flat
```

The second fails silently: the request still answers `500`, only the metric and the
span exception go missing. Nothing in the adapter can detect it.

**`gin.Default()` is the first case, so the count is fine** — its
`Use(Logger(), Recovery())` runs at construction, before `Instrument`. Its real
problem is smaller and different: `gin.Recovery` has **no `http.ErrAbortHandler`
exemption**. That sentinel means "drop this connection without a response";
`gintel.Recovery` re-raises it deliberately, `gin.Recovery` turns it into a `500`,
so the connection is not dropped and you get a duplicate stack log. Prefer
`gin.New()` plus `gin.Logger()` if you want request logging.

If you want your own recovery to be innermost (custom response body, extra
logging), hand recording over explicitly:

```go
engine.Use(myRecovery())                                        // calls endpoint.Recovered
handler := gintel.Instrument(engine, nethttp.WithoutRecovery())
```

See [Recovery middleware](echo-raw.md#3-recovery-middleware) for the shape — it is
a single call, and returning `false` is your signal to re-panic
`http.ErrAbortHandler`.

---

## 3. What you get automatically

For every matched route, with **zero per-handler code**:

- `http.route` stamped on the server span **and** the standard
  `http.server.request.duration` metric, and the span renamed to the semconv
  `"{method} {route}"` form, so traces and semconv metrics are route-attributed.
  Because that metric carries `http.response.status_code` too, request and failure
  rates per route come from it directly — there is no separate per-endpoint
  counter. `c.Error(...)` is not consulted anywhere: the recorded status is the one
  the client received.
- The standard `http.server.request.duration` / `active_requests` and a server
  span (from the core `nethttp.Handler` that `Instrument` wraps around the engine).
  To keep k8s probes and `/metrics` scrapes out of traces and metrics, opt in to
  filtering: `Instrument(engine, nethttp.WithSkipPaths(nethttp.DefaultSkipPaths...))`.
- Panics → `http.server.panics` + an exception/stack on the span + an error log,
  and a `500` response. (`http.ErrAbortHandler` is re-raised untouched.)

Example queries (PromQL, if exporting to Prometheus via the collector):

```promql
# request rate per route
sum by (http_route) (rate(http_server_request_duration_seconds_count[5m]))

# failure ratio per route
sum by (http_route) (rate(http_server_request_duration_seconds_count{http_response_status_code=~"5.."}[5m]))
  / sum by (http_route) (rate(http_server_request_duration_seconds_count[5m]))

# p95 latency per route
histogram_quantile(0.95, sum by (le, http_route) (rate(http_server_request_duration_seconds_bucket[5m])))
```

> **Two layers, one count.** `Instrument` installs `gintel.Recovery()` *and* keeps
> `nethttp.Handler`'s. The framework layer consumes handler panics before the
> outer one sees them, so nothing is double counted — and the outer one is the
> only one outside the engine, so it is what covers middleware you registered with `engine.Use(...)` **before** calling `Instrument`. A panic there would
> otherwise escape to net/http with no metric, no span error, and no 500.
>
> `nethttp.WithoutRecovery()` suppresses **both**, not just `nethttp.Handler`'s.
> The chain is then left with no recovery at all: panics escape to net/http and
> are not counted. Only pass it when an outer layer recovers and calls
> `endpoint.Recovered` itself.

---

## 4. Composing the middleware yourself (optional)

If you maintain your own middleware chain, use the pieces directly instead of
`Instrument`:

```go
engine := gin.New()
engine.Use(gintel.Recovery()) // panic -> Recovered + 500
engine.Use(gintel.RouteTag()) // http.route -> span + duration metric
// ... your other middleware, then routes ...

srv := &http.Server{Addr: ":8080", Handler: nethttp.Handler(engine)} // spans + server metrics
```

`gintel.Recovery()` must sit inside `nethttp.Handler` (it does here, because it's a
Gin middleware inside the engine, and `nethttp.Handler` wraps the engine). Keep
**both** layers, as `Instrument` does: the Gin one handles handler panics and
consumes them, so nothing is double counted, while `nethttp.Handler`'s is the only
one outside the engine and therefore the only thing that catches a panic in
middleware registered before `gintel.Recovery()`.

Register all of these **before** your routes — Gin applies global middleware only
to routes added afterward. `Instrument` enforces this by panicking if the engine
already has routes; a hand-rolled chain gets no such warning.

---

## 5. Outbound calls, logs, and extra metrics

These come from the **core** packages and are identical to any other integration:

```go
import (
    "net/http"

    "github.com/stakater/operator-utils/telemetry-web/endpoint"
    "github.com/stakater/operator-utils/telemetry-web/logging"
    "github.com/stakater/operator-utils/telemetry-web/nethttp"
)

func getTenant(c *gin.Context) {
    ctx := c.Request.Context()

    logging.Logger().InfoContext(ctx, "fetching tenant", "id", c.Param("id"))

    // Outbound call that carries the trace to the next hop. Building the request
    // with the inbound ctx is what makes propagation work: the transport injects
    // traceparent from req.Context(). A plain http.NewRequest silently dead-ends
    // the trace, even through this client.
    req, _ := http.NewRequestWithContext(ctx, http.MethodGet, tenantSvcURL, nil)
    resp, err := nethttp.HTTPClient().Do(req)
    _ = resp; _ = err

    // Optional: instrument an internal operation by name (outcome from err):
    var opErr error
    finish := endpoint.Instrument(ctx, "tenant.load")
    defer func() { finish(&opErr) }()
    // ... opErr = ... ...
}
```

To route an existing client (e.g. a generated SDK's `*http.Client`) through trace
propagation, wrap it once at startup: `nethttp.WrapClient(sdkClient)`. And if you
build your own client (timeouts, connection pools), wrap just its transport:

```go
client := &http.Client{
    Timeout:   5 * time.Second,
    Transport: nethttp.Transport(nil), // or nethttp.Transport(yourCustomTransport)
}
```

---

## Notes

- Metric cardinality is bounded because `RouteTag()` stamps `c.FullPath()` (the
  template `/api/v1/tenants/:id`), never the raw path. Unmatched requests (404
  scans) get no `http.route` at all, so a scan mints no time series.
- Panicked requests appear on `http.server.panics` and as a `500` on the duration
  metric.
- Nothing is exported until a collector is reachable at the configured OTLP
  endpoint; for local dev run one and set `Insecure: true`.
