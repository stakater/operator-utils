# objstate — per-object state metrics

`pkg/objstate` publishes metrics about the OBJECTS an operator manages — one
CostJob's last success, one supervisor's state, one TemplateInstance's
readiness — with correct series lifecycle: **a series exists exactly while its
object does.**

```go
import "github.com/stakater/operator-utils/observability/pkg/objstate"
```

## Why sync instruments can't do this

A sync gauge keeps exporting its last value for every attribute set ever
recorded. Delete the CostJob and its frozen `last_success` gauge keeps a
staleness alert firing forever. On raw Prometheus vectors this forces
`DeletePartialMatch` choreography at every reconcile exit and every deletion
path — machinery each operator reinvents, with the same bugs (a state
republish that resets a counter, a stale enum value that lingers).

`objstate` is that machinery once, done the OTel-native way: **observable**
instruments served by one callback that reports only the objects currently in
the `Set`. Absence is automatic. There is nothing to delete by hand.

## Model

A `Set` holds many objects of one kind, identified by a fixed tuple of
attribute keys:

```go
set, err := objstate.NewSet(pub.Meter(), "costjob", "namespace")
```

Instruments are registered once, on the Set:

| Instrument | Semantics |
|---|---|
| `Value(name, desc)` | plain per-object gauge |
| `Timestamp(name, desc)` | unix seconds, **absent until first set** — the lazy semantics freshness alerts rely on (a job that never succeeded has no last-success series to age) |
| `Enum(name, desc, key, values...)` | **one-hot** over a closed set; every object emits every value — exactly one `1` once set, **all zeros before**, so "tracked but never ran" stays expressible |
| `Condition(name, desc, reasonKey)` | `0`/`1` with the current reason as an attribute — exactly one reason series per object |
| `Counter(name, desc)` | per-object monotonic counter (reconcile storms); **survives `Put`**, dies with the object |

Object lifecycle:

```go
// Replaces the object's whole state: anything not set inside fn is gone after,
// so a stale enum value or condition reason CANNOT linger. Counters survive.
set.Put(objstate.ID{"daily", "finops"},
    []attribute.KeyValue{attribute.String("type", "ResourceCostCollection")},
    func(st *objstate.State) {
        st.Enum(status, "Failed")
        st.Timestamp(lastSuccess, t)      // only when there was one
        st.Condition(ready, false, "OpenCostUnreachable")
        st.Value(renderedResources, 7)
    })

set.Inc(reconciles, objstate.ID{"daily", "finops"}) // outside Put; may precede it

set.Delete(objstate.ID{"daily", "finops"}) // every series vanishes next scrape
```

Names are validated with the module's `naming` rules (reserved prefixes
rejected); duplicate names on one Set are rejected across instrument kinds.
All methods are safe for concurrent use; the callback serves every instrument
from one consistent snapshot.

## Worked example: a CostJob freshness pipeline

```go
set, _ := objstate.NewSet(pub.Meter(), "costjob", "namespace")
lastSuccess, _ := set.Timestamp(
    "finops_costjob_last_successful_execution_timestamp_seconds",
    "Unix time of the last successful collection; absent until one succeeds.")
status, _ := set.Enum(
    "finops_costjob_last_execution_status",
    "One-hot over the execution status enum; all-zero = never executed.",
    "status", "Success", "Failed", "Error", "Pending")

// In Reconcile, after reading the CR status:
set.Put(objstate.ID{cj.Name, cj.Namespace},
    []attribute.KeyValue{attribute.String("type", string(cj.Spec.Type))},
    func(st *objstate.State) {
        if t := cj.Status.LastSuccessfulExecutionTime; t != nil {
            st.Timestamp(lastSuccess, t.Time)
        }
        if s := cj.Status.LastExecutionStatus; s != "" {
            st.Enum(status, s)
        }
    })

// In the NotFound branch:
set.Delete(objstate.ID{req.Name, req.Namespace})
```

Alert on it with `time() - ..._timestamp_seconds > <threshold>` and
`max by (...) (status) >= 0 unless on (...) ..._timestamp_seconds` — the
all-zero presence and the lazy timestamp make both expressions sound.

## Reconcile storms

```go
reconciles, _ := set.Counter("template_operator_instance_reconciles_total",
    "Reconciles of this instance - topk(rate(...)) names the stormer.")
// first statement after a successful Get, before any branch:
set.Inc(reconciles, objstate.ID{obj.Name, obj.Namespace})
```

controller-runtime's totals are per controller; one hot object hides in the
aggregate. A per-object counter that `Put` cannot reset and deletion removes
is the storm signal done right.
