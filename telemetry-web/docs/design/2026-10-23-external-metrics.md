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

1. **A switch in `Config`, not an injected provider.** The two modules stay
   independent and meet only through OpenTelemetry's global provider.
   Injecting a provider (`Config.MeterProvider`) was rejected: the
   `Publisher` exposes no provider, and it raises questions about who sets
   and retires the global. Depending on `observability` from `telemetry-web`
   was rejected: a web library would pull in controller-runtime.
2. **Named `ExternalMetrics`.** It says what is true: the metrics exist,
   someone else owns the provider. `DisableMetrics` would suggest the HTTP
   metrics stop, which they do not.
3. **Opt in.** The zero value keeps today's behaviour, so finops-gateway
   and any other user is unaffected.
4. **No environment variable.** `OTEL_METRICS_EXPORTER=none` means "no
   metrics at all", the opposite of this mode. A code switch is clearer.
5. **Malformed `Config.OTLPEndpoint` fails `Init` in both modes.** Changed
   in review (#33): the original decision was no early check.
6. Changed in review (#33): `nethttp` follows the global MeterProvider per
   request, removing the ordering rule.

## Design

### The field

```go
// ExternalMetrics: another component, such as observability's Publisher,
// owns the global MeterProvider. Init then sets up tracing only and never
// builds, starts runtime metrics on, sets or resets that provider.
ExternalMetrics bool
```

### What Init does when it is set

Implemented in `telemetry.go`: `Init` and `startMetrics`, which returns
early in this mode.

| Step                                        | Default | `ExternalMetrics` |
| ------------------------------------------- | ------- | ----------------- |
| error handler, service name, resource       | yes     | yes               |
| propagators, sampler, tracer provider       | yes     | yes               |
| metric exporter and meter provider          | yes     | **no**            |
| `otel.SetMeterProvider`                     | yes     | **no**            |
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

1. With `ExternalMetrics`, the global meter provider after `Init` is the
   same object the caller set before it.
2. After shutdown it is still that object; the tracer provider is the
   no-op.
3. `retire` with the external flag leaves the meter provider alone. Tested
   directly: since the endpoint check, no input a test can provide fails
   `Init` after it starts changing global state. `retire(true)` is tested
   the same way.
4. End to end: the caller's provider with a manual reader, `Init` with
   `ExternalMetrics`, one request through `nethttp.Handler` and one
   `endpoint.Instrument`. The reader holds `http.server.request.duration`
   and `endpoint.duration`, and no `go.*` runtime metrics.
5. The existing tests pass, covering the default mode. The failure-path
   test for the default mode now calls `retire(true)` directly
   (`TestRetireResetsBothProvidersWhenItOwnsMetrics`).
6. `nethttp.Handler` records into the provider that is global when the
   request arrives (`TestHandlerFollowsTheGlobalMeterProvider`).

## Out of scope

- A Prometheus exporter inside `telemetry-web`. Metrics on `/metrics` are
  `observability`'s job.
- Changes to `observability`; #28 is used as it is.
- The gateway's wiring, chart and ServiceMonitor: see the gateway's spec.
