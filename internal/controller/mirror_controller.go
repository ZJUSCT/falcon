package controller

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
	"github.com/ZJUSCT/falcon/internal/config"
)

const (
	MirrorFinalizer       = "mirrors.zjusct.io/storage-cleanup"
	SyncRequestAnnotation = mirrorv1alpha1.SyncRequestAnnotation
	MirrorLabel           = "mirrors.zjusct.io/mirror"
	// SyncTimestampLabel carries the Unix seconds timestamp of a sync task
	// (allocated once when the controller creates the task) on every
	// snapshot-scoped child: the sync Job, the VolumeSnapshot and the publish
	// PVC.
	SyncTimestampLabel = "mirrors.zjusct.io/sync-timestamp"
	// ComponentLabel is the standard recommended component label (values:
	// sync/snapshot/publish-data/publish-http/publish-rsync/proxy-cache); it
	// replaced the custom mirrors.zjusct.io/role label. Publish children use
	// the per-service-key value (publish-<key>) so each Service selects only
	// its own pods.
	ComponentLabel = "app.kubernetes.io/component"

	conditionReady       = "Ready"
	conditionProgressing = "Progressing"
	conditionDegraded    = "Degraded"
)

type MirrorReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	Now       func() time.Time
	// Config is the loaded controller configuration (required). The publish
	// section (config publish.*) gates publish HTTPRoute generation, the sync
	// section the global concurrency cap.
	Config *config.Config
	// SyncLimiter enforces the global cap of concurrently running sync Jobs
	// (config sync.maxConcurrent). Required.
	SyncLimiter *SyncLimiter
	// UsageReader optionally reports the on-disk usage of a publish PVC as
	// seen by the kubelet running its publish pod; it backs
	// status.sizeBytes (best-effort accounting, see publishPVCUsage). When
	// nil, size accounting is skipped entirely.
	UsageReader PVCUsageReader
}

// RBAC (rendered into the namespaced Role in config/rbac; the controller only
// ever touches its own namespace):
//
// +kubebuilder:rbac:groups=mirrors.zjusct.io,resources=mirrors,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=mirrors.zjusct.io,resources=mirrors/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=mirrors.zjusct.io,resources=mirrors/finalizers,verbs=get;patch;update
// +kubebuilder:rbac:groups=mirrors.zjusct.io,resources=proxymirrors,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=mirrors.zjusct.io,resources=proxymirrors/status,verbs=get;patch;update
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;patch;update;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;patch;update;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch;update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch;update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;patch;update;delete
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;patch;update;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch;create;patch;update;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;patch;update;delete
//
// nodes/proxy is cluster-scoped and cannot live in the namespaced Role: the
// chart grants it through the controller.rbac.nodeStats ClusterRole
// (kubelet stats summary via the API server node proxy — the source of
// status.sizeBytes; the apiserver only registers a nodes/proxy subresource,
// there is no nodes/stats route).
// +kubebuilder:rbac:groups="",resources=nodes/proxy,verbs=get

