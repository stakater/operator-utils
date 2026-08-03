# Guide: integrating telemetry with Echo (raw, no adapter)

> **There is now an [`adapters/echo` module](echo-adapter.md)** that packages
> everything below into one call — prefer it for Echo services. This guide
> remains as the reference for wiring the framework-agnostic core by hand
> (useful for frameworks that have no adapter, or to understand what the
> adapter does).

The core is framework-agnostic, so you can wire Echo yourself with a few lines.
This guide shows the full pattern and, at the end, a small reusable Echo
middleware that reproduces what the adapters do (automatic route-templated
per-endpoint metrics).

See also: [API reference](../reference.md) · [Echo adapter guide](echo-adapter.md) ·
[Gin adapter guide](gin-adapter.md).

The building blocks, all from the core:

| Concern | Core API |
| --- | --- |
| setup | `telemetry.Init` |
| server spans + `http.server.*` metrics + panic backstop | `nethttp.Handler(e)` |
| panic recording | `endpoint.Recovered` |
| per-endpoint metrics | `endpoint.Record` (or `endpoint.Instrument`) |
| outbound propagation | `nethttp.HTTPClient` / `WrapClient` |
| trace-correlated logs | `logging.Logger` |

`*echo.Echo` implements `http.Handler`, so the core `nethttp.Handler` wraps the
whole engine exactly like it would a stdlib mux.

---

## 1. Add the core module

Pin the core module to a commit — Go resolves it to a pseudo-version:

```sh
go get github.com/stakater/operator-utils/telemetry-web@<commit-sha>
go mod tidy
```

Your `go.mod` then records something like:

```gomod
require github.com/stakater/operator-utils/telemetry-web v0.0.0-20260708082012-8f1fdddd3dca
```

Prefer this pinned version over a local `replace` directive — it is reproducible
for every clone and in CI; reserve `replace`/`go.work` for developing the library
itself. (You do **not** add an adapter module when wiring raw.)

---

## 2. Wire `main`

Key differences from a naive Echo setup:

- **Serve through `nethttp.Handler(e)`**, not `e.Start()` — that is what adds
  spans and server metrics and the panic backstop around the whole engine.
