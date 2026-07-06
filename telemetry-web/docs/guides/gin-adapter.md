# Guide: integrating telemetry with Gin (via the adapter)

The `adapters/gin` module wires the whole library into a Gin service in **one
call**: framework-native panic recovery, automatic per-endpoint metrics keyed by
the *matched route template*, and the core server spans + metrics. This is the
recommended path for a Gin app.

See also: [API reference](../reference.md) · [Echo (raw) guide](echo-raw.md).

---

## 1. Add the modules

Both the core and the adapter are nested modules in this repo, so point at them
with `replace`. In your service's `go.mod`:

```gomod
require (
    github.com/stakater/operator-utils/telemetry-web v0.0.0
    github.com/stakater/operator-utils/telemetry-web/adapters/gin v0.0.0
)

replace github.com/stakater/operator-utils/telemetry-web => ../telemetry
replace github.com/stakater/operator-utils/telemetry-web/adapters/gin => ../telemetry/adapters/gin
```

Adjust the paths to wherever `telemetry/` sits relative to your service, then
`go mod tidy`.

---

## 2. Wire `main`

Two calls do it: `telemetry.Init` once, and `teleg.Instrument(engine)` to get the
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
    teleg "github.com/stakater/operator-utils/telemetry-web/adapters/gin" // aliased: gin-gonic already owns "gin"
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
    engine.Use(gin.Logger()) // optional; do NOT use gin.Default() (its recovery would pre-empt ours)
    handler := teleg.Instrument(engine)

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

**Ordering rule:** call `teleg.Instrument(engine)` right after `gin.New()`, before
`engine.GET/POST/...`. Gin binds a route's middleware chain at registration time,
so global middleware must be installed first. The returned `handler` wraps the
engine *by reference*, so routes you register afterward are still served.

**Do not use `gin.Default()`** — it installs Gin's own recovery, which would catch
panics before the telemetry recovery runs. Use `gin.New()` and add `gin.Logger()`
yourself if you want request logging.

---

## 3. What you get automatically

For every matched route, with **zero per-handler code**:

- `http.endpoint.requests{endpoint="/api/v1/tenants/:id", outcome="success|failure"}`
  — `failure` when the response status is `≥ 500` or the handler set `c.Error(...)`.
- The standard `http.server.request.duration` / `active_requests` and a server
  span (from the core `nethttp.Handler` that `Instrument` wraps around the engine).
- Panics → `http.server.panics` + an exception/stack on the span + an error log,
  and a `500` response. (`http.ErrAbortHandler` is re-raised untouched.)

Example queries (PromQL, if exporting to Prometheus via the collector):

```promql
# per-endpoint request rate
sum by (endpoint) (rate(http_endpoint_requests_total[5m]))

# per-endpoint failure ratio
sum by (endpoint) (rate(http_endpoint_requests_total{outcome="failure"}[5m]))
  / sum by (endpoint) (rate(http_endpoint_requests_total[5m]))
```

---

## 4. Composing the middleware yourself (optional)

If you maintain your own middleware chain, use the pieces directly instead of
`Instrument`:

```go
engine := gin.New()
engine.Use(teleg.Recovery()) // panic -> RecordPanic + 500
engine.Use(teleg.Metrics())  // per-endpoint metrics by route template
// ... your other middleware, then routes ...

srv := &http.Server{Addr: ":8080", Handler: nethttp.Handler(engine)} // spans + server metrics
```

`teleg.Recovery()` must sit inside `nethttp.Handler` (it does here, because it's a
Gin middleware inside the engine, and `nethttp.Handler` wraps the engine).

---

## 5. Outbound calls, logs, and extra metrics

These come from the **core** packages and are identical to any other integration:

```go
import (
    "github.com/stakater/operator-utils/telemetry-web/endpoint"
    "github.com/stakater/operator-utils/telemetry-web/logging"
    "github.com/stakater/operator-utils/telemetry-web/nethttp"
)

func getTenant(c *gin.Context) {
    ctx := c.Request.Context()

    logging.Logger().InfoContext(ctx, "fetching tenant", "id", c.Param("id"))

    // Outbound call that carries the trace to the next hop:
    resp, err := nethttp.HTTPClient().Do(reqWithContext(ctx))
    _ = resp; _ = err

    // Optional: instrument an internal operation by name (outcome from err):
    var opErr error
    finish := endpoint.Instrument(ctx, "tenant.load")
    defer func() { finish(&opErr) }()
    // ... opErr = ... ...
}
```

To route an existing client (e.g. a generated SDK's `*http.Client`) through trace
propagation, wrap it once at startup: `nethttp.WrapClient(sdkClient)`.

---

## Notes

- Per-endpoint metric cardinality is bounded because `Metrics()` uses
  `c.FullPath()` (the template `/api/v1/tenants/:id`), never the raw path.
  Unmatched requests (404 scans) are skipped.
- Panicked requests appear on `http.server.panics`, not as a per-endpoint
  `failure` data point — consistent with the library's panic philosophy.
- Nothing is exported until a collector is reachable at the configured OTLP
  endpoint; for local dev run one and set `Insecure: true`.
