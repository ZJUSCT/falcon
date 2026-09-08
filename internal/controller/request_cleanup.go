package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

// Called inside the terminal status patch. Completion and its cleanup receipt
// are durable together, so retained true annotations cannot start another run.
func queueRequestCleanup(m *mirrorv1alpha1.Mirror, syncRequest, abortRequest bool) {
	syncRequest = syncRequest && m.SyncRequested()
	abortRequest = abortRequest && m.AbortRequested()
	if syncRequest || abortRequest {
		m.Status.RequestCleanup = &mirrorv1alpha1.MirrorRequestCleanup{Token: m.ResourceVersion, Sync: syncRequest, Abort: abortRequest}
	}
}

// Metadata and status use separate API writes. Atomically removing requests and
// acknowledging the receipt in metadata closes the crash window between those
// writes: a newly submitted true must survive a restart after removal succeeded.
func (r *MirrorReconciler) reconcileRequestCleanup(ctx context.Context, m *mirrorv1alpha1.Mirror) (ctrl.Result, error) {
	receipt := m.Status.RequestCleanup
	if m.Annotations[mirrorv1alpha1.RequestCleanupAnnotation] != receipt.Token {
		before := m.DeepCopy()
		if m.Annotations == nil {
			m.Annotations = map[string]string{}
		}
		if receipt.Sync && m.SyncRequested() {
			delete(m.Annotations, mirrorv1alpha1.SyncRequestAnnotation)
		}
		if receipt.Abort && m.AbortRequested() {
			delete(m.Annotations, mirrorv1alpha1.AbortRequestAnnotation)
		}
		m.Annotations[mirrorv1alpha1.RequestCleanupAnnotation] = receipt.Token
		if err := r.Patch(ctx, m, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	return r.patchStatusWithResult(ctx, m, ctrl.Result{RequeueAfter: time.Second}, func() { m.Status.RequestCleanup = nil })
}

func (r *MirrorReconciler) discardRequest(ctx context.Context, m *mirrorv1alpha1.Mirror, key, reason string) (bool, error) {
	if _, exists := m.Annotations[key]; !exists {
		return false, nil
	}
	before := m.DeepCopy()
	delete(m.Annotations, key)
	if err := r.Patch(ctx, m, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, err
	}
	if r.Recorder != nil {
		r.Recorder.Event(m, corev1.EventTypeNormal, "RequestIgnored", reason)
	}
	return true, nil
}

func (r *MirrorReconciler) discardSyncRequest(ctx context.Context, m *mirrorv1alpha1.Mirror) (bool, error) {
	if value, exists := m.Annotations[mirrorv1alpha1.SyncRequestAnnotation]; exists && value != "true" {
		return r.discardRequest(ctx, m, mirrorv1alpha1.SyncRequestAnnotation, "sync-request only accepts the string true")
	}
	if m.SyncRequested() && m.Status.CurrentSync != nil && !m.Status.CurrentSync.Manual {
		return r.discardRequest(ctx, m, mirrorv1alpha1.SyncRequestAnnotation, "a synchronization is already in progress")
	}
	return false, nil
}
