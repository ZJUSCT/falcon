package controller

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

// Both kubectl and the WebUI submit an annotation. Persist cancellation before
// deleting anything, and consume inapplicable requests before admitting new work.
func (r *MirrorReconciler) reconcileAbortRequest(ctx context.Context, m *mirrorv1alpha1.Mirror) (bool, error) {
	if !m.AbortRequested() {
		return r.discardRequest(ctx, m, mirrorv1alpha1.AbortRequestAnnotation, "abort-request only accepts the string true")
	}
	current := m.Status.CurrentSync
	if current != nil && current.Phase == mirrorv1alpha1.SyncPhaseCancelling {
		return false, nil
	}
	if current == nil || (current.Phase != mirrorv1alpha1.SyncPhasePending && current.Phase != mirrorv1alpha1.SyncPhaseRunning) {
		return r.discardRequest(ctx, m, mirrorv1alpha1.AbortRequestAnnotation, "no cancellable synchronization is running")
	}
	job := &batchv1.Job{}
	err := r.liveReader().Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: currentSyncJobName(m)}, job)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if err == nil {
		// Criteria conditions can precede terminal conditions while Pods drain.
		for _, condition := range job.Status.Conditions {
			if condition.Status == corev1.ConditionTrue && (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed || condition.Type == batchv1.JobSuccessCriteriaMet || condition.Type == batchv1.JobFailureTarget) {
				return r.discardRequest(ctx, m, mirrorv1alpha1.AbortRequestAnnotation, "the synchronization Job has already finished")
			}
		}
	}
	_, patchErr := r.patchStatus(ctx, m, func() {
		if err == nil {
			current.StartedAt = job.Status.StartTime.DeepCopy()
		}
		current.Phase = mirrorv1alpha1.SyncPhaseCancelling
	})
	return true, patchErr
}
