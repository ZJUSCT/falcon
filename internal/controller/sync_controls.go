package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

const syncComponent = "sync"

var errSyncPaused = errors.New("automatic synchronization is paused")

func (r *MirrorReconciler) liveReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// Cancellation is durable before deletion. Foreground GC gives the kubelet time
// to stop writers; neither terminating Pods nor their concurrency slot are
// forcibly removed. An unreachable node may therefore delay cancellation.
func (r *MirrorReconciler) reconcileCancellation(ctx context.Context, mirror *mirrorv1alpha1.Mirror, publication publicationHealth) (ctrl.Result, error) {
	current := mirror.Status.CurrentSync
	jobName := currentSyncJobName(mirror)
	job := &batchv1.Job{}
	err := r.liveReader().Get(ctx, client.ObjectKey{Namespace: mirror.Namespace, Name: jobName}, job)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if err == nil {
		r.SyncLimiter.Acquire(jobName, true)
		if job.Status.StartTime != nil && current.StartedAt == nil {
			if _, err := r.patchStatus(ctx, mirror, func() { current.StartedAt = job.Status.StartTime.DeepCopy() }); err != nil {
				return ctrl.Result{}, err
			}
		}
		if job.DeletionTimestamp.IsZero() {
			policy := metav1.DeletePropagationForeground
			uid := job.UID
			if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy, Preconditions: &metav1.Preconditions{UID: &uid}}); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
		}
		return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: 2 * time.Second}, func() {
			applyMirrorConditions(mirror, publication, "SyncCancelling", "waiting for synchronization workload to stop", nil)
		})
	}
	// Also cover orphaned Pods if the Job was independently deleted. A Pod is
	// safe only after Kubernetes reports terminal execution or removes it via GC.
	pods := &corev1.PodList{}
	if err := r.liveReader().List(ctx, pods, client.InNamespace(mirror.Namespace), client.MatchingLabels{MirrorLabel: childBase(mirror.Name), ComponentLabel: syncComponent, SyncTimestampLabel: fmt.Sprint(currentSyncTimestamp(mirror))}); err != nil {
		return ctrl.Result{}, err
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			r.SyncLimiter.Acquire(jobName, true)
			return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: 2 * time.Second}, func() {
				applyMirrorConditions(mirror, publication, "SyncCancelling", "waiting for synchronization workload to stop", nil)
			})
		}
	}
	now := r.now()
	_, err = r.patchStatusWithResult(ctx, mirror, ctrl.Result{}, func() {
		message := "synchronization cancelled by operator"
		mirror.Status.LastAttempt = &mirrorv1alpha1.MirrorSyncStatus{JobName: jobName, Phase: mirrorv1alpha1.SyncPhaseCancelled, StartedAt: current.QueuedAt.DeepCopy(), FinishedAt: timePtr(now), Message: message}
		if current.StartedAt != nil {
			mirror.Status.LastSync = &mirrorv1alpha1.MirrorSyncStatus{JobName: jobName, Phase: mirrorv1alpha1.SyncPhaseCancelled, StartedAt: current.StartedAt.DeepCopy(), FinishedAt: timePtr(now), Message: message}
		}
		queueRequestCleanup(mirror, current.Manual, true)
		mirror.Status.CurrentSync = nil
		mirror.Status.NextSyncAt = timePtr(now.Add(mirror.Spec.Sync.Interval.Duration))
		if mirror.Spec.Sync.Paused && (current.StartedAt != nil || mirror.Status.PausedAt == nil) {
			mirror.Status.PausedAt = timePtr(now)
		}
		mirror.Status.ObservedGeneration = mirror.Generation
		applyMirrorConditions(mirror, publication, "SyncCancelled", message, nil)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	r.SyncLimiter.Release(jobName)
	if mirror.Status.RequestCleanup != nil {
		return r.reconcileRequestCleanup(ctx, mirror)
	}
	return ctrl.Result{RequeueAfter: mirror.Spec.Sync.Interval.Duration}, nil
}

// Rebuild occupied slots from live Jobs before admitting work, including Jobs
// being cancelled. This closes the restart window before their Mirrors have
// individually reconciled.
func (r *MirrorReconciler) restoreSyncSlots(ctx context.Context, namespace string) error {
	snapshot := r.SyncLimiter.observedSnapshot()
	occupied := make(map[string]struct{})
	jobs := &batchv1.JobList{}
	if err := r.liveReader().List(ctx, jobs, client.InNamespace(namespace), client.MatchingLabels{ComponentLabel: syncComponent}); err != nil {
		return err
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if !jobSucceeded(job) && !jobFailed(job) || !job.DeletionTimestamp.IsZero() {
			occupied[job.Name] = struct{}{}
		}
	}
	pods := &corev1.PodList{}
	if err := r.liveReader().List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{ComponentLabel: syncComponent}); err != nil {
		return err
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		base, timestamp := pod.Labels[MirrorLabel], pod.Labels[SyncTimestampLabel]
		if base != "" && timestamp != "" {
			occupied[resourceName(base, "sync-"+timestamp)] = struct{}{}
		}
	}
	r.SyncLimiter.reconcileObserved(snapshot, occupied)
	return nil
}

// Admission uses the live Mirror version, not a potentially lagging watch
// cache. An edit after this check can still race Job creation; the following
// optimistic status patch stops publication and the serialized cancellation
// reconcile drains the newly-created Job before clearing the transaction.
func (r *MirrorReconciler) checkSyncAdmission(ctx context.Context, mirror *mirrorv1alpha1.Mirror) error {
	latest := &mirrorv1alpha1.Mirror{}
	if err := r.liveReader().Get(ctx, client.ObjectKeyFromObject(mirror), latest); err != nil {
		return err
	}
	if latest.ResourceVersion != mirror.ResourceVersion {
		return apierrors.NewConflict(mirrorv1alpha1.GroupVersion.WithResource("mirrors").GroupResource(), mirror.Name, errors.New("mirror changed before synchronization admission"))
	}
	if latest.AbortRequested() || latest.Status.Publication != nil || latest.Status.CurrentSync == nil || latest.Status.CurrentSync.Phase == mirrorv1alpha1.SyncPhaseCancelling || !latest.DeletionTimestamp.IsZero() {
		return apierrors.NewConflict(mirrorv1alpha1.GroupVersion.WithResource("mirrors").GroupResource(), mirror.Name, errors.New("synchronization attempt is no longer eligible for admission"))
	}
	if latest.Spec.Sync.Paused && !latest.Status.CurrentSync.Manual {
		return errSyncPaused
	}
	return nil
}

// syncSpecHash keeps configuration-triggered synchronization independent from
// publication, metadata, storage and automatic/manual mode changes.
func syncSpecHash(mirror *mirrorv1alpha1.Mirror) (string, error) {
	spec := mirror.Spec.Sync
	spec.Paused = false
	encoded, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(encoded)), nil
}