func (r *MirrorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.APIReader = mgr.GetAPIReader()
	return ctrl.NewControllerManagedBy(mgr).
		For(&mirrorv1alpha1.Mirror{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&snapshotv1.VolumeSnapshot{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Service{}).
		Owns(&appsv1.Deployment{}).
		Owns(&gatewayv1.HTTPRoute{}).
		Named("mirror").
		Complete(r)
}

func (r *MirrorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reconcileErr error) {
	logger := log.FromContext(ctx)
	mirror := &mirrorv1alpha1.Mirror{}
	if err := r.Get(ctx, req.NamespacedName, mirror); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !mirror.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, mirror)
	}
	if !controllerutil.ContainsFinalizer(mirror, MirrorFinalizer) {
		before := mirror.DeepCopy()
		controllerutil.AddFinalizer(mirror, MirrorFinalizer)
		if err := r.Patch(ctx, mirror, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	defer func() {
		result, reconcileErr = r.handleDerivedResourceInvalid(ctx, mirror, result, reconcileErr)
	}()

	// Removing a service is an operational request, not merely a validation
	// concern. Honour it even when another part of the new spec is invalid or cancellation is
	// blocked, so neither can accidentally keep an endpoint online.
	if err := r.cleanupDisabledPublishChildren(ctx, mirror); err != nil {
		return ctrl.Result{}, err
	}

	if mirror.Status.RequestCleanup != nil {
		return r.reconcileRequestCleanup(ctx, mirror)
	}
	if handled, err := r.reconcileAbortRequest(ctx, mirror); err != nil || handled {
		return ctrl.Result{RequeueAfter: time.Second}, err
	}
	if changed, err := r.discardSyncRequest(ctx, mirror); err != nil || changed {
		return ctrl.Result{RequeueAfter: time.Second}, err
	}
	if mirror.Status.CurrentSync != nil && mirror.Status.CurrentSync.Phase == mirrorv1alpha1.SyncPhaseCancelling {
		return r.reconcileCancellation(ctx, mirror, publicationHealth{ready: publishHTTPEnabled(mirror) && r.Config.PublishEnabled() && mirrorWasReady(mirror), reason: "Cancellation", message: "preserving existing publication"})
	}

	if errs := validateMirror(mirror); len(errs) > 0 {
		message := errs.ToAggregate().Error()
		logger.Info("Mirror specification is invalid", "errors", message)
		return r.patchStatus(ctx, mirror, func() {
			mirror.Status.ObservedGeneration = mirror.Generation
			setCondition(mirror, conditionReady, conditionStatus(publishHTTPEnabled(mirror) && r.Config.PublishEnabled() && mirrorWasReady(mirror)), "InvalidSpec", message)
			setCondition(mirror, conditionProgressing, conditionStatus(mirror.Status.Publication != nil), "InvalidSpec", message)
			setCondition(mirror, conditionDegraded, metav1.ConditionTrue, "InvalidSpec", message)
		})
	}

	publication, err := r.reconcileActivePublication(ctx, mirror)
	if err != nil {
		return ctrl.Result{}, err
	}
	acceptedSpecHash, err := syncSpecHash(mirror)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Existing objects can establish their baseline only when the controller
	// already observed this exact generation. Never acknowledge an unseen spec.
	if mirror.Status.LastAcceptedSpecHash == "" && mirror.Status.ObservedGeneration == mirror.Generation {
		if _, err := r.patchStatus(ctx, mirror, func() { mirror.Status.LastAcceptedSpecHash = acceptedSpecHash }); err != nil {
			return ctrl.Result{}, err
		}
	}
	if !mirror.Spec.Sync.Paused && mirror.Status.PausedAt != nil {
		if _, err := r.patchStatus(ctx, mirror, func() { mirror.Status.PausedAt = nil }); err != nil {
			return ctrl.Result{}, err
		}
	}
	if mirror.Spec.Sync.Paused && mirror.Status.CurrentSync == nil && mirror.Status.PausedAt == nil {
		if _, err := r.patchStatus(ctx, mirror, func() { mirror.Status.PausedAt = timePtr(r.now()) }); err != nil {
			return ctrl.Result{}, err
		}
	}
	if mirror.Status.Publication != nil {
		return r.reconcilePublication(ctx, mirror, publication)
	}
	// Enabling publication consumes the retained snapshot before admitting any
	// new synchronization, including while the schedule is paused.
	if mirror.Status.CurrentSync == nil && publishEnabled(mirror) && mirror.Status.LastSnapshot != nil && mirror.Status.LastSnapshot.Name != mirror.Status.ActiveSnapshot {
		return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: time.Second}, func() {
			mirror.Status.Publication = publicationFromSnapshot(mirror.Status.LastSnapshot)
			publication.progressing = true
			publication.reason, publication.message = publicationRestoring, "publishing the latest ready snapshot"
			applyMirrorConditions(mirror, publication, "", "", nil)
		})
	}
	if (publication.progressing || publication.failure != nil) && (mirror.Status.CurrentSync == nil || mirror.Status.CurrentSync.StartedAt == nil) {
		usage, ok := int64(0), false
		if mirror.Status.ActivePVC != "" && mirror.Status.SizeBytes == 0 {
			usage, ok = r.publishPVCUsage(ctx, mirror, mirror.Status.ActivePVC)
		}
		return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
			if ok {
				mirror.Status.SizeBytes = usage
			}
			applyMirrorConditions(mirror, publication, "PublicationPending", "waiting for publication and old Pods to finish", nil)
		})
	}
	if mirror.Status.CurrentSync != nil {
		return r.reconcileSync(ctx, mirror, publication)
	}

	if err := r.pruneOldSnapshots(ctx, mirror); err != nil {
		return ctrl.Result{}, err
	}
	manualDue := mirror.SyncRequested()
	if mirror.Spec.Sync.Paused && !manualDue {
		return r.patchStatus(ctx, mirror, func() {
			mirror.Status.ObservedGeneration = mirror.Generation
			if mirror.Status.PausedAt == nil {
				mirror.Status.PausedAt = timePtr(r.now())
			}
			applyMirrorConditions(mirror, publication, "Paused", "automatic synchronization is paused", nil)
		})
	}

	now := r.now()
	specDue := mirror.Status.LastAcceptedSpecHash != "" && mirror.Status.LastAcceptedSpecHash != acceptedSpecHash
	bootstrapDue := mirror.Status.ActivePVC == "" && mirror.Status.LastAttempt == nil
	scheduleDue := mirror.Status.NextSyncAt != nil && !mirror.Status.NextSyncAt.After(now)

	if manualDue || specDue || bootstrapDue || scheduleDue {
		return r.startSync(ctx, mirror, manualDue, publication)
	}

	// Publish PVC content is immutable, so the kubelet-reported usage recorded
	// once stays accurate forever: backfill only while sizeBytes is still
	// unset, best-effort as everywhere else.
	var pvcUsage int64
	pvcUsageOK := false
	if mirror.Status.ActivePVC != "" && mirror.Status.SizeBytes == 0 {
		pvcUsage, pvcUsageOK = r.publishPVCUsage(ctx, mirror, mirror.Status.ActivePVC)
	}

	nextResult := ctrl.Result{}
	if mirror.Status.NextSyncAt != nil {
		nextResult.RequeueAfter = time.Until(mirror.Status.NextSyncAt.Time)
		if nextResult.RequeueAfter < time.Second {
			nextResult.RequeueAfter = time.Second
		}
	}
	return r.patchStatusWithResult(ctx, mirror, nextResult, func() {
		mirror.Status.ObservedGeneration = mirror.Generation
		if pvcUsageOK {
			mirror.Status.SizeBytes = pvcUsage
		}
		applyMirrorConditions(mirror, publication, "Idle", "no synchronization is running", nil)
	})
}

