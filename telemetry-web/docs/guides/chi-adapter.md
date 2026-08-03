# Guide: integrating telemetry with chi (via the adapter)

The `adapters/chi` module wires the whole library into a chi service in **one
call**: panic recovery, server spans and metrics keyed by the *matched route
pattern*, and trace-correlated logging.

See also: [API reference](../reference.md) · [Gin adapter guide](gin-adapter.md) ·
[Echo adapter guide](echo-adapter.md).

---

## 1. Add the modules

Both the core and the adapter are nested modules in this repo (not yet tagged),
so pin them to a commit — Go resolves it to a pseudo-version:

```sh
go get github.com/stakater/operator-utils/telemetry-web@<commit-sha>
go get github.com/stakater/operator-utils/telemetry-web/adapters/chi@<commit-sha>
go mod tidy
```

Prefer this pinned version over a local `replace` directive — it is reproducible
for every clone and in CI. Reserve `replace`/`go.work` for developing the
library itself.

---

## 2. Wire `main`

Two calls do it: `telemetry.Init` once, and `chitel.Instrument(r)` to get the
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

    "github.com/go-chi/chi/v5"

    "github.com/stakater/operator-utils/telemetry-web"
    "github.com/stakater/operator-utils/telemetry-web/adapters/chi" // package chitel — no alias needed
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()

    // 1. Initialise telemetry once.
    shutdown, err := telemetry.Init(ctx, telemetry.Config{
        ServiceName: "my-chi-service",
        Environment: os.Getenv("ENVIRONMENT"),                 // optional
        Insecure:    os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true",
    })
    if err != nil {
        panic(err)
    }

    // 2. Build the router and instrument it BEFORE registering routes
    //    (chi panics if Use is called after the first route).
    r := chi.NewRouter()
    handler := chitel.Instrument(r)

    // ... register routes on `r` here ...
    r.Get("/users/{id}", getUser)

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

### If you already use `middleware.Recoverer`

`recover()` consumes the panic, so **the innermost recovery is the only one that
records it**. On chi ours is `nethttp.Handler`'s, *outside* the router, so any
`r.Use(middleware.Recoverer)` is inner to it and takes over the count — unlike gin
and echo there is no placement that makes it harmless.

The request still answers `500` either way; what goes missing is
`http.server.panics` and the exception on the span, silently.

`middleware.Recoverer` is redundant here — `nethttp.Recovery` already answers `500`
and re-raises `http.ErrAbortHandler`. Drop it. If you want your own recovery (custom
response body, extra logging), hand recording over explicitly:

```go
r.Use(myRecovery())                                          // calls endpoint.Recovered
handler := chitel.Instrument(r, nethttp.WithoutRecovery())   // ours steps aside
```

See [Recovery middleware](echo-raw.md#3-recovery-middleware) for the shape —
`endpoint.Recovered` is a single call, and returning `false` is your signal to
re-panic `http.ErrAbortHandler`.

---

## 3. What you get automatically

For every matched route, with **zero per-handler code**:

- `http.route` stamped on the server span **and** the standard
  `http.server.request.duration` metric, and the span renamed to the semconv
  `"{method} {route}"` form. chi fills the route pattern in *before* invoking the
  handler, so `RouteTag` stamps from a `defer` and a panicked request keeps its
  route too — same as gin and echo. Mounted subrouters record chi's joined
  pattern (`/api/users/{id}`) as-is.
  Because that metric carries `http.response.status_code` too, request and failure
  rates per route come from it directly — there is no separate per-endpoint
  counter.
- The standard `http.server.request.duration` / `active_requests` and a server
  span. To keep k8s probes and `/metrics` scrapes out of traces and metrics,
  opt in to filtering:
  `Instrument(r, nethttp.WithSkipPaths(nethttp.DefaultSkipPaths...))`.
- Panics → `http.server.panics` + an exception/stack on the span + an error
  log, and a `500` response. (`http.ErrAbortHandler` is re-raised untouched.)

> **One layer here, unlike gin and echo.** `chitel.Recovery` *is*
> `nethttp.Recovery`, so `Instrument` installs none of its own and lets `Handler`
> do the recovering — adding both would count every panic twice.
>
> `nethttp.WithoutRecovery()` therefore leaves the chain with no recovery at all:
> panics escape to net/http and are not counted. Only pass it when an outer layer
> recovers and calls `endpoint.Recovered` itself.

---

## 4. Composing the middleware yourself (optional)

chi middleware is plain `func(http.Handler) http.Handler`, so the pieces
compose like any net/http chain:

```go
r := chi.NewRouter()
r.Use(chitel.RouteTag()) // http.route -> span + duration metric, survives panics
// ... your other middleware, then routes ...

srv := &http.Server{Addr: ":8080", Handler: nethttp.Handler(r)} // spans + server metrics + recovery
```

Do **not** add `chitel.Recovery()` on top of `nethttp.Handler`: `chitel.Recovery`
*is* `nethttp.Recovery`, and `nethttp.Handler` already installs it, so you would
count every panic twice. Use it only when you are not going through
`nethttp.Handler`, or alongside `nethttp.WithoutRecovery()`:

```go
r.Use(chitel.Recovery())
srv := &http.Server{Handler: nethttp.Handler(r, nethttp.WithoutRecovery())}
```

---

## 5. Outbound calls, logs, and extra metrics

Identical to every other integration — core packages only. See the
[Gin guide §5](gin-adapter.md#5-outbound-calls-logs-and-extra-metrics) for the
full example: `nethttp.HTTPClient()` with `http.NewRequestWithContext(ctx, …)`
for propagation, `logging.Logger().InfoContext(ctx, …)` for correlated logs,
and `endpoint.Instrument(ctx, "op.name")(&err)` for named operations.

---

## Notes

- Metric cardinality is bounded because `RouteTag()` stamps chi's
  `RoutePattern()` (the template `/users/{id}`), never the raw path. Unmatched
  requests (404 scans) have an empty pattern and get no `http.route` at all, so a
  scan mints no series.
- The core `Recovery` wraps the ResponseWriter with `httpsnoop`, which preserves
  the optional interfaces, so `Flusher`/`Hijacker` reach your handler and SSE and
  WebSocket upgrades work. The shared conformance suite asserts this on every
  adapter — a wrapper that dropped them would make gin's and echo's `Flush()`
  panic on a type assertion.
- A path excluded via `nethttp.WithSkipPaths` records nothing at all: no span and
  no duration observation.
- Panicked requests appear on `http.server.panics` and as a `500` on the duration
  metric.
