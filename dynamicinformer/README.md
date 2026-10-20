# dynamicinformer

A lifecycle-aware Dynamic Watch Registry for Kubernetes operators that need to watch arbitrary resource types determined at runtime.

## Problem

Kubernetes operators that manage child resources dynamically face a recurring challenge: they need to watch arbitrary resource types determined at runtime, not at compile time. Naively starting an informer per consumer per resource type leads to duplicate watches, wasted API server connections, and no clean way to tear them down when consumers are removed.

## What This Package Does

- **No duplicate informers.** The same GVR + namespace + identity combination is watched exactly once, regardless of how many consumers reference it.
- **No duplicate registrations.** The same consumer + watch key combination produces at most one handler. Repeated calls are idempotent.
- **Automatic stale watch cleanup.** When a consumer's required watch set changes, stale watches are released automatically.
- **Dynamic resource support.** Resource types are not known at compile time; the registry accepts any GVR.
- **Clean lifecycle management.** Watches are started and stopped at runtime via context cancellation, with no leaked goroutines.
- **Per-consumer callbacks.** Each consumer registers an `EventCallback` per watch key. The registry manages handler registration and deregistration.
- **Full event fidelity.** Callbacks receive `oldObj` on updates and correct tombstone unwrapping on deletes.
- **Optional cache sync.** Consumers who need to guarantee no missed events can wait via the returned `HasSynced` handle.

## Installation

```bash
go get github.com/stakater/operator-utils/dynamicinformer
```

## Quick Start

```go
import (
    "context"
    "time"

    "k8s.io/apimachinery/pkg/runtime/schema"
    "k8s.io/client-go/dynamic"

    di "github.com/stakater/operator-utils/dynamicinformer"
)

// Create the registry (once, typically at startup)
dynamicClient := dynamic.NewForConfigOrDie(restConfig)
registry := di.NewWatchRegistry(dynamicClient, 30*time.Second)

// Define what to watch
watches := []di.WatchRequest{
    {
        Key: di.WatchKey{
            GVR:       schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
            Namespace: "default",
        },
        Callback: di.EventCallback{
            OnAdd:    func(obj *unstructured.Unstructured) { /* handle add */ },
            OnUpdate: func(old, new *unstructured.Unstructured) { /* handle update */ },
            OnDelete: func(obj *unstructured.Unstructured) { /* handle delete */ },
        },
    },
}

// Register watches — returns registrations with HasSynced for optional cache sync
regs, err := registry.EnsureWatchSet(ctx, "my-consumer", watches)

// Later: update the watch set (adds new watches, removes stale ones automatically)
regs, err = registry.EnsureWatchSet(ctx, "my-consumer", updatedWatches)

// On shutdown: release all watches for this consumer
err = registry.ReleaseAll("my-consumer")
```

## API

### `NewWatchRegistry`

```go
func NewWatchRegistry(dynamicClient dynamic.Interface, resyncPeriod time.Duration) *WatchRegistry
```

Creates a new registry. The `resyncPeriod` controls how often each informer relists from the API server. A non-zero value (typically 30s) is required to recover from dropped events. Safe for concurrent use.

### `EnsureWatchSet`

```go
func (r *WatchRegistry) EnsureWatchSet(
    parentCtx  context.Context,
    consumerID string,
    watches    []WatchRequest,
) ([]*WatchRegistration, error)
```

The primary API. Takes the **complete set** of watches a consumer currently needs and reconciles the registry to match:
- New keys get informers started and handlers registered.
- Existing keys are returned as-is (idempotent).
- Keys no longer in the set are released automatically.
- An empty or nil slice releases all watches (equivalent to `ReleaseAll`).

**`parentCtx` must outlive the consumer's individual operations** — typically the manager or application context. Per-reconcile contexts are cancelled when the reconcile returns, which would immediately stop informers.

**Callbacks are registered once and not updated.** They must derive all context from the event object and stable references, not from call-site-scoped state.

### `ReleaseAll`

```go
func (r *WatchRegistry) ReleaseAll(consumerID string) error
```

Releases all watches for a consumer. Idempotent — calling on an unknown consumer returns nil.

### `NewWatchRegistryWithProvider`

```go
func NewWatchRegistryWithProvider(provider ClientProvider, resyncPeriod time.Duration, opts ...RegistryOption) *WatchRegistry
```

