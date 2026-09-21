# Reference documentation

In-depth reference for the `observability` module. For a top-level overview
and the integration quickstart, see the [module README](../README.md). For a
runnable program, see [`example/`](../example/).

| Doc | What's in it |
|---|---|
| [configuration.md](configuration.md) | Every field on `publisher.Config`, `publisher.PrometheusConfig` and `publisher.OTLPConfig`, default values, environment-variable overrides, and the merge order |
| [instruments.md](instruments.md) | The `Counter`, `Gauge`, and `Histogram` interfaces — method signatures, semantics, attribute usage, when to pick each |
| [custom-metrics.md](custom-metrics.md) | Registering metrics via `publisher.CustomMetrics`: validation rules, reserved prefixes, duplicate-name handling, `Must*` panic semantics |
| [exporters.md](exporters.md) | The Prometheus `/metrics` reader and its name translation, OTLP gRPC vs HTTP wiring, stdout exporter, headers, TLS, compression, reader composition, graceful degradation |
| [extension-points.md](extension-points.md) | Using `resource.Build` and the `bridge` package without going through the `Publisher` — for callers who wire their own `MeterProvider` |

## Quick package map

| Package | Import path |
|---|---|
| Publisher (main entry point) | `github.com/stakater/operator-utils/observability/pkg/publisher` |
| Instrument interfaces | `github.com/stakater/operator-utils/observability/pkg/instrument` |
| Naming validators | `github.com/stakater/operator-utils/observability/pkg/naming` |
| Resource builder | `github.com/stakater/operator-utils/observability/pkg/resource` |
| Bridge / runtime wiring | `github.com/stakater/operator-utils/observability/pkg/bridge` |
