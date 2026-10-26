# telemetry-web: leave metrics to another component

- **Status:** design agreed 2026-10-23, implemented on this branch.
- **Tracked by:** SA-9148 (observability for cloud-orchestrator-gateway,
  part of the epic SA-9145).
- **Consumer that drives it:** cloud-orchestrator-gateway. Its half of the
  design is in that repository, in `docs/superpowers/specs/`,
  `2026-10-23-gateway-metrics-on-prometheus-design.md`.
- **Depends on:** operator-utils #28, which lets the `observability`
  module serve metrics on controller-runtime's `/metrics`.

## Why

A service that is both an operator and a web server wants two things from
operator-utils:

- **Metrics on its `/metrics` endpoint**, scraped by the cluster's
  Prometheus. They are vital for the cluster, so they are always on. The
  `observability` module provides this (with #28): its `Publisher` owns an
  OpenTelemetry meter provider and exposes it through controller-runtime's
  registry.
- **Tracing and HTTP instrumentation**, which only `telemetry-web` provides:
  a span and a duration metric per route, spans for the expensive stages,
  trace ids on log lines.

Today the two cannot be combined. `telemetry-web`'s `Init` always builds
its own meter provider, which only pushes over OTLP (the OpenTelemetry
Protocol), and makes it the global one. That replaces the `Publisher`'s
provider, so the HTTP metrics never reach `/metrics`. It also starts the
Go runtime metrics a second time, and on shutdown it resets the global
meter provider to a no-op, which would switch off the operator's metrics.

## Decisions

All agreed on 2026-10-23.

1. **An injected provider, `Config.MeterProvider`.** Changed in review
   (#33): the first version was an `ExternalMetrics` switch; an injected
   provider makes the dependency explicit and works with any provider. A
   nil pointer behind the interface is rejected. The two modules still
   stay independent: `telemetry-web` takes the standard
   `metric.MeterProvider` interface, so it does not depend on
   `observability` (a web library would pull in controller-runtime).
2. **Opt in.** The zero value keeps today's behaviour, so finops-gateway
   and any other user is unaffected.
3. **No environment variable.** `OTEL_METRICS_EXPORTER=none` means "no
   metrics at all", the opposite of this mode. A code setting is clearer.
4. **Malformed `Config.OTLPEndpoint` fails `Init` in both modes.** Changed
   in review (#33): the original decision was no early check.
5. Changed in review (#33): `nethttp` follows the global MeterProvider per
   request, removing the ordering rule.

## Design

### The field

```go
// MeterProvider, when set, is the provider another component owns, such as
// observability's Publisher. Init makes it the global one and sets up
// tracing only; shutdown neither flushes nor resets it.
MeterProvider metric.MeterProvider
```

### What Init does when it is set

Implemented in `telemetry.go`: `Init` and `startMetrics`, which returns
early in this mode.

| Step                                        | Default | `MeterProvider` set |
| ------------------------------------------- | ------- | ----------------- |
| error handler, service name, resource       | yes     | yes               |
| propagators, sampler, tracer provider       | yes     | yes               |
| metric exporter and meter provider          | yes     | **no**            |
| `otel.SetMeterProvider`                     | its own | the given one     |
| Go runtime metrics                          | yes     | **no**            |
| shutdown flushes                            | traces, metrics | traces only |
| shutdown or failure resets to no-op         | both providers | tracer only |

`retire()` (`telemetry.go`) gets a parameter saying whether to reset the
meter provider.
Everything else about `Init` stays as it is: `ErrAlreadyInitialized`, the
rule about calling it twice, and the protocol and TLS rules for traces.

### Instrumentation

`endpoint` already re-checks the global meter provider on each use
(`endpoint/endpoint.go`, `get()`). `nethttp.Handler` now does the same
(`nethttp/nethttp.go`, `followMeter`): otelhttp binds its instruments to the
provider it is built with, so the handler is rebuilt whenever the global
provider changes (one atomic load and a comparison per request). The
other component's provider may therefore be created before or after any
handler, and a re-Init needs no rebuilt handlers.

### Endpoint check

`Init` calls `validEndpoint` (`telemetry.go`) before it takes the lock or
changes any global state, in both modes. A URL form (with `://`) must
parse; a bare form must be `host:port`. Endpoints from `OTEL_*` variables
are not checked: the SDK reads them and reports problems when exporting.

### Docs

- README: a short section "Metrics owned by another component", with the
  `observability` example.
- `docs/reference.md`: the field and the table above.

## Testing

Written test first, in this module:

1. With `MeterProvider`, the global meter provider after `Init` is the
   given one, whatever was global before.
2. After shutdown it is still that object; the tracer provider is the
   no-op.
3. A nil pointer inside `MeterProvider` is rejected before anything
   global changes (`TestInitRejectsANilPointerMeterProvider`).
4. `retire(false)` leaves the meter provider alone. Tested
   directly: since the endpoint check, no input a test can provide fails
   `Init` after it starts changing global state. `retire(true)` is tested
   the same way.
5. End to end: the caller's provider with a manual reader, `Init` with
   `MeterProvider`, one request through `nethttp.Handler` and one
   `endpoint.Instrument`. The reader holds `http.server.request.duration`
   and `endpoint.duration`, and no `go.*` runtime metrics.
6. The existing tests pass, covering the default mode. The failure-path
   test for the default mode now calls `retire(true)` directly
   (`TestRetireResetsBothProvidersWhenItOwnsMetrics`).
7. `nethttp.Handler` records into the provider that is global when the
   request arrives (`TestHandlerFollowsTheGlobalMeterProvider`).

## Out of scope

- A Prometheus exporter inside `telemetry-web`. Metrics on `/metrics` are
  `observability`'s job.
- Changes to `observability`; #28 is used as it is.
- The gateway's wiring, chart and ServiceMonitor: see the gateway's spec.