Creates a registry that resolves the client for each watch from `provider`, keyed by `WatchKey.Identity`. Watches under different identities never share an informer, even for the same GVR and namespace, since they may be authorized differently. The registry treats the identity as an opaque string; what it maps to (impersonation, a token, another cluster) is the provider's concern. `NewWatchRegistry` is the single-client form: it serves the empty identity only and rejects any other.

```go
// ClientProvider supplies the dynamic client for an identity. "" is the default client.
type ClientProvider interface {
    ClientFor(identity string) (dynamic.Interface, error)
}

// ClientReleaser is optional. The registry calls Release exactly once for every
// ClientFor call that returned a client, when the informer started with it stops.
type ClientReleaser interface {
    Release(identity string)
}
```

`ClientFor` is called every time an informer starts, so providers should cache clients per identity. A provider that also implements `ClientReleaser` can bound that cache with a reference count: increment in `ClientFor`, decrement in `Release`, evict at zero. Both calls run while the registry holds its lock, so they must not call back into the registry. Providers that do not implement `ClientReleaser` are never notified.

### `WithInformerFunc`

```go
type InformerFunc func(cli dynamic.Interface, key WatchKey, resyncPeriod time.Duration) (cache.SharedIndexInformer, error)

func WithInformerFunc(f InformerFunc) RegistryOption
```

Replaces how the registry builds informers. Pass it to either constructor. `WithInformerFunc(nil)` keeps the default dynamic informer.

- Return a fresh, unstarted, non nil informer. The registry owns it and starts and stops it with the watch, so never return one from a long lived factory.
- Set the transform and key func here, they must be in place before `Run`. Indexers can be added here or later on the shared informer, as long as names do not clash. A separate registry is only needed for a different key func or transform.
- The func runs while the registry holds its lock, like `ClientFor`. Keep it quick and never call back into the registry.
- A returned error, a nil informer or a panic fails `EnsureWatchSet` for that call, rolls back the keys it added and releases the client.

Example with a kcp cluster aware informer, keyed `cluster|namespace/name`:

```go
func newClusterAwareInformer(cli dynamic.Interface, key di.WatchKey, resync time.Duration) (cache.SharedIndexInformer, error) {
    ri := cli.Resource(key.GVR).Namespace(key.Namespace) // "" lists all namespaces
    lw := &cache.ListWatch{
        ListWithContextFunc:  func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) { return ri.List(ctx, o) },
        WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) { return ri.Watch(ctx, o) },
    }
    inf := kcpinformers.NewSharedIndexInformerWithOptions(lw, &unstructured.Unstructured{}, cache.SharedIndexInformerOptions{
        ResyncPeriod: resync,
        Indexers:     cache.Indexers{kcpcache.ClusterIndexName: kcpcache.ClusterIndexFunc},
    })
    if err := inf.SetTransform(dropManagedFields); err != nil {
        return nil, err
    }
    return inf, nil
}

registry := di.NewWatchRegistryWithProvider(provider, 30*time.Second, di.WithInformerFunc(newClusterAwareInformer))
```

## Convenience Callbacks

Callback helpers are provided for common patterns. All use non-blocking channel sends (dropped events are recovered by informer resync).

### `EventCallbackFromMapFunc`

The foundational helper. Takes a `MapFunc` that maps an event object to zero or more `reconcile.Request`s, giving the consumer full control over what gets enqueued:

```go
eventCh := make(chan event.GenericEvent, 1024)

callback := di.EventCallbackFromMapFunc(
    func(obj *unstructured.Unstructured) []reconcile.Request {
        annotations := obj.GetAnnotations()
        name := annotations["example.com/owner-name"]
        ns := annotations["example.com/owner-namespace"]
        if name == "" || ns == "" {
            return nil
        }
        return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}}
    },
    eventCh,
)
```

### `LabelSelectorCallback`

Filters events by label selector, then delegates to a `MapFunc` to determine which reconcile requests to enqueue:

```go
selector, _ := labels.Parse("app=nginx")
eventCh := make(chan event.GenericEvent, 1024)

callback := di.LabelSelectorCallback(selector,
    func(obj *unstructured.Unstructured) []reconcile.Request {
        return []reconcile.Request{{NamespacedName: types.NamespacedName{
            Name:      obj.GetAnnotations()["example.com/owner-name"],
            Namespace: obj.GetAnnotations()["example.com/owner-namespace"],
        }}}
    },
    eventCh,
)
```

