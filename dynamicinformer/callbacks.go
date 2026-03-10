package dynamicinformer

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// MapFunc maps an event object to zero or more reconcile requests.
// This follows the same pattern as controller-runtime's handler.MapFunc.
type MapFunc func(obj *unstructured.Unstructured) []reconcile.Request

// EventCallbackFromMapFunc returns an EventCallback that applies mapFunc to each
// event object and enqueues the resulting reconcile requests as GenericEvents.
//
// Uses non-blocking sends; dropped events are recovered by the informer's resync.
func EventCallbackFromMapFunc(
	mapFunc MapFunc,
	eventCh chan<- event.GenericEvent,
) EventCallback {
	enqueue := func(obj *unstructured.Unstructured) {
		for _, req := range mapFunc(obj) {
			select {
			case eventCh <- event.GenericEvent{
				Object: &metav1.PartialObjectMetadata{
					ObjectMeta: metav1.ObjectMeta{
						Name:      req.Name,
						Namespace: req.Namespace,
					},
				},
			}:
			default:
				// Channel full — resync will recover
			}
		}
	}
	return EventCallback{
		OnAdd:    enqueue,
		OnUpdate: func(_, newObj *unstructured.Unstructured) { enqueue(newObj) },
		OnDelete: enqueue,
	}
}

// LabelSelectorCallback returns an EventCallback that filters events by label selector
// and delegates to mapFunc to determine which reconcile requests to enqueue.
//
// Uses non-blocking sends; dropped events are recovered by the informer's resync.
func LabelSelectorCallback(
	selector labels.Selector,
	mapFunc MapFunc,
	eventCh chan<- event.GenericEvent,
) EventCallback {
	return EventCallbackFromMapFunc(func(obj *unstructured.Unstructured) []reconcile.Request {
		if !selector.Matches(labels.Set(obj.GetLabels())) {
			return nil
		}
		return mapFunc(obj)
	}, eventCh)
}

// EnqueueByOwnerAnnotationCallback returns an EventCallback that resolves the owning
// resource from annotations on the event object. This is useful for cross-namespace
// ownership where OwnerReferences cannot be used.
//
// ownerNameAnnotation and ownerNamespaceAnnotation are the annotation keys that identify
// the owner's name and namespace on child resources.
//
// Uses non-blocking sends; dropped events are recovered by the informer's resync.
func EnqueueByOwnerAnnotationCallback(
	ownerNameAnnotation string,
	ownerNamespaceAnnotation string,
	eventCh chan<- event.GenericEvent,
) EventCallback {
	return EventCallbackFromMapFunc(func(obj *unstructured.Unstructured) []reconcile.Request {
		annotations := obj.GetAnnotations()
		if annotations == nil {
			return nil
		}

		ownerName := annotations[ownerNameAnnotation]
		ownerNamespace := annotations[ownerNamespaceAnnotation]
		if ownerName == "" || ownerNamespace == "" {
			return nil
		}

		return []reconcile.Request{
			{NamespacedName: types.NamespacedName{Name: ownerName, Namespace: ownerNamespace}},
		}
	}, eventCh)
}