// startSync begins a new synchronization run. The Unix seconds timestamp is
// allocated ONCE here, when the controller creates the sync task, and
// propagates from status.currentSync.queuedAt into every derived name: the
// sync Job `<base>-sync-<ts>`, the VolumeSnapshot and the publish PVC (which
// share the name `<base>-snap-<ts>`). The sync PVC has the fixed name
// `<base>-sync` (no timestamp) and is reused across runs. Whether the
// timestamp is free (no existing Job/PVC/VolumeSnapshot carrying it) is
// checked when the sync Job is created — see lookupOrCreateSyncJob.
func (r *MirrorReconciler) startSync(ctx context.Context, mirror *mirrorv1alpha1.Mirror, manual bool, publication publicationHealth) (ctrl.Result, error) {
	acceptedSpecHash, err := syncSpecHash(mirror)
	if err != nil {
		return ctrl.Result{}, err
	}
	now := r.now()
	// One accepted generation per Mirror per second. A follow-up after a
	// completed/cancelled generation waits for the next actual second; never
	// allocate a future timestamp or reuse the completed generation's objects.
	if mirror.Status.CurrentSync != nil || mirror.Status.Publication != nil {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if previous := mirror.Status.LastAcceptedSyncAt; previous != nil && now.Unix() <= previous.Unix() {
		return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: time.Unix(previous.Unix()+1, 0).Sub(now)}, func() {
			applyMirrorConditions(mirror, publication, "SyncSecondPending", "waiting for the next generation second", nil)
		})
	}
	timestamp := now.Unix()
	base := childBase(mirror.Name)
	syncPVCName := mirror.Status.WorkPVC
	if syncPVCName == "" {
		syncPVCName = resourceName(base, "sync")
	}
	jobName := resourceName(base, fmt.Sprintf("sync-%d", timestamp))

	if r.Recorder != nil {
		r.Recorder.Eventf(mirror, corev1.EventTypeNormal, "SynchronizationStarted", "Starting synchronization run with Job %s", jobName)
	}
	return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: time.Second}, func() {
		mirror.Status.ObservedGeneration = mirror.Generation
		mirror.Status.LastAcceptedSpecHash = acceptedSpecHash
		mirror.Status.LastAcceptedSyncAt = timePtr(now.Truncate(time.Second))
		mirror.Status.WorkPVC = syncPVCName
		mirror.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{
			QueuedAt: timePtr(now.Truncate(time.Second)),
			Phase:    mirrorv1alpha1.SyncPhasePending,
			Manual:   manual,
		}
		mirror.Status.PausedAt = nil
		mirror.Status.NextSyncAt = nil
		applyMirrorConditions(mirror, publication, "SynchronizationStarted", "preparing synchronization run", nil)
	})
}

func currentSyncTimestamp(mirror *mirrorv1alpha1.Mirror) int64 {
	if mirror.Status.CurrentSync == nil || mirror.Status.CurrentSync.QueuedAt == nil {
		return 0
	}
	return mirror.Status.CurrentSync.QueuedAt.Unix()
}

func currentSyncJobName(mirror *mirrorv1alpha1.Mirror) string {
	return resourceName(childBase(mirror.Name), fmt.Sprintf("sync-%d", currentSyncTimestamp(mirror)))
}

func currentSyncSnapshotName(mirror *mirrorv1alpha1.Mirror) string {
	return resourceName(childBase(mirror.Name), fmt.Sprintf("snap-%d", currentSyncTimestamp(mirror)))
}

func (r *MirrorReconciler) reconcileSync(ctx context.Context, mirror *mirrorv1alpha1.Mirror, publication publicationHealth) (ctrl.Result, error) {
	if mirror.Status.CurrentSync.Phase == mirrorv1alpha1.SyncPhaseSnapshotting {
		return r.reconcileSyncSnapshot(ctx, mirror, publication)
	}
	if mirror.Status.CurrentSync.Phase == mirrorv1alpha1.SyncPhaseCancelling {
		return r.reconcileCancellation(ctx, mirror, publication)
	}
	if err := r.ensureSyncPVC(ctx, mirror); err != nil {
		return ctrl.Result{}, err
	}
	job, err := r.lookupOrCreateSyncJob(ctx, mirror)
	if errors.Is(err, errSyncPaused) {
		return r.patchStatus(ctx, mirror, func() {
			if mirror.Status.PausedAt == nil {
				mirror.Status.PausedAt = timePtr(r.now())
			}
			applyMirrorConditions(mirror, publication, "SchedulePaused", "queued automatic synchronization is paused", nil)
		})
	}
	if errors.Is(err, errSyncQueued) {
		// The global sync concurrency cap (sync.maxConcurrent) is reached:
		// leave the Job uncreated and retry shortly. The queued sync may
		// start later than status.nextSyncAt.
		return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
			applyMirrorConditions(mirror, publication, "SyncQueued",
				fmt.Sprintf("sync Job %s is queued: global sync concurrency limit (%d) reached", currentSyncJobName(mirror), r.Config.Sync.MaxConcurrent), nil)
		})
	}
	if errors.Is(err, errSnapshotTimestampConflict) {
		// The timestamp allocated at sync task creation is already taken by an
		// existing Job/PVC/VolumeSnapshot: stop this reconcile without clearing
		// the pending pipeline. Degraded + Warning event tell the operator to
		// check for the leftover same-second object and remove it if safe;
		// retrying after a minute avoids a requeue storm.
		if r.Recorder != nil {
			r.Recorder.Eventf(mirror, corev1.EventTypeWarning, "SnapshotTimestampConflict",
				"Synchronization run cannot start: %s. Check for the leftover same-second Job/PVC/VolumeSnapshot and remove it if safe.", err.Error())
		}
		return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: time.Minute}, func() {
			message := err.Error()
			applyMirrorConditions(mirror, publication, "SnapshotTimestampConflict", message,
				&conditionFailure{reason: "SnapshotTimestampConflict", message: message})
		})
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	terminal := jobFailed(job) || jobSucceeded(job)
	if terminal {
		// The Job reached a terminal state: its concurrency slot is freed.
		// Release is idempotent, so later reconciles that still see the Job
		// while the publication completes cannot free someone else's slot.
		r.SyncLimiter.Release(job.Name)
	} else {
		// Non-terminal Job (just created here, or found running — e.g. after
		// a controller restart): make sure it counts against the cap.
		r.SyncLimiter.Acquire(job.Name, true)
	}

	if err := r.observeSyncJob(ctx, mirror, job); err != nil {
		return ctrl.Result{}, err
	}
	if terminal {
		if err := r.pruneJobs(ctx, mirror); err != nil {
			return ctrl.Result{}, err
		}
		if mirror.Status.CurrentSync != nil && mirror.Status.CurrentSync.Phase == mirrorv1alpha1.SyncPhaseSnapshotting {
			return r.reconcileSyncSnapshot(ctx, mirror, publication)
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if !jobSucceeded(job) {
		return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
			reason, message := "SyncJobRunning", fmt.Sprintf("Job %s is running", job.Name)
			if mirror.Status.CurrentSync.StartedAt == nil {
				reason, message = "SyncJobPending", fmt.Sprintf("waiting for Job %s to start", job.Name)
			}
			applyMirrorConditions(mirror, publication, reason, message, nil)
		})
	}

	return ctrl.Result{}, nil
}

