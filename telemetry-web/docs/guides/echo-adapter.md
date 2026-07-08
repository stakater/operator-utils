# Guide: integrating telemetry with Echo (via the adapter)

The `adapters/echo` module wires the whole library into an Echo service in **one
call**: framework-native panic recovery, automatic per-endpoint metrics keyed by
the *matched route template*, and the core server spans + metrics. This is the
recommended path for an Echo app; [echo-raw.md](echo-raw.md) shows the same
wiring done by hand.

See also: [API reference](../reference.md) · [Gin adapter guide](gin-adapter.md).

---

## 1. Add the modules

Both the core and the adapter are nested modules in this repo (not yet tagged),
so pin them to a commit — Go resolves it to a pseudo-version:

```sh
go get github.com/stakater/operator-utils/telemetry-web@<commit-sha>
go get github.com/stakater/operator-utils/telemetry-web/adapters/echo@<commit-sha>
go mod tidy
```

Your `go.mod` then records something like:

```gomod
require (
    github.com/stakater/operator-utils/telemetry-web v0.0.0-20260707172313-d97fce7d0cfb
    github.com/stakater/operator-utils/telemetry-web/adapters/echo v0.0.0-20260707172313-d97fce7d0cfb
)
```

Prefer this pinned version over a local `replace` directive — it is reproducible
for every clone and in CI. Reserve `replace`/`go.work` for developing the
library itself.

---

## 2. Wire `main`

Two calls do it: `telemetry.Init` once, and `echotel.Instrument(e)` to get the
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

    "github.com/labstack/echo/v4"

    "github.com/stakater/operator-utils/telemetry-web"
    "github.com/stakater/operator-utils/telemetry-web/adapters/echo" // package echotel — no alias needed
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()

    // 1. Initialise telemetry once.
    shutdown, err := telemetry.Init(ctx, telemetry.Config{
        ServiceName: "my-echo-service",
        Environment: os.Getenv("ENVIRONMENT"),                 // optional
        Insecure:    os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true",
    })
    if err != nil {
        panic(err)
    }

    // 2. Build the engine and instrument it.
    e := echo.New()
    e.HideBanner = true
    handler := echotel.Instrument(e) // do NOT also add middleware.Recover() (it would pre-empt ours)

    // ... register routes on `e` here (before or after Instrument — both work) ...
    e.GET("/users/:id", getUser)

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

**Serve the returned handler**, not `e.Start()` — the wrapper is what adds spans,
server metrics, and the panic backstop. Unlike Gin there is no ordering rule:
Echo applies `e.Use(...)` middleware to routes registered before or after the
call, so `Instrument` can run at any point during setup.

**Do not add `middleware.Recover()`** — Echo's own recovery would swallow panics
before the telemetry recovery sees them.

---

## 3. What you get automatically

For every matched route, with **zero per-handler code**:

- `http.endpoint.requests{endpoint="/users/:id", outcome="success|failure"}` —
  `failure` when the request failed **server-side**: the handler returned a
  non-HTTP error (Echo turns it into a `500`), returned an `*echo.HTTPError`
  with code ≥ 500, or wrote a ≥ 500 status directly. A returned 4xx
  (`echo.NewHTTPError(http.StatusNotFound, …)`) is a *client* error and counts
  as `success`, consistent with the Gin adapter.
- `http.route` stamped on the server span **and** the standard
  `http.server.request.duration` metric, so traces and semconv metrics are
  route-attributed too.
- The standard `http.server.request.duration` / `active_requests` and a server
  span (from the core `nethttp.Handler` that `Instrument` wraps around the engine).
  To keep k8s probes and `/metrics` scrapes out of traces and metrics, opt in to
  filtering: `Instrument(e, nethttp.WithSkipPaths(nethttp.DefaultSkipPaths...))`.
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
e := echo.New()
e.Use(echotel.Recovery()) // panic -> RecordPanic + 500; keep it OUTSIDE Metrics
e.Use(echotel.RouteTag()) // http.route -> span + duration metric
e.Use(echotel.Metrics())  // per-endpoint metrics by route template (optional if semconv is enough)
// ... your other middleware, then routes ...

srv := &http.Server{Addr: ":8080", Handler: nethttp.Handler(e)} // spans + server metrics
```

Keep `Recovery` before `Metrics` (outermost) so a panicking handler unwinds past
the record call and surfaces only on the panic counter. Both must sit inside
`nethttp.Handler` (they do here — they're engine middleware, and the engine is
what `nethttp.Handler` wraps).

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

func getUser(c echo.Context) (err error) {
    ctx := c.Request().Context()

    logging.Logger().InfoContext(ctx, "fetching user", "id", c.Param("id"))

    // Outbound call that carries the trace to the next hop. Building the request
    // with the inbound ctx is what makes propagation work: the transport injects
    // traceparent from req.Context(). A plain http.NewRequest silently dead-ends
    // the trace, even through this client.
    req, _ := http.NewRequestWithContext(ctx, http.MethodGet, userSvcURL, nil)
    resp, err2 := nethttp.HTTPClient().Do(req)
    _ = resp; _ = err2

    // Optional: instrument an internal operation by name (outcome from err):
    defer endpoint.Instrument(ctx, "user.get")(&err)
    // ... return err ...
    return err
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

- Per-endpoint metric cardinality is bounded because `Metrics()` uses `c.Path()`
  (the template `/users/:id`), never the raw path. Unmatched requests (404
  scans) have an empty `c.Path()` and are skipped.
- Panicked requests appear on `http.server.panics`, not as a per-endpoint
  `failure` data point — consistent with the library's panic philosophy.
- Echo runs its error handler *after* the middleware chain returns, so the
  adapter classifies the outcome from the **returned error** (resolving
  `*echo.HTTPError` codes), not from the not-yet-written response status.
- Nothing is exported until a collector is reachable at the configured OTLP
  endpoint; for local dev run one and set `Insecure: true`.
