# Configuration

`publisher.New` takes a `publisher.Config` value. The zero value with only
`OperatorName` set is a valid configuration that enables both default
instrumentations (controller-runtime bridge and Go-runtime metrics).

```go
type Config struct {
    OperatorName                   string
    Version                        string
    OTLP                           *OTLPConfig
    Stdout                         bool
    DisableControllerRuntimeBridge bool
    DisableGoRuntime               bool
    Logger                         logr.Logger
}
```

## Config fields

### `OperatorName string` (required)

Used for the `service.name` OTel resource attribute. Must be non-empty —
`publisher.New` returns an error if it is empty. There is no further
validation; the value is passed through to the OTel SDK as-is. Choose a
short, stable identifier (e.g. `"my-operator"`).

Overridden by the `OTEL_SERVICE_NAME` environment variable if set.

### `Version string` (optional)

Used for the `service.version` resource attribute. Defaults to `"unknown"`
when left empty.

### `OTLP *OTLPConfig` (optional)

If non-nil, an OTLP exporter is constructed and attached to a periodic
reader. See [OTLPConfig fields](#otlpconfig-fields) below.

If nil but `OTEL_EXPORTER_OTLP_ENDPOINT` is set in the environment, an
OTLP exporter is still constructed from env-derived defaults — see
[Environment-variable overrides](#environment-variable-overrides).

If nil and no OTLP env vars are set, OTLP export is disabled. The
publisher logs a warning at construction time if neither OTLP nor
`Stdout` produces a reader.

### `Stdout bool` (optional)

When true, a stdout metric exporter is added alongside any OTLP exporter.
Intended for local development so custom metrics are visible without a
running collector. Default `false`.

The stdout exporter uses the OTel SDK's default periodic interval
(60 seconds). There is no separate configuration knob for the stdout
push cadence in this module.

### `DisableControllerRuntimeBridge bool` (optional)

When true, the Prometheus bridge producer that reads from
controller-runtime's `prometheus.Registry` is **not** attached to the OTLP
reader. Default `false`, meaning the bridge is enabled and
controller-runtime metrics flow into the OTLP push path.

The bridge has no effect on the `/metrics` endpoint either way — that
endpoint is owned by controller-runtime and never touched by this module.

### `DisableGoRuntime bool` (optional)

When true, `bridge.StartGoRuntime` is not called and Go-runtime metrics
(GC, heap, goroutines, etc. from `otel/contrib/instrumentation/runtime`)
are not collected. Default `false`.

### `Logger logr.Logger` (optional)

Logger for warnings emitted by the publisher (OTLP exporter construction
failure, "no readers configured", graceful-degradation messages). Defaults
to `logr.Discard()` when the zero `logr.Logger` is passed. The provided
logger is wrapped with `WithName("observability")`.

## OTLPConfig fields

```go
type OTLPConfig struct {
    Endpoint    string
    Protocol    string
    Insecure    bool
    Headers     map[string]string
    Timeout     time.Duration
    Compression string
    Interval    time.Duration
}
```

### `Endpoint string`

Collector address. Accepts either `host:port` (e.g.
`"otel-collector.observability.svc:4317"`) or `scheme://host:port/path`.
The value is passed straight through to the OTel exporter; this module
does not parse or validate it.

Overridden by `OTEL_EXPORTER_OTLP_ENDPOINT` if set.

### `Protocol string`

Either `"grpc"` (default) or `"http/protobuf"`. Any other value falls
through to gRPC.

Overridden by `OTEL_EXPORTER_OTLP_PROTOCOL` if set.

### `Insecure bool`

When true, TLS is disabled entirely and the exporter uses plaintext
(`grpc.WithInsecure` for gRPC, `otlpmetrichttp.WithInsecure` for HTTP).
Typical for in-cluster traffic that terminates TLS at the collector or
relies on a service mesh. Default `false`.

This is **not** "TLS with skipped certificate verification". When
`Insecure` is false, the OTel exporter uses default system TLS.

### `Headers map[string]string`

Per-export headers. Merged into the request headers on every export
(e.g. authentication tokens, tenant identifiers).

When `OTEL_EXPORTER_OTLP_HEADERS` is set, its values are merged into
this map (env values overwrite struct values for the same key).

### `Timeout time.Duration`

Per-export timeout. Defaults to `10s`.

### `Compression string`

Either `"gzip"` (default) or `""` (no compression). Applied to both gRPC
and HTTP. Any other value is treated as no compression.

### `Interval time.Duration`

Periodic push interval for the OTLP reader. Defaults to `30s`.

This interval applies only to the OTLP exporter. The stdout exporter (if
enabled) uses the OTel SDK's own default interval and is not affected.

## Environment-variable overrides

Standard OTel SDK environment variables override the corresponding fields
on `Config` and `OTLPConfig`. Env vars are applied **after** struct
defaults, so they always win when both are set.

| Variable | Overrides | Notes |
|---|---|---|
| `OTEL_SERVICE_NAME` | `OperatorName` | If both code and env are set, env wins |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `OTLP.Endpoint` | Also materializes `OTLP` from `nil` if any OTLP env var is set |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `OTLP.Protocol` | Same materialization behavior |
| `OTEL_EXPORTER_OTLP_HEADERS` | Merged into `OTLP.Headers` | Format: `k1=v1,k2=v2`; whitespace around keys/values is trimmed; malformed pairs are silently skipped |

The "env materializes OTLP" rule is what lets an operator ship with OTLP
disabled by default and have ops turn it on later with an env var, no
rebuild required.

If only `OTEL_SERVICE_NAME` is set (no OTLP env vars), the `OTLP` field
stays `nil` — no surprise network egress.

## Merge order

When `publisher.New` runs, it processes the config in this order:

1. **Built-in defaults** are applied to whatever the caller passed in
   (`Version → "unknown"`, `OTLP.Protocol → "grpc"`, etc.). Fields that
   are already set are left alone.
2. **Environment variables** are read and applied on top of the result.
   These always win over struct values.
3. **OTLP defaults** are re-applied if an OTLP block now exists (e.g. env
   just materialized one). This is a no-op safety net — env-materialized
   OTLP blocks already get defaults during step 2.

## Lifecycle

```go
ctx, cancel := signal.NotifyContext(context.Background(),
    syscall.SIGINT, syscall.SIGTERM)
defer cancel()

pub, err := publisher.New(ctx, publisher.Config{
    OperatorName: "my-operator",
    OTLP:         &publisher.OTLPConfig{Endpoint: "collector:4317", Insecure: true},
})
if err != nil {
    // Only fatal on misconfiguration (e.g., empty OperatorName).
    // OTLP unreachability is logged, not returned.
    os.Exit(1)
}
defer func() {
    shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancelShutdown()
    _ = pub.Shutdown(shutdownCtx)
}()
```

- Construct the publisher **before** the controller-runtime manager.
- `publisher.New` does not block on network I/O — OTLP exporters dial
  lazily on first export.
- `Shutdown` honours the provided context and always returns `nil`. Final
  flush errors (collector unreachable, double-shutdown on the provider)
  are logged at info level and swallowed.
- Use a fresh `context.Background()` with a short timeout for `Shutdown`,
  not the cancelled signal context — otherwise the final flush has no
  time budget.

See [exporters.md](exporters.md) for the full graceful-degradation contract.