// observeSyncJob persists a successful Job's completion before any snapshot or
// publication work. A terminal success without completionTime is inconsistent;
// it must never erase history or progress to publication.
func (r *MirrorReconciler) observeSyncJob(ctx context.Context, mirror *mirrorv1alpha1.Mirror, job *batchv1.Job) error {
	phase := mirrorv1alpha1.SyncPhasePending
	if job.Status.StartTime != nil {
		phase = mirrorv1alpha1.SyncPhaseRunning
	}
	var finished *metav1.Time
	message := ""
	switch {
	case jobFailed(job):
		phase = mirrorv1alpha1.SyncPhaseFailed
		message = jobFailureMessage(job)
		for _, condition := range job.Status.Conditions {
			if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue && !condition.LastTransitionTime.IsZero() {
				finished = condition.LastTransitionTime.DeepCopy()
				break
			}
		}
	case jobSucceeded(job):
		phase = mirrorv1alpha1.SyncPhaseSucceeded
		finished = job.Status.CompletionTime.DeepCopy()
		if finished == nil || finished.Unix() <= 0 {
			err := fmt.Errorf("sync Job %s/%s is successful but status.completionTime is missing or invalid", job.Namespace, job.Name)
			if r.Recorder != nil {
				r.Recorder.Event(mirror, corev1.EventTypeWarning, "SyncJobStatusInvalid", err.Error())
			}
			if _, patchErr := r.patchStatus(ctx, mirror, func() {
				setCondition(mirror, conditionDegraded, metav1.ConditionTrue, "SyncJobStatusInvalid", err.Error())
			}); patchErr != nil {
				return patchErr
			}
			return err
		}
	}
	_, err := r.patchStatus(ctx, mirror, func() {
		current := mirror.Status.CurrentSync
		current.StartedAt = job.Status.StartTime.DeepCopy()
		current.Phase = phase
		if phase != mirrorv1alpha1.SyncPhaseSucceeded && phase != mirrorv1alpha1.SyncPhaseFailed {
			return
		}
		now := r.now()
		mirror.Status.LastSync = &mirrorv1alpha1.MirrorSyncStatus{
			JobName: job.Name, Phase: phase, StartedAt: current.StartedAt.DeepCopy(), FinishedAt: finished.DeepCopy(), Message: message,
		}
		if phase == mirrorv1alpha1.SyncPhaseFailed {
			mirror.Status.LastAttempt = mirror.Status.LastSync.DeepCopy()
			mirror.Status.LastAttempt.StartedAt = current.QueuedAt.DeepCopy()
		}
		mirror.Status.NextSyncAt = timePtr(now.Add(mirror.Spec.Sync.Interval.Duration))
		mirror.Status.Sync.Phase = mirrorv1alpha1.SyncStateWaiting
		if phase == mirrorv1alpha1.SyncPhaseSucceeded {
			mirror.Status.LastSuccessfulSyncAt = finished.DeepCopy()
			mirror.Status.ConsecutiveFailures = 0
			current.Phase = mirrorv1alpha1.SyncPhaseSnapshotting
		} else if mirror.Status.ConsecutiveFailures < mirror.Spec.Sync.FailureRetryLimit {
			mirror.Status.ConsecutiveFailures++
			mirror.Status.NextSyncAt = timePtr(now.Add(mirror.Spec.Sync.RetryInterval.Duration))
			mirror.Status.Sync.Phase = mirrorv1alpha1.SyncStateRetrying
		}
		if phase == mirrorv1alpha1.SyncPhaseFailed {
			queueRequestCleanup(mirror, current.Manual, false)
			mirror.Status.CurrentSync = nil
		}
		if mirror.Spec.Sync.Paused {
			mirror.Status.PausedAt = timePtr(now)
		}
		applyMirrorConditions(mirror, publicationHealth{ready: mirrorWasReady(mirror), reason: "SynchronizationCompleted", message: "synchronization Job completed"}, "SynchronizationCompleted", message, nil)
	})
	if err == nil && mirror.Status.RequestCleanup != nil {
		_, err = r.reconcileRequestCleanup(ctx, mirror)
	}
	return err
}

