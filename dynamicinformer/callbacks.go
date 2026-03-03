package dynamicinformer

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// EnqueueOwnersCallback returns an EventCallback that resolves the owner from the
// object's OwnerReferences at event time. Events are sent to eventCh as GenericEvents
// containing the owner's name and the object's namespace.
//
// Uses non-blocking sends; dropped events are recovered by the informer's resync.
func EnqueueOwnersCallback(
	ownerGVK schema.GroupVersionKind,
	eventCh chan<- event.GenericEvent,
) EventCallback {
	enqueue := func(obj *unstructured.Unstructured) {
		for _, ref := range obj.GetOwnerReferences() {
			refGV, err := schema.ParseGroupVersion(ref.APIVersion)
			if err != nil {
				continue
			}
			if refGV.Group == ownerGVK.Group && ref.Kind == ownerGVK.Kind {
				select {
				case eventCh <- event.GenericEvent{
					Object: &metav1.PartialObjectMetadata{
						ObjectMeta: metav1.ObjectMeta{
							Name:      ref.Name,
							Namespace: obj.GetNamespace(),
						},
					},
				}:
				default:
					// Channel full - resync will recover
				}
			}
		}
	}
	return EventCallback{
		OnAdd:    enqueue,
		OnUpdate: func(_, newObj *unstructured.Unstructured) { enqueue(newObj) },
		OnDelete: enqueue,
	}
}

// LabelSelectorCallback returns an EventCallback that enqueues events for objects
// matching the given label selector. Events contain the object's own name and namespace.
//
// Uses non-blocking sends; dropped events are recovered by the informer's resync.
func LabelSelectorCallback(
	selector labels.Selector,
	eventCh chan<- event.GenericEvent,
) EventCallback {
	enqueue := func(obj *unstructured.Unstructured) {
		if selector.Matches(labels.Set(obj.GetLabels())) {
			select {
			case eventCh <- event.GenericEvent{
				Object: &metav1.PartialObjectMetadata{
					ObjectMeta: metav1.ObjectMeta{
						Name:      obj.GetName(),
						Namespace: obj.GetNamespace(),
					},
				},
			}:
			default:
				// Channel full - resync will recover
			}
		}
	}
	return EventCallback{
		OnAdd:    enqueue,
		OnUpdate: func(_, newObj *unstructured.Unstructured) { enqueue(newObj) },
		OnDelete: enqueue,
	}
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
	enqueue := func(obj *unstructured.Unstructured) {
		annotations := obj.GetAnnotations()
		if annotations == nil {
			return
		}

		ownerName := annotations[ownerNameAnnotation]
		ownerNamespace := annotations[ownerNamespaceAnnotation]
		if ownerName == "" || ownerNamespace == "" {
			return
		}

		select {
		case eventCh <- event.GenericEvent{
			Object: &metav1.PartialObjectMetadata{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ownerName,
					Namespace: ownerNamespace,
				},
			},
		}:
		default:
			// Channel full - resync will recover
		}
	}
	return EventCallback{
		OnAdd:    enqueue,
		OnUpdate: func(_, newObj *unstructured.Unstructured) { enqueue(newObj) },
		OnDelete: enqueue,
	}
}
