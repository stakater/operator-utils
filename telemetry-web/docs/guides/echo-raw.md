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
| panic recording | `endpoint.RecordPanic` |
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
require github.com/stakater/operator-utils/telemetry-web v0.0.0-20260707172313-d97fce7d0cfb
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
    e.Use(TelemetryMetrics())  // automatic per-endpoint metrics (see §4)

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

Forward panics to `endpoint.RecordPanic`, respond `500`, and re-raise
`http.ErrAbortHandler` so net/http handles it.

```go
func TelemetryRecovery() echo.MiddlewareFunc {
    return func(next echo.HandlerFunc) echo.HandlerFunc {
        return func(c echo.Context) error {
            defer func() {
                if rec := recover(); rec != nil {
                    if rec == http.ErrAbortHandler {
                        panic(rec)
                    }
                    endpoint.RecordPanic(c.Request().Context(), rec)
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

## 4. Automatic per-endpoint metrics middleware

This is the piece the Gin adapter gives you for free. Echo's `c.Path()` returns
the matched route template (e.g. `/users/:id`), so key the metric on it.

```go
func TelemetryMetrics() echo.MiddlewareFunc {
    return func(next echo.HandlerFunc) echo.HandlerFunc {
        return func(c echo.Context) error {
            err := next(c)

            route := c.Path()
            if route != "" {
                // Echo runs its error handler AFTER the middleware chain returns,
                // so a returned error is the reliable failure signal here; also
                // catch handlers that set a 5xx status directly.
                failed := err != nil || c.Response().Status >= 500
                endpoint.Record(c.Request().Context(), route, failed)
            }
            return err
        }
    }
}
```

That emits `http.endpoint.requests{endpoint="/users/:id", outcome}` for every
matched route — identical to the Gin adapter's output. Unmatched requests have an
empty `c.Path()` and are skipped, keeping cardinality bounded.

The adapters additionally stamp `http.route` on the server span and the
`http.server.request.duration` metric. To match that here, add this where the
route is known (imports: `otelhttp`, `semconv`, `otel/trace`):

```go
attr := semconv.HTTPRoute(route)
labeler, _ := otelhttp.LabelerFromContext(c.Request().Context())
labeler.Add(attr)                                                  // -> duration metric
trace.SpanFromContext(c.Request().Context()).SetAttributes(attr)   // -> span
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