// reconcileDelete drains a deleting Mirror in three strictly ordered phases
// before the finalizer is removed:
//
//  1. Workloads: the sync Jobs (mounting the sync PVC) and the publish
//     Deployments (mounting the snapshot-clone PVCs). Owner-reference GC
//     would remove them anyway, but only after the CR itself is gone — and
//     the CR cannot go while this finalizer blocks it. Deleting them here
//     first breaks the cycle that otherwise deadlocks deletion: the PVCs
//     cannot disappear while pvc-protection holds them for mounted pods, and
//     the pods live as long as their Job/Deployment does.
//  2. PVCs: the sync claim and the publish clones.
//  3. VolumeSnapshots — last, so every publish clone (a ZFS clone depends on
//     its origin snapshot) is gone before the snapshot it was cloned from.
//
// Each phase deletes its objects and requeues until a pass finds the list
// empty; only then does the next phase start. Workload deletion explicitly
// uses foreground propagation so Kubernetes drains dependent Pods instead of
// orphaning them and leaving PVC protection blocked indefinitely.
func (r *MirrorReconciler) reconcileDelete(ctx context.Context, mirror *mirrorv1alpha1.Mirror) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(mirror, MirrorFinalizer) {
		return ctrl.Result{}, nil
	}

	// Phase 1: workloads. Both kinds go in the same pass, and the phase
	// requeues while either list still returns objects.
	remaining, err := r.deleteOwnedChildren(ctx, mirror,
		&batchv1.JobList{}, &appsv1.DeploymentList{})
	if err != nil {
		return ctrl.Result{}, err
	}
	if remaining {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Phase 2: PVCs — the sync claim and the publish clones, the latter
	// before the snapshots they were cloned from.
	remaining, err = r.deleteOwnedChildren(ctx, mirror, &corev1.PersistentVolumeClaimList{})
	if err != nil {
		return ctrl.Result{}, err
	}
	if remaining {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Phase 3: VolumeSnapshots.
	remaining, err = r.deleteOwnedChildren(ctx, mirror, &snapshotv1.VolumeSnapshotList{})
	if err != nil {
		return ctrl.Result{}, err
	}
	if remaining {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	before := mirror.DeepCopy()
	controllerutil.RemoveFinalizer(mirror, MirrorFinalizer)
	return ctrl.Result{}, r.Patch(ctx, mirror, client.MergeFrom(before))
}

// deleteOwnedChildren uses labels to locate candidates, then verifies ownership.
// Only this Mirror's children keep a deletion phase pending. UID preconditions
// prevent deletion of a replacement object created after the list was observed.
func (r *MirrorReconciler) deleteOwnedChildren(ctx context.Context, mirror *mirrorv1alpha1.Mirror, lists ...client.ObjectList) (bool, error) {
	remaining := false
	for _, list := range lists {
		if err := r.List(ctx, list, client.InNamespace(mirror.Namespace), client.MatchingLabels{MirrorLabel: childBase(mirror.Name)}); err != nil {
			return remaining, err
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			return remaining, err
		}
		for _, item := range items {
			object, ok := item.(client.Object)
			if !ok {
				return remaining, fmt.Errorf("list item of %T is not a client.Object", item)
			}
			if !metav1.IsControlledBy(object, mirror) {
				continue
			}
			remaining = true
			if !object.GetDeletionTimestamp().IsZero() {
				continue
			}
			uid := object.GetUID()
			deleteOptions := []client.DeleteOption{&client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}}
			switch object.(type) {
			case *batchv1.Job, *appsv1.Deployment:
				// Never rely on API defaults: Job deletion may default to
				// orphan propagation, leaving mounted Pods behind permanently.
				deleteOptions = append(deleteOptions, client.PropagationPolicy(metav1.DeletePropagationForeground))
			}
			if err := r.Delete(ctx, object, deleteOptions...); err != nil && !apierrors.IsNotFound(err) {
				return remaining, err
			}
		}
	}
	return remaining, nil
}

func (r *MirrorReconciler) patchStatus(ctx context.Context, mirror *mirrorv1alpha1.Mirror, mutate func()) (ctrl.Result, error) {
	return r.patchStatusWithResult(ctx, mirror, ctrl.Result{}, mutate)
}

func (r *MirrorReconciler) patchStatusWithResult(ctx context.Context, mirror *mirrorv1alpha1.Mirror, result ctrl.Result, mutate func()) (ctrl.Result, error) {
	before := mirror.DeepCopy()
	mutate()
	r.updateSyncPhase(mirror)
	if err := r.Status().Patch(ctx, mirror, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

func (r *MirrorReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func timePtr(value time.Time) *metav1.Time {
	t := metav1.NewTime(value.UTC())
	return &t
}

func setCondition(mirror *mirrorv1alpha1.Mirror, conditionType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&mirror.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: mirror.Generation,
		Reason:             reason,
		Message:            message,
	})
}

func conditionStatus(value bool) metav1.ConditionStatus {
	if value {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func mirrorWasReady(mirror *mirrorv1alpha1.Mirror) bool {
	condition := meta.FindStatusCondition(mirror.Status.Conditions, conditionReady)
	return condition != nil && condition.Status == metav1.ConditionTrue
}

func proxyWasReady(proxy *mirrorv1alpha1.ProxyMirror) bool {
	condition := meta.FindStatusCondition(proxy.Status.Conditions, conditionReady)
	return condition != nil && condition.Status == metav1.ConditionTrue
}

type conditionFailure struct {
	reason  string
	message string
}

// publicationHealth describes the currently active generation independently
// of a newer synchronization transaction. In particular, a rolling
// Deployment can be available (Ready) and still converging (Progressing).
type publicationHealth struct {
	ready       bool
	progressing bool
	reason      string
	message     string
	failure     *conditionFailure
}

// reconcileActivePublication reconciles an idle active generation, but only
// observes it while synchronization or a pending publication owns the next generation. Re-applying ActivePVC during a
// pending publication would otherwise revert the Deployment away from the new
// PVC on every reconcile. Availability is intentionally weaker than rollout
// convergence: maxUnavailable=0 keeps an old pod serving while the new pod is
// coming up.
func (r *MirrorReconciler) reconcileActivePublication(ctx context.Context, mirror *mirrorv1alpha1.Mirror) (publicationHealth, error) {
	if !publishEnabled(mirror) {
		drained, err := publishPodsDrained(ctx, r.Client, mirror)
		if err != nil {
			return publicationHealth{}, err
		}
		// A redirect-mode http key routes without publishing: observe the
		// redirect route instead of declaring the endpoint disabled.
		if hostname, redirecting := mirror.Spec.Publish.HTTP.RedirectActive(); redirecting {
			return r.redirectPublicationHealth(ctx, mirror, hostname, drained)
		}
		return publicationHealth{progressing: !drained, reason: "HTTPDisabled", message: "no HTTP endpoint is configured; waiting for any removed workloads to drain"}, nil
	}

	if mirror.Status.ActivePVC == "" && mirror.Status.Publication == nil {
		return publicationHealth{reason: "Pending", message: "waiting for a ready snapshot to publish"}, nil
	}
	// Re-apply the active publication while no newer synchronization
	// transaction or publication is pending; the candidate would be undone by
	// re-asserting ActivePVC here.
	if mirror.Status.CurrentSync == nil && mirror.Status.Publication == nil {
		if _, err := r.ensurePublish(ctx, mirror, mirror.Status.ActivePVC); err != nil {
			return publicationHealth{}, err
		}
	}

	available, converged, err := observePublishChildren(ctx, r.Client, mirror)
	if err != nil {
		return publicationHealth{}, err
	}
	drained, err := publishPodsDrained(ctx, r.Client, mirror)
	if err != nil {
		return publicationHealth{}, err
	}
	converged = converged && drained
	// With http+rsync both serving, availability follows the user-facing http
	// endpoint alone. In redirect mode there is no http workload; rsync's own
	// availability stands, and the route is judged further down like any
	// publish HTTPRoute.
	if mirror.Spec.Publish.HTTP.Serving() {
		httpOnly := mirror.DeepCopy()
		httpOnly.Spec.Publish.Rsync = nil
		available, _, err = observePublishChildren(ctx, r.Client, httpOnly)
		if err != nil {
			return publicationHealth{}, err
		}
	}
	failure, err := publishDeploymentFailure(ctx, r.Client, mirror, mirrorPublishProtocols(mirror)...)
	if err != nil {
		return publicationHealth{}, err
	}
	health := publicationHealth{
		failure:     failure,
		ready:       available,
		progressing: !converged,
		reason:      "Published",
		message:     "all requested publish Deployments are available",
	}
	if !available {
		health.reason = "PublishUnavailable"
		health.message = "waiting for all requested publish Deployments and Services to become available"
	} else if !converged {
		health.reason = "PublishRollout"
		health.message = "the active publication remains available while a publish Deployment is rolling out"
	}

	if !publishHTTPEnabled(mirror) {
		health.ready = false
		health.reason = "HTTPDisabled"
		health.message = "no HTTP endpoint is configured"
		return health, nil
	}
	if !r.Config.PublishEnabled() {
		health.ready = false
		health.progressing = true
		health.reason = "HTTPRouteDisabled"
		health.message = "HTTP publishing is requested but route generation is disabled"
		health.failure = &conditionFailure{reason: health.reason, message: health.message}
		return health, nil
	}
	if err := ensurePublishedMirrorRoute(ctx, r, mirror); err != nil {
		return publicationHealth{}, err
	}
	routeState, routeMessage, err := publishRouteHealth(ctx, r.Client, mirror)
	if err != nil {
		return publicationHealth{}, err
	}
	switch routeState {
	case publishRouteRejected:
		health.ready = false
		health.progressing = true
		health.reason = "HTTPRouteRejected"
		health.message = routeMessage
		health.failure = &conditionFailure{reason: health.reason, message: routeMessage}
		if r.Recorder != nil {
			r.Recorder.Event(mirror, corev1.EventTypeWarning, "HTTPRouteRejected", routeMessage)
		}
	case publishRoutePending:
		health.ready = false
		health.progressing = true
		health.reason = "HTTPRoutePending"
		health.message = routeMessage
	}
	return health, nil
}

// redirectPublicationHealth observes a redirect-mode http key of an otherwise
// non-publishing Mirror (no serving http, no rsync): it maintains the redirect
// publish HTTPRoute and reports the gateway's acceptance of it, mirroring the
// serving tail of reconcileActivePublication. Readiness additionally requires
// removed serving workloads to drain — a serving -> redirect switch tears the
// http Deployment/Service down through cleanupDisabledPublishChildren.
func (r *MirrorReconciler) redirectPublicationHealth(ctx context.Context, mirror *mirrorv1alpha1.Mirror, hostname string, drained bool) (publicationHealth, error) {
	health := publicationHealth{
		ready:       drained,
		progressing: !drained,
		reason:      "RedirectActive",
		message:     fmt.Sprintf("every public path redirects to %s (302)", hostname),
	}
	if !r.Config.PublishEnabled() {
		health.ready = false
		health.progressing = true
		health.reason = "HTTPRouteDisabled"
		health.message = "redirect is requested but route generation is disabled"
		health.failure = &conditionFailure{reason: health.reason, message: health.message}
		return health, nil
	}
	if err := ensurePublishedMirrorRoute(ctx, r, mirror); err != nil {
		return publicationHealth{}, err
	}
	routeState, routeMessage, err := publishRouteHealth(ctx, r.Client, mirror)
	if err != nil {
		return publicationHealth{}, err
	}
	switch routeState {
	case publishRouteRejected:
		health.ready = false
		health.progressing = true
		health.reason = "HTTPRouteRejected"
		health.message = routeMessage
		health.failure = &conditionFailure{reason: health.reason, message: routeMessage}
		if r.Recorder != nil {
			r.Recorder.Event(mirror, corev1.EventTypeWarning, "HTTPRouteRejected", routeMessage)
		}
	case publishRoutePending:
		health.ready = false
		health.progressing = true
		health.reason = "HTTPRoutePending"
		health.message = routeMessage
	}
	return health, nil
}

// applyMirrorConditions is the single condition projection for a valid
// Mirror. Publication health, current synchronization activity and the last
// completed synchronization are orthogonal facts, so Ready and Degraded may
// both legitimately be true.
func applyMirrorConditions(mirror *mirrorv1alpha1.Mirror, publication publicationHealth, progressReason, progressMessage string, currentFailure *conditionFailure) {
	if publication.reason == "" {
		publication.reason = "PublicationObserved"
	}
	setCondition(mirror, conditionReady, conditionStatus(publication.ready), publication.reason, publication.message)
	if progressReason != "" {
		mirror.Status.Sync.Reason, mirror.Status.Sync.Message = progressReason, progressMessage
	}
	if mirror.Status.Publication != nil || publication.progressing {
		setCondition(mirror, conditionProgressing, metav1.ConditionTrue, publication.reason, publication.message)
	} else {
		setCondition(mirror, conditionProgressing, metav1.ConditionFalse, "Idle", "no publication is in progress")
	}

	failure := currentFailure
	if failure == nil {
		failure = publication.failure
	}
	if failure == nil && mirror.Status.LastSync != nil && mirror.Status.LastSync.Phase == mirrorv1alpha1.SyncPhaseFailed {
		failure = &conditionFailure{reason: "SynchronizationFailed", message: mirror.Status.LastSync.Message}
	}
	if failure != nil {
		setCondition(mirror, conditionDegraded, metav1.ConditionTrue, failure.reason, failure.message)
	} else {
		setCondition(mirror, conditionDegraded, metav1.ConditionFalse, "AsExpected", "")
	}
}

func validateMirror(mirror *mirrorv1alpha1.Mirror) field.ErrorList {
	path := field.NewPath("spec")
	var errs field.ErrorList
	if mirror.Spec.Sync.Interval.Duration <= 0 {
		errs = append(errs, field.Invalid(path.Child("sync", "interval"), mirror.Spec.Sync.Interval.Duration.String(), "must be greater than zero"))
	}
	if mirror.Spec.Sync.RetryInterval.Duration <= 0 {
		errs = append(errs, field.Invalid(path.Child("sync", "retryInterval"), mirror.Spec.Sync.RetryInterval.Duration.String(), "must be greater than zero"))
	}
	if mirror.Spec.Sync.Timeout.Duration <= 0 {
		errs = append(errs, field.Invalid(path.Child("sync", "timeout"), mirror.Spec.Sync.Timeout.Duration.String(), "must be greater than zero"))
	}
	// sync.podTemplate: at least one container with an image (the former
	// sync.image/sync.command requirements moved into the template), and no
	// user volume clashing with the injected writable sync-data volume.
	syncTemplatePath := path.Child("sync", "podTemplate")
	if len(mirror.Spec.Sync.PodTemplate.Spec.Containers) == 0 {
		errs = append(errs, field.Required(syncTemplatePath.Child("spec", "containers"), "must declare at least one container"))
	} else if mirror.Spec.Sync.PodTemplate.Spec.Containers[0].Image == "" {
		errs = append(errs, field.Required(syncTemplatePath.Child("spec", "containers").Index(0).Child("image"), "must not be empty"))
	}
	for i := range mirror.Spec.Sync.PodTemplate.Spec.Volumes {
		if mirror.Spec.Sync.PodTemplate.Spec.Volumes[i].Name == SyncDataVolumeName {
			errs = append(errs, field.Invalid(syncTemplatePath.Child("spec", "volumes").Index(i).Child("name"), SyncDataVolumeName,
				"this volume name is reserved: the controller injects it itself"))
		}
	}
	storagePath := path.Child("storage", "pvcTemplate")
	if len(mirror.Spec.Storage.PVCSpec.AccessModes) == 0 {
		errs = append(errs, field.Required(storagePath.Child("accessModes"), "must declare at least one access mode"))
	}
	requestedStorage := mirror.Spec.Storage.PVCSpec.Resources.Requests[corev1.ResourceStorage]
	if requestedStorage.IsZero() || requestedStorage.Sign() < 0 {
		errs = append(errs, field.Required(storagePath.Child("resources", "requests", string(corev1.ResourceStorage)), "must be greater than zero"))
	}
	if mirror.Spec.Storage.SyncStorageClassName == "" {
		errs = append(errs, field.Required(path.Child("storage", "syncStorageClassName"), "must not be empty"))
	}
	if mirror.Spec.Storage.PublishStorageClassName == "" {
		errs = append(errs, field.Required(path.Child("storage", "publishStorageClassName"), "must not be empty"))
	}
	// volumeName is deliberately absent from the rejection list: it is the
	// one PVC field allowed to differ between the sync and publish claims —
	// the sync claim may pre-bind an existing PV with it (cross-instance
	// migration), while newDataClaim strips it for the publish clones.
	if mirror.Spec.Storage.PVCSpec.StorageClassName != nil || mirror.Spec.Storage.PVCSpec.DataSource != nil || mirror.Spec.Storage.PVCSpec.DataSourceRef != nil || mirror.Spec.Storage.PVCSpec.Selector != nil {
		errs = append(errs, field.Invalid(storagePath, mirror.Spec.Storage.PVCSpec, "storageClassName, dataSource, dataSourceRef, and selector are Falcon-managed or unsupported in pvcTemplate"))
	}
	if mirror.Spec.Storage.VolumeSnapshotClassName == "" {
		errs = append(errs, field.Required(path.Child("storage", "volumeSnapshotClassName"), "is required for atomic publication"))
	}
	services := mirror.Spec.Publish
	servicesPath := path.Child("services")
	// The http key either serves (a containerful podTemplate; redirect ignored)
	// or redirects (no serving podTemplate, a redirect hostname). The CRD
	// enforces the either-or at admission (CEL on MirrorHTTPServiceSpec);
	// these checks keep the InvalidSpec path complete for specs that bypassed it.
	if http := services.HTTP; http != nil {
		httpPath := servicesPath.Child("http")
		switch {
		case http.Serving():
			errs = append(errs, validateMirrorService(&http.MirrorServiceSpec, httpPath)...)
		case http.Redirect == "":
			errs = append(errs, field.Required(httpPath, "must declare a serving podTemplate (spec.containers) or a redirect"))
		default:
			errs = append(errs, validateRedirectHostname(http.Redirect, httpPath.Child("redirect"))...)
		}
		// Alias paths of a declared http service apply in BOTH modes; an
		// absent key may park anything.
		errs = append(errs, validateHTTPAliases(http, mirror.Name, httpPath.Child("aliases"))...)
	}
	if services.Rsync != nil {
		errs = append(errs, validateMirrorService(services.Rsync, servicesPath.Child("rsync"))...)
	}
	// There is no placement validation: node locality is K8s-native on both
	// sides (the bound PV's nodeAffinity, enforced by the scheduler). The
	// choice of the publish StorageClass (Falcon recommends an
	// Immediate-binding one, see the field's documentation) is entirely the
	// operator's. Multi-replica publishing on shared (RWX) storage is a legal
	// extension.
	return errs
}

// publishEnabled reports whether at least one spec.publish key is enabled
// (a mirror with everything disabled syncs but publishes nothing).
func publishEnabled(mirror *mirrorv1alpha1.Mirror) bool {
	return mirror.Spec.Publish.AnyEnabled()
}

// redirectHostnamePattern is the admission-time Pattern of the redirect field
// — the gateway API's PreciseHostname shape (a lowercase DNS hostname; no
// scheme, port, or path), so the value maps onto the generated
// RequestRedirect filter hostname without further normalization.
var redirectHostnamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// validateRedirectHostname checks the redirect target of a redirect-mode http
// service. The schema Pattern is the admission-time gate; this mirror of it
// keeps the InvalidSpec path complete for specs that bypassed admission.
func validateRedirectHostname(hostname string, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	if len(hostname) > 253 || !redirectHostnamePattern.MatchString(hostname) {
		errs = append(errs, field.Invalid(path, hostname, "must be a lowercase DNS hostname (no scheme, port, or path)"))
	}
	return errs
}

// validateMirrorService validates one ENABLED publish service of a Mirror
// (absent keys are skipped entirely — anything may be parked there). The CRD
// additionally enforces the declaration-time presence of podTemplate.spec at
// admission; the controller-side checks keep the InvalidSpec path complete
// for specs that bypassed it.
func validateMirrorService(service *mirrorv1alpha1.MirrorServiceSpec, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	templatePath := path.Child("podTemplate")
	errs = append(errs, validatePublishPodTemplate(&service.PodTemplate, templatePath, PublishDataVolumeName)...)
	// Data integrity: every mount of the controller-injected mirror-data
	// volume — including extra user mounts in sidecars or init containers —
	// must be read-only. The controller forces the volume source read-only
	// regardless, but a writable mount attempt must not slip through silently.
	for i := range service.PodTemplate.Spec.Containers {
		container := &service.PodTemplate.Spec.Containers[i]
		errs = append(errs, validateMirrorDataMounts(container.VolumeMounts, templatePath.Child("spec", "containers").Index(i))...)
	}
	for i := range service.PodTemplate.Spec.InitContainers {
		container := &service.PodTemplate.Spec.InitContainers[i]
		errs = append(errs, validateMirrorDataMounts(container.VolumeMounts, templatePath.Child("spec", "initContainers").Index(i))...)
	}
	return errs
}

// validateMirrorDataMounts rejects writable volumeMounts of the injected
// mirror-data publish PVC volume.
func validateMirrorDataMounts(mounts []corev1.VolumeMount, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	for i := range mounts {
		if mounts[i].Name == PublishDataVolumeName && !mounts[i].ReadOnly {
			errs = append(errs, field.Invalid(path.Child("volumeMounts").Index(i).Child("readOnly"), false,
				"mounts of the injected mirror-data publish PVC volume must always be read-only"))
		}
	}
	return errs
}

// validateHTTPAliases validates the additional public path prefixes of an
// ENABLED http service: no duplicate, no alias equal to the canonical
// /<mirror name> path, and the syntax rules (start with '/', no trailing '/',
// no '//', no whitespace). This controller-side check is the SOLE syntax
// enforcement (the CRD carries only the MaxItems/MaxLength bounds — a CEL
// mirror of these rules was deliberately dropped); keeping it in the
// InvalidSpec path buys precise error messages. Cross-route precedence is the
// gateway's business (see routeGatewayRejection). Case-sensitive on purpose:
// CR names are bound by DNS rules while alias paths are not.
func validateHTTPAliases(http *mirrorv1alpha1.MirrorHTTPServiceSpec, mirrorName string, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	canonical := "/" + mirrorName
	seen := map[mirrorv1alpha1.MirrorHTTPAlias]bool{}
	for i, alias := range http.Aliases {
		aliasPath := path.Index(i)
		value := string(alias)
		if seen[alias] {
			errs = append(errs, field.Duplicate(aliasPath, value))
			continue
		}
		seen[alias] = true
		if value == canonical {
			errs = append(errs, field.Invalid(aliasPath, value, "must not equal the canonical path "+canonical))
			continue
		}
		switch {
		case !strings.HasPrefix(value, "/"):
			errs = append(errs, field.Invalid(aliasPath, value, "must start with '/'"))
		case strings.HasSuffix(value, "/"):
			errs = append(errs, field.Invalid(aliasPath, value, "must not end with '/'"))
		case strings.Contains(value, "//"):
			errs = append(errs, field.Invalid(aliasPath, value, "must not contain '//'"))
		case strings.ContainsFunc(value, unicode.IsSpace):
			errs = append(errs, field.Invalid(aliasPath, value, "must not contain whitespace"))
		}
	}
	return errs
}

func jobSucceeded(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobFailed(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return job.Status.Failed > 0 && job.Status.Active == 0
}

func jobFailureMessage(job *batchv1.Job) string {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			if condition.Message != "" {
				return condition.Message
			}
			return condition.Reason
		}
	}
	return fmt.Sprintf("Job %s failed", job.Name)
}
