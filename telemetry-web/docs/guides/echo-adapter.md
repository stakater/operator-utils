# Guide: integrating telemetry with Echo (via the adapter)

The `adapters/echo` module wires the whole library into an Echo service in **one
call**: framework-native panic recovery, server spans and metrics keyed by the
*matched route template*, and trace-correlated logging. This is the
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

> Use the **full 40 character** SHA. A short one makes Go resolve the *parent*
> module and fail with a confusing error, since `telemetry-web` is nested:
> `module github.com/stakater/operator-utils@e559a46 found, but does not contain
> package github.com/stakater/operator-utils/telemetry-web`.

Your `go.mod` then records something like:

```gomod
require (
    github.com/stakater/operator-utils/telemetry-web v0.0.0-<timestamp>-<full-commit-sha>
    github.com/stakater/operator-utils/telemetry-web/adapters/echo v0.0.0-<timestamp>-<full-commit-sha>
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
    handler := echotel.Instrument(e) // installs Recovery + RouteTag, wraps e in nethttp.Handler

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

### If you already use `middleware.Recover()`

`recover()` consumes the panic, so **the innermost recovery is the only one that
records it**. `Instrument` installs `echotel.Recovery` with `e.Use`, and Echo runs
`Use` middleware outermost-first, so placement decides:

```go
e.Use(middleware.Recover())      // OUTER to ours: harmless, panic still counted
handler := echotel.Instrument(e)
```

```go
handler := echotel.Instrument(e)
e.Use(middleware.Recover())      // INNER to ours: swallows it, http.server.panics stays flat
```

Neither is detectable from inside the adapter, and the second one fails silently:
the request still answers `500`, only the metric and the span exception go missing.

`middleware.Recover()` is also redundant here — `echotel.Recovery` already answers
`500` and re-raises `http.ErrAbortHandler`, the same two things Echo's does. The
simplest wiring is to drop it. If you want your own recovery to be innermost
(custom response body, extra logging), hand recording over explicitly:

```go
e.Use(myRecovery())                                 // calls endpoint.Recovered
handler := echotel.Instrument(e, nethttp.WithoutRecovery())
```

See [Recovery middleware](echo-raw.md#3-recovery-middleware) for what `myRecovery`
looks like — `endpoint.Recovered` is a single call.

---

## 3. What you get automatically

For every matched route, with **zero per-handler code**:

- `http.route` stamped on the server span **and** the standard
  `http.server.request.duration` metric, and the span renamed to the semconv
  `"{method} {route}"` form, so traces and semconv metrics are route-attributed.
  Because that metric carries `http.response.status_code` too, request and failure
  rates per route come from it directly — there is no separate per-endpoint
  counter. Echo runs its error handler *after* the middleware chain, so a returned
  `*echo.HTTPError` becomes its own code and any other error becomes Echo's default
  `500`; the adapter does not interpret any of that, `otelhttp` records the status
  as written to the response.
- The standard `http.server.request.duration` / `active_requests` and a server
  span (from the core `nethttp.Handler` that `Instrument` wraps around the engine).
  To keep k8s probes and `/metrics` scrapes out of traces and metrics, opt in to
  filtering: `Instrument(e, nethttp.WithSkipPaths(nethttp.DefaultSkipPaths...))`.
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

> **Two layers, one count.** `Instrument` installs `echotel.Recovery()` *and* keeps
> `nethttp.Handler`'s. The framework layer consumes handler panics before the
> outer one sees them, so nothing is double counted — and the outer one is the
> only one outside the engine, so it is what covers `e.Pre(...)` middleware, which Echo runs before the entire `Use` chain. A panic there would
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
e := echo.New()
e.Use(echotel.Recovery()) // panic -> Recovered + 500
e.Use(echotel.RouteTag()) // http.route -> span + duration metric
// ... your other middleware, then routes ...

srv := &http.Server{Addr: ":8080", Handler: nethttp.Handler(e)} // spans + server metrics
```

Both must sit inside `nethttp.Handler` — they do here, since they are engine
middleware and the engine is what `nethttp.Handler` wraps.

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

- Metric cardinality is bounded because `RouteTag()` stamps `c.Path()` (the
  template `/users/:id`), never the raw path. Unmatched requests (404 scans) have
  an empty `c.Path()` and get no `http.route` at all, so a scan mints no series.
- Panicked requests appear on `http.server.panics` and as a `500` on the duration
  metric.
- Echo runs its error handler *after* the middleware chain returns, so the status
  is written outside the handler. The adapter does not resolve it — `otelhttp`
  observes the response, so the recorded `http.response.status_code` is what the
  client received, including the case where the handler had already committed a
  `200` before returning an error.
- Nothing is exported until a collector is reachable at the configured OTLP
  endpoint; for local dev run one and set `Insecure: true`.
