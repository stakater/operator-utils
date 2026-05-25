# operator-utils

A Go utility library for building Kubernetes operators with [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime). It eliminates common boilerplate around reconciliation, resource lifecycle, and dynamic watches so operator authors can focus on business logic.

## Installation

```bash
go get github.com/stakater/operator-utils
```

## Packages

### `dynamicinformer` — Dynamic Watch Registry

A concurrency-safe registry that manages dynamic informers for arbitrary `GroupVersionResource` types at runtime. Useful when the set of resources an operator needs to watch is not known at compile time (e.g., determined by CR spec fields).

**Key features:**

- **Declarative watch sets** — call `EnsureWatchSet` with the complete set of watches a consumer needs; the registry adds new watches and removes stale ones automatically.
- **Informer deduplication** — multiple consumers watching the same GVR/namespace share a single underlying informer.
- **Automatic cleanup** — when a consumer no longer needs a watch, its event handler is removed. When no consumers remain for an informer, it is stopped.
- **Atomic rollback** — if registering a new handler fails, previously added handlers in the same call are rolled back so the consumer retains its prior state.
- **Callback helpers** — `EventCallbackFromMapFunc`, `LabelSelectorCallback`, and `EnqueueByOwnerAnnotationCallback` adapt common patterns (map functions, label filtering, cross-namespace ownership) into `EventCallback` values ready for registration.

```go
registry := dynamicinformer.NewWatchRegistry(dynamicClient, 30*time.Second)

regs, err := registry.EnsureWatchSet(ctx, "my-controller", []dynamicinformer.WatchRequest{
    {
        Key:      dynamicinformer.WatchKey{GVR: configmapsGVR, Namespace: "default"},
        Callback: dynamicinformer.EventCallbackFromMapFunc(myMapFunc, eventCh),
    },
})
```

See [dynamicinformer/example/main.go](dynamicinformer/example/main.go) for a full interactive demo.

### `observability` — OpenTelemetry Metrics Publisher

A standalone Go module ([`observability/`](observability/README.md)) that wires up an OTel `MeterProvider` for operators built on controller-runtime. Custom and Go-runtime metrics are pushed over OTLP; controller-runtime's existing Prometheus `/metrics` endpoint is left untouched, with a one-directional bridge feeding its metrics into the same OTLP path.

**Key features:**

- **One call to construct** — `publisher.New(ctx, publisher.Config{...})` sets up the MeterProvider, OTLP and/or stdout readers, controller-runtime bridge, and Go-runtime instrumentation.
- **Graceful degradation** — collector unreachable at startup or shutdown is logged, never returned as an error.
- **Env-var overrides** — standard OTel env vars (`OTEL_SERVICE_NAME`, `OTEL_EXPORTER_OTLP_ENDPOINT`, etc.) override the Go config so ops can flip OTLP on without a rebuild.
- **Custom metric registry** — `pub.Custom().MustCounter/Gauge/Histogram(name, description)` with strict naming validation and duplicate-name guards.

```go
pub, _ := publisher.New(ctx, publisher.Config{
    OperatorName: "my-operator",
    OTLP:         &publisher.OTLPConfig{Endpoint: "collector:4317", Insecure: true},
})
defer pub.Shutdown(context.Background())

reconcileTotal := pub.Custom().MustCounter("reconcile_total", "...")
reconcileTotal.Inc(ctx, attribute.String("result", "success"))
```

See [observability/README.md](observability/README.md) for the quickstart, [observability/docs/](observability/docs/README.md) for reference docs, and [observability/example/](observability/example/) for a runnable program.

### `util/reconciler` — Reconcile Result Helpers

Standardizes reconcile outcomes and status condition management.

- `ManageSuccess` / `ManageError` — update status conditions on any CR that implements the `ConditionsStatusAware` interface, then return the appropriate `ctrl.Result`.
- `DoNotRequeue`, `RequeueWithError`, `RequeueAfter` — self-documenting wrappers for common `ctrl.Result` patterns.

```go
// On error
return reconciler.ManageError(client, obj, err, true /* retriable */)

// On success
return reconciler.ManageSuccess(client, obj)

// Simple requeue
return reconciler.RequeueAfter(30 * time.Second)
```

### `util/finalizer` — Finalizer Management

Safe helpers to add, remove, and check finalizers on any `metav1.Object`.

```go
finalizer.AddFinalizer(obj, "my-finalizer")
finalizer.DeleteFinalizer(obj, "my-finalizer")
if finalizer.HasFinalizer(obj, "my-finalizer") { ... }
```

### `util/resourcefiltering` — Allow/Deny Resource Filtering

Embeddable `AllowDeny` type (with generated deep-copy support) for CRD specs that need user-configurable allow/deny lists.

- **Allow takes precedence** — if an allow list is set, only listed values pass (deny is ignored).
- **Nil-safe** — `IsAllowed(nil, name)` returns `false`.

```go
ad := &resourcefiltering.AllowDeny{
    Allow: &resourcefiltering.Allow{Literal: []string{"ns-a", "ns-b"}},
}
ad.Pass("ns-a") // true
ad.Pass("ns-c") // false
```

### `util/crd` — CRD Status Checks

```go
if crd.Established(myCRD) {
    // CRD is accepted and the API server is serving it
}
```

### `util/objects` — CreateOrUpdate with Diff Logging

Wraps `controllerutil.CreateOrUpdate` to optionally produce a diff of changes (when `DEBUG` is enabled).

### `util/secrets` — Secret Data Loader

Retrieves a specific key from a Kubernetes Secret by name and namespace, using either a `client.Reader` or `client.Client`.

```go
token, err := secrets.LoadSecretData(apiReader, "my-secret", "default", "token")
```

### `util` — General Helpers

- `GetOperatorNamespace` — reads the in-cluster namespace from the service account mount.
- `StringP` / `PString` — pointer ↔ value conversions for strings.
- `TimeElapsed` — defer-friendly function timer for quick profiling.
- `GetLabelSelector` — builds a `labels.Selector` from a single key/value pair.

### `util/labels` — Label Helpers

- `AddLabel` — nil-safe insert/update of a label in a map.
- `GetLabelSelector` — builds a `labels.Selector` from a key/value pair.
