package reconciler

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
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

var log = logf.Log.WithName("operator-utils").WithName("reconciler")

//ManageError will set status of the passed CR to a error condition
func ManageError(client client.Client, obj Resource, issue error, isRetriable bool) (ctrl.Result, error) {
	if reconcileStatusAware, updateStatus := (obj).(ConditionsStatusAware); updateStatus {
		condition := metav1.Condition{
			Type:               "ReconcileError",
			LastTransitionTime: metav1.Now(),
			Message:            issue.Error(),
			Reason:             FailedReason,
			Status:             "True",
		}
		conditions := []metav1.Condition{condition}
		reconcileStatusAware.SetReconcileStatus(conditions)
		err := client.Status().Update(context.Background(), obj)
		if err != nil {
			log.Error(err, "unable to update status")
			return ctrl.Result{}, err
		}
	} else {
		log.Info("object is not ReconcileStatusAware, not setting status")
	}

	if isRetriable {
		return ctrl.Result{}, issue
	}

	// Log error in case it's not Retriable error
	if issue != nil {
		log.Error(issue, "reconciliation error")
	}

	return ctrl.Result{}, nil
}

// ManageSuccess will update the status of the CR and return a successful reconcile result
func ManageSuccess(client client.Client, obj Resource) (ctrl.Result, error) {
	if reconcileStatusAware, updateStatus := (obj).(ConditionsStatusAware); updateStatus {
		condition := metav1.Condition{
			Type:               "ReconcileSuccess",
			LastTransitionTime: metav1.Now(),
			Message:            SuccessfulMessage,
			Reason:             SuccessfulReason,
			Status:             "True",
		}
		conditions := []metav1.Condition{condition}
		reconcileStatusAware.SetReconcileStatus(conditions)
		err := client.Status().Update(context.Background(), obj)
		if err != nil {
			log.Error(err, "unable to update status")
			return ctrl.Result{}, err
		}
	} else {
		log.Info("object is not ReconcileStatusAware, not setting status")
	}
	return ctrl.Result{}, nil
}

// DoNotRequeue won't requeue a CR for reconciliation
func DoNotRequeue() (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

// RequeueWithError will requeue the CR for reconciliation with an error
func RequeueWithError(err error) (ctrl.Result, error) {
	return ctrl.Result{}, err
}

// RequeueAfter will requeue the CR to be reconciled after a time duration
func RequeueAfter(requeueTime time.Duration) (ctrl.Result, error) {
	return ctrl.Result{Requeue: true, RequeueAfter: requeueTime}, nil
}
