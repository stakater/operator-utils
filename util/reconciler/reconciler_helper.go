package reconciler

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type ConditionsStatusAware interface {
	GetReconcileStatus() []metav1.Condition
	SetReconcileStatus(conditions []metav1.Condition)
}

//Resource represents a kubernetes Resource
type Resource interface {
	metav1.Object
	runtime.Object
}
