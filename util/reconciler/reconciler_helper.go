package reconciler

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	// RunningReason - Condition is running
	RunningReason = "Running"
	// SuccessfulReason - Condition is running due to reconcile being successful
	SuccessfulReason = "Successful"
	// FailedReason - Condition is failed due to ansible failure
	FailedReason = "Failed"
	// UnknownFailedReason - Condition is unknown
	UnknownFailedReason = "Unknown"
)

const (
	// RunningMessage - message for running reason.
	RunningMessage = "Running reconciliation"
	// SuccessfulMessage - message for successful reason.
	SuccessfulMessage = "Awaiting next reconciliation"
)

// ConditionsStatusAware represents a CRD type that has been enabled with ReconcileStatus
type ConditionsStatusAware interface {
	GetReconcileStatus() []metav1.Condition
	SetReconcileStatus(conditions []metav1.Condition)
}

//Resource represents a kubernetes Resource
type Resource interface {
	metav1.Object
	runtime.Object
}