- **Replace `middleware.Recover()`** with a recovery middleware that calls
  `endpoint.RecordPanic` (Echo's default recover would swallow the panic before
  telemetry sees it).

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
    "github.com/stakater/operator-utils/telemetry-web/endpoint"
    "github.com/stakater/operator-utils/telemetry-web/nethttp"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()

    shutdown, err := telemetry.Init(ctx, telemetry.Config{
        ServiceName: "my-echo-service",
        Environment: os.Getenv("ENVIRONMENT"),
        Insecure:    os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true",
    })
    if err != nil {
        panic(err)
    }

    e := echo.New()
    e.HideBanner = true
    e.Use(TelemetryRecovery()) // instead of middleware.Recover()
    e.Use(TelemetryRouteTag())  // http.route on span + duration metric (see §4)
    e.Use(TelemetryMetrics())   // optional per-endpoint counter (see §4)

    // ... register routes ...
    e.GET("/users/:id", getUser)

    // Serve the engine wrapped by the core Handler (spans + server metrics).
    srv := &http.Server{Addr: ":8080", Handler: nethttp.Handler(e)}
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
    _ = shutdown(sctx)

    _ = endpoint.Record // keep imports tidy if you trim the example
}
```

> Unlike Gin, Echo applies `e.Use(...)` middleware to all routes regardless of
> whether they were registered before or after the `Use` call, so ordering
> between `Use` and route registration doesn't matter here. The recovery/metrics
> middleware still sit *inside* `nethttp.Handler` (they're engine middleware, and
> the engine is what `nethttp.Handler` wraps), so the span exists in `ctx` and the
> `500` is measured.

---

## 3. Recovery middleware

`endpoint.Recovered` is the whole decision: it records the panic and returns
`false` only for `http.ErrAbortHandler`, which you must re-panic so net/http
handles it. Using it rather than `RecordPanic` directly is what keeps a hand-wired
framework behaving identically to the adapters.

```go
func TelemetryRecovery() echo.MiddlewareFunc {
    return func(next echo.HandlerFunc) echo.HandlerFunc {
        return func(c echo.Context) error {
            defer func() {
                if rec := recover(); rec != nil {
                    // Pass the matched template so a panic spike names an endpoint.
                    if !endpoint.Recovered(c.Request().Context(), rec, semconv.HTTPRoute(c.Path())) {
                        panic(rec)
                    }
                    _ = c.NoContent(http.StatusInternalServerError)
                }
            }()
            return next(c)
        }
    }
}
```

After a recovered panic the handler returns `nil` (its zero return value) with a
`500` already written — so Echo's error handler does not run for it, matching the
"panic became a 500" behavior.

---

## 4. Route attribution and per-endpoint metrics

The first thing to wire is `http.route`, since it is what makes the standard
`http.server.request.duration` metric break down per route and renames the span
to the semconv `"{method} {route}"` form. Echo's `c.Path()` returns the matched
template (e.g. `/users/:id`), so stamp from there:

```go
func TelemetryRouteTag() echo.MiddlewareFunc {
    return func(next echo.HandlerFunc) echo.HandlerFunc {
        return func(c echo.Context) error {
            if route := c.Path(); route != "" {
                nethttp.StampRoute(c.Request().Context(), c.Request().Method, route)
            }
            return next(c)
        }
    }
}
```

With that in place the duration histogram already gives you request and failure
rates per route, so the per-endpoint counter below is **optional** — the adapters
leave it off unless you pass `nethttp.WithEndpointMetrics()`:

```promql
sum by (http_route) (rate(http_server_request_duration_count{http_response_status_code=~"5.."}[5m]))
```

If you do want the simpler `{endpoint, outcome}` label pair, use
`nethttp.RecordRoute` — the same helper the adapters call, so your service
classifies outcomes identically (`failure` iff status ≥ 500, unmatched routes and
skipped paths ignored):

```go
func TelemetryMetrics() echo.MiddlewareFunc {
    return func(next echo.HandlerFunc) echo.HandlerFunc {
        return func(c echo.Context) error {
            err := next(c)
            nethttp.RecordRoute(c.Request().Context(), c.Path(), status(c, err))
            return err
        }
    }
}

// Echo runs its error handler AFTER the middleware chain returns, so for a
// returned error the response status is not written yet and the error carries
// the answer.
func status(c echo.Context, err error) int {
    if err != nil {
        var he *echo.HTTPError
        if errors.As(err, &he) {
            return he.Code
        }
        return http.StatusInternalServerError
    }
    return c.Response().Status
}
```

---

## 5. Per-handler instrumentation (alternative / addition)

If you prefer explicit names for specific operations instead of (or on top of)
the middleware, use `endpoint.Instrument`. Echo handlers already return `error`,
so the finisher reads it directly:

```go
func getUser(c echo.Context) (err error) {
    ctx := c.Request().Context()
    defer endpoint.Instrument(ctx, "user.get")(&err)

    logging.Logger().InfoContext(ctx, "fetching user", "id", c.Param("id"))
    // ... return err ...
    return err
}
```

`name` must be a low-cardinality constant.

---

## 6. Outbound calls and logs

Same as any integration — use the core clients/logger so the trace propagates and
logs correlate:

```go
// Build the request with the inbound ctx — the transport injects traceparent
// from req.Context(), so a plain http.NewRequest silently dead-ends the trace.
req, _ := http.NewRequestWithContext(ctx, http.MethodGet, downstreamURL, nil)
resp, err := nethttp.HTTPClient().Do(req) // injects traceparent
logging.Logger().ErrorContext(ctx, "downstream failed", "err", err)
```

Wrap an existing client once at startup with `nethttp.WrapClient(client)` to add
propagation in place, or wrap just the transport of a client you build yourself:

```go
client := &http.Client{
    Timeout:   5 * time.Second,
    Transport: nethttp.Transport(nil), // or nethttp.Transport(yourCustomTransport)
}
```

---

## Summary

An Echo service needs, versus the two-line Gin adapter path: `telemetry.Init`,
serve via `nethttp.Handler(e)`, and two small middlewares (`TelemetryRecovery`,
`TelemetryMetrics`) plus the standard outbound-client / logger usage. Everything
else — sampling, propagation, resource attributes, exemplars, the metric set — is
identical, because it all lives in the framework-agnostic core.
