# Exporters

The publisher can construct up to two metric readers, one per exporter:

- **OTLP reader** — periodic push to a collector over gRPC or HTTP. Driven by
  `Config.OTLP` (and OTLP environment variables).
- **Stdout reader** — periodic dump of metrics to stdout for local
  development. Driven by `Config.Stdout`.

Both are optional. If neither is configured, the publisher constructs
successfully with no readers and logs a single warning at startup:

```
no metric readers configured; custom metrics will not be exported
```

The `Publisher` still works in this state — registrations succeed and
recording calls are non-fatal — they just go nowhere.

## OTLP exporter

### Protocol selection

The `Config.OTLP.Protocol` field selects the wire protocol:

| Value | Behaviour |
|---|---|
| `"grpc"` (default) | Uses `otlpmetricgrpc`, gRPC over HTTP/2 |
| `"http/protobuf"` | Uses `otlpmetrichttp`, protobuf over HTTP/1.1 |
| anything else | Falls through to gRPC silently |

Unknown values are not currently logged as warnings; this may change.

### Endpoint format

Endpoint is passed through to the underlying OTel exporter without parsing:

- `host:port` — both gRPC and HTTP exporters accept this form
- `scheme://host:port/path` — also accepted; the scheme part is honoured
  by the HTTP exporter and ignored by gRPC

There is no separate `Path` field. For HTTP/protobuf, the default path is
`/v1/metrics`; override by including it in `Endpoint`.

### TLS

`OTLPConfig.Insecure` controls plaintext vs TLS:

| Insecure | gRPC | HTTP |
|---|---|---|
| `false` (default) | TLS via system roots | TLS via system roots |
| `true` | Plaintext (`grpc.WithInsecure`) | Plaintext (`otlpmetrichttp.WithInsecure`) |

There is no `InsecureSkipVerify` (TLS-on-but-don't-verify-certs). If you
need that, fall back to `pub.Meter()` and construct the exporter yourself.

### Headers

`OTLPConfig.Headers` is sent on every export, merged with values from
`OTEL_EXPORTER_OTLP_HEADERS` if that env var is set. The format for the
env var is `k1=v1,k2=v2`:

```
OTEL_EXPORTER_OTLP_HEADERS="Authorization=Bearer abc,X-Tenant=acme"
```

Whitespace around keys and values is trimmed. Malformed pairs (missing
`=`) are silently skipped.

Headers are typically used for:
- Authentication tokens (`Authorization: Bearer …`)
- Tenant identifiers for multi-tenant collectors
- Workspace identifiers for SaaS observability backends

### Compression

`OTLPConfig.Compression` is either `"gzip"` (default) or `""`. Any other
value is treated as no compression.

Gzip is reasonable for most workloads — the OTLP wire format compresses
well and the CPU cost is negligible compared to the network savings.

### Timeout and interval

| Field | Default | Meaning |
|---|---|---|
| `Timeout` | `10s` | Per-export deadline. If an export takes longer, it is cancelled and the data lost |
| `Interval` | `30s` | Periodic push cadence — how often the exporter flushes accumulated metrics |

Choose `Timeout` < `Interval`, otherwise overlapping exports can pile up.

### Graceful degradation

The publisher's contract for OTLP unreachability:

1. `publisher.New` **never** returns an error because the collector is
   down. Exporter construction in OTel does not dial — connection happens
   lazily on first export.
2. Failed exports happen in the OTel SDK's background goroutine. The
   publisher does not propagate them to the operator's code path. They
   are visible only via the SDK's internal error handler (which defaults
   to stderr).
3. `Shutdown` swallows flush errors. The final flush attempt runs against
   the configured `Timeout`; if it fails, the error is logged at info
   level and `Shutdown` still returns `nil`.

The net effect: an OTLP outage does not crash, slow, or block the
operator. Custom metrics keep being recorded; they just don't reach the
collector until it comes back.

## Stdout exporter

Enabled by `Config.Stdout = true`. Constructs a `stdoutmetric` reader
that prints accumulated metrics as JSON to the process's stdout on a
fixed cadence (60 seconds — the OTel SDK default).

There is no configuration knob for the stdout interval in this module.
For ad-hoc shorter cadences in local dev, send `SIGINT` and let the
graceful shutdown's final flush print the latest values.

The stdout exporter does **not** have a bridge producer attached — it
prints only metrics owned by the OTel SDK (custom metrics + Go runtime
metrics if enabled). Controller-runtime metrics, which flow exclusively
through the bridge to OTLP, are not visible on stdout.

Typical local dev configuration:

```go
pub, _ := publisher.New(ctx, publisher.Config{
    OperatorName: "demo",
    Stdout:       true,
})
```

## Controller-runtime bridge producer

When `Config.OTLP` is set and `Config.DisableControllerRuntimeBridge` is
false (default), a Prometheus bridge producer is attached to the OTLP
reader. The producer reads from `sigs.k8s.io/controller-runtime/pkg/metrics.Registry`
on every OTLP flush and includes those metrics in the OTLP payload.

The bridge is attached **only** to the OTLP reader, never to the stdout
reader. Controller-runtime metrics never flow through stdout.

See [extension-points.md](extension-points.md) for using the bridge
producer with a custom-built MeterProvider.

## Resource attributes

Every exported metric carries the OTel Resource constructed by
`resource.Build`, with these attributes:

| Attribute | Source |
|---|---|
| `service.name` | `Config.OperatorName` |
| `service.version` | `Config.Version` (or `"unknown"`) |
| `service.instance.id` | `POD_NAME` env, falls back to `os.Hostname()` |
| `k8s.pod.name` | Same as `service.instance.id` |
| `k8s.namespace.name` | `POD_NAMESPACE` env (omitted if unset) |

For more on the resource, see [extension-points.md](extension-points.md).

## Reader composition

The two readers run independently. Each maintains its own collection
state, so a custom metric recorded once will appear once in each
configured exporter. No reader is "downstream" of another.

Example: a `Counter.Inc` call with both OTLP and Stdout enabled will
result in:

- The increment being aggregated in the OTel SDK's per-instrument state.
- On the next OTLP flush (30s by default), the OTLP exporter ships the
  accumulated counter delta.
- On the next stdout flush (60s by default), the stdout exporter prints
  the same accumulated counter delta.

If both flushes happen at the same time, you may see the same value
exported twice — once to OTLP, once to stdout. That's expected; OTLP
backends use the resource attributes to deduplicate.
