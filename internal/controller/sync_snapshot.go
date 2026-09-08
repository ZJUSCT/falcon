package controller

import (
	"context"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

// A successful Job is never rerun to recover a snapshot failure. The work PVC
// remains protected by CurrentSync until storage delivers a ready snapshot.
func (r *MirrorReconciler) reconcileSyncSnapshot(ctx context.Context, m *mirrorv1alpha1.Mirror, health publicationHealth) (ctrl.Result, error) {
	ready, message, err := r.ensureSnapshot(ctx, m)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready || message != "" {
		reason := "Snapshotting"
		var failure *conditionFailure
		if message != "" {
			reason = "SnapshotFailed"
			failure = &conditionFailure{reason: reason, message: message}
		} else {
			message = "waiting for synchronization snapshot readyToUse"
		}
		return r.patchStatusWithResult(ctx, m, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
			applyMirrorConditions(m, health, reason, message, failure)
		})
	}
	result, err := r.patchStatusWithResult(ctx, m, ctrl.Result{RequeueAfter: time.Second}, func() {
		current := m.Status.CurrentSync
		m.Status.LastSnapshot = &mirrorv1alpha1.MirrorSnapshotStatus{
			Name:     currentSyncSnapshotName(m),
			QueuedAt: current.QueuedAt.DeepCopy(), JobName: currentSyncJobName(m),
		}
		m.Status.LastAttempt = &mirrorv1alpha1.MirrorSyncStatus{
			JobName: currentSyncJobName(m), Phase: mirrorv1alpha1.SyncPhaseSucceeded,
			StartedAt: current.QueuedAt.DeepCopy(), FinishedAt: timePtr(r.now()),
		}
		if publishEnabled(m) {
			m.Status.Publication = publicationFromSnapshot(m.Status.LastSnapshot)
			health.progressing = true
			health.reason, health.message = publicationRestoring, "ready synchronization snapshot delivered to publication"
		}
		queueRequestCleanup(m, current.Manual, false)
		m.Status.CurrentSync = nil
		applyMirrorConditions(m, health, "SynchronizationCompleted", "synchronization delivered a ready snapshot", nil)
	})
	if err == nil && m.Status.RequestCleanup != nil {
		return r.reconcileRequestCleanup(ctx, m)
	}
	return result, err
}

func publicationFromSnapshot(snapshot *mirrorv1alpha1.MirrorSnapshotStatus) *mirrorv1alpha1.MirrorPublicationStatus {
	return &mirrorv1alpha1.MirrorPublicationStatus{
		QueuedAt: snapshot.QueuedAt.DeepCopy(), JobName: snapshot.JobName,
		Snapshot: snapshot.Name, Phase: publicationRestoring,
	}
}