### `EnqueueByOwnerAnnotationCallback`

Routes events to an owning resource identified by annotations. Useful for cross-namespace ownership where `OwnerReferences` cannot be used:

```go
eventCh := make(chan event.GenericEvent, 1024)

callback := di.EnqueueByOwnerAnnotationCallback(
    "example.com/owner-name",
    "example.com/owner-namespace",
    eventCh,
)
```

## Controller-Runtime Integration

The registry is designed to work with controller-runtime's `source.Channel` for feeding events into a controller's work queue:

```go
func SetupWithManager(mgr ctrl.Manager) error {
    dynamicClient := dynamic.NewForConfigOrDie(mgr.GetConfig())
    registry := di.NewWatchRegistry(dynamicClient, 30*time.Second)

    eventCh := make(chan event.GenericEvent, 1024)

    // Stable callback — created once, resolves ownership from annotations at event time
    ownerCallback := di.EventCallbackFromMapFunc(
        func(obj *unstructured.Unstructured) []reconcile.Request {
            annotations := obj.GetAnnotations()
            name := annotations["example.com/owner-name"]
            ns := annotations["example.com/owner-namespace"]
            if name == "" || ns == "" {
                return nil
            }
            return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}}
        },
        eventCh,
    )

    reconciler := &MyReconciler{
        client:        mgr.GetClient(),
        registry:      registry,
        managerCtx:    mgr.GetContext(),  // must outlive individual reconciles
        ownerCallback: ownerCallback,
    }

    return ctrl.NewControllerManagedBy(mgr).
        For(&v1alpha1.MyResource{}).
        WatchesRawSource(source.Channel(eventCh, &handler.EnqueueRequestForObject{})).
        Complete(reconciler)
}
```

Inside the reconciler:

```go
func (r *MyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    // ... discover which resource types to watch ...

    watches := []di.WatchRequest{
        {
            Key:      di.WatchKey{GVR: deploymentGVR, Namespace: "team-alpha"},
            Callback: r.ownerCallback,
        },
        {
            Key:      di.WatchKey{GVR: configmapGVR, Namespace: "team-beta"},
            Callback: r.ownerCallback,
        },
    }

    // Reconcile watches — adds new, removes stale
    _, err := r.registry.EnsureWatchSet(r.managerCtx, string(myResource.UID), watches)
    if err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{}, nil
}
```

On deletion (requires a finalizer to ensure cleanup runs before garbage collection):

```go
func (r *MyReconciler) reconcileDelete(ctx context.Context, obj *v1alpha1.MyResource) (ctrl.Result, error) {
    if err := r.registry.ReleaseAll(string(obj.UID)); err != nil {
        log.Error(err, "failed to release watches")
    }

    controllerutil.RemoveFinalizer(obj, finalizerName)
    return ctrl.Result{}, r.client.Update(ctx, obj)
}
```

## Optional Cache Sync

Handlers registered before the informer's initial list completes may miss events. If your consumer needs to guarantee no missed events, wait for cache sync before proceeding:

```go
regs, err := registry.EnsureWatchSet(ctx, consumerID, watches)
if err != nil {
    return err
}
for _, reg := range regs {
    if !cache.WaitForCacheSync(ctx.Done(), reg.HasSynced) {
        return fmt.Errorf("cache sync failed for %v", reg.Key)
    }
}
```

After sync, `reg.GetIndexer()` returns the informer's cache (`GetByKey`, `List`, `ByIndex`). The cache is shared with every consumer of the same key, so treat its objects as read only. Cache keys come from the `InformerFunc`: with the kcp example above they are `cluster|namespace/name`, so `GetByKey("ns/name")` finds nothing.

`GetIndexer` returns `ErrWatchStopped` if the watch has already stopped for this consumer: it released the key (`ReleaseAll`, or a later `EnsureWatchSet` without it), or `parentCtx` was cancelled. `HasSynced` returns false in the same cases. Keep the registration, not the indexer, and call `GetIndexer` on every read, so a stopped watch is noticed.

## Running the Interactive Example

An interactive example program is included that demonstrates the registry against a live cluster.

### Prerequisites

- A running Kubernetes cluster (e.g. [kind](https://kind.sigs.k8s.io/))
- `kubectl` configured to talk to the cluster

### Create a kind cluster (if needed)

```bash
kind create cluster --name demo
```

### Run the example

```bash
cd dynamicinformer
kubectl config use-context kind-demo
go run ./example/
```

### Interact with it

The example walks through four steps. In a separate terminal, run the suggested `kubectl` commands at each step:

**Step 1 — Watch ConfigMaps:**

```bash
kubectl create configmap demo-cm --from-literal=key=value     # triggers ADD
kubectl patch configmap demo-cm -p '{"data":{"key":"updated"}}'  # triggers UPDATE
kubectl delete configmap demo-cm                                  # triggers DELETE
```

Press Enter in the example terminal to continue.

**Step 2 — Add Secrets watch (ConfigMaps watch stays):**

The ConfigMap informer is reused (deduplication). A new Secrets informer starts. Existing Secrets appear as ADD events.

```bash
kubectl create secret generic demo-secret --from-literal=pw=test  # triggers ADD
kubectl delete secret demo-secret                                   # triggers DELETE
```

Press Enter to continue.

**Step 3 — Remove ConfigMaps watch (keep only Secrets):**

The ConfigMap informer is stopped (stale watch cleanup). Only Secret events are delivered.

```bash
kubectl create configmap invisible --from-literal=k=v   # no event expected
kubectl delete configmap invisible                        # no event expected
```

Press Enter to continue.

**Step 4 — ReleaseAll:**

All watches are released. No more events are delivered regardless of what happens in the cluster.

Press Enter to exit.

### Cleanup

```bash
kind delete cluster --name demo
```

## Design Decisions

| Decision | Rationale |
|---|---|
| `EnsureWatchSet` (declarative) over `EnsureWatch`/`ReleaseWatch` (imperative) | Consumers state what they want; the registry diffs internally. No consumer-side bookkeeping needed. |
| Per-key callbacks over a single callback | Different resource types may need different event handling. Consumers that want one callback for all keys pass the same `EventCallback` in every `WatchRequest`. |
| Reverse index (`consumerKeys`) | Gives O(1) lookup of a consumer's keys. Makes stale-watch detection a set difference instead of a full scan. |
| Stable callbacks, not call-site closures | `EnsureWatchSet` registers handlers once. Callbacks resolve all dynamic state from the event object at invocation time. |
| Expose `HasSynced` instead of waiting internally | Most consumers don't need the guarantee — resync catches missed events. Those who do can opt in. |
| Non-blocking channel sends in helpers | Backpressure drops events instead of blocking informer goroutines. The non-zero resync period recovers dropped events on the next relist. |
| Best-effort handler removal | If `RemoveEventHandler` fails (typically because the informer is already stopped), the handler is removed from the map regardless. |
| Indexers and transform on the `InformerFunc`, not on `WatchRequest` | The informer is shared per `WatchKey`, so per request options would conflict. Key func and transform must be set before `Run`, so they are fixed per registry. |
| Optional `ClientReleaser` over a `Release` method on `ClientProvider` | Existing providers keep working unchanged. The registry type-asserts once at construction and pairs one `Release` with every `ClientFor` that returned a client (a `(nil, nil)` result is rejected and not released), so a provider can reference-count without knowing anything about informers. |

## Known Limitations

- **Event gap during initial list.** Handlers registered before the informer syncs may miss events. Use `HasSynced` to wait if needed.
- **Informer startup errors are not surfaced programmatically.** The reflector retries with backoff and logs errors. Monitor reflector logs for persistent failures (RBAC, missing GVR).
- **Parent context cancellation is not a release.** Cancelling `parentCtx` stops every informer but leaves registry entries in place and never calls `Release`. It is meant for process shutdown; use `ReleaseAll` to tear a consumer down while running. Registrations of those entries return `ErrWatchStopped`, and a later `EnsureWatchSet` reuses the stopped entries, so do not reuse the registry after cancelling `parentCtx`.
- **Lock held during informer creation.** `ClientFor`, the `InformerFunc` and `Release` all run under the registry mutex, which keeps `EnsureWatchSet` all or nothing. Keep them quick and never call back into the registry: the mutex is not reentrant, so a callback deadlocks, and a slow func (discovery, network I/O) stalls every other caller. List and watch happen asynchronously in `Run`, so building an informer is cheap.
- **All handlers on a shared informer receive all events.** Filtering is each handler's responsibility.

## Testing

```bash
cd dynamicinformer
go test -race -v ./...
```
